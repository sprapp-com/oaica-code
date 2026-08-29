#!/bin/bash
# run_variant.sh VARIANT [extra vllm args...]
#
# launch -> wait_up -> load (6 min, concurrency 8) -> verdict, appended to
# results.txt. Intended for a human/orchestrator to invoke per variant; NOT
# invoked with a full 6-minute run by the smoke test (see launch.sh comment
# / spec) -- smoke test uses --minutes 0.5 --concurrency 2 directly.
set -u

WORKDIR=/workspace/mtp_bisect
PORT=30130

if [ $# -lt 1 ]; then
  echo "usage: $0 VARIANT [extra vllm args...]" >&2
  exit 1
fi

VARIANT=$1
shift
LOGFILE="$WORKDIR/${VARIANT}.log"

echo "=== launching variant '$VARIANT' ==="
PID=$("$WORKDIR/launch.sh" "$VARIANT" "$@" | tail -1)
echo "PID=$PID"

echo "=== waiting for it to come up ==="
UP_OUT=$("$WORKDIR/wait_up.sh")
echo "$UP_OUT"

if ! echo "$UP_OUT" | grep -q '^UP'; then
  VERDICT="${VARIANT}: CRASHED (failed to come up)"
  echo "$VERDICT" | tee -a "$WORKDIR/results.txt"
  exit 1
fi

echo "=== running load for 6 minutes, concurrency 8 ==="
LOAD_OUT=$(python3 "$WORKDIR/load.py" --port "$PORT" --concurrency 8 --minutes 6 2>&1)
LOAD_RC=$?
echo "$LOAD_OUT"

echo "=== post-run check ==="
alive=0
if [ -n "$PID" ] && kill -0 "$PID" 2>/dev/null; then
  alive=1
fi

reason=""
if grep -qE 'AcceleratorError|illegal memory|EngineDeadError|Traceback' "$LOGFILE" 2>/dev/null; then
  reason=$(grep -oE 'AcceleratorError|illegal memory|EngineDeadError|Traceback' "$LOGFILE" | sort -u | tr '\n' ',' | sed 's/,$//')
fi

if [ "$alive" = "1" ] && [ -z "$reason" ] && [ "$LOAD_RC" -eq 0 ]; then
  VERDICT="${VARIANT}: SURVIVED"
else
  if [ -z "$reason" ]; then
    if [ "$alive" = "0" ]; then
      reason="process died"
    else
      reason="load.py detected engine-death signal (exit $LOAD_RC)"
    fi
  fi
  VERDICT="${VARIANT}: CRASHED (${reason})"
fi

echo "$VERDICT" | tee -a "$WORKDIR/results.txt"
