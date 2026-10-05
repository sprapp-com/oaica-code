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

## 2. The prebuilt `oaica` binary — licence with a free 14-day trial

BizTransit Sdn Bhd sells a licence for the prebuilt, supported binary with
`oaica launch`. A fresh install may use it **free for 14 days** with no
activation code; after the trial, paid features require a key. The free tier
(local models, `oaica serve`, your own endpoints and API keys) is never gated
and does not expire.

The line between the two is one sentence: **free is everything a launch needs to
run, paid is everything that makes it better than it has to be.** Free keeps one
primary model, the one secondary/subagent model (`--sonnet-model`), and a saved
`--plan`. Paid is the rest — the background tier (`--haiku-model`), context
escalation (`--oversize`), sharding (`--shard`), the guided setup (`--wizard`),
the route policies that choose a leg for you or split traffic, any vendor API the
catalog prices as paid, and oaica's own hosted models. A student or a teaching
lab can get the whole free tier under an Education key that does not expire —
same capability set as the free tier, none of oaica's own models. The full matrix,
including the education flow and the comparison with Ollama, is
`docs/EDITIONS.md`.

| | |
|---|---|
| Price and tax | Shown at checkout. Where Paddle carries the sale, Paddle is the merchant of record and both calculates **and remits** the tax. On the Stripe rail the price is the total: Stripe Tax is not available to this seller's Malaysian Stripe account, so no tax is computed or added there, and a business tax ID can still be entered for B2B. |
| Seller and payment | BizTransit Sdn Bhd, through Stripe; Paddle (Paddle.com Market Ltd) acts as the merchant of record for a sale it processes. Card data never reaches our servers. |
| Key | `oaica-lic-…`, shown once after payment (and receipted by email by the payment provider). The key is the same product whichever rail processed the payment — it behaves identically. Keep it like a password; we store only its hash. |
| Activations | Up to three machines per key through `oaica activate <key>`. A key injected as `OAICA_LICENSE_KEY` (a secret-manager or CI deployment) is checked as a key — valid, not refunded — without binding a machine, so the three-machine limit applies to `oaica activate`. |
| Checking | The binary revalidates against the licence server at most every 7 days, sending the key and the machine's activation id (see `docs/ENTERPRISE.md`, row 7). It keeps working for up to 30 days if the server is unreachable. |
| Revocation | A refunded or charged-back payment revokes the key; the next revalidation reports it. |
| Transfer | The key is for the purchaser's use across their own machines; do not publish or resell it. |

Refunds: contact oaica@sprapp.com. Statutory consumer rights are not affected by anything here.

## 3. API plans — subscriptions

The hosted API plans (lite / pro / pro+ / max) are retired: oaica ships no hosted
service of its own, and no `oaica-sk-…` keys are issued. If you run your own
router, its subscriptions and the Terms of Service / Privacy Policy for them are
yours to serve (the retired gateway copy is kept in `tools/gateway/legal/` for
reference). The one-off licence key (`oaica-lic-…`, section 2) is unaffected.

## 4. Models and third-party components

Model weights are **not** covered by the licence of this product. Each model keeps its own licence
(recorded in the model manifest and in the Terms of Service, section 6); using a model means complying with it.
Third-party Go modules keep their own licences (`go.mod`). "Ollama" and other third-party names belong to their
owners.

## 5. Governing law

Malaysian law; the courts of Malaysia have exclusive jurisdiction (Terms of Service, section 10).

## 6. Not legal advice

This summary was prepared for BizTransit Sdn Bhd's product. Have counsel confirm it (in particular refund
wording, consumer-law carve-outs for your buyers' countries, and SST registration status) before relying on it.
