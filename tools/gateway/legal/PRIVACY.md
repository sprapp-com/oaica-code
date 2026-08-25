# oaica Inference API — Privacy Policy

Effective: 2026-08-26

Operator: BizTransit Sdn Bhd, Malaysia ("oaica", "we").
Contact: biztransit@bcz.com

## What this covers
The OpenAI-compatible inference API at https://api.oaica.com (the "Service"),
including access via OpenRouter and any other reseller that forwards requests to us.

## What we receive
- Request content: the prompts, messages, tool definitions and sampling
  parameters sent to `/v1/chat/completions` and `/v1/completions`.
- Request metadata: timestamp, HTTP status, request path, the API key that
  authenticated the request, token counts, which backend served it.
- We do not receive end-user identity from OpenRouter; we see OpenRouter's
  key and IP, not yours.

## What we store
- Prompts and completions: NOT written to disk by our gateway, load balancer
  or auth layer. They exist only in memory (host RAM and GPU memory,
  including the inference engine's prefix cache) for the duration needed to
  serve the request and are then evicted. We do not keep copies.
- Metadata (timestamps, status codes, key id, backend, token counts,
  latency): kept in process logs for up to 30 days for debugging,
  abuse detection and billing reconciliation, then deleted.
- We do not build user profiles and we do not sell any data.

## Training
We do not use your prompts, completions or any request content to train,
fine-tune, evaluate or improve any model. We serve a third-party open-weight
model as published; we do not modify it with customer data.

## Where processing happens
- Inference: GPU servers located in Japan (JP). Our company is headquartered in
  Malaysia; the compute is not. This capacity is rented
  from a third-party GPU marketplace (Vast.ai) on shared physical hardware
  that we do not own or physically control. The hardware host can in
  principle access memory on that machine; we mitigate by not persisting
  request content, but you should not send data whose exposure to the
  hardware host would be unacceptable.
- Network transit: requests pass through Cloudflare (tunnel/edge) and an
  encrypted SSH tunnel operated by us. Cloudflare may process connection
  metadata at whichever edge location is nearest to the sender.
- Failover: we do not currently route to any other region. If a managed
  failover (e.g. Replicate, United States) is enabled in future, this policy
  and our OpenRouter listing will be updated first.

## Sub-processors
Cloudflare, Inc. (edge/tunnel); Vast.ai host (GPU hardware, JP).
No others at this time.

## Security
TLS to Cloudflare; encrypted tunnel to the inference host; Bearer-key
authentication; no request-body logging in our software. There is no SOC 2,
ISO 27001 or similar certification.

## Your rights
Because we do not store request content or identify end users, we generally
cannot locate or delete data about a specific person. Requests about
metadata logs, or any other privacy question: biztransit@bcz.com. Malaysian
Personal Data Protection Act 2010 applies to any personal data we do hold.

## Changes
We will post changes here and update the effective date. Material changes
to retention, training or processing location will be announced to
OpenRouter before they take effect.
