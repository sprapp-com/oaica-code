# Licensing

**Licensor / operator:** BizTransit Sdn Bhd (Company No. 891234-X)
Level 28, Lingkaran Syed Putra, Mid Valley City, 59200 Kuala Lumpur, Malaysia
Contact: oaica@sprapp.com

This document says what is free, what is paid, and what governs each. It is a plain-language summary, not a
substitute for the licence texts it points to.

## 1. The source code — proprietary (a fork of Ollama)

oaica is a derivative work of [Ollama](https://github.com/ollama/ollama). The
upstream Ollama code keeps its MIT licence, reproduced in `LICENSE` — that
licence covers the upstream code only. The oaica modifications, the oaica
source, and the prebuilt binary are **proprietary**, sold and licensed under the
End User Licence Agreement in `EULA.md`. This repository is not public source.
The upstream MIT rights in Ollama's own code are unaffected by this.

## 2. The prebuilt `oaica` binary — free tier, trial, and paid plans

BizTransit Sdn Bhd licenses the prebuilt, supported binary with `oaica launch`.
A fresh install may use it **free for 14 days** with no activation code; after
the trial, paid features require a key. The free tier (local models,
`oaica serve`, your own endpoints and API keys) is never gated and does not
expire.

New sales are subscriptions, in USD:

| Plan | Price | Covers |
|---|---|---|
| `oaica-code` (the launcher) | **USD 7/month or USD 59/year** | the paid features on top of the free tier |
| Bundle (launcher + self-hosted engine) | **USD 29/month or USD 249/year** — planned, **not on sale yet** | the launcher and the engine; offered only when its engine, rights and compatibility gates pass |
| Version licence (capped experiment) | **USD 299 once**, or a founder version licence at **USD 249 once** — **not on sale** | perpetual use of the eligible versions bought, with 12 months of eligible updates and support; each capped at 50 sales |

A subscription covers the paid features, updates and standard support for the
paid period: cancel renewal in the customer portal and keep access until the
paid-through time. No plan includes a compute credit — model/API usage and
hardware are supplied and paid for by the buyer. Licences bought earlier on
perpetual terms keep working on those terms and are never converted into a
subscription.

The line between the two is one sentence: **free is everything a launch needs to
run, paid is everything that makes it better than it has to be.** Free keeps one
primary model, the one secondary/subagent model (`--sonnet-model`), and a saved
`--plan`. Paid is the rest — the background tier (`--haiku-model`), context
escalation (`--oversize`), sharding (`--shard`), the guided setup (`--wizard`),
the route policies that choose a leg for you or split traffic, any vendor API the
catalog prices as paid, and oaica's first-party models (the `oaica-*` ids,
resolved against the gateway you point `OAICA_GATEWAY_URL` at — oaica ships no
hosted service of its own). A student or a teaching lab can get the whole free
tier under an Education key — renewable yearly, same capability set as the free
tier, none of oaica's first-party models. The full matrix,
including the education flow and the comparison with Ollama, is
`docs/EDITIONS.md`.

| | |
|---|---|
| Price and tax | Shown at checkout. Where Paddle carries the sale, Paddle is the merchant of record and both calculates **and remits** the tax. On the Stripe rail the price is the total: Stripe Tax is not available to this seller's Malaysian Stripe account, so no tax is computed or added there, and a business tax ID can still be entered for B2B. |
| Seller and payment | BizTransit Sdn Bhd, through Stripe; Paddle (Paddle.com Market Ltd) acts as the merchant of record for a sale it processes. Card data never reaches our servers. |
| Key | `olk_…`, shown once after payment (and receipted by email by the payment provider). **The launcher licence-key format is the same for subscriptions and one-off purchases**: a subscription key is still a launcher licence key (`olk_` plus 32 hex characters), and the key is the same product whichever rail processed the payment. Keep it like a password; we store only its hash. |
| Activations | Earlier perpetual keys keep their three-machine limit, exactly as sold. A new subscription plan covers one named user on two personal devices. A key injected as `OAICA_LICENSE_KEY` (a secret-manager or CI deployment) is checked as a key — valid, not refunded — without binding a machine, so the machine limit applies to `oaica activate`. |
| Checking | The binary revalidates against the licence server at most every 7 days, sending the key and the machine's activation id (see `docs/ENTERPRISE.md`, row 7). It keeps working for up to 30 days if the server is unreachable; a subscription's cached rights run only to the earlier of its paid-through time and that offline deadline. |
| Revocation | A refunded or charged-back payment revokes the key; the next revalidation reports it. Cancelling a subscription is not revocation: access runs to the paid-through time, after which the paid features stop and the free tier keeps working. |
| Transfer | The key is for the purchaser's use across their own machines; do not publish or resell it. |

Refunds: 30 days after the initial purchase — contact oaica@sprapp.com; a renewal follows the payment provider's terms, and annual billing is not a monthly cancellation entitlement (the year is paid for, not held monthly). Statutory consumer rights are not affected by anything here.

## 3. Hosted API plans — retired

The hosted API plans (lite / pro / pro+ / max) are retired: oaica ships no hosted
service of its own, and no `osk_…` keys are issued. If you run your own
router, its subscriptions and the Terms of Service / Privacy Policy for them are
yours to serve (the retired gateway copy is kept in `tools/gateway/legal/` for
reference). The launcher licence key (`olk_…`, section 2) is unaffected.

## 4. Models and third-party components

Model weights are **not** covered by the licence of this product. Each model keeps its own licence
(recorded in the model manifest, and, for models served through the retired hosted API, in the
archived Terms of Service, section 6); using a model means complying with it.
Third-party Go modules keep their own licences (`go.mod`). "Ollama" and other third-party names belong to their
owners.

## 5. Governing law

Malaysian law; the courts of Malaysia have exclusive jurisdiction (the archived Terms of Service, section 10).

## 6. Not legal advice

This summary was prepared for BizTransit Sdn Bhd's product. Have counsel confirm it (in particular refund
wording, consumer-law carve-outs for your buyers' countries, and SST registration status) before relying on it.
