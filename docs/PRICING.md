# kat-awq Pricing (2026-08-21)

## Cost basis (real, measured)

- Fleet: 6x kat-awq vLLM replicas, A100 SXM4 80GB, a100b (vast.ai), $1.056/hr median/GPU → **$6.34/hr fleet**.
- Verified throughput ceiling: **N≈192 concurrent req/port, ~27,000 tok/s aggregate** (Karpathy-style empirical sweep, N=128/192/256, real plateau+decline confirmed — see loop research summary 2026-08-20/21).
- Raw compute cost: $6.34/hr ÷ (27,000 tok/s × 3600s) = $6.34 / 97.2M tok = **$0.0652/M tokens blended**.
- Config-lever tuning (`--gpu-memory-utilization`) tested, made it worse (-4.9%) — fleet is compute-bound at this load, this is the real floor on current hardware.

## Competitor reference (OpenRouter, real pricing pulled 2026-08-20)

| Model | In $/M | Out $/M |
|---|---|---|
| DeepSeek V4 Flash 0731 | 0.065 | 0.14 |
| KAT-Coder-Air V2.5 | 0.15 | 0.60 |
| KAT-Coder-Pro V2 | 0.30 | 1.20 |
| KAT-Coder-Pro V2.5 | 0.74 | 2.96 |
| Ollama cloud | flat sub, not metered | — |

## New rates (effective 2026-08-21)

| | In $/M | Out $/M |
|---|---|---|
| **kat-awq** | **$0.05** | **$0.12** |

- Undercuts DeepSeek V4 Flash on both legs (0.065→0.05 in, 0.14→0.12 out).
- Blended margin @ typical coding-agent traffic mix (~1:4 in:out) ≈ 0.05×0.2+0.12×0.8 = $0.106/M vs $0.0652/M cost → **~63% margin**, healthy buffer for burst/idle GPU time not counted in the $6.34/hr denominator.
- Not the theoretical floor — floor is ~$0.065/M raw compute. Pushing rates lower means either thinner margin or scaling to denser GPUs (H200/B200) or more replicas to spread fixed costs; not done today, tracked as future work.

## Deploy

No live billing config found in this repo (`oaica-code`) — this file is the rate card of record. Propagate to whatever service reads pricing (api.sprapp.com billing, if separate repo) manually; ping when that location is known so this doc can link to it directly.
