#!/bin/bash
# kat-awq vLLM fleet watchdog for a100b. Replaces the naive "relaunch every
# 15s forever" loop, which crash-looped ~49k times when the weights vanished.
#
# Why each piece exists (all real incidents, 2026-08-23..29):
#   preflight   /dev/shm/kat_awq was deleted by another session's cleanup;
#               vLLM exited instantly on "config.json not found" and the old
#               loop relaunched it every 15s with no restore and no alert.
#   restore     the re-downloaded snapshot lacks a chat_template, so every
#               chat completion 400s while GET /v1/models is 200. The vendored
#               tokenizer_config.json carries the working template; restore()
#               re-applies it after any re-download (sha256-checked).
#   backoff     a launch that dies in <60s is a config/weights problem, not a
#               transient; exponential backoff stops a fatal misconfig from
#               hammering the GPU and the log.
#   caches      VLLM_CACHE_ROOT / TORCHINDUCTOR_CACHE_DIR off the 16G root
#               overlay, which is 100% full (any write to ~/.cache wedges the
#               process with a zombie GPU allocation).
#   stall       2026-08-28: GPU2 was LISTENING and answering GET /v1/models
#               200 the whole time, but every real chat completion queued
#               behind a scheduler wedged by ~10 concurrent long-context
#               requests -- oaicalb's own health probe (a real 1-token chat
#               completion, not just GET /v1/models) marked it DOWN, but
#               nothing on THIS box's watchdog noticed or acted; a human had
#               to manually curl-probe, SIGTERM (which the wedged process
#               ignored), then SIGKILL both api_server and the orphaned
#               VLLM::EngineCore child before it recovered. stall_check()
#               below automates exactly that sequence.
#
# Usage: nohup /workspace/vllm_awq_watchdog.sh >/workspace/vllm_awq_watchdog.log 2>&1 &
set -u

MODEL_DIR=/dev/shm/kat_vision_awq
HF_REPO=Ar4ikov/KAT-Coder-V2.5-Dev-AWQ-W4A16-ASYM
HF_REV=446ea8c64909baff9a94c627b25765915b2c211d   # pinned; do not float
TOK_PATCH=/workspace/kat_awq.tokenizer_config.json   # vendored from tools/a100b/
TOK_SHA=3af7344522ce9496a14b7e701ecf14bb0aeeb067583a0c90681ccc3b75b49eea
LOG=/workspace/vllm_awq_watchdog.log
ALERT=/workspace/vllm_awq_watchdog.ALERT           # touched on any restore/backoff/stall-kill; poll it

# STALL_PROBE_MODEL: served-model-name to probe with a real 1-token chat
# completion (not just GET /v1/models, which stays 200 through a wedged
# scheduler). STALL_PROBE_TIMEOUT_SEC bounds one probe attempt.
# STALL_FAIL_THRESHOLD consecutive chat-probe failures/timeouts, while the
# port is still LISTENING (i.e. not caught by the ordinary crash path
# above), before a replica is force-killed and relaunched.
STALL_PROBE_MODEL="${STALL_PROBE_MODEL:-oaica-35b-a3b-vision}"
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
  [ -f "$MODEL_DIR/config.json" ] && [ -f "$MODEL_DIR/model.safetensors.index.json" ] \
    && [ -f "$MODEL_DIR/tokenizer_config.json" ] && ls "$MODEL_DIR"/model-*.safetensors >/dev/null 2>&1
}

# tokenizer_ok: the chat_template patch is in place (sha256 match).
tokenizer_ok() {
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
  log "launching oaica-35b-a3b-vision on GPU${gpu} :${port}"
  CUDA_VISIBLE_DEVICES=$gpu nohup python3 -m vllm.entrypoints.openai.api_server \
    --model "$MODEL_DIR" --served-model-name oaica-35b-a3b-vision --port "$port" --host 0.0.0.0 \
    --enable-auto-tool-choice --tool-call-parser qwen3_coder --reasoning-parser qwen3 \
    --gpu-memory-utilization 0.9 --kv-cache-dtype fp8 \
    --limit-mm-per-prompt '{"image": 2}' --max-model-len 262144 \
    --max-num-batched-tokens 8192 --max-num-seqs 18 --enable-prefix-caching \
    --no-async-scheduling \
    > "$logfile" 2>&1 &
  disown
}

# per-port backoff state: last launch epoch + current delay
declare -A LAST DELAY STALLFAILS
for r in ${REPLICAS:-0:30199 2:30105}; do p=${r##*:}; LAST[$p]=0; DELAY[$p]=15; STALLFAILS[$p]=0; done

listening() { ss -ltn 2>/dev/null | grep -q ":$1 "; }
# booting: an api_server for this port exists but is not listening yet.
# vLLM takes ~100 s to load 21 GB of AWQ weights and warm up; during that
# window the port is closed but the launch is healthy. Without this check
# the watchdog launched a SECOND api_server on the same port every backoff
# tick (seen 2026-08-25: 3 launches in 90 s, one left as a 3 MB zombie).
booting() { pgrep -f "api_server.*--port $1 " >/dev/null 2>&1; }

# chat_probe_ok: a real 1-token chat completion, the same signal oaicalb's
# own probe_model uses -- GET /v1/models stays 200 through a wedged
# scheduler (that's exactly what happened 2026-08-28), so only a real
# generation request proves the port is actually serving, not just bound.
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
# only api_server leaves the GPU "used" with nothing serving it (see `oaica
# gpu clean` in the client CLI, built for the same reason). This function
# does the a100b-side equivalent inline since a watchdog can't shell out to
# a client-side Go binary.
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
  # Orphaned EngineCore: reparented to PID 1, no longer matches the
  # "--port N" pgrep pattern above once api_server is gone, so it is found
  # by process name only. This is the exact class of stray process that
  # cost real debugging time earlier this session (mistaken for a "rogue
  # spawner") until traced to this same root cause.
  local i
  for i in 1 2 3 4 5 6; do
    local orphans
    orphans=$(pgrep -f 'VLLM::EngineCore' 2>/dev/null)
    [ -z "$orphans" ] && break
    for pid in $orphans; do
      local ppid
      ppid=$(awk '{print $4}' "/proc/$pid/stat" 2>/dev/null)
      if [ "$ppid" = "1" ]; then
        alert ":${port} killing orphaned VLLM::EngineCore PID $pid (reparented to init)"
        kill -9 "$pid" 2>/dev/null
      fi
    done
    sleep 2
  done
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
        force_kill_replica "$port"
        STALLFAILS[$port]=0
        LAST[$port]=$now
        DELAY[$port]=15
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

# REPLICAS: "gpu:port" pairs. Vision adapter (model-vision.safetensors)
# replaces plain kat-awq; served model name is oaica-35b-a3b-vision
# (renamed 2026-08-28, was kat-awq). GPU5 is NOT usable: another session's
# 52 GB job plus the malay35b-offload server leave 5.9/79 GiB, and vLLM
# refuses to start below gpu_memory_utilization * total (~73 GiB).
REPLICAS="${REPLICAS:-0:30106 1:30108}"

log "watchdog start (pinned $HF_REPO@$HF_REV) replicas=$REPLICAS stall_probe=${STALL_PROBE_MODEL} stall_threshold=${STALL_FAIL_THRESHOLD}x${STALL_PROBE_TIMEOUT_SEC}s"
while true; do
  for r in $REPLICAS; do
    tick "${r%%:*}" "${r##*:}" "/workspace/vllm_awq_gpu${r%%:*}.log"
  done
  sleep 15
done
