# kat-awq Pricing (2026-08-21)

> **Note (2026-08-26):** the cost basis below was measured against a
> 6-replica sweep on 2026-08-21. The public fleet behind `api.oaica.com`
> has run **2 replicas** since 2026-08-25 — the rates themselves are
> unchanged, but treat the $/hr fleet cost and throughput ceiling as
> historical context, not the current deployed capacity. The rates in
> force are deployed in `tools/a100b/gateway.json`, served live at
> `GET /models`.

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

## Monthly subscription plans — proposal, not yet deployed (2026-08-29)

Metered per-token billing above is for API/router traffic. This is a
separate, flat-rate monthly plan proposal for the same product,
positioned against competitors' own monthly plans rather than their
pay-as-you-go API rates — the more relevant comparison for a subscriber
choosing a coding-agent plan rather than metering their own API calls.

### Competitor pricing pulled live (2026-08-29)

**Pay-as-you-go, $/M tokens:**

| Model | Input | Output | Notes |
|---|---|---|---|
| GLM-5.3-Flash | $0.15 (promo $0.075 til 2026-09-09) | $0.50 (promo $0.25) | |
| DeepSeek V4 Flash | $0.007–0.014 cache-hit / $0.22–0.44 cache-miss | $0.66–1.32 | off-peak/peak split |
| MiniMax M3 | $0.30 ($0.23 via OpenRouter) | $1.20 ($0.96 via OpenRouter) | doubles above 512K input |

**GLM's own Coding Plan (monthly subscription):**

| Tier | Monthly | Annual-equivalent/mo |
|---|---|---|
| Lite | $18 | $12.60 |
| Pro | $80 | $50.40 |
| Max | $168 | $112 |

Credit-based (not simple token counts): refreshes on a 5-hour window plus
a weekly cap, off-peak usage costs half the credits.

**MiniMax's own Token Plan (monthly subscription, checked 2026-08-29):**

| Tier | Monthly | Tokens included | Effective $/M |
|---|---|---|---|
| Plus | $20 | ~1.633B | $0.0122 |
| Max | $50 | ~5.053B | $0.0099 |
| Ultra | $120 | ~9.796B | $0.0122 |

Also a rolling window underneath the headline monthly figure (5-hour +
weekly), not a flat allowance — same mechanism GLM uses. Their effective
$/M is *below* their own metered API rate ($0.30/$1.20) and below our
raw compute floor ($0.0652/M): the nominal monthly total is a marketing
ceiling backed by average usage running far under it, not a per-token
cost guarantee. This only works at MiniMax's fleet scale — copying their
nominal token counts onto a 2-GPU fleet would be overselling capacity we
don't have.

### Proposed oaica plan — competitive headline numbers via the same rolling-window mechanism, scaled to real 2-replica capacity

Flat monthly totals (my first draft: 60M/250M/800M) were 10-15x worse
per-token than MiniMax's headline numbers and wouldn't read as
competitive next to them. Matching MiniMax's mechanism (a large
advertised monthly ceiling, actually throttled by a 5-hour + weekly
rolling window) lets the headline numbers compete honestly without
promising throughput this fleet can't deliver:

| Tier | $/mo | Headline monthly tokens | Real throttle (5hr / weekly) |
|---|---|---|---|
| Starter | **$9** | up to 100M | 8M / 5hr, 40M / week |
| Pro | **$25** | up to 400M | 25M / 5hr, 130M / week |
| Team | **$59** | up to 1B | 60M / 5hr, 320M / week |

- Pro ($25) undercuts MiniMax Plus ($20 → but ours reads as 400M vs
  their 1.633B, so still behind on the headline number — see caveat
  below) and undercuts GLM Lite ($18) on price while beating it on
  headline tokens.
- The 5hr/weekly caps are the load-bearing part: they cap what any
  single subscriber can actually pull from a 2-GPU fleet regardless of
  the monthly ceiling, the same protection MiniMax's own limits provide
  at their scale.
- **Honest caveat**: we cannot match MiniMax's raw nominal token counts
  (1.6B–9.8B/mo) on 2 GPUs even with rolling-window throttling — their
  numbers reflect fleet capacity orders of magnitude larger than ours.
  Compete on price and on GLM's more comparable numbers; against MiniMax
  specifically, compete on price-per-included-token and message this as
  "no cache-miss penalty, flat committed rate" rather than out-numbering
  their headline figure.

### Forward infra assumption: RunPod, not vast.ai (2026-08-29)

Future capacity is planned on RunPod, not the a100b vast.ai box the
measurements above were taken on. Real RunPod rates, checked live:

| | $/hr per A100 80GB |
|---|---|
| On-demand (Secure Cloud) | **$1.39** |
| Serverless (active compute only, scales to zero when idle) | **$2.72** |

Serverless is ~2x the on-demand rate per active hour, but bills nothing
between requests — see the hybrid recommendation below.

### Subscriber count needed (on-demand, RunPod $1.39/hr)

2 replicas, always-on → **$2,002/mo** fixed GPU cost (2 × $1.39/hr × 24
× 30). Target ~$3,000/mo revenue (1.5x fixed cost, buffer for non-GPU
overhead).

