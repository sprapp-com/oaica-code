#!/bin/bash
# Cut the public API over from oaica.samwong.com to api.oaica.com.
#
# Runs on the LAPTOP. Needs a Cloudflare API token for the account that owns
# oaica.com (Cloudflare.com@unisqu.com, account 125f38561fc4bea693a94f17a503a049)
# with:  Zone:DNS:Edit (oaica.com)  +  Account:Cloudflare Tunnel:Edit.
# Every token seen so far (7ZA9…, cfut_…) is read-only there -- verified by a
# DNS-create probe returning "Authentication error". Put a write token in
# ~/.secrets/cloudflare_oaica.env as CF_API_TOKEN=... and run this.
#
# What it does, idempotently:
#   1. create (or reuse) a tunnel named "oaica-api" in the unisqu account
#   2. set its ingress: api.oaica.com -> http://localhost:8081 (the gateway)
#   3. CNAME api.oaica.com -> <tunnel>.cfargotunnel.com (proxied)
#   4. run cloudflared ON a100b with the tunnel token (no .91 hop)
#   5. verify /health /models /privacy /terms /status + a metered stream
# oaica.samwong.com is NOT touched; retire it only after step 5 passes.
set -euo pipefail
. ~/.secrets/cloudflare_oaica.env
: "${CF_API_TOKEN:?CF_API_TOKEN missing in ~/.secrets/cloudflare_oaica.env}"
ACCT=125f38561fc4bea693a94f17a503a049
ZONE=b26efc782d3dbf7b7fc7dff2384db1ca
HOST=api.oaica.com
API=https://api.cloudflare.com/client/v4
cf() { curl -sS -m 20 -H "Authorization: Bearer $CF_API_TOKEN" -H "Content-Type: application/json" "$@"; }
ok() { python3 -c 'import json,sys; d=json.load(sys.stdin); sys.exit(0 if d.get("success") else 1)'; }

echo "== 0. token must be able to write oaica.com"
cf -X POST "$API/zones/$ZONE/dns_records" -d '{"type":"TXT","name":"_cfwritetest.'"$HOST"'","content":"probe","ttl":60}' > /tmp/cf_w.json
if ! ok < /tmp/cf_w.json; then echo "TOKEN IS READ-ONLY on oaica.com:"; cat /tmp/cf_w.json; exit 2; fi
RID=$(python3 -c 'import json; print(json.load(open("/tmp/cf_w.json"))["result"]["id"])'); cf -X DELETE "$API/zones/$ZONE/dns_records/$RID" >/dev/null
echo "   write OK"

echo "== 1. tunnel oaica-api (reuse if present)"
TID=$(cf "$API/accounts/$ACCT/cfd_tunnel?name=oaica-api&is_deleted=false" | python3 -c 'import json,sys; r=json.load(sys.stdin)["result"]; print(r[0]["id"] if r else "")')
if [ -z "$TID" ]; then
  TID=$(cf -X POST "$API/accounts/$ACCT/cfd_tunnel" -d '{"name":"oaica-api","config_src":"cloudflare"}' | python3 -c 'import json,sys; print(json.load(sys.stdin)["result"]["id"])')
  echo "   created $TID"
else echo "   reusing $TID"; fi
TOKEN=$(cf "$API/accounts/$ACCT/cfd_tunnel/$TID/token" | python3 -c 'import json,sys; print(json.load(sys.stdin)["result"])')

echo "== 2. ingress $HOST -> http://localhost:8081"
cf -X PUT "$API/accounts/$ACCT/cfd_tunnel/$TID/configurations" -d '{"config":{"ingress":[{"hostname":"'"$HOST"'","service":"http://localhost:8081"},{"service":"http_status:404"}]}}' | ok && echo "   set"

echo "== 3. DNS CNAME $HOST -> $TID.cfargotunnel.com (proxied)"
EXIST=$(cf "$API/zones/$ZONE/dns_records?name=$HOST" | python3 -c 'import json,sys; r=json.load(sys.stdin)["result"]; print(r[0]["id"] if r else "")')
BODY='{"type":"CNAME","name":"'"$HOST"'","content":"'"$TID"'.cfargotunnel.com","proxied":true,"ttl":1}'
if [ -n "$EXIST" ]; then cf -X PUT "$API/zones/$ZONE/dns_records/$EXIST" -d "$BODY" | ok && echo "   updated"; else cf -X POST "$API/zones/$ZONE/dns_records" -d "$BODY" | ok && echo "   created"; fi

echo "== 4. run cloudflared on a100b with the tunnel token"
ssh prism-a100b "mkdir -p /workspace/cf && cat > /workspace/cf/run.sh <<'RS'
#!/bin/bash
exec /usr/local/bin/cloudflared tunnel --no-autoupdate run --token \"\$(cat /workspace/cf/token)\"
RS
chmod 700 /workspace/cf/run.sh; umask 077; printf '%s' '$TOKEN' > /workspace/cf/token
for p in \$(pgrep -f 'cloudflared tunnel'); do kill \$p; done; sleep 1
setsid nohup /workspace/cf/run.sh > /workspace/cf/cloudflared.log 2>&1 < /dev/null &
sleep 8; grep -E 'Registered tunnel connection|ERR' /workspace/cf/cloudflared.log | tail -3"

echo "== 5. verify"
sleep 5
for p in /health /privacy /terms /status; do printf '   %-9s ' "$p"; curl -s -m 20 -o /dev/null -w '%{http_code}\n' "https://$HOST$p"; done
KEY=$(cat ~/.secrets/oaica_openrouter_key)
printf '   /models   '; curl -s -m 20 -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $KEY" "https://$HOST/models"
printf '   stream    '; curl -s -m 60 -N -X POST "https://$HOST/v1/chat/completions" -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{"model":"oaica/kat-awq","stream":true,"messages":[{"role":"user","content":"say ok"}],"max_tokens":4}' -o /tmp/cut.sse -w '%{http_code} '; grep -o '"usage":{[^}]*}' /tmp/cut.sse | tail -1
echo "== done. Now run: tools/a100b/rebrand-api-oaica-com.sh  (swaps hostname in legal pages/docs/form, rebuilds+redeploys gateway)"
