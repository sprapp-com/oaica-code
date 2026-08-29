#!/bin/bash
# GPU7 Nemotron sibling of vllm_awq_watchdog.sh. Same supervisor structure
# (preflight, restore, backoff, caches, stall probing, oom_watch,
# protect_oom, orphan sweep) applied to a single replica of a different
# model on GPU7, fronted by its own oaicalb on :30120
# (/workspace/nemotron-oaicalb.json).
#
# Why each piece exists -- see vllm_awq_watchdog.sh for the original
# incident history this design is copied from; summarized here:
#   preflight   weights/tokenizer can vanish from /dev/shm at any time
#               (another session's cleanup, node restart); relaunching
#               blind into missing config.json crash-loops forever.
#   restore     re-download on missing weights.
#   backoff     a launch that dies in <60s is a config/weights problem,
#               not a transient; exponential backoff avoids hammering the
#               GPU and the log.
#   caches      VLLM_CACHE_ROOT / TORCHINDUCTOR_CACHE_DIR off the 16G root
#               overlay, which fills fast and wedges the process.
#   stall       GET /v1/models can stay 200 while the scheduler is wedged;
#               only a real chat completion proves liveness. stall_check
#               (via chat_probe_ok/making_progress/force_kill_replica)
#               automates detect -> confirm-not-busy -> kill -> orphan
#               sweep.
#
# Deployed at /workspace/nemotron_watchdog.sh.
# Usage: nohup /workspace/nemotron_watchdog.sh >/workspace/nemotron_watchdog.out 2>&1 &
set -u

# NemotronHForCausalLM (hybrid Mamba/MoE), compressed-tensors W4A16.
# No MTP/speculative config, no vision. TOK_SHA is left EMPTY by default:
# this model's tokenizer_config.json ships its own working chat_template,
# so there is no vendored patch to apply here (unlike kat_awq's need for
# an external chat_template restore). If a future re-download regresses
# the chat_template, pin TOK_SHA to the known-good hash and fill in
# TOK_PATCH with a vendored copy:
#   sha256sum "$MODEL_DIR/tokenizer.json"
# tokenizer_ok() below treats an empty TOK_SHA as "skip the check".
MODEL_DIR=/dev/shm/nemotron35_lightning_30b_a3b
HF_REPO=useful-quants/NVIDIA-Nemotron-3.5-Lightning-30B-A3B-W4A16
HF_REV=main
TOK_PATCH=/workspace/nemotron.tokenizer_config.json   # last-resort restore target only; unused while TOK_SHA is empty
TOK_SHA=""
LOG=/workspace/nemotron_watchdog.log
ALERT=/workspace/nemotron_watchdog.ALERT           # touched on any restore/backoff/stall-kill; poll it

# STALL_PROBE_MODEL: served-model-name to probe with a real 1-token chat
# completion (not just GET /v1/models, which stays 200 through a wedged
# scheduler). STALL_PROBE_TIMEOUT_SEC bounds one probe attempt.
# STALL_FAIL_THRESHOLD consecutive chat-probe failures/timeouts, while the
# port is still LISTENING (i.e. not caught by the ordinary crash path
# above), before a replica is force-killed and relaunched.
STALL_PROBE_MODEL="${STALL_PROBE_MODEL:-oaica-nemotron-30b-a3b}"
STALL_PROBE_TIMEOUT_SEC="${STALL_PROBE_TIMEOUT_SEC:-15}"
STALL_FAIL_THRESHOLD="${STALL_FAIL_THRESHOLD:-3}"

mkdir -p /workspace/.vllm_cache /workspace/.torch_cache
export LD_LIBRARY_PATH=/usr/local/lib/python3.12/dist-packages/nvidia/cu13/lib
export VLLM_CACHE_ROOT=/workspace/.vllm_cache
export TORCHINDUCTOR_CACHE_DIR=/workspace/.torch_cache
export HF_HOME=/workspace/.hf_home

log() { echo "$(date -Is) $*" | tee -a "$LOG"; }
alert() { log "ALERT: $*"; echo "$(date -Is) $*" >> "$ALERT"; }

# weights_ok: every file vLLM opens at startup is present.
weights_ok() {
  # This W4A16 repo ships ONE model.safetensors (17 GB, no index); accept
  # that or the sharded index+model-*.safetensors layout the KAT repo uses.
  [ -f "$MODEL_DIR/config.json" ] && [ -f "$MODEL_DIR/tokenizer_config.json" ] || return 1
  [ -f "$MODEL_DIR/model.safetensors" ] && return 0
  [ -f "$MODEL_DIR/model.safetensors.index.json" ] && ls "$MODEL_DIR"/model-*.safetensors >/dev/null 2>&1
}