Blended ARPU at the 70/25/5 Starter/Pro/Team mix above ≈ **$15.60/subscriber**.

**→ ~193 subscribers to break even with a margin buffer**, always-on
2-replica RunPod on-demand.

### The real constraint isn't subscriber count — it's whether subscribers are actually heavy users

The 193-subscriber number assumes a generic SaaS utilization pattern
(most subscribers use a small fraction of their allowance most of the
time). That assumption is risky for THIS product specifically: agentic
coding tools are heavy by nature, unlike casual chat. Grounded in this
session's own real traffic (not a generic assumption) rather than
guessing:

- Real usage tonight: **41.7M tokens over 7.05 hours**, across up to 3
  client machines running live coding-agent sessions (`internal-91`,
  meterhub-tracked) ≈ **~2M tokens/hour per active machine**.
- A genuinely heavy daily subscriber (4 real active coding hours/day, 22
  workdays/month) ≈ **~174M tokens/month** — this is a real, derived
  number, not a guess.
- Hard fleet ceiling, 2 replicas: **23.3B tokens/month** at 100%
  utilization (9,000 tok/s aggregate, scaled from the documented 27,000
  tok/s 6-replica benchmark).
- If usage concentrates in a realistic ~12hr/day peak window (not spread
  flat across 24h) rather than the hard ceiling, that's **~67 fully-heavy
  subscribers** before the fleet saturates at peak — well under the 193
  needed to break even on revenue.

**This is the actual finding: revenue breakeven (~193) and physical
capacity breakeven for an all-heavy-user population (~67) are close
enough that if more than roughly a third of subscribers turn out to be
daily-heavy users, the fleet saturates before the business breaks even
on a purely flat-rate model.** For a casual-chat SaaS this gap is usually
10-50x, not ~3x — coding-agent products don't get that safety margin for
free.

### Caching narrows this gap, doesn't close it

Prefix caching is live and confirmed working: **58-66% cache hit rate**
measured directly from both replicas' logs right now (GPU0 65.7%, GPU1
57.5-60%), consistent with the ~9.5x TTFT speedup on cache hits
documented 2026-08-27. The $0.0652/M compute floor above predates that
confirmation (measured 2026-08-21) and real production traffic — repeated
system prompts and tool schemas within a session — caches better than a
synthetic benchmark sweep. Effective real cost is very likely below that
stale floor today, which improves margin and modestly raises the real
throughput ceiling (a cache hit consumes far less GPU time than a cache
miss), but this hasn't been re-measured under real cache-hit conditions —
treat it as directional upside, not a number to build a P&L on yet. It
narrows the gap between revenue breakeven and capacity breakeven; it does
not close it on its own.

### Recommendation: hybrid reserved + serverless, not flat always-on

A flat 2-3 replica reservation, paid 24/7 regardless of load, is the
wrong shape for this traffic: real usage is bursty (concentrated in
working hours, near-zero overnight), and the revenue/capacity gap above
means over-provisioning for peak wastes money at 3am while
under-provisioning risks saturating at peak.

1. **Small reserved base** (1 on-demand replica, $1.39/hr = ~$1,000/mo) —
   kept warm to avoid cold-start latency (10-60s on a fresh serverless
   instance is not acceptable for an interactive coding assistant) and
   to guarantee a floor of service quality.
2. **Serverless burst overflow** for peak load beyond what the reserved
   base can absorb — pay the ~2x premium ($2.72/hr) only for the hours
   actually needed, not 24/7. This is the standard shape every
   competitor checked here effectively uses too (MiniMax/GLM's rolling
   5hr/weekly windows exist specifically to throttle demand against
   finite real capacity, same problem, different mechanism).
3. **Metered overage beyond the plan cap**, not a hard wall — same
   pattern as MiniMax's $5-100 credit top-ups. A subscriber who blows
   past their tier's rolling-window cap becomes additional revenue
   (billed at the metered rate, $0.05/$0.12 or updated post-caching
   numbers) instead of a capacity crisis or a support ticket.
4. **Instrument real per-subscriber usage from day one** (the entitlement
   infra in `tools/gateway`/`tools/meterhub` already tracks per-key
   usage — this is exactly what it's for) and revisit the tier mix
   assumption (70/25/5) against real data within the first month, not a
   quarter. The 193-subscriber number is only as good as that
   assumption, and it's currently unverified against this product's
   actual usage pattern.

**Not yet implemented**: no billing/subscription enforcement exists in
the codebase today (see `tools/gateway`'s entitlement check for the
active/canceled/suspended primitive this could build on — it currently
gates access, not usage against a token allowance).

## Deploy

This file is the rate card of record; the deployed pricing lives in
`tools/a100b/gateway.json` (the `oaica-gateway` config), served live at
`GET /models` on `api.oaica.com`. Any rate change here must be propagated
to that file by hand and reloaded (see "Rotating the OpenRouter key" in
`docs/OPENROUTER_PROVIDER.md` for the SIGHUP reload mechanism).
