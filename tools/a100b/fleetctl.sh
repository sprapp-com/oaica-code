#!/bin/bash
# fleetctl.sh -- add/remove vLLM replicas on a100b WITHOUT restarting the
# watchdog and WITHOUT dropping a single in-flight request.
#
# Deployed at /workspace/fleetctl.sh.
#
# Two things make this necessary (both real, 2026-08-30):
#   1. The replica set used to live inside vllm_awq_watchdog.sh, so changing
#      it meant kill+restart of the watchdog. During one such restart a
#      `pgrep -f`/`ps|grep` kill loop matched the INVOKING SSH SHELL's own
#      argv (the pattern was sitting in that shell's command line) and killed
#      the deploy session mid-flight -- leaving no watchdog running at all.
#   2. In another, a long-lived deploy shell's argv matched the watchdog's
#      booting() pattern ("api_server.*--port PORT "), so the watchdog
#      believed a replica was still booting and refused to relaunch it for 8
#      minutes.
# Hence two hard rules in this script:
#   * never `pkill -f`; find the process by the PID that OWNS THE LISTENING
#     SOCKET (`ss -ltnp`) and confirm it via /proc/<pid>/cmdline, or
#   * when a pattern must be used, COMPOSE it so this script's own argv can
#     never contain the literal being matched (pat="${A}server..." with
#     A=api_ -- our argv holds '${A}server', the target's holds 'api_server').
#
# Ordering guarantees (this is the whole point):
#   add    launch -> replica answers /v1/models 200 -> optional validation
#          -> ONLY THEN enter the LB. No cold backend ever receives traffic.
#   remove leave the LB (SIGHUP, LB stops routing new requests) -> wait for
#          established connections to drain -> leave the conf (watchdog will
#          not relaunch) -> ONLY THEN SIGTERM. No in-flight request is cut.
#
# Usage:
#   fleetctl.sh status
#   fleetctl.sh add    GPU:PORT
#   fleetctl.sh remove GPU:PORT
#   fleetctl.sh drain  GPU:PORT   (same as remove, but leaves it running)
set -u

