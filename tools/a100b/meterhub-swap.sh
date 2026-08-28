#!/bin/bash
# Atomically swap the running meterhub for /workspace/meterhub-linux-amd64.
# Kills by exact pid of the LISTENER on :8095 (never pgrep -f).
set -u
OLD=$(ss -ltnp 2>/dev/null | grep ':8095' | grep -oE 'pid=[0-9]+' | head -1 | cut -d= -f2)
echo "old listener pid=${OLD:-none}"
[ -n "$OLD" ] && kill "$OLD" && sleep 1
nohup /workspace/meterhub-linux-amd64 --config /workspace/meterhub.json >> /workspace/meterhub.log 2>&1 < /dev/null &
sleep 1
NEW=$(ss -ltnp 2>/dev/null | grep ':8095' | grep -oE 'pid=[0-9]+' | head -1 | cut -d= -f2)
echo "new listener pid=${NEW:-NONE}"
tail -5 /workspace/meterhub.log
