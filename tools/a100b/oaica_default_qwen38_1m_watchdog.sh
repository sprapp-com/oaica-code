#!/bin/bash
# Keep the two dedicated 1M-context Qwen3.8-27B oaica-default replicas up.
# This pool is intentionally separate from vllm_awq_watchdog.sh: that script
# owns the legacy oaica-35b-a3b deployment and must not overwrite its model.
set -u

MODEL_DIR=${MODEL_DIR:-/dev/shm/oaica_default_jc1da_1m}
SERVED_MODEL=${SERVED_MODEL:-oaica-default}
REPLICAS=${REPLICAS:-"2:30502 3:30503 4:30500"}
LOG_DIR=${LOG_DIR:-/workspace/oaica_default_qwen38_1m}

mkdir -p "$LOG_DIR" /workspace/.vllm_cache /workspace/.torch_cache
export LD_LIBRARY_PATH=/usr/local/lib/python3.12/dist-packages/nvidia/cu13/lib
export VLLM_CACHE_ROOT=/workspace/.vllm_cache
export TORCHINDUCTOR_CACHE_DIR=/workspace/.torch_cache
export HF_HOME=/workspace/.hf_home

weights_ok() {
  [ -f "$MODEL_DIR/config.json" ] && [ -f "$MODEL_DIR/tokenizer_config.json" ] &&
    [ -f "$MODEL_DIR/model.safetensors.index.json" ] &&
    ls "$MODEL_DIR"/model-*.safetensors >/dev/null 2>&1
}

listening() { ss -ltn 2>/dev/null | grep -q ":$1 "; }
booting() { pgrep -f "vllm serve $MODEL_DIR.*--port $1 " >/dev/null 2>&1; }

launch() {
  local gpu=$1 port=$2 log="$LOG_DIR/vllm_gpu${gpu}.log"
  [ -s "$log" ] && mv -f "$log" "${log}.prev"
  echo "$(date -Is) launching $SERVED_MODEL on GPU${gpu} :${port}" >> "$LOG_DIR/watchdog.log"
  VLLM_DISABLED_KERNELS=HummingLinearKernel VLLM_ALLOW_LONG_MAX_MODEL_LEN=1 \
  CUDA_VISIBLE_DEVICES=$gpu nohup vllm serve "$MODEL_DIR" \
    --served-model-name "$SERVED_MODEL" --port "$port" --host 127.0.0.1 \
    --enable-auto-tool-choice --tool-call-parser qwen3_coder --reasoning-parser qwen3 \
    --gpu-memory-utilization 0.90 --kv-cache-dtype fp8 --gdn-prefill-backend triton \
    --limit-mm-per-prompt '{"image": 2}' --max-model-len 1048576 \
    --max-num-batched-tokens 8192 --max-num-seqs 2 --enable-prefix-caching \
    --mamba-block-size 16384 --enforce-eager --enable-prompt-tokens-details \
    > "$log" 2>&1 &
}

weights_ok || { echo "missing model files in $MODEL_DIR" >&2; exit 2; }
while :; do
  for replica in $REPLICAS; do
    gpu=${replica%%:*}; port=${replica##*:}
    listening "$port" || booting "$port" || launch "$gpu" "$port"
  done
  sleep 15
done
