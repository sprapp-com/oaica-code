# OpenRouter provider application — final answers (verified 2026-08-26)

> **Domain:** `api.oaica.com` is the final, live hostname (cut over from the
> interim `oaica.samwong.com` on 2026-08-26 07:30 +08 / 2026-08-25 23:30 UTC,
> commit b8c3ed9e, via the `oaica-api` Cloudflare tunnel running on a100b;
> see `tools/a100b/README.md`). The CLI users install from oaica.com (0.4.0)
> defaults to it.

Every row below was re-verified against the live stack on the date above
(see the log at the end). Anything marked **you** needs a human to type it
into the form.

| Form field | Answer |
|---|---|
| Company | BizTransit Sdn Bhd |
| Website | https://oaica.com/ |
| Email | oaica@sprapp.com |
| Display name | Oaica |
| Slug | oaica |
| Distinguishing features | Unique Models, Unique Infrastructure, Low Pricing |
| Extra details | Self-hosted vLLM fleet (2x A100 80GB replicas, least-conn LB with chat-aware health probes, automatic failover between replicas). Serves KAT-Coder-V2.5-Dev (35B MoE, int4 AWQ) with 262k context and tool calling. No cloud markup. Per-key concurrency limits; per-request token ledger. Single host, no cross-region failover yet (see /status). |
| URL to /models API | https://api.oaica.com/models — **public**, no key needed (also at `/v1/models`); served from config, so it stays up even if a backend is down. Completions still require `Authorization: Bearer <key>`; **you** paste the key from `~/.secrets/oaica_openrouter_key` (laptop) wherever the form asks for the API credential. |
| API base URL | https://api.oaica.com/v1 |
| Privacy Policy URL | https://api.oaica.com/privacy |
| Terms of Service URL | https://api.oaica.com/terms |
| Data policy | We do not train on, sell, or persist prompts or completions. Gateway, load balancer and auth layer are pass-through reverse proxies with no request-body logging (verified: a marker prompt sent through the live stack appears in no log). Request content lives only in RAM/GPU memory while served. Metadata (timestamp, request id, key label, model id and upstream model id, endpoint path, stream flag, HTTP status, prompt/completion token counts, latency, and two bookkeeping flags — whether upstream reported usage and whether the client aborted) is retained in an append-only, owner-only (0600) billing ledger for payout reconciliation; no request content, client IP or end-user identity is written. |
| Supported input modalities | Text only. The AWQ quant of kat-awq carries a vision config but produces garbage on image input; the gateway refuses images with a 400 and advertises `input_modalities: [text]`. |
| Supported output modalities | Text |
| Inference location | **JP** (the GPU host is in Kanazawa, Japan per its public IP; Cloudflare edge transits traffic but does not run inference). Do NOT put MY here -- MY is HQ, not compute. |
| HQ location | MY (Malaysia) |
| Output limits | `max_completion_tokens` 32768 on streaming. Non-streaming requests are clamped to 4096 output tokens and time out with HTTP 504 after 90 s (so the reply completes inside Cloudflare's 100 s edge timeout even at the ~80 tok/s a stream gets under full load); use `stream:true` for anything longer. Reasoning is on by default (vLLM `--reasoning-parser qwen3`) and can be disabled per request with `chat_template_kwargs: {"enable_thinking": false}`, which the gateway forwards unchanged. Thinking tokens count toward `max_tokens` and are billed as completion tokens; with very small `max_tokens` the visible `content` is `null` (non-stream) or only an empty first delta (stream) while `message.reasoning` is populated. `tools`/`tool_choice`, `response_format: json_object` and `stop` are supported and were verified live (see log). |
| Model | `kat-awq` = Ar4ikov/KAT-Coder-V2.5-Dev-AWQ-W4A16-ASYM @ 446ea8c6 (int4 AWQ of Kwaipilot/KAT-Coder-V2.5-Dev). Licence: Apache-2.0 on both the base and the quant (verified on HF) -- commercial hosted serving permitted. |
| Pricing | $0.05 / M input, $0.12 / M output (published in `/models` as per-token decimals) |
| Capacity | 2 replicas (GPU0 + GPU2). Measured through the public path at the 32-concurrency cap: ~2.5k tok/s aggregate, ~80 tok/s per stream. 32 is a request-admission cap; at the full 262k context about 19 requests fit in KV cache fleet-wide, beyond that requests queue. |
| Status / health | https://api.oaica.com/status (statement) and https://api.oaica.com/health (200/503, for monitors) |

## Before you click submit

1. Toggle the OpenRouter **privacy/data-policy** setting so free/stealth
   providers are allowed if you intend to *consume* models too (unrelated to
   listing, but the same account).
2. Point a free uptime monitor at `/health` and note its public page in
   the form's status field if asked.
3. Rotate the gateway key after OpenRouter has it stored, if you handed it
   over any channel other than the form itself (procedure: "Rotating the
   OpenRouter key" in `docs/OPENROUTER_PROVIDER.md`).
4. Make sure oaica@sprapp.com is a real, monitored mailbox before
   submitting — OpenRouter invites it to a Slack Connect channel and the
   legal pages tell customers to write to it.

## Verification log (2026-08-26, laptop -> public URL)

Independent re-check of every row above (15 agents, read-only, adversarial
re-check of each discrepancy), then fixes applied and re-verified:

- `GET /models` -> 200 with or without a key (made public 2026-08-26 after the decision matrix; `POST /v1/chat/completions` without a key -> 401 `{"error":{"code":"invalid_api_key",…}}`); one entry `kat-awq`: `pricing {prompt:"0.00000005", completion:"0.00000012"}`, `context_length 262144`, `max_completion_tokens 32768`, `architecture.input_modalities ["text"]`, `supported_parameters [max_tokens, temperature, top_p, stream, tools, tool_choice, stop, seed, response_format]`, `created` stable across calls. `/v1/models` identical.
- `/privacy` `/terms` `/status` `/health` -> 200; legal pages name BizTransit Sdn Bhd / oaica@sprapp.com / JP inference / ledger retention; `/status` states single host, no cross-region failover.
- No `X-Katlb-Backend` / `X-Gatekeeper-*` headers on `/models` or on a chat completion.
- Image `image_url` part -> 400, OpenAI error shape, not forwarded.
- Tools: `finish_reason: tool_calls`, `tool_calls[0].function = get_weather({"city": "Johor Bahru"})` — `chatcmpl-b9284eee7a411f9e`.
- `response_format: {type: json_object}` -> `{"capital": "Kuala Lumpur"}` (valid JSON) — `chatcmpl-a027c424c7ae3357`.
- `stop: ["seven"]` -> `one, two, three, four, five, six, ` `finish_reason: stop` — `chatcmpl-9ed312dcf839c412`.
- Tiny `max_tokens` -> `content: null`, `reasoning` populated; both replicas run `--reasoning-parser qwen3 --enable-auto-tool-choice --tool-call-parser qwen3_coder --max-model-len 262144`.
- Box (read-only): replicas listening on :30199 (GPU0, 74.6 GB used) and :30105 (GPU2, 74.6 GB); gatekeeper `openrouter` tier = 32; cloudflared token tunnel running on the box; public IP geolocates to Japan.
- Licences via `huggingface_hub`: base `Kwaipilot/KAT-Coder-V2.5-Dev` and quant `Ar4ikov/KAT-Coder-V2.5-Dev-AWQ-W4A16-ASYM` @ 446ea8c6 both `apache-2.0`.
- Ledger fields (gateway `ledgerEntry`): ts, request_id, key, model, upstream_model, path, stream, status, prompt_tokens, completion_tokens, latency_ms, usage_seen, aborted — no content, no IP.
- Found and fixed the same day: non-stream clamp 8192 could not finish inside the 90 s upstream timeout (8 real 504s in the ledger) -> 4096; `/health` probe now cached 10 s so a burst of unauthenticated GETs cannot occupy customer slots; PRIVACY.md no longer mentions an SSH hop; on-box swap scripts no longer carry a key literal and are 0700.
- Not re-measured (no load generation): the ~2.5k tok/s aggregate / ~80 tok/s per stream figures come from the 2026-08-25 32-way load test recorded in the ledger (`n=32, status 200, stream`).