# tokenizer_ok: skipped (returns true) when TOK_SHA is empty -- this model's
# own tokenizer_config.json is trusted as-is. Fill TOK_SHA in to enable the
# sha256 gate the same way the production watchdog does.
tokenizer_ok() {
  [ -z "$TOK_SHA" ] && return 0
  [ -f "$MODEL_DIR/tokenizer_config.json" ] \
    && [ "$(sha256sum "$MODEL_DIR/tokenizer_config.json" | cut -d' ' -f1)" = "$TOK_SHA" ]
}

restore_weights() {
  alert "weights missing at $MODEL_DIR -- re-downloading $HF_REPO@$HF_REV"
  python3 - <<PY
from huggingface_hub import snapshot_download
snapshot_download(repo_id="$HF_REPO", revision="$HF_REV", local_dir="$MODEL_DIR",
                  allow_patterns=["*.safetensors","*.json","*.txt"])
PY
}

restore_tokenizer() {
  if [ ! -f "$TOK_PATCH" ]; then
    alert "tokenizer patch $TOK_PATCH missing; chat completions WILL 400 until it is restored"
    return 1
  fi
  log "applying vendored tokenizer_config.json (chat_template patch)"
  install -m 0644 "$TOK_PATCH" "$MODEL_DIR/tokenizer_config.json"
}

preflight() {
  weights_ok || restore_weights || return 1
  tokenizer_ok || restore_tokenizer || return 1
  return 0
}

launch() {
  local gpu=$1 port=$2 logfile=$3
  # Preserve the PREVIOUS boot's full log before truncating for the new one
  # -- `> "$logfile"` below destroys whatever crash evidence was in there.
  # Only one snapshot kept (not unbounded) -- enough to diagnose "why did
  # the last boot die" without accumulating logs forever.
  [ -s "$logfile" ] && mv -f "$logfile" "${logfile}.prev_crash"
  log "launching oaica-nemotron-30b-a3b on GPU${gpu} :${port}"
  CUDA_VISIBLE_DEVICES=$gpu nohup python3 -m vllm.entrypoints.openai.api_server \
    --model "$MODEL_DIR" --served-model-name oaica-nemotron-30b-a3b --port "$port" --host 0.0.0.0 \
    --quantization compressed-tensors --dtype bfloat16 \
    --max-model-len 262144 --max-num-batched-tokens 8192 --max-num-seqs 8 \
    --gpu-memory-utilization 0.82 --enforce-eager \
    --enable-prefix-caching --enable-chunked-prefill \
    --enable-auto-tool-choice --tool-call-parser qwen3_xml \
    --reasoning-parser nemotron_v3 \
    --enable-prompt-tokens-details --trust-remote-code \
    > "$logfile" 2>&1 &
  disown
}

# REPLICAS: single source of truth, set once here (mirrors the production
# watchdog's fix for the same "two independent stale defaults" trap).
REPLICAS="${REPLICAS:-7:30107}"

