# oaica.com — website specification (2026-08-31)

Single source of truth for what the public site must say and offer. Every
number here is live in production; when the rate card or models change,
this file changes in the same commit (`docs/PRICING.md` is the pricing
authority, this file mirrors it for the site).

Hosting today: Cloudflare Pages project **`oaica-install`** serves
oaica.com from this repo's `site/` (landing + `/download/*` +
`/install.sh`). The marketing/sales page being built separately
(oaica-com.pages.dev, peer session) must fold into this spec, not fork it.

## Pages

### 1. Landing (`/`)
- Hero: one sentence — "Run serious coding models. Hosted API or your own
  GPU. Ollama-simple." Primary CTA: the install one-liner in a copy box:
  `curl -fsSL https://oaica.com/install.sh | bash`
  Secondary CTA: "Get an API key" (mailto/form until self-serve exists).
- Three-column what-you-get, mapping to the three real usage modes:
  1. **Hosted API** — OpenAI- and Anthropic-compatible, 262k context,
     vision, prefix-cache discounts.
  2. **Self-host** — `oaica pull` + `oaica serve` (llama.cpp), free.
  3. **Claude Code ready** — `oaica launch claude --model
     oaica-35b-a3b-vision`; context window auto-configured.
- Live proof points (update quarterly, keep honest): 4×A100 fleet,
  zero-downtime deploys, per-session prefix-cache affinity (measured
  ~55% hit rate on agent workloads), usage-metered billing to the token.
- Footer: GitHub (sprapp-com/oaica-code), status, terms/privacy,
  oaica@sprapp.com.

### 2. Pricing (`/pricing`)
Must show the REAL deployed card (per M tokens), context-tiered:

| prompt size | input (uncached) | cache-hit | output |
|---|---|---|---|
| ≤ 32k | $0.05 | $0.008 | $0.28 |
| 32k–128k | $0.06 | $0.008 | $0.28 |
| > 128k | $0.10 | $0.008 | $0.28 |

- One-line explanations users actually need: the bracket is chosen by the
  request's real prompt size; cached prefix always bills at $0.008; agent
  sessions that reuse context pay mostly cache rates.
- Comparison row (kept factual, sourced in PRICING.md): DeepSeek V4 Flash
  official $0.22–0.44 in / $0.66–1.32 out; KAT-Coder-Air $0.15/$0.60.
- Subscriptions: publish only when a plan is deployed; until then a
  "flat monthly plans — talk to us" line with the priority-tier pitch
  (priority keys skip large-context queuing).

### 3. Models (`/models`)
Generated from the gateway's own `/v1/models` (id, context_length,
max_completion_tokens, input_modalities, pricing) — never hand-edited.
Today that is:
- **oaica-35b-a3b-vision** — 262,144 ctx, 32k max output, text+image
  (2 images/request), tiered pricing above.
- **oaica-nemotron-30b-a3b** — 262,144 ctx, text-only, reasoning +
  tool-calling. Mark "on-demand" while its pool is paused.
Each model row: curl example (OpenAI shape) + `oaica run` example.

### 4. Docs (`/docs`)
Quickstart mirrors README.md's three paths, in this order:
1. Hosted: signin → `oaica run` → `oaica launch claude`.
2. API direct: base URL `https://api.oaica.com/v1`, bearer key, OpenAI
   chat completions + Anthropic `/v1/messages`; streaming with usage
   (`input_tokens`/`cache_read_input_tokens` populated — clients can
   meter context correctly). Note: scripted clients MUST set a custom
   User-Agent (default `Python-urllib` is bot-blocked at the edge).
3. Self-host: `GET /v1/catalog` (public) lists pullable GGUFs
   (`qwen2.5-0.5b` smoke model, `oaica-nemotron-30b-a3b` 25 GB Q4_K_M);
   `oaica pull <model>` → `oaica serve <model>` (needs `llama-server`);
   licensed models return 401 `license_required` without
   `OAICA_LICENSE_KEY` — link the future Pro/self-host license page.

### 5. Download (`/download/`)
Exists (archives + SHA256SUMS + VERSION.txt). Add an index page listing
platforms with sha256s; keep `VERSION.txt` as the machine-readable
pointer (the CLI's update check reads it).

### 6. Status (`/status`)
Until a real status provider is wired: a static line linking
`https://api.oaica.com/health` and the UptimeRobot/Better Stack public
page once created (TODO.md item, still open).

## Non-negotiables

- Prices, model list and context sizes on the site must match the
  gateway config byte-for-byte — generate, don't transcribe.
- No claims ahead of reality: no SLA, no "unlimited", no model that is
  not currently servable (paused pools say "on-demand").
- Every page reachable in ≤2 clicks from the landing hero; the install
  one-liner appears on landing, docs, and download.
- Keep the site static (Pages); anything dynamic reads the public
  gateway endpoints (`/v1/models`, `/v1/catalog`, `/health`) client-side.

## Open items for the site (owner decisions)
1. Self-serve key issuance (today: email). Blocks real conversion.
2. Stripe checkout for plans (docs/PRICING.md "Billing provider").
3. Publish subscription plans once deployed.
4. Pro self-host license page (docs/PRISM_BUNDLING_SCOPE.md pricing Qs).
