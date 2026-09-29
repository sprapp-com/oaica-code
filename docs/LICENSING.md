# Licensing

**Licensor / operator:** BizTransit Sdn Bhd (Company No. 891234-X)
Level 28, Lingkaran Syed Putra, Mid Valley City, 59200 Kuala Lumpur, Malaysia
Contact: oaica@sprapp.com

This document says what is free, what is paid, and what governs each. It is a plain-language summary, not a
substitute for the licence texts it points to.

## 1. The source code — MIT

The source in this repository is licensed under the MIT License (`LICENSE`). The copyright notice of the upstream
project (Ollama) is kept as the licence requires; changes made by BizTransit Sdn Bhd carry the second notice in
that file. You may build, modify and redistribute the source under those terms, **without any key**.

## 2. The prebuilt `oaica` binary — one-off licence

BizTransit Sdn Bhd sells a **one-off licence** for the convenience of using the prebuilt, supported binary with
`oaica launch`. It is not DRM and does not restrict the MIT rights in section 1: a build from source needs no key.

| | |
|---|---|
| Price and tax | Shown at checkout. Tax (SST/VAT/GST/sales tax) is calculated by Stripe Tax from your billing address and shown before you pay; a business tax ID can be entered for B2B. |
| Seller and payment | BizTransit Sdn Bhd, through Stripe. Card data never reaches our servers. |
| Key | `oaica-lic-…`, shown once after payment (and receipted by email from Stripe). Keep it like a password; we store only its hash. |
| Activations | Up to three machines per key (`oaica activate <key>`, or `OAICA_LICENSE_KEY` for a deployment that injects it). |
| Checking | The binary revalidates against the licence server at most every 7 days, sending the key and the machine's activation id (see `docs/ENTERPRISE.md`, row 7). It keeps working for up to 30 days if the server is unreachable. |
| Revocation | A refunded or charged-back payment revokes the key; the next revalidation reports it. |
| Transfer | The key is for the purchaser's use across their own machines; do not publish or resell it. |

Refunds: contact oaica@sprapp.com. Statutory consumer rights are not affected by anything here.

## 3. API plans — subscriptions

The hosted API (lite / pro / pro+ / max) is a separate monthly subscription with its own key (`oaica-sk-…`),
governed by the Terms of Service and Privacy Policy served at `https://api.oaica.com/terms` and `/privacy`
(sources: `tools/gateway/legal/`). Cancelling stops renewal; a failed payment moves the key to `past_due` and then
`canceled` per `docs/BILLING_ENTITLEMENT.md`.

## 4. Models and third-party components

Model weights are **not** covered by the MIT licence of this repository. Each model keeps its own licence
(recorded in the model manifest and in the Terms of Service, section 6); using a model means complying with it.
Third-party Go modules keep their own licences (`go.mod`). "Ollama" and other third-party names belong to their
owners.

## 5. Governing law

Malaysian law; the courts of Malaysia have exclusive jurisdiction (Terms of Service, section 10).

## 6. Not legal advice

This summary was prepared for BizTransit Sdn Bhd's product. Have counsel confirm it (in particular refund
wording, consumer-law carve-outs for your buyers' countries, and SST registration status) before relying on it.
