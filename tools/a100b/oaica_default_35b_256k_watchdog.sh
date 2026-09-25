#!/bin/bash
# Keep oaica-default on the complete calibrated OAICA 35B-A3B W4A16 build.
# The 256k tier is deliberate: it is the high-concurrency production tier;
# all replicas share the same window so a load balancer cannot misroute a
# long request to a smaller backend.
set -u

MODEL_DIR=${MODEL_DIR:-/dev/shm/oaica35b_vision_yarn_calibrated_awq}
SERVED_MODEL=${SERVED_MODEL:-oaica-default}
REPLICAS=${REPLICAS:-"2:30502 3:30503 4:30500"}
LOG_DIR=${LOG_DIR:-/workspace/oaica_default_35b_256k}

mkdir -p "$LOG_DIR" /workspace/.vllm_cache /workspace/.torch_cache
export LD_LIBRARY_PATH=/usr/local/lib/python3.12/dist-packages/nvidia/cu13/lib
export VLLM_CACHE_ROOT=/workspace/.vllm_cache
export TORCHINDUCTOR_CACHE_DIR=/workspace/.torch_cache
export HF_HOME=/workspace/.hf_home

weights_ok() {
  [ -f "$MODEL_DIR/config.json" ] && [ -f "$MODEL_DIR/tokenizer_config.json" ] &&
    [ -f "$MODEL_DIR/model.safetensors.index.json" ] &&
    find -L "$MODEL_DIR" -maxdepth 1 -name '*.safetensors' -type f | grep -q .
}

listening() { ss -ltn 2>/dev/null | grep -q ":$1 "; }
booting() { pgrep -f "vllm serve $MODEL_DIR.*--port $1 " >/dev/null 2>&1; }

launch() {
  local gpu=$1 port=$2 log="$LOG_DIR/vllm_gpu${gpu}.log"
  [ -s "$log" ] && mv -f "$log" "${log}.prev"
  echo "$(date -Is) launching $SERVED_MODEL on GPU${gpu} :${port}" >> "$LOG_DIR/watchdog.log"
  VLLM_DISABLED_KERNELS=HummingLinearKernel CUDA_VISIBLE_DEVICES=$gpu nohup vllm serve "$MODEL_DIR" \
    --served-model-name "$SERVED_MODEL" --port "$port" --host 127.0.0.1 \
    --enable-auto-tool-choice --tool-call-parser qwen3_coder --reasoning-parser qwen3 \
    --gpu-memory-utilization 0.90 --kv-cache-dtype fp8 --gdn-prefill-backend triton \
    --limit-mm-per-prompt '{"image": 2}' --max-model-len 262144 \
    --max-num-batched-tokens 8192 --max-num-seqs 18 --enable-prefix-caching \
    --mamba-block-size 16384 --enable-prompt-tokens-details > "$log" 2>&1 &
}

weights_ok || { echo "missing complete model files in $MODEL_DIR" >&2; exit 2; }
while :; do
  for replica in $REPLICAS; do
    gpu=${replica%%:*}; port=${replica##*:}
    listening "$port" || booting "$port" || launch "$gpu" "$port"
  done
  sleep 15
done
