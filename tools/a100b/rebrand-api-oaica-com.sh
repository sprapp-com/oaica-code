#!/bin/bash
# Second half of the api.oaica.com cutover: swap the public hostname in the
# legal pages, docs and form answers, rebuild the gateway (legal pages are
# embedded), redeploy it on a100b, verify, then commit.
# Run AFTER tools/a100b/cutover-api-oaica-com.sh has verified the new host.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
OLD=oaica.samwong.com; NEW=api.oaica.com
echo "== 1. rewrite $OLD -> $NEW in tracked files"
git grep -l "$OLD" | xargs sed -i "s/$OLD/$NEW/g"
git grep -n "$OLD" && { echo "leftover refs above"; exit 2; } || echo "   none left"

echo "== 2. rebuild gateway (embeds legal pages) + tests"
(cd tools/gateway && go test ./... >/dev/null && GOOS=linux GOARCH=amd64 go build -o oaica-gateway . && echo "   built")

echo "== 3. redeploy on a100b"
scp -q -o StrictHostKeyChecking=no -i ~/.ssh/id_vastai -P 20278 tools/gateway/oaica-gateway root@202.122.49.242:/workspace/oaica-gateway.new
ssh prism-a100b 'chmod 0755 /workspace/oaica-gateway.new && mv /workspace/oaica-gateway.new /workspace/oaica-gateway && setsid nohup /workspace/gw-swap.sh > /workspace/gw-swap.out 2>&1 < /dev/null & sleep 5; cat /workspace/gw-swap.out'

echo "== 4. verify on $NEW"
for p in /health /privacy /terms /status; do printf '   %-9s ' "$p"; curl -s -m 20 -o /tmp/pg -w '%{http_code} ' "https://$NEW$p"; grep -c "$NEW" /tmp/pg | sed 's/^/host-refs=/'; done

echo "== 5. commit"
git add -A tools/gateway/legal docs tools/a100b/README.md
git commit -q -m "cutover: public API is api.oaica.com (was oaica.samwong.com)

oaica.com is the product domain; api.oaica.com now fronts the gateway via a
cloudflared tunnel running directly on a100b (one hop fewer than the old
.91 path). Legal pages, provider doc and form answers updated. The old
hostname stays live until this is confirmed in OpenRouter, then its .91
ingress can be removed.

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_0169r8kLWMBjJWR81WxAmEBm" && git log --oneline -1
