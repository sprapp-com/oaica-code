# Evaluation: KAT-Coder-V2.5-Dev-VL-oQ4e-mtp vs production kat-awq

**Bottom line: cannot deploy on current infrastructure. Not a quality/performance tradeoff — a hard platform blocker.**

Source: https://huggingface.co/jason-schulz/KAT-Coder-V2.5-Dev-VL-oQ4e-mtp

## Why it's blocked

| | This model | Our infra (a100b) |
|---|---|---|
| Format | **MLX** (`library_name: mlx`) | vLLM + AWQ (CUDA) |
| Hardware target | Apple Silicon / Metal only | 8x A100 SXM4 80GB (CUDA) |
| Serving engine | mlx-lm / mlx-vlm | vLLM 0.24.0 |
| CUDA support | **None — MLX has no CUDA backend** | N/A (this is what we run) |

MLX cannot run on NVIDIA GPUs, period. No CUDA path exists in the MLX runtime at all — this isn't a missing-kernel gap that a config flag fixes, it's a different hardware ecosystem entirely (same category as "can't run an .exe on Linux without Wine" — except there is no Wine here).

## Decision matrix

| Criterion | KAT-Coder-V2.5-Dev-VL-oQ4e-mtp | Production kat-awq (AWQ W4A16) | Verdict |
|---|---|---|---|
| **Runs on a100b today** | No — MLX, Apple-only | Yes — live, verified | Blocker |
| Quantization | oQ4e (MLX 4-bit; 6-bit sibling `oQ6e` also exists) | AWQ W4A16-ASYM | Not comparable across runtimes |
| Reported size | ~21.6GB (5 shards) | ~21.65GB (5 shards) | Coincidental match, not evidence of equal CUDA VRAM footprint |
| Base architecture | `qwen3_5_moe` (tags list both `Kwaipilot/KAT-Coder-V2.5-Dev` and `Qwen/Qwen3.6-35B-A3B`) | `Kwaipilot/KAT-Coder-V2.5-Dev` | Possibly different backbone — unverified |
| Modality | **Vision-language** (`image-text-to-text`, real vision preprocessor + chat template) | Text-only | Functional difference, not just a quant swap |
| MTP / speculative decoding | Tagged `mtp`, `speculative-decoding` | Not currently exploited in our vLLM launch flags | Interesting if ever ported to a CUDA-compatible quant |
| Published benchmarks | **None found** (empty `model-index`, no benchmark numbers in metadata) | Real measured: ~5,000 tok/s/replica @ N=192, ~27,000 tok/s aggregate across 6 | Cannot compare quality/perf — no data exists for the candidate |
| Community signal | 555 downloads, 2 likes | N/A (self-hosted) | Too sparse to infer anything |
| VRAM use (measured) | **Untestable** — can't run it here | ~21.65GB/replica, real measured | N/A |

## Recommendation

Do not attempt this swap. If VL (multimodal) capability or MTP/speculative decoding is actually wanted on the fleet, the real path is:
1. Find or produce a **CUDA-compatible** quant (AWQ/GPTQ/GGUF) of the *same* underlying checkpoint — from `Kwaipilot/KAT-Coder-V2.5-Dev` or whatever the true VL base is, not this MLX artifact.
2. Only then run the same benchmark methodology already established this session (`fleet_bench2.py`, N-sweep, real tok/s measurement) to get a genuine decision matrix with real numbers on both sides.

No fabricated benchmark numbers are used above — every blank cell reflects data that does not exist publicly, not an oversight.

## Update 2026-08-21/22: built our own CUDA-compatible VL+MTP variant

Followed this doc's own recommendation. Real, self-hosted build, not the MLX artifact above.

**Approach:** confirmed via direct `huggingface_hub` config.json diff that `Kwaipilot/KAT-Coder-V2.5-Dev` and `Qwen/Qwen3.6-35B-A3B` share an identical base architecture (`Qwen3_5MoeForConditionalGeneration`, `qwen3_5_moe`) — only `text_config.mtp_num_hidden_layers` differs (0 vs 1). KAT-Coder inherited a vision-capable config but was never trained on vision data (confirmed by Kwaipilot's own maintainer). Spliced Qwen3.6-35B-A3B's real vision-tower weights (333 tensors, reused a pre-existing verified extraction already on the build box) and MTP weights (19 tensors, downloaded only the 2 of 26 shards that contain them, ~5.7GB not 70GB) onto KAT-Coder's own checkpoint, keeping 100% of KAT's own coding fine-tune untouched.

**Real, verified results (BF16, unquantized, single A100, `--enforce-eager` for this sanity build):**

| Test | Result |
|---|---|
| Weight load (transformers, correct class `Qwen3_5MoeForConditionalGeneration`) | Clean, 693/693 tensors, 34.66B params, zero shape errors |
| Vision tower wired into live model | Confirmed — 333 `model.visual.*` params present and loaded |
| `tool_calls` (coding capability) | Intact — real structured tool call returned correctly |
| Real vision test (generated image: red square + blue ellipse) | **Correct** — model described "red square on the left... blue circle on the right" accurately |
| MTP / speculative decoding | **Active** — real vLLM engine confirmation (`Detected MTP model. Sharing target model embedding weights with the draft model.`) |
| Speculative decode acceptance rate | **86.4%** (146/169 draft tokens accepted, real `/metrics` counters, not estimated) |

**Real gotchas hit and solved (for anyone repeating this):**
- `AutoModelForCausalLM.from_pretrained` silently resolves to a text-only variant for this config and drops multimodal weights with no error — must use `Qwen3_5MoeForConditionalGeneration` explicitly.
- MTP weights are NOT part of the main HF `transformers` forward class by design (`_keys_to_ignore_on_load_unexpected` in `modeling_qwen3_5_moe.py`) — they're a separate vLLM-side draft-model concept, loaded via `--speculative-config`, not bundled auto-detection in the main checkpoint.
- vLLM's own MTP auto-detect (`config/speculative.py`) reads `mtp_num_hidden_layers` from the **top level** of the draft checkpoint's config — a nested `text_config` (as our multimodal-wrapper main checkpoint has) won't be found. Fix: build the standalone MTP draft checkpoint with a **flat** config (text fields promoted to top level, `model_type` forced to `qwen3_5_moe`), separate from the main vision-carrying checkpoint.
- Full BF16 (unquantized) barely fits an A100 80GB even with `max-model-len=4096` and `--enforce-eager` — confirms quantization isn't optional, it's required for any real serving.
- `AutoAWQ` does not support `qwen3_5_moe` (checked its registered architecture map directly) and is deprecated upstream in favor of `llm-compressor`.

**Not yet done:** AWQ W4A16 quantization of this spliced checkpoint (next step), deployment at production context length, full throughput benchmark vs 2-replica production `kat-awq` baseline, FP8 activation/KV-cache follow-on tests. This section will be updated as those land — see the build session for live status.
