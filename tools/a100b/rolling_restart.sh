#!/bin/bash
# rolling_restart.sh -- restart the oaica-35b-a3b-vision replicas ONE AT A TIME
# so the fleet keeps serving (e.g. after changing launch flags in
# vllm_awq_watchdog.sh, which the watchdog only applies on relaunch).
#
# Why this exists: a hand-rolled loop on 2026-08-29 checked "is :PORT UP"
# right after sending SIGTERM -- the load balancer's status still showed the
# OLD process as UP for a moment, the check passed instantly, and all three
# replicas were killed within one second: a fleet-wide outage of ~4 minutes
# instead of a rolling restart. The rules below make that impossible:
#   1. record the OLD api_server pid;
#   2. SIGTERM it and wait until that exact pid is GONE;
#   3. wait until a NEW api_server pid exists for the port (the watchdog
#      relaunches it) AND the LB reports "UP ... probe=ok" for the port;
#   4. only then move to the next replica;
#   5. before killing anything, require at least MIN_HEALTHY other replicas
#      to be UP so a single sick replica never takes the fleet with it.
#
# Usage: /workspace/rolling_restart.sh [port ...]   (default: all in REPLICAS)
# Requires the watchdog to be running (it does the relaunching).
set -u
STATUS_URL=${STATUS_URL:-http://127.0.0.1:8092/status}
REPLICAS="${REPLICAS:-0:30106 1:30108 2:30110}"
BOOT_CAP_SEC=${BOOT_CAP_SEC:-600}
MIN_HEALTHY=${MIN_HEALTHY:-1}

log() { echo "$(date -u +%H:%M:%S) $*"; }
api_pid() { pgrep -f "^python3 -m vllm.entrypoints.openai.api_server.*--port $1 " | head -1; }
healthy_count() { curl -s -m 5 "$STATUS_URL" | grep -c "UP .*probe=ok"; }
port_healthy() { curl -s -m 5 "$STATUS_URL" | grep -q "$1 UP .*probe=ok"; }

pgrep -f '^/bin/bash /workspace/vllm_awq_watchdog.sh$' >/dev/null || { log "ABORT: watchdog is not running -- nothing would relaunch the replicas"; exit 2; }

ports=("$@")
if [ ${#ports[@]} -eq 0 ]; then for r in $REPLICAS; do ports+=("${r##*:}"); done; fi

for port in "${ports[@]}"; do
  old=$(api_pid "$port")
  others=$(( $(healthy_count) - $(port_healthy "$port" && echo 1 || echo 0) ))
  if [ "$others" -lt "$MIN_HEALTHY" ]; then
    log "ABORT before :$port -- only $others other healthy replica(s), need >= $MIN_HEALTHY"; exit 3
  fi
  if [ -z "$old" ]; then log ":$port has no api_server (already down); waiting for the watchdog to bring it up"; else
    log ":$port old pid=$old -> SIGTERM"; kill "$old"
    t0=$SECONDS
    while kill -0 "$old" 2>/dev/null; do
      [ $((SECONDS-t0)) -ge 60 ] && { log ":$port pid $old ignored SIGTERM for 60s -> SIGKILL"; kill -9 "$old"; }
      sleep 2
    done
    log ":$port old pid gone after $((SECONDS-t0))s"
  fi
  t0=$SECONDS
  while :; do
    new=$(api_pid "$port")
    if [ -n "$new" ] && [ "$new" != "$old" ] && port_healthy "$port"; then break; fi
    [ $((SECONDS-t0)) -ge "$BOOT_CAP_SEC" ] && { log "ABORT: :$port not healthy after ${BOOT_CAP_SEC}s (new pid=${new:-none})"; exit 4; }
    sleep 10
  done
  log ":$port UP again, new pid=$new after $((SECONDS-t0))s, cmdline has: $(tr '\0' ' ' </proc/$new/cmdline | grep -o -- '--speculative-config [^ ]*' | cut -c1-90)"
done
log "rolling restart complete"; curl -s -m 5 "$STATUS_URL"
