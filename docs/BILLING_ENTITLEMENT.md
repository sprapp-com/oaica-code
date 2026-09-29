# Billing & entitlement — how a request is admitted

Forward pointer from `tools/meterhub/main.go` (`subscriberStatus`) and
`tools/gateway/main.go` (`checkWindowCap`). This is the operational
description of the admission path as it exists today: who decides, where
the state lives, what happens when the decision service is unreachable,
and what is still missing before a real payment processor is connected.

## The one rule

**No billing service is ever on the request's critical path.** A gateway
answers a completion from local state; the central service (`meterhub`)
is consulted through a short-TTL read-through cache and, when it is
unreachable, the request degrades to a configured policy rather than
hanging or erroring. Inference keeps serving through a meterhub outage —
it just reports usage late, and the local JSONL ledger on each box stays
the durable audit trail.

## The layers

| Layer | Lives in | Role |
|---|---|---|
| Per-box ledger | `writeLedger`, `tools/gateway/main.go` | Append-only JSONL, one row per served request. Durable audit trail; never depends on meterhub. |
| Aggregation | `tools/meterhub` (SQLite) | Each gateway additionally reports the same row via `POST /ingest`, async and best-effort. Answers "tokens/requests for key X across every region" in one query. |
| Entitlement state | meterhub `subscribers` table | One row per key label: `status`, `plan`, `source`, `external_id`. |
| Enforcement | `entitlementCache`, `tools/gateway/main.go` | Fast local cache in front of meterhub; decides whether THIS request proceeds. |

## Status values

| Status | Served? | When it is written |
|---|---|---|
| `active` | yes | Normal subscription. |
| `past_due` | yes | A failed charge. Matches Stripe's own grace-period semantics — a failed card is not an instant cutoff. |
| `canceled` | no | Subscription ended. |
| `suspended` | no | Administrative block. |
| (no row) | policy-dependent | Unknown key label. See fail-open/fail-closed below. |

Writes come from two places, both landing on the same table so nothing
about the gateway's read path changes when a real processor is wired up:

- `POST /subscribers/set` — the manual control surface. This is what
  "easily block an unsubscribed user" means operationally today: post
  `status=canceled`.
- Stripe does not talk to meterhub. It talks to **oaica-saas**
  (`POST /billing/webhook`: signature-verified, idempotent per event id,
  ignores out-of-order events), which pushes each subscription's state to
  `POST /subscribers/set` here and answers `GET /entitlement/:key_label`.
  See `oaica-saas/docs/stripe-setup.md`. (The old unsigned
  `/subscribers/webhook` route was removed in round 132.)

## The second condition: rolling-window caps

Being entitled and being *within plan* are separate questions. An
`active`/`past_due` subscriber can still be over their plan's rolling
5-hour or 7-day cap, which is what `checkWindowCap` decides. As of
2026-09-28 the caps count **requests**, not tokens — the rate card
(`planLimits` in `tools/meterhub/main.go`, published in
`docs/PRICING.md`) sells requests per window, and `usage.request_id` is
that table's primary key, so `COUNT(*)` over the window is the request
count. The window's token total is still reported beside the count for
the audit trail and no longer gates anything.

Two consequences worth knowing operationally:

- The **flag** carries the semantics, not the gateway binary. An older
  gateway reading a current meterhub is capped by requests too, because
  it is meterhub that computes the `over` booleans.
- A key whose `plan` is an unknown slug (an old `starter`/`pro`/`team`
  row provisioned by hand before the slugs were settled) resolves to **no
  plan** and therefore **no cap**. It is served, unmetered against any
  cap, until its `plan` is corrected. There is no default cap.

## Fail-open vs fail-closed

`EntitlementFailOpen` (gateway config, default **false**) decides what an
*unresolvable* entitlement question means. Two distinct degradations are
covered by it:

1. meterhub unreachable, non-200, or undecodable;
2. the key label has no subscriber row at all.

Default false = **fail closed**: an unreachable billing service blocks
inference, and an unknown key is refused
(`"no active subscription for this key"`). Default true = **fail open**:
both cases are admitted.

This is a real trade-off and neither setting is "safe" in the abstract:

- Fail open during an outage is a **billing bypass** — every key,
  including canceled ones, is served for as long as the outage lasts.
- Fail closed during an outage is a **self-inflicted denial of service**
  for paying customers.

For production, the intended shape is fail-open *with alerting on the
degraded path*, because inference availability is the thing customers
buy; the bypass is bounded and visible.

### Degraded decisions do not stick

A degraded answer is cached for **5 seconds**, not the full
`EntitlementCacheTTLSec` (`entitlementCache.check`, the M6 fix from the
2026-09-01 security audit). Before that, a single blip stamped a
ttl-long billing bypass per key in fail-open mode, or refused real
traffic for the same window in fail-closed mode. The cap lookup inside
`fetchAndDecide` degrades the same way and gets the same short TTL — a
status lookup that answers authoritatively does not make the cap answer
authoritative (round 32, B-F2).

## Overage billing

`EntitlementOverageBilling` (default false) turns an over-cap request
from a block into an admission flagged `Overage=true` on the ledger row,
for a billing job to charge at the overage rate afterward. It applies
**only** to an otherwise-active subscriber exceeding a window cap — never
to a canceled/suspended key. The switch is off by default because
turning it on silently changes existing 429 behaviour for every gateway
that already has `EntitlementEnabled` on.

## What is still missing

- **Stripe account setup.** The integration is written and tested in
  oaica-saas (Checkout + Stripe Tax, signed webhook, one-off licence and
  subscription keys); it needs the Stripe account, tax registrations,
  products/prices and Worker secrets described in
  `oaica-saas/docs/stripe-setup.md`, and the public origin serving
  `web/` plus the `/billing`, `/license`, `/entitlement` routes.
- **Automation-tier metering.** The caps count requests in a rolling
  window, which a scripted workload can satisfy cheaply — many small
  requests consume the same window budget as one large one. Until there
  is a requests-per-minute throttle and fair-use language for scripted
  workloads, treat the request cap as a fair-use signal, not as
  protection against a determined scripted client.
- **No plan is not a cap.** See above: an unrecognised plan slug is
  served uncapped. Provisioning must use a slug from `planLimits`.
