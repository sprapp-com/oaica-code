# Metering audit — tokens, caching, cost (2026-08-29)

Ten claims about the gateway → oaicalb → meterhub metering pipeline,
each verified against production on a100b with live requests, ledger
rows and code references. Numbers are measured; "by construction" means
proven from code, not from a dedicated probe.

| # | Claim | Verdict | Evidence |
|---|---|---|---|
| C1 | Non-stream ledger tokens = vLLM `usage` | PASS | direct :30106 `{prompt 28, completion 8}`; gateway ledger row identical |
| C2 | Streaming captures the final usage chunk | PASS | gateway forces `stream_options.include_usage=true` (`tools/gateway/main.go`); ledger row `usage_seen:true`, 30/8 tokens = SSE usage chunk |
| C3 | `cached_tokens` is reported on prefix-cache hits | **FAIL** | vLLM 0.24.0 returned `prompt_tokens_details: null` for an identical 4,010-token prompt sent twice while its own log showed `Prefix cache hit rate: 38.1%`; 0 of 3,134 ledger rows had `cached_tokens>0`. Cause: `--enable-prompt-tokens-details` is off by default (`cli_args.py: enable_prompt_tokens_details = False`). Effect: the `cached_prompt` rate in `oaica-gateway.json` never applied; cache hits billed at the fresh rate. **Fixed** by adding the flag to `tools/a100b/vllm_awq_watchdog.sh` (rolling restart) |
| C4 | `cost_usd` formula | PASS | `computeCostUSD`: `(prompt−cached)·prompt + cached·cached_prompt + completion·completion`, clamps `cached>prompt`; hand-recomputed rows match to 8 decimals |
| C5 | meterhub 1-h totals = ledger totals | PASS | key `internal-91`, 108 rows: prompt 15,042,477 / completion 56,800 / $0.75893985 identical on both sides |
| C6 | No double counting; probes not metered | PASS | `X-Oaica-Metered` (gateway) ↔ `requestAlreadyMetered` (oaicalb); health probes POST straight to replicas, never through a metered handler; 14 stray `direct` rows in 24 h, $0 |
| C7 | Error rows carry 0 tokens / 0 cost | PASS | 565 non-200 rows sampled: `prompt 0, completion 0, usage_seen:false` |
| C8 | Aborted streams | PASS (correctness) / **gap** (billing) | 51 rows `aborted:true`, all `0 tokens, $0` even at 66 s latency — GPU work done before the client disconnected is unbilled. Policy decision pending (see below) |
| C9 | Reasoning tokens included in `completion_tokens` | PASS (by construction) | gateway and oaicalb read upstream's top-level `completion_tokens`, never a content-only count |
| C10 | 5-h subscriber window = ledger sum | PARTIAL | 58,271,063 vs 58,430,737 (0.27 %) — window-edge timing (~38 s of traffic at ~4,200 tok/s), not a counting error |

## What changed because of this audit

* `--enable-prompt-tokens-details` added to the replica launch flags so
  vLLM populates `usage.prompt_tokens_details.cached_tokens`; the
  cache-hit discount in `docs/PRICING.md` is real only from this point.
  Everything metered before it was billed at the fresh-token rate
  (an over-charge relative to the rate card, never an under-charge).

## Open policy question (C8)

A client that aborts mid-stream has already consumed prefill and some
decode on a GPU. Options, in increasing aggressiveness:
1. keep as is (no bill, but record `estimated_prompt_tokens` and the
   number of streamed content chunks on the row so the leak is measurable);
2. bill the prompt at the estimated size plus streamed chunks as
   completion tokens, flagged `estimated:true` on the ledger row;
3. bill the prompt in full from a pre-generation `count_tokens` call.
Not implemented pending a decision.
