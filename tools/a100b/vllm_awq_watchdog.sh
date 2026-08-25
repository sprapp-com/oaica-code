#!/bin/bash
# kat-awq vLLM fleet watchdog for a100b. Replaces the naive "relaunch every
# 15s forever" loop, which crash-looped ~49k times when the weights vanished.
#
# Why each piece exists (all real incidents, 2026-08-23..26):
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
#
# Usage: nohup /workspace/vllm_awq_watchdog.sh >/workspace/vllm_awq_watchdog.log 2>&1 &
set -u

MODEL_DIR=/dev/shm/kat_awq
HF_REPO=Ar4ikov/KAT-Coder-V2.5-Dev-AWQ-W4A16-ASYM
HF_REV=446ea8c64909baff9a94c627b25765915b2c211d   # pinned; do not float
TOK_PATCH=/workspace/kat_awq.tokenizer_config.json   # vendored from tools/a100b/
TOK_SHA=3af7344522ce9496a14b7e701ecf14bb0aeeb067583a0c90681ccc3b75b49eea
LOG=/workspace/vllm_awq_watchdog.log
ALERT=/workspace/vllm_awq_watchdog.ALERT           # touched on any restore/backoff; poll it

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
  log "launching kat-awq on GPU${gpu} :${port}"
  CUDA_VISIBLE_DEVICES=$gpu nohup python3 -m vllm.entrypoints.openai.api_server \
    --model "$MODEL_DIR" --served-model-name kat-awq --port "$port" --host 0.0.0.0 \
    --enable-auto-tool-choice --tool-call-parser qwen3_coder --reasoning-parser qwen3 \
    --max-model-len 262144 --enable-prefix-caching \
    > "$logfile" 2>&1 &
  disown
}

# per-port backoff state: last launch epoch + current delay
declare -A LAST DELAY
for r in ${REPLICAS:-0:30199}; do p=${r##*:}; LAST[$p]=0; DELAY[$p]=15; done

listening() { ss -ltn 2>/dev/null | grep -q ":$1 "; }

tick() {
  local gpu=$1 port=$2 logfile=$3 now
  now=$(date +%s)
  listening "$port" && { DELAY[$port]=15; return; }
  # not listening: respect backoff
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

# REPLICAS: "gpu:port" pairs. GPU5 is deliberately NOT here as of 2026-08-26:
# it is fully occupied by the malay35b-offload prism_server (22 GB) plus
# another session's 52 GB job, so a kat-awq launch there OOMs on startup
# (5.9/79 GiB free vs the 72.9 GiB vLLM wants). Re-add "5:30105" once GPU5
# is actually free -- the watchdog will pick it up on the next tick.
REPLICAS="${REPLICAS:-0:30199}"

log "watchdog start (pinned $HF_REPO@$HF_REV) replicas=$REPLICAS"
while true; do
  for r in $REPLICAS; do
    tick "${r%%:*}" "${r##*:}" "/workspace/vllm_awq_gpu${r%%:*}.log"
  done
  sleep 15
done
