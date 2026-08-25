# OpenRouter provider — self-hosted kat-awq + Cog fallback

How we publish kat-awq as an OpenRouter inference provider: a self-hosted
primary (near-zero marginal cost on the already-rented a100b) with a
Replicate/Cog fallback for managed uptime/scale.

## Architecture

```
OpenRouter
   │
   ├─ [PRIMARY]  https://oaica.samwong.com/v1   (cloudflared tunnel)
   │              └─ oaica-gateway (:8081, Bearer key auth)
   │                   ├─ GET  /models               -> standardized OpenAI list (from config)
   │                   └─ POST /v1/chat/completions   -> katlb (:30099) -> kat-awq fleet
   │
   └─ [FALLBACK] Replicate (Cog-packaged kat-awq)   — paid only on failover
```

## Components

| Piece | Where | Status |
|---|---|---|
| kat-awq fleet | a100b, vLLM, **2 replicas**: GPU0 :30199 + GPU2 :30105, hardened watchdog `/workspace/vllm_awq_watchdog.sh` (see `tools/a100b/`) | Running, load-balanced by katlb (chat-aware probe) |
| Gateway (`tools/gateway/oaica-gateway`) | a100b:8081, binary+config+ledger under `/workspace/` (root overlay is full), launched by `/workspace/gw-swap.sh` | Running (v2: hashed keys, metering, gatekeeper upstream) |
| Gateway tunnel | .91 systemd user service `oaica-gateway-tunnel.service` (8081->a100b:8081) | Running |
| Cloudflare DNS | `oaica.samwong.com` CNAME -> tunnel `bbd1217e` | Created |
| Cloudflare ingress | tunnel public hostname `oaica.samwong.com -> http://localhost:8081` | **Manual (dashboard)** |
| Cog fallback (`tools/cog-kat-awq/`) | Replicate | Project written, not pushed |

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
- Everything else is 404 before touching the proxy (vLLM's /metrics,
  /tokenize, dev endpoints are unreachable from the public key).
- SIGHUP reloads config and rebuilds the upstream proxy; a bad file is
  rejected and the previous config kept -- reload never kills the gateway.

Upstream is **gatekeeper (:30098)**, not raw katlb, so external traffic gets
the `openrouter` tier's concurrency cap (32) and cannot starve the three
internal machines that share katlb.

Build: `cd tools/gateway && go build -o oaica-gateway main.go`.

## Public URL (one manual step)

DNS CNAME and the SSH tunnel are set up. The remaining step is **only doable
in the Cloudflare dashboard** (the tunnel is token-based; it reads ingress
from Cloudflare, not a local file):

> Cloudflare dashboard -> tunnel `bbd1217e-...` -> Public Hostnames -> add
> `oaica.samwong.com` -> service `http://localhost:8081`.

Then `https://oaica.samwong.com/models` and
`https://oaica.samwong.com/v1/chat/completions` are live.

## Cog fallback

`tools/cog-kat-awq/` — a Cog project wrapping the same AWQ weights via vLLM.
Deploy: `cog login && cog push r8.im/<you>/kat-awq`. It's the failover route;
Replicate charges only when traffic actually goes there, so it's cheap
insurance. Replicate models can be exposed to OpenRouter via Replicate's own
OpenAI-compatible integration (a second route behind the self-host primary).

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
