#!/bin/bash
# nemotron_gpu7_launch.sh — launches nvidia-nemotron-3.5-lightning-30b-a3b
# on GPU7, standalone (no watchdog, no oaicalb — single replica, own port).
# Not templated into vllm_awq_watchdog.sh's REPLICAS mechanism: that
# watchdog is oaica-35b-a3b-vision-specific (preflight/restore logic keyed
# to that model's HF repo). This is a plain launch script, run manually or
# supervised by a future generic watchdog.
#
# Real gaps recorded here, not silently worked around:
#   - --enforce-eager: torch.compile's cache needs an EXEC-capable
#     filesystem. /dev/shm is mounted noexec on this box (confirmed via
#     `mount`), and /workspace (the only exec-capable writable mount) was
#     96% full (2.4GB free) when this was set up, insufficient for the
#     compile cache. Losing torch.compile costs throughput; fix properly by
#     freeing real /workspace space (NOT by deleting other sessions' files
#     without asking) or finding another exec-capable mount before removing
#     this flag.
#   - --max-model-len 1048576 (1M) is a deliberate, explicit choice PAST the
#     model's native window: config.json's max_position_embeddings is
#     262144. The vendor's own example (examples/start_vllm_3090_640k.sh in
#     the HF repo) only validates out to 640K, on a single RTX 3090, with
#     VLLM_ALLOW_LONG_MAX_MODEL_LEN=1 already required at 640K. Quality on
#     contexts well past 262144 is unverified at any length past what the
#     vendor tested, more so approaching 1M.
#   - --gpu-memory-utilization 0.82 (not 0.9): GPU7 is on a shared vast.ai
#     host — free VRAM fluctuates (~68-71 GiB observed) as OTHER RENTERS'
#     jobs on the same physical GPUs come and go, invisible to this
#     container's process table (their PIDs show as [Not Found] in
#     nvidia-smi). 0.9 failed to fit at boot time; 0.82 leaves real margin.
#
# Usage: nohup ./nemotron_gpu7_launch.sh > /workspace/nemotron_gpu7.log 2>&1 &
set -u

MODEL_DIR=/dev/shm/nemotron35_lightning_30b_a3b
HF_REPO=useful-quants/NVIDIA-Nemotron-3.5-Lightning-30B-A3B-W4A16
PORT=30107
GPU=7

mkdir -p /workspace/.hf_home
export LD_LIBRARY_PATH=/usr/local/lib/python3.12/dist-packages/nvidia/cu13/lib
export HF_HOME=/workspace/.hf_home
export VLLM_ALLOW_LONG_MAX_MODEL_LEN=1

if [ ! -f "$MODEL_DIR/config.json" ]; then
  echo "weights missing at $MODEL_DIR -- downloading $HF_REPO"
  python3 - <<PY
from huggingface_hub import snapshot_download
snapshot_download(repo_id="$HF_REPO", local_dir="$MODEL_DIR")
PY
fi

CUDA_VISIBLE_DEVICES=$GPU exec python3 -m vllm.entrypoints.openai.api_server \
  --model "$MODEL_DIR" --served-model-name nvidia-nemotron-3.5-lightning-30b-a3b \
  --port "$PORT" --host 0.0.0.0 \
  --quantization compressed-tensors --dtype bfloat16 \
  --gpu-memory-utilization 0.82 \
  --max-model-len 1048576 \
  --max-num-seqs 8 --max-num-batched-tokens 8192 \
  --enable-chunked-prefill --enforce-eager \
  --enable-auto-tool-choice --tool-call-parser hermes
