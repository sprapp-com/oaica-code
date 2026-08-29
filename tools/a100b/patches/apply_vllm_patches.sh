#!/bin/bash
# Apply oaica's local vLLM patches to the installed site-packages on a100b.
# Idempotent: skips a patch that is already applied, refuses one that
# conflicts. Keeps a pristine copy of each touched file next to it as
# <file>.oaica-orig so `--revert` can restore it. Re-run after any vLLM
# reinstall/upgrade (and re-verify the patch still applies -- these target
# vLLM 0.24.0 exactly; a newer vLLM may already carry the upstream fix).
#
#   apply_vllm_patches.sh            # apply all patches in this directory
#   apply_vllm_patches.sh --revert   # restore the pristine files
set -u
SP=${SP:-/usr/local/lib/python3.12/dist-packages}
DIR=$(cd "$(dirname "$0")" && pwd)
mode=${1:-apply}
cd "$SP" || exit 1
for p in "$DIR"/*.diff; do
  files=$(grep '^+++ b/' "$p" | sed 's#^+++ b/##')
  if [ "$mode" = "--revert" ]; then
    for f in $files; do
      [ -f "$f.oaica-orig" ] && cp -p "$f.oaica-orig" "$f" && echo "reverted $f"
    done
    continue
  fi
  if patch -p1 -R --dry-run -s < "$p" >/dev/null 2>&1; then
    echo "already applied: $(basename "$p")"; continue
  fi
  if ! patch -p1 --dry-run -s < "$p" >/dev/null 2>&1; then
    echo "CONFLICT, not applied: $(basename "$p")"; exit 2
  fi
  for f in $files; do [ -f "$f.oaica-orig" ] || cp -p "$f" "$f.oaica-orig"; done
  patch -p1 -s < "$p" && echo "applied: $(basename "$p")"
done
# Triton caches compiled kernels by source hash, so no cache flush is needed;
# a restarted vLLM picks the patched kernel up on its next JIT.
