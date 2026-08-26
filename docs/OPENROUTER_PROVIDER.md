# OpenRouter provider — self-hosted kat-awq

How we publish kat-awq as an OpenRouter inference provider: self-hosted on
the already-rented a100b (near-zero marginal cost). There is NO failover
region today -- see "Cog fallback (not built)" below before claiming one
anywhere.

## Architecture

```
OpenRouter
   │
   ├─ [PRIMARY]  https://api.oaica.com/v1   (cloudflared tunnel)
                  └─ oaica-gateway (:8081, sha256 Bearer key, metering ledger)
                       ├─ GET  /health /privacy /terms /status  (public, no key)
                       ├─ GET  /models                            -> from config (pricing, context)
                       └─ POST /v1/chat/completions               -> gatekeeper (:30098) -> katlb (:30099) -> 2x kat-awq
```

## Components

| Piece | Where | Status |
|---|---|---|
| kat-awq fleet | a100b, vLLM, **2 replicas**: GPU0 :30199 + GPU2 :30105, hardened watchdog `/workspace/vllm_awq_watchdog.sh` (see `tools/a100b/`) | Running, load-balanced by katlb (chat-aware probe) |
| Gateway (`tools/gateway/oaica-gateway`) | a100b:8081, binary+config+ledger under `/workspace/` (root overlay is full), launched by `/workspace/gw-swap.sh` | Running (v2: hashed keys, metering, gatekeeper upstream) |
| Cloudflare tunnel | `oaica-api` tunnel (unisqu account, `125f3856…`); cloudflared runs ON a100b (`/workspace/cf/run.sh`, token at `/workspace/cf/token`) -> `localhost:8081`. No `.91` hop, no SSH tunnel. | Running |
| Cloudflare DNS + ingress | `api.oaica.com` CNAME -> tunnel `oaica-api`, ingress `api.oaica.com -> http://localhost:8081` | Live |
| Legal pages | embedded in the gateway binary from `tools/gateway/legal/*.md`, served at `/privacy` `/terms` `/status` | Live |

## The gateway

`tools/gateway/` — Go, flat-config (SIGHUP reloadable), same pattern as
`gatekeeper`/`katlb`. Config:

```json
{
  "upstream_addr": "http://127.0.0.1:30098",
  "listen_addr":   ":8081",
  "ledger_path":   "/workspace/oaica-gateway-ledger.jsonl",
  "api_keys":      [ {"sha256": "<sha256 hex of the key>", "label": "openrouter"} ],
  "models": [ {
    "id": "kat-awq", "upstream_id": "kat-awq", "owned_by": "oaica",
    "context_length": 262144, "max_completion_tokens": 32768,
    "pricing": {"prompt": "0.00000005", "completion": "0.00000012"}
  } ]
}
```

Keys are stored as sha256 digests, never plaintext (`printf '%s' "$KEY" |
sha256sum`). The live key is in `~/.secrets/oaica_openrouter_key` on the
laptop only -- it must never appear in this repo.

- `GET /health` (unauthenticated) -> 200 only when the upstream answers,
  else 503. Point uptime monitors and OpenRouter here.
- `GET /models` returns a standardized OpenAI list with `context_length`,
  `max_completion_tokens` and per-token `pricing` (served from config, so it
  stays up even if a backend is momentarily down).
- `POST /v1/chat/completions` + `/v1/completions`: model id validated against
  the config (accepts `kat-awq` or `oaica/kat-awq`) and rewritten to
  `upstream_id`; the caller's key is stripped and the gateway's own upstream
  credential (`OAICA_GATEWAY_UPSTREAM_KEY`, a gatekeeper key on the
  `openrouter` tier) is attached; streaming requests get
  `stream_options.include_usage=true` injected so vLLM emits a final usage
  chunk. Every completion is appended to the JSONL ledger (request id, key
  label, model, prompt/completion tokens, status, latency, `usage_seen`).
- Non-streaming requests are clamped to **4096** output tokens (down from
  8192) and get an HTTP 504 if they haven't finished within 90s — use
  `stream:true` for longer replies.
