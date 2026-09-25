# oaica-code on Lemon Squeezy — product + pricing proposal (2026-09-11)

**Status: proposal for review. Nothing here is deployed except the $20
one-off license gate already in `cmd/launch/license.go`.**

## 1. Can it be sold on Lemon Squeezy? Yes — it already half is

- `oaica activate <key>` (commit 6ecd41ac, 2026-09-04) validates against
  Lemon Squeezy's License API (`/v1/licenses/activate` + `/validate`),
  binds the key to this machine (instance_id), caches 7 days, 30-day
  offline grace. Checkout URL const: `oaicaPurchaseURL` in
  `cmd/launch/license.go` — still a placeholder, replace once the LS
  product exists.
- Lemon Squeezy is a **merchant of record**: they collect and remit
  VAT/GST/SST worldwide, handle chargebacks, issue invoices. For a
  Malaysian seller shipping a $20–$200 CLI to developers in 50+ countries
  that's the entire reason to use it over direct Stripe (see
  `PRICING.md` §"Billing provider" — that section argued Stripe-direct
  for *usage-metered API billing*; this doc is about the **CLI as a
  digital product**, a different SKU where MoR wins).
- Fee: **5% + $0.50 per transaction** (LS's published MoR rate; verify
  on lemonsqueezy.com/pricing before launch — site blocked automated
  fetch on 2026-09-11). LS was acquired by Stripe (2024) and is being
  folded into Stripe MoR; expect a migration, but the License API and
  product structure carry over.
- What LS supports natively that we need: one-off products, recurring
  subscriptions, license keys on **both** (key stays valid while the sub
  is active, flips to `expired`/`disabled` on cancel — `/validate` then
  returns invalid and our gate blocks launch after the grace window),
  per-variant activation limits (seats), Customer Portal, webhooks.

## 2. What is actually being sold

oaica-code is MIT. Source builds are free forever and that cannot change.
The product is the **convenience path**: prebuilt signed binaries
(`install.sh`/`.ps1`), auto-update, and `oaica launch` wiring for
Claude Code, Codex CLI/App, Cline, OpenCode, Pi, Hermes, Droid, Kimi,
Qwen, Copilot, VS Code, OpenClaw, Muse, OMP — one command that writes
correct provider config for each tool and routes them to our hosted
model or the user's own remotes (OpenRouter, local Ollama, etc.).

Buyers: individual developers first, small teams second. Same buyer
profile as Warp, Raycast Pro, Cursor — people who pay $10–$30/mo for
tooling without procurement.

## 3. Proposed structure (three variants on one LS product)

| Variant | Price | Seats (LS activation limit) | Includes | Notes |
|---|---|---|---|---|
| **Personal** | **$20 one-off** | 2 machines | Binary, updates for 12 months (major-version cutoff), all `launch` targets, BYO-key remotes | Already implemented. Non-commercial + solo commercial use |
| **Pro** | **$9/mo or $79/yr** | 3 machines | Everything in Personal + free updates while active + **$5/mo of hosted oaica API credit** included + priority Discord/email support | Subscription. Credit makes it the funnel into hosted API (`PRICING.md` request-cap plans) |
| **Team** | **$29/seat/yr, min 5 seats** | 1 machine/seat, transferable | Pro features, one invoice, seat admin via LS portal, shared `plans.json` profiles | Annual only — teams don't want monthly |

Rationale per row:

- **Personal $20 one-off** — keep. This is the impulse-buy tier; the
  license.go gate is built for it. 12-month update window (not lifetime)
  so a v2 can be re-sold to the same buyer. LS supports "lifetime"
  keys; enforce the update cutoff in the updater, not the license.
- **Pro subscription** — the real money is not the CLI, it's that Pro
  holders default to our hosted model. $5/mo credit costs us ~$0.30 at
  the $0.055/M cost basis if fully used (90 M tokens), usually far less.
  Net after LS fee on $9: $8.05. Even if every subscriber burns the full
  credit we clear ~$7.70/mo, and unused credit is pure margin.
- **Team** — priced so 5 seats = $145/yr, under most managers' no-approval
  threshold. Enforce seats with LS activation limit = seat count; the
  existing `/validate` loop already handles "activation limit reached".

## 4. Unit economics (per 1,000 buyers, blended guess: 70% Personal, 25% Pro-annual, 5% Team-5-seat)

| | Gross/yr | LS fee (5%+$0.50) | Net/yr |
|---|---|---|---|
| 700 × Personal $20 | $14,000 | $1,050 | $12,950 |
| 250 × Pro $79/yr | $19,750 | $1,113 | $18,637 |
| 50 × Team 5 seats $145 | $7,250 | $388 | $6,862 |
| **Total** | **$41,000** | **$2,551** | **$38,449** |

Plus hosted-API pull-through from the 300 Pro/Team users (not counted).
Cost of goods is ~zero (GitHub Actions release builds, Cloudflare Pages
for the installer). Pro credit worst case: 250 × $0.30 × 12 = $900/yr.

## 5. Where the lines are (must decide before creating the LS product)

1. **Source builds stay unrestricted.** We gate the *binary*, not the
   code. Anyone who builds from source is a free user, and that's the
   adoption funnel (Ollama playbook). Don't add source-level checks.
2. **Free trial vs. no trial.** Recommend **no trial, 14-day refund**
   (LS handles refunds; `/validate` flips the key to invalid on refund,
   gate blocks after grace). A free tier of the binary = nobody pays $20.
3. **Hosted API is NOT bundled beyond the Pro credit.** API usage stays
   on the metered plans in `PRICING.md`. Bundling unlimited API into a
   $9 CLI sub repeats the MiniMax loss-leader trap documented there.
4. **Update window enforcement** needs a small change: updater checks
   `license.json` purchase date vs. release date. ~1 day of work.
5. **Team seat admin** is LS Customer Portal only for v1 — no custom
   dashboard.

## 6. Launch checklist

- [ ] Create LS store + product "oaica-code", 3 variants above, license
      keys ON, activation limits 2 / 3 / per-seat.
- [ ] Replace `oaicaPurchaseURL` placeholder with real checkout URL.
- [ ] Verify LS fee card + subscription-key expiry behavior on a test
      product before pricing is public (site blocked fetch 2026-09-11).
- [ ] Updater: 12-month window for Personal.
- [ ] Webhook `subscription_expired` → nothing needed (validate handles
      it), but `license_key_created` → email onboarding is worth wiring.
- [ ] Pro credit: LS webhook `order_created` (variant=Pro) → meterhub
      grants $5 monthly credit to the linked oaica account. Needs the
      LS customer email ↔ oaica account link at activation time.
- [ ] Refund policy + Terms page (LS requires one).
