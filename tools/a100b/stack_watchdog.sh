#!/bin/bash
# Control-plane supervisor for a100b: keeps oaicalb, gatekeeper and the gateway
# alive. The vLLM replicas have their own watchdog (vllm_awq_watchdog.sh);
# before this script NOTHING supervised the three proxies -- a crash of any
# one silently took the public API down, and the legacy /root/*_watchdog.sh
# loops would relaunch the OLD binaries from /root (seen 2026-08-25:
# "bind: address already in use" in /tmp/katlb.log as the legacy loop fought (pre-rename; log now oaicalb.log)
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

start_oaicalb() {
  log "starting oaicalb on :30099"
  nohup /workspace/oaicalb-linux-amd64 -config /workspace/oaicalb.json >> /workspace/oaicalb.log 2>&1 < /dev/null &
}
# Second model pool (Nemotron on GPU7, replica :30107) has its own oaicalb
# so a hung Nemotron replica can never be picked for an oaica-35b request
# and vice versa. The gateway reaches it directly via the model's own
# upstream_addr (no gatekeeper hop: the gateway already authenticates the
# caller, and :30120 binds loopback only).
start_nemotron_lb() {
  log "starting nemotron oaicalb on :30120"
  nohup /workspace/oaicalb-linux-amd64 -config /workspace/nemotron-oaicalb.json >> /workspace/nemotron-oaicalb.log 2>&1 < /dev/null &
}
start_gatekeeper() {
  # Since 2026-08-26 gatekeeper lives under /workspace like everything else
  # (binary 0700, config 0600 with plaintext keys -- gatekeeper does not
  # hash). The legacy /root/gatekeeper_watchdog.sh loop is disabled.
  log "starting gatekeeper on :30098"
  nohup /workspace/gatekeeper -config /workspace/gatekeeper.json >> /workspace/gatekeeper.log 2>&1 < /dev/null &
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

# The public path itself: the token-based `oaica-api` tunnel. cloudflared
# has no listening port, so detect it by its exact argv (-x: whole process
# name, then the --token flag that only OUR tunnel uses; other sessions run
# `cloudflared tunnel --url ...` quick tunnels on this box and must not be
# matched). Before 2026-08-26 nothing restarted it -- a crash silently took
# api.oaica.com down while every internal port stayed green.
tunnel_running() { pgrep -x cloudflared -a 2>/dev/null | grep -q -- '--token'; }
start_tunnel() {
  [ -x /workspace/cf/run.sh ] || { log "ALERT: /workspace/cf/run.sh missing -- public API is DOWN"; return; }
  log "starting cloudflared (oaica-api tunnel)"
  nohup /workspace/cf/run.sh >> /workspace/cf/cloudflared.log 2>&1 < /dev/null &
}

log "stack watchdog start"
while true; do
  listening 30099 || start_oaicalb
  listening 30120 || start_nemotron_lb
  listening 30098 || start_gatekeeper
  listening 8081  || start_gateway
  tunnel_running  || start_tunnel
  sleep 10
done
