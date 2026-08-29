#!/bin/bash
# oaicalb_reload.sh -- apply an edited /workspace/oaicalb.json to the RUNNING
# load balancer without restarting it (SIGHUP; see tools/oaicalb/main.go,
# backendPool). Validates the file first so a typo never reaches the LB,
# then shows the LB's own log line for the reload and the resulting
# /status. Only the backend list is reloadable; listen addresses, probe
# settings and metering still need a restart.
#
# Usage: /workspace/oaicalb_reload.sh
set -u
CFG=${CFG:-/workspace/oaicalb.json}
LOG=${LOG:-/workspace/oaicalb.log}
STATUS_URL=${STATUS_URL:-http://127.0.0.1:8092/status}

python3 -c "import json,sys; d=json.load(open('$CFG')); bl=d.get('backend_configs') or d.get('backends') or []; assert bl, 'no backends'; print('config ok:', len(bl), 'backend(s)')" || { echo "REFUSING: $CFG is not valid -- LB left untouched"; exit 2; }
PID=$(ss -ltnp 2>/dev/null | grep ':30099 ' | grep -oE 'pid=[0-9]+' | head -1 | cut -d= -f2)
[ -n "$PID" ] || { echo "no oaicalb listening on :30099"; exit 3; }
before=$(wc -l < "$LOG")
kill -HUP "$PID" && echo "SIGHUP sent to oaicalb pid $PID"
sleep 1
tail -n +"$((before+1))" "$LOG" | grep "SIGHUP" || echo "(no reload line yet -- check $LOG)"
sleep 2
curl -s -m 5 "$STATUS_URL"
