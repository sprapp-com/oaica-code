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

# 2026-08-29: swapped to the MTP (multi-token-prediction, speculative
# decoding) variant -- KAT-Coder-V2.5-Dev-MTP, int4 AutoRound quant (NOT
# the same quant method as the old AWQ model this replaced). Served model
# name is unchanged (oaica-35b-a3b-vision) so no client-side change is
# needed. TOK_SHA is this model's OWN tokenizer_config.json hash (it ships
# a complete, working chat_template -- no external patch needed, unlike
# the old model); restore_tokenizer() below is effectively a no-op for
# this model (TOK_PATCH is stale/irrelevant to it) since tokenizer_ok()
# will match TOK_SHA directly on every normal boot.
# 2026-08-30: switched to the AWQ W4A16 build (kat_awq shards + bf16 MTP
# head + bf16 vision tower, symlink-grafted; see its README.md). It is a
# LOCAL composite, not an HF snapshot -- HF_REPO is empty on purpose so a
# missing-weights preflight alerts instead of downloading the old
# AutoRound checkpoint over it.
MODEL_DIR=/dev/shm/oaica-35b-a3b-awq-mtp-vision
HF_REPO=
HF_REV=main
TOK_PATCH=/workspace/kat_awq.tokenizer_config.json   # vendored from tools/a100b/ (stale for this model, kept as last-resort restore target only)
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
  if [ -z "$HF_REPO" ]; then
    alert "weights missing at $MODEL_DIR and no HF_REPO to restore from (composite build: check the symlink targets in $MODEL_DIR and its SHASUMS)"
    return 1
  fi
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
  # 2026-08-29 incident: GPU0/1 crash-looped 6x over 90 minutes (orphaned
  # EngineCore killed repeatedly by sweep_orphans) and every single
  # traceback was gone by the time anyone looked, because each relaunch's
  # truncation raced the investigation. Only one snapshot kept (not
  # unbounded) -- enough to diagnose "why did the last boot die" without
  # accumulating logs forever.
  [ -s "$logfile" ] && mv -f "$logfile" "${logfile}.prev_crash"
  log "launching oaica-35b-a3b-vision (MTP) on GPU${gpu} :${port}"
  # --speculative-config is the model's own required recipe (its README)
  # for the MTP head to do anything -- without it the MTP layers are just
  # unused weight, no speedup, but not a crash risk either.
  # --gpu-memory-utilization/--max-num-seqs deliberately do NOT match the
  # model card's own example (0.55 / 8): that recipe was sized for a
  # GB10's 128GB unified memory, not this box's 80GB-per-GPU A100s. Kept
  # at our own already-validated A100 numbers instead -- the memory
  # profile difference between AWQ and this int4-AutoRound quant is in
  # the weights only (~21GB either way), not the KV-cache budget, so
  # reusing our proven values is the safer bet for real concurrency here.
  # num_speculative_tokens dropped 3 -> 1 after a real 2026-08-29 crash:
  # GPU1+GPU2 both died with "torch.AcceleratorError: CUDA error: an
  # illegal memory access was encountered" in the sampling/token-transfer
  # path, matching vLLM's own boot-time warning ("Enabling
  # num_speculative_tokens > 1 will run multiple times of forward on same
  # MTP layer, which may result in lower acceptance rate") -- >1 is
  # flagged by vLLM itself as an atypical, less-hardened configuration.
  #
  # "enforce_eager":true (draft only; the target model keeps its PIECEWISE
  # CUDA graphs -- see SpeculativeConfig.enforce_eager and
  # llm_base_proposer.initialize_cudagraph_keys in vLLM 0.24.0) is the
  # upstream-endorsed workaround for that illegal-memory-access: vllm
  # issue #53726 root-causes it to the MTP draft's CUDA-graph replay
  # racing the GDN/Mamba recurrent-state updates, and running the draft
  # eagerly eliminated it in the reporters' soaks. No released vLLM
  # (0.24-0.28) carries the real fix (PRs #50729/#53613). Measured on
  # GPU7 2026-08-29 with production-shaped load: throughput-neutral
  # (184 vs 182 tok/s mean, acceptance 1.83 vs 1.75) and identical KV
  # cache size. Could not reproduce the crash on demand in ~60 min of
  # load, so this is a mitigation with upstream evidence, not a proven
  # fix; the OOM/crash instrumentation below attributes any recurrence.
  #
  # --enable-prompt-tokens-details: vLLM 0.24.0 defaults this to OFF
  # (cli_args.py: enable_prompt_tokens_details = False), so every
  # response carried "prompt_tokens_details": null even at a live 38%
  # prefix-cache hit rate -- the gateway's cached_tokens was 0 on all
  # 3,134 ledger rows of 2026-08-29 and the cached_prompt price never
  # applied (metering audit, same day). With the flag, usage carries
  # prompt_tokens_details.cached_tokens and cache hits bill at the
  # discounted rate as the rate card says.
  # AWQ build requirements (its README.md, measured on A100): the Humming
  # linear kernel must be disabled and --gdn-prefill-backend triton is
  # mandatory (the FlashInfer GDN prefill path deadlocks).
  #
  # 256k production tier = the checkpoint's "MTP off, KV fp8" launch
  # (owner's A100 bench, 2026-08-30): 95 users at >=30 tok/s, 2110 tok/s
  # aggregate. MTP2 + bf16 KV is the 8k/16k tier (198 tok/s single-stream,
  # 166 users) -- at 256k the fp8 KV doubles cache capacity and MTP's
  # verify tax outweighs its per-user gain once N grows. No speculative
  # config => no MTP draft => the IMA race that forced enforce_eager and
  # --no-async-scheduling is gone, so async scheduling is back on.
  VLLM_DISABLED_KERNELS=HummingLinearKernel \
  CUDA_VISIBLE_DEVICES=$gpu nohup python3 -m vllm.entrypoints.openai.api_server \
    --model "$MODEL_DIR" --served-model-name oaica-35b-a3b-vision --port "$port" --host 0.0.0.0 \
    --enable-auto-tool-choice --tool-call-parser qwen3_coder --reasoning-parser qwen3 \
    --gpu-memory-utilization 0.9 --kv-cache-dtype fp8 --gdn-prefill-backend triton \
    --limit-mm-per-prompt '{"image": 2}' --max-model-len 262144 \
    --max-num-batched-tokens 12288 --max-num-seqs 18 --enable-prefix-caching \
    --enable-prompt-tokens-details \
    > "$logfile" 2>&1 &
  disown
}

