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

- Undercuts DeepSeek V4 Flash on both legs (0.065→0.05 in, 0.14→0.12 out) **as of 2026-08-20 — no longer true, see the 2026-08-29 refresh below: DeepSeek has since dropped to $0.03/$0.10.**
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

## Per-token vs. per-request billing (2026-08-29)

Question: for the subscription/API tiers above, bill by token or by
request count? Answer, grounded in real meterhub data (991 real
completions, `internal-91` key, 2026-08-29):

- **Prompt-size spread is wide and skewed**: p10=50,270 tokens,
  p50=123,448, p90=186,903, max=229,718 — **~3.7x spread p10→p90**, and
  this traffic is dominated by long-context agentic sessions, not short
  chat turns (median request alone is bigger than most competitors'
  entire context window).
- **Compute cost scales with tokens, not requests**: prefill cost is
  ~linear in prompt length. A 230K-token request does meaningfully more
  GPU work than a 50K-token one; a flat per-request price cannot track
  that without either overcharging light users or undercharging (and
  losing money on) heavy ones — and with a spread this wide, that
  mispricing compounds fast at volume.
- **`CostUSD` (see `tools/gateway`'s `computeCostUSD`) already reflects
  the real cost driver** — per-token, split fresh vs. cache-hit prompt
  tokens, plus completion tokens. A per-request meter would need an
  entirely separate cost model that diverges from measured infra spend
  the moment request sizes vary this much, which they do.
- **Every real competitor referenced in this doc bills per-token**
  (OpenRouter, DeepSeek, MiniMax, GLM). Per-request pricing is a chat-app
  pattern for roughly-uniform request sizes, not agentic/long-context
  inference.

**Recommendation, already the current architecture**: keep the
**subscription** layer flat-monthly (Starter/Pro/Team), gated by
**token-based** rolling-window caps (5h/weekly — see `checkWindowCap` in
`tools/meterhub`), not request-count caps. Bill the **metered/API** tier
strictly per-token (`gwPricing`, `computeCostUSD`, cache-hit pricing,
overage billing — all live). Request count is fine as a secondary
abuse/rate-limit signal, never as the primary billing unit for this
workload.

## Single-GPU (1x A100) subscription plan — proposal, not yet deployed (2026-08-29)

The multi-GPU plan above assumes a fleet; this sizes a plan for the
minimum viable deployment, 1x A100, using real measured numbers instead
of the original 6-GPU synthetic benchmark.

**Real inputs:**
- Cost: RunPod on-demand A100 80GB, $1.39/hr → **$1,014.70/mo** (730hr).
- Measured real throughput: **~17M tokens/hr per replica** under
  sustained heavy load (2026-08-29, prefill-dominated — this workload's
  real traffic runs p50≈120K/p90≈185K prompt tokens per request, not
  casual chat; see the per-token vs per-request analysis above for the
  full distribution).
- Cost basis: ~$0.055/M tokens blended (real traffic's actual
  prompt:completion mix, ~216:1 — heavily prefill-weighted, not the
  1:4 in:out ratio assumed in the multi-GPU section's margin calc).

**Recommended tiers (single A100, two tiers only — a third/Team tier is
not safe to offer on one GPU with no failover):**

| Tier | Price/mo | 5h rolling cap | Weekly cap | Max tokens/mo if maxed |
|---|---|---|---|---|
| Starter | $12 | 1.5M tokens | 6M tokens | ~26M |
| Pro | $29 | 5M tokens | 20M tokens | ~87M |

Caps sized against real request size, not arbitrary: a real request
here averages ~120K tokens, so Pro's 5M/5h cap covers roughly 40 real
requests in a 5-hour session — a genuine long agentic coding session,
not a toy limit. Bigger than this and one subscriber alone can saturate
the whole GPU (17M tok/hr capacity, zero redundancy on a single
replica).

**Cost to serve one fully-maxed Pro subscriber**: 87M tokens/mo ×
~$0.055/M ≈ **$4.80/mo** — well under the $29 price even at worst case,
before accounting for the near-certainty that most subscribers never
actually max out every rolling window every month.

**Breakeven (mixed cohort, 70% Starter / 30% Pro — a realistic mix, not
all-Pro):**
- Weighted avg revenue/subscriber: 0.7×$12 + 0.3×$29 = $17.10/mo
- Subscribers needed to cover $1,014.70/mo: **≈60 subscribers**

**Capacity check** (the real constraint on one GPU, not revenue): if
all 60 subscribers maxed their caps simultaneously → 60 × weighted-avg
44.3M tokens ≈ **2.66B tokens/mo**, vs. the GPU's theoretical ceiling of
17M×730 ≈ **12.4B tokens/mo** — safe at ~21% of max capacity even in
the worst case. Real subscribers never all max out simultaneously, so
this has room to grow past 60 before GPU throughput (not margin)
becomes the binding constraint.

**Bottom line**: ~60 subscribers on this Starter/Pro mix covers the
1x-A100 GPU cost with healthy margin, and the rolling-window token caps
(already built and enforced — see `tools/meterhub`'s `checkWindowCap`)
are what make this safe: they bound worst-case cost per subscriber
regardless of how heavy any individual user's real usage gets.

## Deploy

This file is the rate card of record; the deployed pricing lives in
`tools/a100b/gateway.json` (the `oaica-gateway` config), served live at
`GET /models` on `api.oaica.com`. Any rate change here must be propagated
to that file by hand and reloaded (see "Rotating the OpenRouter key" in
`docs/OPENROUTER_PROVIDER.md` for the SIGHUP reload mechanism).

## Competitor refresh + how to actually compete (2026-08-29)

The 2026-08-20 table above is stale — DeepSeek in particular has dropped
price since. Refreshed live:

| | Input $/M | Output $/M | vs. oaica ($0.05 / $0.12) |
|---|---|---|---|
| **oaica (current)** | $0.05 | $0.12 | — |
| DeepSeek V4 Flash 0731 | $0.03 | $0.10 | **they're cheaper, both legs** |
| GLM-5.3-Flash | $0.075 (promo, expires 2026-09-09) | $0.25 (promo) | we're 1.5-2x cheaper |
| MiniMax M3 | $0.30 ($0.23 via OpenRouter) | $1.20 ($0.96 via OpenRouter) | we're 4.6-6x cheaper |

**We already beat MiniMax and GLM on raw metered rate.** DeepSeek V4
Flash is the one real competitor undercutting us on sticker price —
and they're a much larger operation (fleet-scale, cache/peak-tiered
pricing we can't structurally replicate on 1-2 GPUs).

**Don't chase DeepSeek's raw rate down** — $0.03/M input is close to or
below our own $0.0652/M raw compute floor once blended; matching it
outright risks trading margin for a race we can't win against a bigger
fleet. The floor doesn't move by wanting it to; it moves by adding
GPUs or denser hardware (H200/B200), neither done today.

**How to actually be more competitive, without cutting the headline
rate below cost:**

> **Status 2026-08-29 (late):** `cached_prompt` was deployed to the gateway
> config the same day, but a metering audit found vLLM 0.24.0 never reported
> `prompt_tokens_details.cached_tokens` (flag `--enable-prompt-tokens-details`
> is off by default), so the discount was inert and every cache hit billed at
> the fresh rate. Fixed by adding the flag to the replica launch
> (`tools/a100b/vllm_awq_watchdog.sh`); verified `cached_tokens: 6336` on a
> repeated 9,016-token prompt. Effective from the rolling restart on
> 2026-08-29 ~22:30 UTC. See `docs/METERING_AUDIT_2026-08-29.md`.

1. **Ship the cache-hit discount that's already built.** `gwPricing.CachedPrompt`
   exists in code (`tools/gateway`) but isn't populated in the deployed
   config — cached prefix tokens still bill at the full $0.05 rate today
   despite costing us near-zero additional GPU compute (prefill is
   skipped on a cache hit; **measured 9.5x TTFT speedup** on repeated
   system prompts, see the stress-test section above). Setting
   `CachedPrompt` to something like **$0.01/M** costs us almost nothing
   (the compute was already saved) and directly beats DeepSeek's own
   cache-hit tier ($0.007-0.014) on a workload we're structurally suited
   for: Claude Code sessions reuse the same huge system prompt + tool
   schema on every turn, which is exactly what prefix caching rewards.
   This is the single highest-leverage, lowest-risk lever available —
   pure upside, no cost-floor risk, code already exists.
2. **Keep the fresh (non-cached) rate at $0.05/$0.12** — margin-safe
   against the $0.0652/M floor, don't compete with DeepSeek there.
3. **Lead the pitch with "flat committed rate, no cache-miss penalty,
   no peak/off-peak markup."** DeepSeek's real pricing is a 4-way split
   (cache-hit/miss × peak/off-peak, $0.007-0.44 range) and GLM halves
   credits off-peak — both cheaper on paper but genuinely harder for a
   customer to predict their bill from. A flat, simple rate is a real
   differentiator even when it isn't the lowest number on the page.
4. **Use the effective blended rate, not the sticker rate, once cache
   pricing ships.** A real Claude Code session (huge shared system
   prompt + tool schema reused every turn) with even 50% cache-hit rate
   nets ~$0.03/M effective input — competitive with DeepSeek's *raw*
   rate while DeepSeek's own cache-hit pricing needs a cache HIT to get
   there too, so this isn't an unfair comparison.
5. **Overage billing + rolling-window caps (both already built and
   live) let subscription headline numbers stay generous-sounding
   without oversell risk** — the mechanism, not the sticker price, is
   what makes MiniMax/GLM's big advertised numbers safe at their scale;
   we can use the same mechanism honestly at ours (see the single-GPU
   plan above).

**Bottom line**: don't try to out-discount DeepSeek's headline number.
Ship the cache-hit pricing that's already coded but not deployed — it's
free margin today (compute already saved by prefix caching) and turns
into a genuine competitive edge against DeepSeek specifically for the
repeat-heavy-context workload this product actually serves.