CONF=${CONF:-/workspace/vllm_awq_replicas.conf}
LBCFG=${LBCFG:-/workspace/oaicalb.json}
LBRELOAD=${LBRELOAD:-/workspace/oaicalb_reload.sh}
LBSTATUS=${LBSTATUS:-http://127.0.0.1:8092/status}
VALIDATE=${VALIDATE:-/workspace/validate_replica.sh}
UP_TIMEOUT=${UP_TIMEOUT:-400}
DRAIN_TIMEOUT=${DRAIN_TIMEOUT:-600}
TERM_GRACE=${TERM_GRACE:-30}
GPU_BUSY_MIB=${GPU_BUSY_MIB:-2048}

say() { echo "$(date -Is) $*"; }
die() { echo "$(date -Is) ERROR: $*" >&2; exit 1; }

usage() {
  cat <<'EOF'
usage: fleetctl.sh <command> [GPU:PORT]

  status              conf, per-replica pid/listening/LB view, GPU memory
  add    GPU:PORT     launch (via watchdog) then, once UP, add to the LB
  remove GPU:PORT     drop from LB, drain, drop from conf, stop the replica
  drain  GPU:PORT     drop from LB and drain only (replica keeps running)

GPU is 0-7, PORT is the api_server port, e.g. fleetctl.sh add 3:30114
EOF
}

# ---------------------------------------------------------------- helpers

parse_spec() {
  local spec=$1
  [[ $spec =~ ^[0-7]:[0-9]+$ ]] || die "bad GPU:PORT '$spec' (want e.g. 3:30114)"
  GPU=${spec%%:*}
  PORT=${spec##*:}
}

conf_read() {
  [ -r "$CONF" ] || { echo ""; return 0; }
  tr '\n\t' '  ' < "$CONF" | tr -s ' ' | sed -e 's/^ *//' -e 's/ *$//'
}

# conf_write ENTRIES...: dedup by gpu:port, sort by gpu (then port), one line.
conf_write() {
  local sorted
  sorted=$(printf '%s\n' "$@" | sed '/^$/d' | sort -u -t: -k1,1n -k2,2n | tr '\n' ' ' \
    | sed -e 's/ *$//')
  printf '%s\n' "$sorted" > "$CONF"
  say "conf $CONF is now: $sorted"
}

# listener_pid PORT: pid of the process that OWNS the listening socket.
# This is the only identification that cannot match our own shell -- a shell
# does not hold a listening socket on the replica's port.
listener_pid() {
  ss -ltnp 2>/dev/null | awk -v p=":$1 " 'index($4, p)' \
    | grep -oE 'pid=[0-9]+' | head -1 | cut -d= -f2
}

listening() { ss -ltn 2>/dev/null | grep -q ":$1 "; }

# api_server_pid PORT: the vLLM api_server serving PORT. Preferred source is
# the LISTENING socket's owner; the fallback scan reads /proc/<pid>/cmdline
# directly and matches a COMPOSED pattern, so this script's own argv (which
# contains the literal '${A}server', not 'api_server') can never self-match.
api_server_pid() {
  local port=$1 pid A pat cl
  pid=$(listener_pid "$port")
  if [ -n "${pid:-}" ] && [ -r "/proc/$pid/cmdline" ]; then
    cl=$(tr '\0' ' ' < "/proc/$pid/cmdline")
    A=api_
    pat="${A}server.*--port $port "
    if [[ $cl =~ $pat ]]; then echo "$pid"; return 0; fi
  fi
  A=api_
  pat="${A}server.*--port $port "
  for pid in $(ls /proc 2>/dev/null | grep -E '^[0-9]+$'); do
    [ "$pid" = "$$" ] && continue
    [ -r "/proc/$pid/cmdline" ] || continue
    cl=$(tr '\0' ' ' < "/proc/$pid/cmdline" 2>/dev/null) || continue
    [[ $cl =~ $pat ]] || continue
    echo "$pid"
    return 0
  done
  return 1
}

established_count() {
  ss -tn state established "( sport = :$1 )" 2>/dev/null | tail -n +2 | grep -c . || true
}

gpu_used_mib() {
  nvidia-smi --id="$1" --query-gpu=memory.used --format=csv,noheader,nounits 2>/dev/null \
    | tr -d ' ' | head -1
}

lb_backends() {
  python3 - "$LBCFG" <<'PY' 2>/dev/null
import json,sys
d=json.load(open(sys.argv[1]))
for b in (d.get("backends") or d.get("backend_configs") or []):
    print(b if isinstance(b,str) else b.get("url",""))
PY
}

# lb_set add|remove URL: rewrite oaicalb.json's backend list, then SIGHUP.
lb_set() {
  local op=$1 url=$2 rc
  [ -w "$LBCFG" ] || die "$LBCFG not writable"
  python3 - "$LBCFG" "$op" "$url" <<'PY'
import json,sys
path,op,url=sys.argv[1],sys.argv[2],sys.argv[3]
d=json.load(open(path))
key="backends" if "backends" in d else "backend_configs"
bl=d.get(key) or []
def u(b): return b if isinstance(b,str) else b.get("url","")
if op=="add":
    if url not in [u(b) for b in bl]:
        bl.append(url)
else:
    bl=[b for b in bl if u(b)!=url]
d[key]=bl
json.dump(d,open(path,"w"),indent=2)
open(path,"a").write("\n")
print("backends now:", [u(b) for b in bl])
PY
  rc=$?
  [ $rc -eq 0 ] || die "failed to $op $url in $LBCFG"
  if [ -x "$LBRELOAD" ]; then
    say "reloading load balancer ($LBRELOAD)"
    "$LBRELOAD" || die "LB reload failed -- $LBCFG was already edited, fix and re-run $LBRELOAD"
  else
    say "WARNING: $LBRELOAD missing/not executable -- LB NOT reloaded"
  fi
}

# ---------------------------------------------------------------- status

cmd_status() {
  local set entry gpu port pid used
  set=$(conf_read)
  echo "conf ($CONF): ${set:-<empty or missing>}"
  echo
  printf '%-10s %-10s %-10s %s\n' GPU PORT PID LISTENING
  for entry in $set; do
    gpu=${entry%%:*}; port=${entry##*:}
    pid=$(api_server_pid "$port" || true)
    printf '%-10s %-10s %-10s %s\n' "$gpu" "$port" "${pid:-none}" \
      "$(listening "$port" && echo yes || echo NO)"
  done
  echo
  echo "load balancer backends ($LBCFG):"
  lb_backends | sed 's/^/  /'
  echo
  echo "load balancer /status:"
  curl -s -m 5 "$LBSTATUS" | sed 's/^/  /' || echo "  (no response from $LBSTATUS)"
  echo
  echo "GPU memory:"
  nvidia-smi --query-gpu=index,memory.used,memory.total --format=csv 2>/dev/null | sed 's/^/  /' \
    || echo "  (nvidia-smi unavailable)"
}

# ---------------------------------------------------------------- add

cmd_add() {
  parse_spec "$1"
  local url="http://127.0.0.1:$PORT" set used pid deadline

  say "step 1/6: preflight for GPU${GPU} :${PORT}"
  if listening "$PORT"; then
    die "port $PORT is already listening (pid $(listener_pid "$PORT")) -- refusing"
  fi
  used=$(gpu_used_mib "$GPU")
  [ -n "${used:-}" ] || die "cannot read nvidia-smi memory for GPU$GPU -- refusing"
  if [ "$used" -gt "$GPU_BUSY_MIB" ]; then
    die "GPU$GPU has ${used} MiB in use by other processes (> ${GPU_BUSY_MIB} MiB) -- refusing"
  fi
  say "  GPU$GPU free (${used} MiB used), port $PORT free"

  set=$(conf_read)
  case " $set " in
    *" $GPU:$PORT "*) say "step 2/6: $GPU:$PORT already in conf" ;;
    *) say "step 2/6: adding $GPU:$PORT to $CONF"
       # shellcheck disable=SC2086
       conf_write $set "$GPU:$PORT" ;;
  esac

  say "step 3/6: waiting for the watchdog to launch it and for /v1/models 200 (up to ${UP_TIMEOUT}s)"
  deadline=$(( $(date +%s) + UP_TIMEOUT ))
  while true; do
    if [ "$(curl -s -o /dev/null -m 5 -w '%{http_code}' "$url/v1/models" 2>/dev/null)" = "200" ]; then
      say "  :$PORT answered /v1/models 200"
      break
    fi
    if [ "$(date +%s)" -ge "$deadline" ]; then
      die ":$PORT did not come up within ${UP_TIMEOUT}s -- it is in the conf (watchdog keeps retrying) but NOT in the LB; check /workspace/vllm_awq_gpu${GPU}.log"
    fi
    sleep 5
  done

  say "step 4/6: validation"
  if [ -x "$VALIDATE" ]; then
    "$VALIDATE" "$PORT" || die "$VALIDATE failed for :$PORT -- NOT adding to the LB"
    say "  $VALIDATE passed"
  else
    say "  $VALIDATE not present -- skipped"
  fi

  say "step 5/6: adding $url to the load balancer (replica is UP first, by design)"
  lb_set add "$url"

  pid=$(api_server_pid "$PORT" || true)
  say "step 6/6: done -- GPU${GPU} :${PORT} serving (pid ${pid:-unknown}) and in the LB"
}

# ---------------------------------------------------------------- drain

# drain_out_of_lb: LB first, then wait for established connections to finish.
drain_out_of_lb() {
  local port=$1 url="http://127.0.0.1:$1" n deadline last=0
  say "step 1/3: removing $url from the load balancer (stops NEW requests being routed)"
  lb_set remove "$url"

  say "step 2/3: draining established connections on :$port (up to ${DRAIN_TIMEOUT}s)"
  deadline=$(( $(date +%s) + DRAIN_TIMEOUT ))
  while true; do
    n=$(established_count "$port")
    if [ "${n:-0}" -eq 0 ]; then
      say "  drained: 0 established connections on :$port"
      return 0
    fi
    if [ "$(date +%s)" -ge "$deadline" ]; then
      say "  WARNING: ${n} connection(s) still established after ${DRAIN_TIMEOUT}s -- continuing anyway"
      return 0
    fi
    if [ $(( $(date +%s) - last )) -ge 15 ]; then
      say "  ${n} established connection(s) remaining on :$port"
      last=$(date +%s)
    fi
    sleep 3
  done
}

cmd_drain() {
  parse_spec "$1"
  drain_out_of_lb "$PORT"
  say "step 3/3: leaving the replica RUNNING (drain only) -- it is still in $CONF, so the watchdog keeps supervising it"
}

# ---------------------------------------------------------------- remove

sweep_orphans_once() {
  local pid ppid comm found=0
  for pid in $(ls /proc 2>/dev/null | grep -E '^[0-9]+$'); do
    [ -r "/proc/$pid/cmdline" ] || continue
    comm=$(tr '\0' ' ' < "/proc/$pid/cmdline" 2>/dev/null) || continue
    case "$comm" in *VLLM::EngineCore*) ;; *) continue ;; esac
    ppid=$(awk '{print $4}' "/proc/$pid/stat" 2>/dev/null)
    if [ "${ppid:-}" = "1" ]; then
      say "  killing orphaned VLLM::EngineCore pid $pid (reparented to init)"
      kill -9 "$pid" 2>/dev/null || true
      found=1
    fi
  done
  [ "$found" = 0 ] && say "  no orphaned VLLM::EngineCore found"
  return 0
}

