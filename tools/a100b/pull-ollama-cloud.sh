#!/bin/bash
# pull-ollama-cloud.sh — pull a set of Ollama :cloud model pointers onto
# every client box's local daemon, in one shot.
#
# Why this exists: `ollama pull <model>:cloud` only registers a lightweight
# manifest pointer on the LOCAL daemon (a few hundred bytes — not the real
# weights, those stay hosted). Each box's daemon needs its own pull; there
# is no fleet-wide propagation. This script is the manual, no-code-change
# path for "add/refresh a cloud model everywhere right now" — see
# docs/MODELS_AND_PLANS.md's "Discovery, drift-safety, and manual refresh"
# section for the full picture (how discovery already stays live, why a
# format change upstream degrades safely instead of corrupting the list,
# and when you'd reach for this script instead of just waiting).
#
# Usage:
#   tools/a100b/pull-ollama-cloud.sh glm-5.3-flash:cloud [more:cloud ...]
#   tools/a100b/pull-ollama-cloud.sh                       # pulls the DEFAULT_MODELS list below
#
# Runs locally AND on every host in HOSTS (edit for your fleet — this
# ships with the boxes this session actually used). ssh must already be
# configured (key auth, no password prompt) for each host.
set -u

HOSTS=(ubuntu@192.168.0.91 ubuntu@192.168.0.46)
DEFAULT_MODELS=(glm-5.3-flash:cloud)

MODELS=("$@")
if [ ${#MODELS[@]} -eq 0 ]; then
  MODELS=("${DEFAULT_MODELS[@]}")
fi

pull_here() {
  for m in "${MODELS[@]}"; do
    echo "[local] pulling $m"
    ollama pull "$m" || echo "[local] FAILED: $m"
  done
}

pull_remote() {
  local host="$1"
  for m in "${MODELS[@]}"; do
    echo "[$host] pulling $m"
    timeout 30 ssh -o ConnectTimeout=15 "$host" "ollama pull '$m'" < /dev/null \
      || echo "[$host] FAILED: $m (host may be unreachable or flaky — see machines-91-and-laptop memory note)"
  done
}

pull_here
for h in "${HOSTS[@]}"; do
  pull_remote "$h"
done

echo
echo "Verify with: ollama list | grep -i cloud   (locally and per-host)"
