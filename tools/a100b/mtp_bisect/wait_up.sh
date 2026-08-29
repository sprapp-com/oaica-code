#!/bin/bash
# wait_up.sh -- poll our test instance's /v1/models until 200 (timeout 480s),
# then do one real 1-token chat completion (matches production's own
# chat_probe_ok, since GET /v1/models can be 200 through a wedged
# scheduler). Prints UP or FAILED (+log tail) on failure.
set -u

PORT=30130
LOGFILE=/workspace/mtp_bisect/current.log
WORKDIR=/workspace/mtp_bisect
TIMEOUT=480
MODEL=oaica-35b-a3b-vision

# figure out which VARIANT.log is the freshest (launch.sh writes VARIANT.log)
LATEST_LOG=$(ls -t "$WORKDIR"/*.log 2>/dev/null | grep -v '\.prev$' | head -1)

deadline=$(( $(date +%s) + TIMEOUT ))
up=0
while [ "$(date +%s)" -lt "$deadline" ]; do
  code=$(curl -s -o /dev/null -w '%{http_code}' -m 5 "http://127.0.0.1:${PORT}/v1/models" 2>/dev/null)
  if [ "$code" = "200" ]; then
    up=1
    break
  fi
  sleep 3
done

if [ "$up" -ne 1 ]; then
  echo "FAILED (never reached 200 on /v1/models within ${TIMEOUT}s)"
  [ -n "${LATEST_LOG:-}" ] && { echo "--- log tail ($LATEST_LOG) ---"; tail -n 60 "$LATEST_LOG"; }
  exit 1
fi

body=$(curl -s -m 60 -o - -w '\n%{http_code}' \
  "http://127.0.0.1:${PORT}/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -d "{\"model\":\"${MODEL}\",\"messages\":[{\"role\":\"user\",\"content\":\"ping\"}],\"max_tokens\":1}" \
  2>/dev/null)
code=${body##*$'\n'}

if [ "$code" = "200" ]; then
  echo "UP"
  exit 0
else
  echo "FAILED (chat completion probe returned HTTP $code)"
  [ -n "${LATEST_LOG:-}" ] && { echo "--- log tail ($LATEST_LOG) ---"; tail -n 60 "$LATEST_LOG"; }
  exit 1
fi