cmd_remove() {
  parse_spec "$1"
  local set new entry pid deadline

  drain_out_of_lb "$PORT"

  say "step 3/5: removing $GPU:$PORT from $CONF (so the watchdog will NOT relaunch it)"
  set=$(conf_read)
  new=""
  for entry in $set; do
    [ "$entry" = "$GPU:$PORT" ] && continue
    new="$new $entry"
  done
  # shellcheck disable=SC2086
  conf_write $new

  say "step 4/5: stopping the api_server on :$PORT"
  pid=$(api_server_pid "$PORT" || true)
  if [ -z "${pid:-}" ]; then
    say "  no api_server found on :$PORT (already stopped)"
  else
    say "  SIGTERM to pid $pid ($(tr '\0' ' ' < "/proc/$pid/cmdline" 2>/dev/null | cut -c1-90))"
    kill -TERM "$pid" 2>/dev/null || true
    deadline=$(( $(date +%s) + TERM_GRACE ))
    while kill -0 "$pid" 2>/dev/null; do
      if [ "$(date +%s)" -ge "$deadline" ]; then
        say "  pid $pid ignored SIGTERM for ${TERM_GRACE}s -- SIGKILL"
        kill -9 "$pid" 2>/dev/null || true
        sleep 3
        break
      fi
      sleep 2
    done
    kill -0 "$pid" 2>/dev/null && say "  WARNING: pid $pid still alive after SIGKILL" \
      || say "  pid $pid is gone"
  fi

  say "step 5/5: orphan sweep + GPU$GPU memory"
  sweep_orphans_once
  sleep 3
  nvidia-smi --id="$GPU" --query-gpu=index,memory.used,memory.total --format=csv 2>/dev/null \
    | sed 's/^/  /' || say "  (nvidia-smi unavailable)"
  say "GPU${GPU} :${PORT} removed"
}

# ---------------------------------------------------------------- main

[ $# -ge 1 ] || { usage; exit 2; }
cmd=$1; shift || true
case "$cmd" in
  status) [ $# -eq 0 ] || { usage; exit 2; }; cmd_status ;;
  add)    [ $# -eq 1 ] || { usage; exit 2; }; cmd_add "$1" ;;
  remove) [ $# -eq 1 ] || { usage; exit 2; }; cmd_remove "$1" ;;
  drain)  [ $# -eq 1 ] || { usage; exit 2; }; cmd_drain "$1" ;;
  -h|--help|help) usage ;;
  *) echo "unknown command: $cmd" >&2; usage; exit 2 ;;
esac
