# OpenRouter provider application — final answers (2026-08-26)

> **Domain:** the URLs below are `api.oaica.com` because that is what is
> live. The intended domain is `api.oaica.com`; the cutover is scripted
> (`tools/a100b/cutover-api-oaica-com.sh`) and blocked only on a
> write-capable Cloudflare token for the unisqu account that owns oaica.com.
> Run the cutover BEFORE submitting so the form carries the final hostname.

Everything below is verified live at time of writing. Anything marked
**you** needs a human to type it into the form.

| Form field | Answer |
|---|---|
| Company | BizTransit Sdn Bhd |
| Website | https://bcz.com/ |
| Email | biztransit@bcz.com |
| Display name | Oaica |
| Slug | oaica |
| Distinguishing features | Unique Models, Unique Infrastructure, Low Pricing |
| Extra details | Self-hosted vLLM fleet (2x A100 80GB replicas, least-conn LB with chat-aware health probes, automatic failover between replicas). Serves KAT-Coder-V2.5-Dev (35B MoE, int4 AWQ) with 262k context and tool calling. No cloud markup. Per-key concurrency limits; per-request token ledger. Single host, no cross-region failover yet (see /status). |
| URL to /models API | https://api.oaica.com/models — **requires** `Authorization: Bearer <key>`; **you** paste the key from `~/.secrets/oaica_openrouter_key` (laptop) into the same form |
| API base URL | https://api.oaica.com/v1 |
| Privacy Policy URL | https://api.oaica.com/privacy |
| Terms of Service URL | https://api.oaica.com/terms |
| Data policy | We do not train on, sell, or persist prompts or completions. Gateway, load balancer and auth layer are pass-through reverse proxies with no request-body logging (verified: a marker prompt sent through the live stack appears in no log). Request content lives only in RAM/GPU memory while served. Metadata (timestamp, status, key label, backend, token counts, latency) is kept up to 30 days for billing reconciliation. |
| Supported output modalities | Text |
| Inference location | **JP** (the GPU host is in Kanazawa, Japan per its public IP; Cloudflare edge transits traffic but does not run inference). Do NOT put MY here -- MY is HQ, not compute. |
| HQ location | MY (Malaysia) |
| Model | `kat-awq` = Ar4ikov/KAT-Coder-V2.5-Dev-AWQ-W4A16-ASYM @ 446ea8c6 (int4 AWQ of Kwaipilot/KAT-Coder-V2.5-Dev). Licence: Apache-2.0 on both the base and the quant (verified on HF) -- commercial hosted serving permitted. |
| Pricing | $0.05 / M input, $0.12 / M output (published in `/models` as per-token decimals) |
| Capacity | 2 replicas, ~3-5k tok/s each, 32 concurrent for the OpenRouter key (gatekeeper tier). Not the historical 6-replica number. |
| Status / health | https://api.oaica.com/status (statement) and https://api.oaica.com/health (200/503, for monitors) |

## Before you click submit

1. Toggle the OpenRouter **privacy/data-policy** setting so free/stealth
   providers are allowed if you intend to *consume* models too (unrelated to
   listing, but the same account).
2. Point a free uptime monitor at `/health` and note its public page in
   the form's status field if asked.
3. Rotate the gateway key after OpenRouter has it stored, if you handed it
   over any channel other than the form itself.
