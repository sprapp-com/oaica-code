#!/bin/bash
set -u
MODEL_DIR=/dev/shm/oaica-35b-a3b-mtp-vision
PORT=30131
GPU=7
LOGFILE=/workspace/vllm_main/main.log
VPY=/workspace/vllm_main/venv/bin/python
mkdir -p /workspace/.vllm_cache /workspace/.torch_cache
export LD_LIBRARY_PATH=/usr/local/lib/python3.12/dist-packages/nvidia/cu13/lib
export VLLM_CACHE_ROOT=/workspace/.vllm_cache
export TORCHINDUCTOR_CACHE_DIR=/workspace/.torch_cache
export HF_HOME=/workspace/.hf_home
[ -s "$LOGFILE" ] && mv -f "$LOGFILE" "${LOGFILE}.prev"
CUDA_VISIBLE_DEVICES=$GPU nohup "$VPY" -m vllm.entrypoints.openai.api_server \
  --model "$MODEL_DIR" \
  --served-model-name oaica-35b-a3b-vision \
  --port "$PORT" --host 0.0.0.0 \
  --enable-auto-tool-choice --tool-call-parser qwen3_coder --reasoning-parser qwen3 \
  --gpu-memory-utilization 0.7 \
  --kv-cache-dtype fp8 \
  --limit-mm-per-prompt '{"image": 2}' \
  --max-model-len 262144 \
  --max-num-batched-tokens 12288 \
  --max-num-seqs 18 \
  --enable-prefix-caching --no-async-scheduling \
  --speculative-config '{"method":"mtp","num_speculative_tokens":1}' \
  "$@" \
  > "$LOGFILE" 2>&1 &
echo $!
