#!/bin/bash
# Control-plane supervisor for a100b: keeps katlb, gatekeeper and the gateway
# alive. The vLLM replicas have their own watchdog (vllm_awq_watchdog.sh);
# before this script NOTHING supervised the three proxies -- a crash of any
# one silently took the public API down, and the legacy /root/*_watchdog.sh
# loops would relaunch the OLD binaries from /root (seen 2026-08-25:
# "bind: address already in use" in /tmp/katlb.log as the legacy loop fought
# the v2 binary).
#
# Detection is by LISTENING PORT, not pgrep -f: a pgrep pattern matches the
# probing shell itself and any stale process, which is exactly how the
# legacy loops went wrong.
#
# Usage: nohup /workspace/stack_watchdog.sh > /workspace/stack_watchdog.out 2>&1 &
# Reboot: cron @reboot or /etc/rc.local must start THIS script and
# vllm_awq_watchdog.sh; see tools/a100b/README.md "Reboot".
set -u
LOG=/workspace/stack_watchdog.log
log() { echo "$(date -Is) $*" | tee -a "$LOG"; }
listening() { ss -ltn 2>/dev/null | grep -q ":$1 "; }

# The gateway's upstream credential is a gatekeeper key on the "openrouter"
# tier. It lives in a 0600 file, never in a script.
GW_KEY_FILE=/workspace/gateway_upstream.key

start_katlb() {
  log "starting katlb on :30099"
  nohup /workspace/katlb-linux-amd64 -config /workspace/katlb-kat-awq.json >> /workspace/katlb.log 2>&1 < /dev/null &
}
start_gatekeeper() {
  log "starting gatekeeper on :30098"
  ( cd /root && nohup ./gatekeeper -config /root/gatekeeper.json >> /workspace/gatekeeper.log 2>&1 < /dev/null & )
}
start_gateway() {
  if [ ! -s "$GW_KEY_FILE" ]; then
    log "ALERT: $GW_KEY_FILE missing/empty -- gateway would forward no upstream key; not starting"
    return
  fi
  log "starting gateway on :8081"
  OAICA_GATEWAY_UPSTREAM_KEY="$(cat "$GW_KEY_FILE")" \
    nohup /workspace/oaica-gateway --config /workspace/oaica-gateway.json >> /workspace/oaica-gateway.log 2>&1 < /dev/null &
}

log "stack watchdog start"
while true; do
  listening 30099 || start_katlb
  listening 30098 || start_gatekeeper
  listening 8081  || start_gateway
  sleep 10
done
