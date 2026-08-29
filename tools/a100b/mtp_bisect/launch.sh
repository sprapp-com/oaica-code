#!/bin/bash
# launch.sh VARIANT [extra vllm args...]
#
# Launches a single throwaway test instance of the MTP vLLM model on GPU7
# (shared with a co-tenant, pid 2707645, ~18GB -- NEVER GPU0-6, NEVER touch
# production replicas on 30106/30108/30110). Mirrors the production launch
# command in vllm_awq_watchdog.sh's launch() exactly, except pinned to
# GPU7/port 30130/--gpu-memory-utilization 0.7, with any extra vllm args
# from the command line overriding/adding to the defaults.
#
# Special extra token: --no-spec means "omit --speculative-config entirely".
#
# EXTRA_ENV (optional, from the calling environment) is a single string of
# space-separated KEY=VALUE pairs, e.g.
#   EXTRA_ENV="VLLM_ATTENTION_BACKEND=TRITON_ATTN" ./launch.sh variant1
# each is exported before launch.
set -u

WORKDIR=/workspace/mtp_bisect
MODEL_DIR=/dev/shm/oaica-35b-a3b-mtp-vision
PORT=30130
GPU=7

if [ $# -lt 1 ]; then
  echo "usage: $0 VARIANT [extra vllm args...]" >&2
  exit 1
fi

VARIANT=$1
shift
LOGFILE="$WORKDIR/${VARIANT}.log"

# --- kill any previous test instance on our port only ---------------------
old_pid=$(pgrep -f "api_server.*--port ${PORT} " 2>/dev/null | head -1)
if [ -n "${old_pid:-}" ]; then
  echo "killing previous test instance PID $old_pid on port $PORT"
  kill "$old_pid" 2>/dev/null
  sleep 3
  kill -0 "$old_pid" 2>/dev/null && kill -9 "$old_pid" 2>/dev/null
fi

# kill orphaned EngineCores that belong to OUR GPU7 test instance only:
# ppid=1 (reparented to init) AND environ shows CUDA_VISIBLE_DEVICES=7.
# Never touch an EngineCore for any other GPU -- that could be another
# tenant's or a production replica's orphan.
for pid in $(pgrep -f 'VLLM::EngineCore' 2>/dev/null); do
  ppid=$(awk '{print $4}' "/proc/$pid/stat" 2>/dev/null)
  [ "$ppid" = "1" ] || continue
  if tr '\0' '\n' < "/proc/$pid/environ" 2>/dev/null | grep -qx 'CUDA_VISIBLE_DEVICES=7'; then
    echo "killing orphaned VLLM::EngineCore PID $pid (GPU7, reparented to init)"
    kill -9 "$pid" 2>/dev/null
  fi
done

# --- env, same as the production watchdog ----------------------------------
mkdir -p /workspace/.vllm_cache /workspace/.torch_cache
export LD_LIBRARY_PATH=/usr/local/lib/python3.12/dist-packages/nvidia/cu13/lib
export VLLM_CACHE_ROOT=/workspace/.vllm_cache
export TORCHINDUCTOR_CACHE_DIR=/workspace/.torch_cache
export HF_HOME=/workspace/.hf_home

# extra EXTRA_ENV="K=V K2=V2" pairs from the caller's environment
if [ -n "${EXTRA_ENV:-}" ]; then
  for kv in $EXTRA_ENV; do
    export "$kv"
    echo "exported $kv"
  done
fi

# --- build default arg list as flag=>value pairs ---------------------------
# Keys are the flag name; values are everything up to (not including) the
# next flag. This lets us detect an override by flag name and drop the
# default before appending extras, rather than relying on vLLM taking the
# "last occurrence wins" for repeated flags.
declare -A DEFAULTS
DEFAULTS["--model"]="$MODEL_DIR"
DEFAULTS["--served-model-name"]="oaica-35b-a3b-vision"
DEFAULTS["--port"]="$PORT"
DEFAULTS["--host"]="0.0.0.0"
DEFAULTS["--enable-auto-tool-choice"]=""
DEFAULTS["--tool-call-parser"]="qwen3_coder"
DEFAULTS["--reasoning-parser"]="qwen3"
DEFAULTS["--gpu-memory-utilization"]="0.7"
DEFAULTS["--kv-cache-dtype"]="fp8"
DEFAULTS["--limit-mm-per-prompt"]='{"image": 2}'
DEFAULTS["--max-model-len"]="262144"
DEFAULTS["--max-num-batched-tokens"]="12288"
DEFAULTS["--max-num-seqs"]="18"
DEFAULTS["--enable-prefix-caching"]=""
DEFAULTS["--no-async-scheduling"]=""
DEFAULTS["--speculative-config"]='{"method":"mtp","num_speculative_tokens":1}'

# ordering of flags in final command (must be stable, dict order isn't)
ORDER=(--model --served-model-name --port --host --enable-auto-tool-choice \
       --tool-call-parser --reasoning-parser --gpu-memory-utilization \
       --kv-cache-dtype --limit-mm-per-prompt --max-model-len \
       --max-num-batched-tokens --max-num-seqs --enable-prefix-caching \
       --no-async-scheduling --speculative-config)

# flags that take no value (boolean/presence-only)
is_flag_only() {
  case "$1" in
    --enable-auto-tool-choice|--enable-prefix-caching|--no-async-scheduling) return 0 ;;
    *) return 1 ;;
  esac
}