# REPLICAS: single source of truth, set once here (was previously two
# independent ${REPLICAS:-...} defaults that had drifted out of sync --
# this array-init loop used a stale "0:30199 2:30105" while the main loop
# below used the real "0:30106 1:30108 2:30110". Harmless in practice
# (bash auto-vivifies unset array elements to 0 in arithmetic context) but
# confusing and a real trap for the next port change.
REPLICAS="${REPLICAS:-0:30106 1:30108 2:30110 7:30112}"

# per-port backoff state: last launch epoch + current delay
declare -A LAST DELAY STALLFAILS
for r in $REPLICAS; do p=${r##*:}; LAST[$p]=0; DELAY[$p]=15; STALLFAILS[$p]=0; done

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
  sweep_orphans ":${port}" 6
}

# sweep_orphans TAG ROUNDS: kill every VLLM::EngineCore reparented to PID 1.
# An EngineCore is vLLM's separate generation process; when its api_server
# dies (SIGKILL from force_kill_replica, OR a crash-on-boot in the "died
# within 60s of launch" path) the child reparents to init and keeps holding
# the full GPU allocation (~74 GB) forever -- nvidia-smi then shows a HOST
# pid that has no /proc entry in this container, which was mistaken for an
# unreclaimable "zombie" on 2026-08-28 (GPU2, PID 2263066, born 20:40:28
# during a burst of boot-deaths that the post-stall-kill sweep never ran
# for). Now called once per main-loop tick as well as after a stall-kill,
# so no code path can leak an EngineCore for more than one tick.
# Matching by process name + ppid=1 only: a healthy EngineCore always has
# a live api_server parent, so this cannot touch a serving replica -- ours
# or any other tenant's on this box.
sweep_orphans() {
  local tag=$1 rounds=${2:-1} i orphans pid ppid
  for i in $(seq 1 "$rounds"); do
    orphans=$(pgrep -f 'VLLM::EngineCore' 2>/dev/null)
    [ -z "$orphans" ] && break
    for pid in $orphans; do
      ppid=$(awk '{print $4}' "/proc/$pid/stat" 2>/dev/null)
      if [ "$ppid" = "1" ]; then
        alert "${tag} killing orphaned VLLM::EngineCore PID $pid (reparented to init)"
        kill -9 "$pid" 2>/dev/null
      fi
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
# engine is honestly still working -- 2026-08-28: killing in that case
# only makes things worse, dropping capacity exactly when more is needed
# and dumping double load on the surviving replica, cascading into a
# thrash loop across both GPUs. Stale or missing log, or throughput
# genuinely at 0, still means kill -- this only protects a BUSY replica,
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

# REPLICAS: "gpu:port" pairs. Vision adapter (model-vision.safetensors)
# replaces plain kat-awq; served model name is oaica-35b-a3b-vision
# (renamed 2026-08-28, was kat-awq). GPU5 is NOT usable: another session's
# 52 GB job plus the malay35b-offload server leave 5.9/79 GiB, and vLLM
# refuses to start below gpu_memory_utilization * total (~73 GiB). GPU2
# added 2026-08-29 as a 3rd replica (:30110) per explicit user request,
# reversing an earlier same-day "reserved for the user" note -- see
# /dev/shm/gpus.md for the full history.

# oom_watch: attribute silent engine deaths to the container's memory cgroup.
# 2026-08-29: three replicas died in the SAME second while completely idle
# (Running: 0 reqs), no traceback, twice in one day -- and
# /sys/fs/cgroup/memory.events showed oom_kill=15 with memory.peak ==
# memory.max (2 TB, of which ~1 TB is other tenants' unreclaimable
# /dev/shm). dmesg is capability-restricted in this container, so the
# only way to prove an OOM kill is to watch the counter: any tick where
# oom_kill moved is alerted with the delta, so the next death correlates
# (or does not) conclusively. memory.current is logged with it.
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

# protect_oom: make our replicas the kernel's LAST choice when the cgroup
# hits its limit. -900 (not -1000) keeps them killable as a true last
# resort so the kernel always has a victim and never wedges; every other
# tenant's process stays at the default 0 and is chosen first. Applied to
# the api_server and its EngineCore child every tick (the child only exists
# after boot, and both get replaced on relaunch), idempotent.
#
# Verified 2026-08-29: this container has NO CAP_SYS_RESOURCE (CapEff
# a80425fb), so lowering oom_score_adj is EACCES for root too. The
# function detects that on its first attempt, alerts once, and disables
# itself -- kept in place because it becomes effective automatically on a
# host/container that grants the capability. Until then the only real
# protection is keeping the cgroup away from its limit: /dev/shm held
# ~1 TB of model dumps (mostly other sessions') at the time.
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
        alert "cannot lower oom_score_adj (no CAP_SYS_RESOURCE in this container) -- OOM protection disabled; replicas stay default-priority OOM victims"
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
    tick "${r%%:*}" "${r##*:}" "/workspace/vllm_awq_gpu${r%%:*}.log"
  done
  protect_oom
  sleep 15
done