- Everything else is 404 before touching the proxy (vLLM's /metrics,
  /tokenize, dev endpoints are unreachable from the public key).
- SIGHUP reloads config and rebuilds the upstream proxy; a bad file is
  rejected and the previous config kept -- reload never kills the gateway.

Upstream is **gatekeeper (:30098)**, not raw katlb, so external traffic gets
the `openrouter` tier's concurrency cap (32) and cannot starve the three
internal machines that share katlb.

Build: `cd tools/gateway && go build -o oaica-gateway main.go`.

## Rotating the OpenRouter key (zero downtime)

1. Generate a new key.
2. Append its sha256 as a second `api_keys` entry in
   `/workspace/oaica-gateway.json` with a dated label (both keys are valid
   while both entries exist).
3. Reload without dropping connections:
   `kill -HUP $(ss -ltnp | grep ':8081' | grep -oE 'pid=[0-9]+' | cut -d= -f2)`
   — the gateway reloads config on SIGHUP.
4. Hand the new key to OpenRouter.
5. Once the ledger shows requests under the new label, remove the old
   `api_keys` entry and HUP again.

## Public URL: api.oaica.com

Cut over from the interim `oaica.samwong.com` path on 2026-08-26 07:30 +08
(2026-08-25 23:30 UTC), commit `b8c3ed9e`. `api.oaica.com` lives in the
unisqu Cloudflare account (`125f3856…`) that owns `oaica.com`; the
`oaica-api` tunnel runs cloudflared directly ON the a100b box
(`/workspace/cf/run.sh`, token at `/workspace/cf/token`), so there is no
`.91` hop and no SSH tunnel anywhere in the public path any more.
`oaica.samwong.com` (samwong account, via `.91`) still resolves but is no
longer the published URL. `https://api.oaica.com/models` and
`https://api.oaica.com/v1/chat/completions` are live.

## Cog fallback (not built -- do not list it)

A Replicate/Cog sketch (cog.yaml + a raw-text `predict.py`, 8192 ctx) was
tried and deleted on 2026-08-26 so nobody mistakes it for capacity. It was
NOT a usable failover: it spoke Cog's predict API, not OpenAI; it had no
tool calling; and Replicate runs in the US, which would change the
inference-location answer on the OpenRouter form. Until a fallback is
actually built, tested end-to-end and its region documented, the provider
is single-region (JP) with no failover.

## Pricing decision (see OpenRouter form)

Per-token, self-hosted primary. kat-awq already priced in remotes.json at
`$0.05/M` input / `$0.12/M` output. Because the a100b is already rented and
kat-awq runs on idle GPUs (~$0 marginal), even low utilization is profit.
Breakeven on a *dedicated* box would need 30-60%+ utilization — don't
dedicate new GPU spend until demand justifies it.

## Gotchas hit

- **Root overlay (`/`) on a100b is 100% full.** Anything the gateway needs
  to write (binary, config, ledger, log) lives under `/workspace/`. Do not
  put files in `/root/` -- `install`/`cp` there fails with ENOSPC.
- `pgrep -f <pattern>` self-matches the calling ssh shell and killing that
  drops the session mid-script. Find the listener by port instead
  (`ss -ltnp | grep :8081`) and run multi-step swaps as a detached on-box
  script (`setsid nohup /workspace/gw-swap.sh`).
- kat-awq model dir on /dev/shm can be deleted by shared-box churn — the
  watchdog crash-loops if the weights vanish. Re-download
  `Ar4ikov/KAT-Coder-V2.5-Dev-AWQ-W4A16-ASYM` to `/dev/shm/kat_awq`.
- The downloaded tokenizer lacks a `chat_template` -> vLLM 400 on
  chat/completions. Fix: copy `chat_template` from a working sibling
  (`kat_coder_vl_mtp`) into `kat_awq/tokenizer_config.json`.
- OpenRouter requires the OpenAI `/models` + `/v1/chat/completions` contract;
  vLLM already speaks it, so the gateway is a thin wrapper, not a rewrite.