# --- parse extras, override/remove/add defaults -----------------------------
NO_SPEC=0
EXTRA_ORDER=()
declare -A EXTRA_VAL
i=0
args=("$@")
n=${#args[@]}
while [ $i -lt $n ]; do
  a="${args[$i]}"
  if [ "$a" = "--no-spec" ]; then
    NO_SPEC=1
    i=$((i+1))
    continue
  fi
  if [[ "$a" == --* ]]; then
    # remove from defaults if present (override)
    unset "DEFAULTS[$a]" 2>/dev/null
    if is_flag_only "$a"; then
      EXTRA_ORDER+=("$a")
      EXTRA_VAL["$a"]=""
      i=$((i+1))
    else
      # next token is the value, unless it's another flag or end of args
      val=""
      if [ $((i+1)) -lt $n ] && [[ "${args[$((i+1))]}" != --* ]]; then
        val="${args[$((i+1))]}"
        i=$((i+2))
      else
        i=$((i+1))
      fi
      EXTRA_ORDER+=("$a")
      EXTRA_VAL["$a"]="$val"
    fi
  else
    i=$((i+1))
  fi
done

if [ "$NO_SPEC" = "1" ]; then
  unset "DEFAULTS[--speculative-config]" 2>/dev/null
fi

# --- assemble final CLI arg array -------------------------------------------
CMD_ARGS=()
for k in "${ORDER[@]}"; do
  if [ -v "DEFAULTS[$k]" ]; then
    CMD_ARGS+=("$k")
    v="${DEFAULTS[$k]}"
    [ -n "$v" ] && CMD_ARGS+=("$v")
  fi
done
for k in "${EXTRA_ORDER[@]}"; do
  CMD_ARGS+=("$k")
  v="${EXTRA_VAL[$k]}"
  [ -n "$v" ] && CMD_ARGS+=("$v")
done

echo "launching variant '$VARIANT' on GPU${GPU}:${PORT}"
echo "args: ${CMD_ARGS[*]}"

[ -s "$LOGFILE" ] && mv -f "$LOGFILE" "${LOGFILE}.prev"

CUDA_VISIBLE_DEVICES=$GPU nohup python3 -m vllm.entrypoints.openai.api_server \
  "${CMD_ARGS[@]}" > "$LOGFILE" 2>&1 &
PID=$!
disown

echo "$PID"