# per-port backoff state: last launch epoch + current delay
declare -A LAST DELAY STALLFAILS
for r in $REPLICAS; do p=${r##*:}; LAST[$p]=0; DELAY[$p]=15; STALLFAILS[$p]=0; done

listening() { ss -ltn 2>/dev/null | grep -q ":$1 "; }
# booting: an api_server for this port exists but is not listening yet.
# vLLM takes time to load weights and warm up; during that window the port
# is closed but the launch is healthy. Without this check the watchdog
# would launch a SECOND api_server on the same port every backoff tick.
booting() { pgrep -f "api_server.*--port $1 " >/dev/null 2>&1; }

# chat_probe_ok: a real 1-token chat completion, the same signal oaicalb's
# own probe_model uses -- GET /v1/models stays 200 through a wedged
# scheduler, so only a real generation request proves the port is actually
# serving, not just bound.
chat_probe_ok() {
  local port=$1
  local body
  body=$(curl -s -m "$STALL_PROBE_TIMEOUT_SEC" -o - -w '\n%{http_code}' \
    "http://127.0.0.1:${port}/v1/chat/completions" \
    -H 'Content-Type: application/json' \
    -d "{\"model\":\"${STALL_PROBE_MODEL}\",\"messages\":[{\"role\":\"user\",\"content\":\"ping\"}],\"max_tokens\":1}" \
    2>/dev/null)
  local code=${body##*$'\n'}
  [ "$code" = "200" ]
}

# force_kill_replica: SIGTERM then, if still alive after a short grace
# period, SIGKILL both the api_server AND any VLLM::EngineCore child --
# vLLM's V1 engine runs generation in a SEPARATE process that survives its
# parent's death (reparents to PID 1) and keeps holding GPU memory; killing
# only api_server leaves the GPU "used" with nothing serving it.
force_kill_replica() {
  local port=$1
  local main_pid
  main_pid=$(pgrep -f "api_server.*--port $port " | head -1)
  if [ -n "$main_pid" ]; then
    alert ":${port} stalled (chat probe failed ${STALL_FAIL_THRESHOLD}x while listening) -- killing PID $main_pid"
    kill "$main_pid" 2>/dev/null
    sleep 5
    kill -0 "$main_pid" 2>/dev/null && { alert ":${port} PID $main_pid ignored SIGTERM -- SIGKILL"; kill -9 "$main_pid" 2>/dev/null; }
  fi
  sweep_orphans ":${port}" 6
}

# sweep_orphans TAG ROUNDS: kill every VLLM::EngineCore reparented to PID 1
# that belongs to THIS model. An EngineCore is vLLM's separate generation
# process; when its api_server dies the child reparents to init and keeps
# holding the full GPU allocation forever -- nvidia-smi then shows a HOST
# pid with no /proc entry in this container.
#
# Matching is by process name + ppid=1 ONLY, same as the production
# watchdog: an EngineCore's argv is literally `VLLM::EngineCore` (vLLM
# rewrites its process title; verified in /proc on a100b 2026-08-29), so
# there is nothing model-specific to filter on. That is safe because a
# healthy EngineCore always has a live api_server parent -- any EngineCore
# under init is garbage no matter which model spawned it, and both
# watchdogs on this box may reap it; the race is harmless (kill -9 on a
# dead pid is a no-op).
sweep_orphans() {
  local tag=$1 rounds=${2:-1} i orphans pid ppid
  for i in $(seq 1 "$rounds"); do
    orphans=$(pgrep -f 'VLLM::EngineCore' 2>/dev/null)
    [ -z "$orphans" ] && break
    for pid in $orphans; do
      ppid=$(awk '{print $4}' "/proc/$pid/stat" 2>/dev/null)
      [ "$ppid" = "1" ] || continue
      alert "${tag} killing orphaned VLLM::EngineCore PID $pid (reparented to init)"
      kill -9 "$pid" 2>/dev/null
    done
    [ "$rounds" -gt 1 ] && sleep 2
  done
}

# making_progress: true only if logfile was written to recently (engine
# process is alive and actively logging, not wedged) AND its most recent
# "Avg generation throughput" line is nonzero (real tokens are coming out,
# not zero forever). A real 1-token chat-completion probe (chat_probe_ok)
# competes for the SAME scheduler queue as genuine traffic, so under heavy
# concurrent load from real clients the probe can time out while the
# engine is honestly still working -- this only protects a BUSY replica,
# never a truly wedged one.
making_progress() {
  local logfile=$1
  [ -f "$logfile" ] || return 1
  local age
  age=$(( $(date +%s) - $(stat -c %Y "$logfile" 2>/dev/null || echo 0) ))
  (( age > 60 )) && return 1
  local line tps
  line=$(grep 'Avg generation throughput' "$logfile" 2>/dev/null | tail -1)
  [ -z "$line" ] && return 1
  tps=$(echo "$line" | grep -oE 'generation throughput: [0-9.]+' | grep -oE '[0-9.]+$')
  [ -z "$tps" ] && return 1
  awk -v t="$tps" 'BEGIN{exit !(t>0)}'
}

tick() {
  local gpu=$1 port=$2 logfile=$3 now
  now=$(date +%s)
  if listening "$port"; then
    if chat_probe_ok "$port"; then
      STALLFAILS[$port]=0
      DELAY[$port]=15
    else
      STALLFAILS[$port]=$(( STALLFAILS[$port] + 1 ))
      log ":${port} chat probe failed (streak ${STALLFAILS[$port]}/${STALL_FAIL_THRESHOLD})"
      if (( STALLFAILS[$port] >= STALL_FAIL_THRESHOLD )); then
        if making_progress "$logfile"; then
          alert ":${port} probe failed ${STALL_FAIL_THRESHOLD}x but engine is still producing tokens (busy under real load, not wedged) -- NOT killing, extending grace"
          STALLFAILS[$port]=$(( STALL_FAIL_THRESHOLD - 1 ))
        else
          force_kill_replica "$port"
          STALLFAILS[$port]=0
          LAST[$port]=$now
          DELAY[$port]=15
        fi
      fi
    fi
    return
  fi
  booting "$port" && return
  # not listening and no process: respect backoff
  if (( now - LAST[$port] < DELAY[$port] )); then return; fi
  if ! preflight; then
    alert "preflight failed for :${port}; backing off ${DELAY[$port]}s"
    LAST[$port]=$now; DELAY[$port]=$(( DELAY[$port] * 2 > 600 ? 600 : DELAY[$port] * 2 ))
    return
  fi
  # a previous launch that died fast (<60s) means a real fault -> back off
  if (( LAST[$port] > 0 && now - LAST[$port] < 60 )); then
    DELAY[$port]=$(( DELAY[$port] * 2 > 600 ? 600 : DELAY[$port] * 2 ))
    alert ":${port} died within 60s of launch; next retry in ${DELAY[$port]}s (see $logfile)"
  fi
  LAST[$port]=$now
  launch "$gpu" "$port" "$logfile"
}

# REPLICAS: "gpu:port" pairs. Single GPU7 replica of
# oaica-nemotron-30b-a3b, fronted by its own oaicalb on :30120
# (/workspace/nemotron-oaicalb.json). Kept separate from the production
# kat/vision fleet's GPUs (0/1/7 shared -- note GPU7 also hosts a
# production replica on :30112; this watchdog only ever touches :30107).

# oom_watch: attribute silent engine deaths to the container's memory cgroup.
# dmesg is capability-restricted in this container, so the only way to
# prove an OOM kill is to watch the counter: any tick where oom_kill moved
# is alerted with the delta, so the next death correlates (or does not)
# conclusively. memory.current is logged with it.
CG_EVENTS=/sys/fs/cgroup/memory.events
OOM_KILLS_SEEN=$(awk '/^oom_kill /{print $2}' "$CG_EVENTS" 2>/dev/null || echo 0)
oom_watch() {
  local now cur
  now=$(awk '/^oom_kill /{print $2}' "$CG_EVENTS" 2>/dev/null) || return 0
  [ -z "$now" ] && return 0
  if [ "$now" != "$OOM_KILLS_SEEN" ]; then
    cur=$(awk '{printf "%.0f", $1/1e9}' /sys/fs/cgroup/memory.current 2>/dev/null)
    alert "cgroup OOM killer fired: oom_kill $OOM_KILLS_SEEN -> $now (memory.current=${cur}GB, limit=$(awk '{printf "%.0f", $1/1e9}' /sys/fs/cgroup/memory.max)GB) -- check which replica died this tick"
    OOM_KILLS_SEEN=$now
  fi
}

# protect_oom: make our replica the kernel's LAST choice when the cgroup
# hits its limit. -900 (not -1000) keeps it killable as a true last
# resort so the kernel always has a victim and never wedges; every other
# tenant's process stays at the default 0 and is chosen first. Applied to
# the api_server and its EngineCore child every tick, idempotent.
#
# This container has been observed to have NO CAP_SYS_RESOURCE (see
# vllm_awq_watchdog.sh), so lowering oom_score_adj may be EACCES for root
# too. The function detects that on its first attempt, alerts once, and
# disables itself -- kept in place because it becomes effective
# automatically on a host/container that grants the capability.
OOM_ADJ="${OOM_ADJ:--900}"
OOM_ADJ_DISABLED=0
protect_oom() {
  local r port pid p
  [ "$OOM_ADJ_DISABLED" = 1 ] && return 0
  for r in $REPLICAS; do
    port=${r##*:}
    pid=$(pgrep -f "api_server.*--port $port " | head -1)
    [ -n "$pid" ] || continue
    for p in "$pid" $(pgrep -P "$pid"); do
      [ "$(cat "/proc/$p/oom_score_adj" 2>/dev/null)" = "$OOM_ADJ" ] && continue
      if echo "$OOM_ADJ" > "/proc/$p/oom_score_adj" 2>/dev/null; then
        log ":${port} oom_score_adj=$OOM_ADJ on pid $p ($(cut -c1-24 /proc/$p/comm 2>/dev/null))"
      else
        alert "cannot lower oom_score_adj (no CAP_SYS_RESOURCE in this container) -- OOM protection disabled; replica stays default-priority OOM victim"
        OOM_ADJ_DISABLED=1
        return 0
      fi
    done
  done
}

log "watchdog start (pinned $HF_REPO@$HF_REV) replicas=$REPLICAS stall_probe=${STALL_PROBE_MODEL} stall_threshold=${STALL_FAIL_THRESHOLD}x${STALL_PROBE_TIMEOUT_SEC}s oom_kill_baseline=$OOM_KILLS_SEEN oom_adj=$OOM_ADJ"
while true; do
  oom_watch
  sweep_orphans "tick" 1
  for r in $REPLICAS; do
    tick "${r%%:*}" "${r##*:}" "/workspace/vllm_nemotron_gpu${r%%:*}.log"
  done
  protect_oom
  sleep 15
done
