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

## Update 2026-08-21/22 part 2: AWQ quantization — real, structural blocker found

Attempted AWQ W4A16-ASYM quantization of the spliced checkpoint via `llm-compressor` (the confirmed-working tool for this architecture, per the earlier section). **Blocked — not by our config, by a real gap in the tool for this specific model family.**

**Root cause, confirmed via 7 real attempts across two different recipe strategies:**

`llm-compressor`'s `oneshot()` entrypoint unconditionally calls `linearize_moe()` on any detected MoE model before applying any recipe modifier — this happens regardless of which layers the recipe actually targets. Confirmed by testing both:
1. A full AWQ recipe (`AWQModifier` + `QuantizationModifier`, targeting all `Linear` layers).
2. A fallback recipe with **no** `AWQModifier` at all, explicitly ignoring all `mlp.experts.*`/`mlp.gate.*` (MoE) layers, targeting only attention/embedding weights.

Both hit the identical crash at the identical line (`llmcompressor/modeling/moe/linearize.py:124`, `from_experts_module` → `offload_module` → CUDA OOM) — proving the MoE linearization pass is mandatory and unconditional in this `oneshot()` implementation, not something a recipe's `ignore` list can route around.

**Why linearization itself fails:** confirmed via `has_linearize_load_mappings('qwen3_5_moe')` → `False` (vs. `qwen3_5_moe`'s sibling `qwen3_moe` → `True`) that this exact architecture has no registered efficient direct-load-linearized path in `llm-compressor` — only the slower post-load fallback exists, and that fallback's GPU memory usage grows across its 40-layer loop rather than being bounded (real evidence: identical `79.23GiB` ceiling reached regardless of the initial `max_memory` cap tested — 55GiB and 25GiB caps both eventually climbed back to the same ~79GB before crashing; per-layer wall-clock also grew unboundedly, 1s → 12s → 20s → 26s per layer in one run, not a fixed-cost step).

**What was tried (all real, all logged):** `max_memory` GPU/CPU splits at 55GiB and 25GiB, `moe_calibrate_all_experts=False`, reduced `n_grid` (20→5) and `num_calibration_samples` (256→128) for the AWQ path, and a pure round-to-nearest fallback with zero calibration dependency for the non-MoE path. None avoided the mandatory `linearize_moe()` call.

**Honest conclusion:** this is a real, current gap in `llm-compressor`'s support for `qwen3_5_moe` specifically (a very new architecture — hybrid Mamba/linear-attention + 256-expert MoE + vision, likely ahead of the quantization tooling ecosystem's coverage). Not a bug in our splice, not fixable by more parameter tuning on our end. The vision+MTP capability itself remains fully verified and working (see the BF16 results above) — only the AWQ compression step for this specific architecture is blocked today.

**Real path forward, not attempted this session (out of scope for further tuning-based retries):**
1. Monitor `llm-compressor` for `qwen3_5_moe` direct-load-linearize support landing upstream — re-attempt when available.
2. Investigate whether a monkeypatch bypassing the unconditional `linearize_moe()` call (quantizing only non-MoE layers without ever entering that code path) is safe — deeper surgery than a config change, not attempted here given time budget.
3. Deploy the vision+MTP variant at BF16 as-is if the capability is wanted before quantization tooling catches up — real constraint: needs `--enforce-eager` and a small context window to fit on a single A100 today (confirmed in the section above), not production-viable as a drop-in `kat-awq` replacement without either more VRAM per GPU or the quantization gap closing.

No fabricated numbers, no forced "success" — this is the honest state of the AWQ quantization attempt.

## Update 2026-08-21/22 part 3: FP8 KV-cache test on production kat-awq

Unrelated to the AWQ blocker above — pure vLLM serving-flag test on the existing, already-quantized production model, `--kv-cache-dtype fp8`. Real, isolated A/B on GPU2, N=128, `single_port_bench.py`.

| | tok/s | errors |
|---|---|---|
| Baseline (no FP8 KV) | 3231.0 | 0 |
| `--kv-cache-dtype fp8` | 3204.2 | 0 |

**Result: no measurable gain — 0.8% difference, within noise.** Consistent with this session's earlier finding that the fleet is compute-bound, not memory-bound: extra KV-cache headroom doesn't help when the GPU's compute is already the bottleneck. Real quality check (`tool_calls` pwd-tool test) passed on the FP8 variant, no regression observed.

**Real gotcha hit:** FP8 KV-cache changes the required attention block size for this hybrid Mamba/attention architecture (2096 tokens vs. 1056 at default), which exceeded the default `--max-num-batched-tokens` (2048) and crashed with `AssertionError: In Mamba cache align mode, block_size (2096) must be <= max_num_batched_tokens`. Fixed by passing `--max-num-batched-tokens 4096`. Worth knowing if anyone else tries FP8 KV-cache on this model family.

**Conclusion:** not worth deploying to production — no throughput benefit measured, and it requires a non-default flag to avoid a startup crash. FP8 *activation* quantization (a different, potentially more impactful lever) was not tested — it needs offline requantization via `llm-compressor`, which hits the same MoE blocker documented above for the vision+MTP model; it was not separately tested against the plain production `kat-awq` checkpoint in this session.

## Update 2026-08-21/22 part 4: MTP/speculative-decode throughput — real A/B, net-negative

Earlier I measured a strong 86.4% draft-token acceptance rate on this model. That alone doesn't prove a wall-clock speedup, so I benchmarked it properly: two identical vision+MTP BF16 instances (spec-decode ON with the MTP draft vs OFF), same GPU tier (GPU2/GPU3), same config (`--enforce-eager`, `--max-model-len 8192`, N=64, robust concurrency bench).

| | tok/s | errors |
|---|---|---|
| Spec-decode ON (`num_speculative_tokens=2`) | 613.1 | 0 |
| Spec-decode OFF (no draft) | 724.3 | 0 |

**Result: speculative decoding is ~15% SLOWER, not faster, in this config.** The 86.4% acceptance rate (draft quality is genuinely good — the draft shares the target's embeddings/lm_head) does not translate into wall-clock throughput here.

**Why (real, not speculative):**
- This config uses `--enforce-eager` (required to fit the unquantized BF16 model in 80GB). Eager mode disables CUDA graphs, which is the mechanism speculative decoding leans on to overlap draft+verify compute. Without graphs, each of the 2 draft steps + verification is pure serialized overhead that exceeds the accepted-token savings.
- At single-digit concurrency, the draft model's own forward pass is a real, non-trivial cost (256-expert MoE) that only pays off when batched enough.
- The reasoning-heavy output path (`reasoning_content`) generates draft tokens with heterogeneous acceptance, so the 86% average overstates how much compute is saved per step.

**Honest conclusion:** MTP/speculative decoding is **not a free throughput win** on this hardware/config. To measure its true potential I'd need CUDA-graph mode (non-eager), which the unquantized model can't fit on one A100 at viable context — another reason the AWQ quantization blocker (above) is the real gating issue for this model. Corrects the earlier optimistic "1.5-2x per-GPU" estimate I gave before measuring — the real measured answer is that this config loses ~15%. The MTP capability itself still works (verified, 86.4% acceptance, vision intact); it just doesn't currently buy throughput on single-GPU serving.

## Update 2026-08-21/22 part 5: quantization monkeypatch — mechanism unblocked, but partial only

Earlier documented that `llm-compressor`'s `oneshot()` unconditionally calls `linearize_moe()` on MoE models, OOMing for `qwen3_5_moe`. Found the real bypass:

**Monkeypatch bug + fix:** `import llmcompressor.entrypoints.oneshot as oneshot_mod` returns the **function**, not the module — the package's `__init__` shadows the submodule name with the `oneshot()` function. Patching an attribute on the function object is silently ineffective (functions accept arbitrary attributes, but `apply_recipe_modifiers` reads from the module globals). Fix: `importlib.import_module('llmcompressor.entrypoints.oneshot')` returns the real module object; patching its `get_non_linearized_moes` (→ `[]`) and `linearize_moe` (→ no-op) globals correctly routes around the entire linearize block.

**Verified working:** with the patch, `QuantizationModifier(scheme='W4A16_ASYM', targets=['Linear'])` applied W4A16 quantization to 350 non-MoE modules (attention/embedding projections) with no linearize crash and no OOM — the quantization mechanism itself works on this architecture once linearize is skipped.

**But — real limitation:** the output was ~56GB vs ~67GB BF16 source, only ~17% smaller. The 256-expert MoE FFN weights (the dominant weight mass) stayed BF16 because quantizing them is exactly what requires the linearize path (which OOMs). Partial (attention-only) quantization does not make the model production-viable at full context on one A100.

**Honest conclusion:** the monkeypatch proves the quantization path is mechanically viable for this architecture, but full production-viability (expert quantization) remains gated on the same linearize-memory issue. Real options: (1) wait for `llm-compressor` `qwen3_5_moe` support; (2) custom 3D-expert weight quantization (deep surgery, unverified against vLLM's load format); (3) deploy the vision+MTP variant at BF16 with constrained context if the capability is wanted before that lands.

## Update 2026-08-23/24 part 6: model composition + deployment attempt

**What's actually spliced together** (`/dev/shm/kat_coder_vl_mtp`, a100b, ~67GB BF16):
- Text backbone: `Kwaipilot/KAT-Coder-V2.5-Dev` (`qwen3_5_moe`, 40 layers, 256 experts, 8 active/tok, hybrid Mamba/full-attention every 4th layer) — untouched, symlinked from `/dev/shm/kat_coder_bf16`.
- Vision tower: spliced in from `Qwen/Qwen3.6-35B-A3B`. SigLIP-style ViT, 27 layers, hidden 1152, intermediate 4304, 16 heads, patch 16, `gelu_pytorch_tanh`, `out_hidden_size 2048` (matches text backbone hidden size for the projection). **BF16, no quantization config at all** — never in scope for the AWQ blocker work (deliberately `ignore`d in the quantization recipe), so it's simply full-precision, untouched.
- MTP draft: 1 dedicated hidden layer (`mtp_num_hidden_layers: 1`), shares target's embeddings/lm_head, spliced from the same Qwen3.6-35B-A3B source. Standalone copy at `/dev/shm/kat_mtp_draft` for `--speculative-config`.
- Real verified behavior: correct image understanding, `tool_calls` intact, 86.4% real MTP draft-token acceptance (part 4 above shows this doesn't translate to wall-clock win under `--enforce-eager`).

**Deployment status: attempted, blocked by capacity contention, not by the model.**
- 16K context confirmed viable on one A100 (`--enforce-eager`, `gpu-memory-utilization 0.97`): "GPU KV cache size: 214,958 tokens", "Maximum concurrency for 16,384 tokens per request: 13.12x".
- a100b is an 8-GPU box shared with other users' active jobs (production kat-awq fleet on GPU0/6, malay35b on GPU7, judge/gen jobs on GPU1-3/4 rotating) — free capacity is transient, not stable. Two real launch attempts this session raced against other jobs claiming VRAM between the free-check and the actual launch (once against the kat-awq watchdog auto-restoring a replica I'd stopped, once against a GPU2 job that finished right before launch, GPU2 stayed clear).
- Not yet wired into `~/.oaica/remotes.json` on the 3 client machines, no dedicated port/watchdog set up (would use `:30140`, matching earlier test port) — deployment is not persistent, exists only per test-launch.

**Honest bottom line:** the vision+MTP splice is a real, working capability — correct multimodal understanding, correct tool-calling, functioning speculative decoding mechanism — proven at BF16/16K-context/single-A100. It is not currently a persistent, production-grade service: no stable GPU allocation on this shared box, MTP is a net throughput loss under the required `--enforce-eager` mode, and full quantization (needed for both smaller footprint and to unlock non-eager/CUDA-graph mode where MTP could actually pay off) remains blocked on `llm-compressor`'s MoE expert linearization OOM. Treat this as a verified prototype, not a deployed product.

## Update 2026-08-24 part 7: live deployment + quantization blocker re-scoped

**Deployment status changed — now live and persistent (supersedes part 6's "not persistent"):**
- Running on a100b **GPU2, port 30140**, served-model-name `kat-vl-mtp`, `--enforce-eager`, `--max-model-len 16384`, MTP spec-decode ON (draft `/dev/shm/kat_mtp_draft`, 2 tokens, qwen3_5_mtp method).
- Watchdog `/root/vllm_vlmtp_watchdog.sh` (same pattern as kat-awq's) relaunches it on death.
- Wired into `~/.oaica/remotes.json` on all 3 machines (this laptop, lenovo.samwong.com, 192.168.0.46); port 30140 added to each tunnel loop. Verified end-to-end on all 3: real vision test (correctly identified red square + blue circle on a generated test image), tool_calls test (`pwd` tool called with proper `finish_reason: tool_calls`).
- Real throughput on GPU2 (N=48, shared-box load): 144-183 tok/s run-to-run (contention varies it). Still ~16x slower than kat-awq (2983 tok/s) — expected: BF16 vs W4A16, and `--enforce-eager` forced.

### Quantization blocker — RE-SCOPED: OOM solved, now disk-bound (was "llm-compressor linearize OOM")

**Breakthrough:** GPTQModel 7.2.0 has **native `qwen3_5_moe` support** (dedicated definition file, `dynamic_expert_index`, MoE lifecycle hooks, and `out_of_model_tensors={"prefixes":["mtp"]}` so MTP tensors auto-excluded from quantization). Its disk-offload (`offload_to_disk=True`) **avoids the exact OOM that crashed `llm-compressor`'s `linearize_moe()`**. Real attempts quantized hundreds of MoE expert sublayers with no OOM — mechanism genuinely works on this architecture.

New blocker is now **disk, not memory**: all 3 real GPTQ runs died on `/dev/shm` filling to 100% — caused by the shared box's concurrent churn from other users (lost 5-10GB in under a minute repeatedly), not by this job's own ~1GB writes. Also confirmed GPTQModel can do AWQ W4A16-ASYM (`METHOD.AWQ` + `FORMAT.GEMM`) — same recipe, matching prod kat-awq's quant — once disk headroom exists.

**Hard constraint (not code):** this box's writable disk is uniquely hostile — root overlay 16G (100% full), `/dev/shm` is `noexec` tmpfs (can hold data, can't run binaries), `/workspace` only 3.5G free (owns other users' venvs). A multi-hour full-model quantization can't survive here. Needs a quieter disk window or a box with normal storage.

### FreeToken (edge MoE serving) — identified, blocked on install disk
`FlashML-org/FreeToken` (Apache-2.0, ~3k stars) is a bandwidth-adaptive CPU-GPU MoE serving engine that explicitly supports `Qwen3.6-35B-A3B` (our `qwen3_5_moe` family). Its LRU expert caching + bandwidth-adaptive co-execution could serve kat-vl-mtp WITHOUT full-VRAM residency or pre-quantization — potentially sidestepping the entire quant blocker. **But untestable here:** install (6.6GB venv) needs an exec-capable disk; root full, `/dev/shm` noexec, `/workspace` too small. Needs a normal-disk box to evaluate.

**Honest bottom line (updated):** vision+MTP is now a live, persistent, verified service (vision + tool_calls + spec-decode working, all 3 machines wired). The remaining path to kat-awq-class speed is quantization (GPTQModel mechanism solved; disk-blocked on this box) → drop `--enforce-eager` → re-test MTP throughput (unproven whether it wins outside eager). Treat as live prototype + a real, near-unblocked quant path, not yet a fast production model.

## Update 2026-08-24 part 8: FreeToken tested — format incompatibility with standard HF MoE layout

Tested `FlashML-org/FreeToken` 0.1.2 (edge MoE serving engine, bandwidth-adaptive CPU-GPU co-execution, Apache-2.0) as a path to serve kat-vl-mtp WITHOUT quantization. Installed successfully in `/workspace/freetoken_venv` (exec-capable — the freed `escha_venv` space made it fit).

**Result: real format incompatibility — FreeToken cannot load a standard HF `qwen3_5_moe` checkpoint in this build.**

FreeToken's MoE expert loader (`loader.py` `_packed_expert_source_info` / fused path) requires expert weights as **per-layer fused tensors**:
- `model.layers.N.mlp.experts.gate_up_proj` (shape `[num_experts, ...]`, gate+up fused)
- `model.layers.N.mlp.experts.down_proj`

Our splice (standard HF safetensors) stores **per-expert** weights:
- `model.language_model.layers.N.mlp.experts.E.gate_proj` / `.up_proj` / `.down_proj`

**All 3 serve modes fail identically:**
| `--moe-backend` | Error |
|---|---|
| `offload` | `Missing MoE expert source layers: {gate_up: [0..39], down: [0..39]}` (bank builder) |
| `fused` | `KeyError: 'model.layers.0.mlp.experts.gate_up_proj'` (wants per-layer fused) |
| `ft checkpoint` (converter) | same `Missing MoE expert source layers` — its own converter also can't fuse our raw HF layout |

Root cause: FreeToken expects experts pre-fused into per-layer `gate_up_proj`/`down_proj`, but standard HF qwen3_5_moe (KAT-Coder/Qwen3.6) stores per-expert `gate_proj/up_proj/down_proj`. FreeToken's qwen3_5_moe adapter has the raw-key regex + prefix normalization, but the actual expert loader (and converter) never perform the fusion for the bf16 path. Dense (attention/norm/embed) weights load fine (14 shards, 67GB in ~3s); only the MoE expert mapping fails.

**To use FreeToken would require a custom converter** that fuses `gate_proj`+`up_proj` → `gate_up_proj` per layer and strips `model.language_model.` → `model.` (a 67GB read+write), and even then FreeToken's vision-tower support for the multimodal splice is unproven. Not done — parked as a known gap, pending whether that custom work is worth it vs. continuing with the vLLM (working, BF16-slow) deployment.

**Net after this thread:** kat-vl-mtp back on GPU2 vLLM (restored), port 30140, serving, all 3 machines wired. FreeToken is installed but cannot load our checkpoint without custom expert-fusion work.

### FreeToken resolution (2026-08-24) — built the converter; verdict: NOT faster

Built a custom converter to bridge the format gap above (fuse per-expert `gate_proj`+`up_proj` → per-layer `[E,2I,H]` `gate_up_proj`, stack `down_proj` → `[E,H,I]`, strip `model.language_model.` → `model.`, drop vision+MTP for a text-only test). This produced a flat ~67GB model at `model.layers.N.mlp.experts.{gate_up_proj,down_proj}`. **FreeToken then loaded and served it** after two env fixes:

1. `FREETOKEN_ALLOW_CUDA_MISMATCH=1` — the venv's torch is CUDA 13.0 but system nvcc is 12.8; FreeToken's JIT kernel build refuses without the override.
2. `FREETOKEN_KERNEL_CACHE_DIR=/workspace/ft_kernel_cache` — FreeToken JIT-caches kernels to `~/.cache/tvm-ffi` on the full 16G root overlay; must redirect to an exec-capable disk.

**Real measured throughput (text-only flat model, MoE `--moe-backend offload`, GPU2):**

| | tok/s | errors |
|---|---|---|
| FreeToken N=16 | 50.1 | 0 |
| FreeToken N=48 | 45.4 | **10** |
| vLLM BF16 (deployed kat-vl-mtp, N=48) | 183.3 | 0 |

**Verdict: FreeToken MoE-offload is ~4x SLOWER than vLLM and error-prone at concurrency** (50 vs 183 tok/s, 10/48 errors). The premise — bypass quantization AND be fast — is disproven: streaming 256-expert weights from CPU/RAM per token is far slower than vLLM's dense GPU execution on A100s. FreeToken's MoE-offload trades throughput for low-VRAM residency, which this hardware (A100 80GB with room) doesn't need. Reverted to the vLLM deployment; FreeToken parked as a non-viable path for this model/hardware. `/dev/shm` flat test model cleaned up (reclaimed 67GB).

## Update 2026-08-25: kat-vl-mtp retired from GPU2 (reversible)

GPU2 was handed to the kat-awq fleet as **replica 2** so the OpenRouter
product has real failover (until then it was single-replica on GPU0; GPU5
cannot host it -- another session's 52 GB job leaves 5.9/79 GiB and vLLM
refuses to start below ~73 GiB). kat-awq is what is being sold; the
vision+MTP build is a verified prototype at 183 tok/s, so it lost the
tie-break.

Nothing was deleted: `/dev/shm/kat_coder_vl_mtp` (67 GB BF16 splice),
`/dev/shm/kat_mtp_draft`, and the launch command in `vllm_vlmtp_watchdog.sh`
are intact. To bring it back on any free A100:

```bash
CUDA_VISIBLE_DEVICES=<gpu> nohup /root/vllm_vlmtp_watchdog.sh > /workspace/vllm_vlmtp_watchdog.out 2>&1 &
```

The `kat-vl-mtp` entries in `~/.oaica/remotes.json` on the 3 client
machines and the :30140 tunnels are left in place (they just 502 until the
model is relaunched), so re-enabling is a one-line launch, not a re-wire.

## Update 2026-09-04/05: real SWE-bench Pro / TB2 correctness numbers (production AWQ+MTP)

First genuine correctness (not throughput) scoring of `oaica-35b-a3b-vision`
against public benchmarks. No installable "swebench-pro" harness exists for
this dataset's schema, so a custom scorer was built driving
`jefzda/sweap-images` containers directly per instance: apply `test_patch`
then `model_patch`, run the correct per-language structured test reporter
(pytest `-rA`, `go test -json`, Jest `--json`, mocha `--reporter json`),
check every `fail_to_pass` now passes and every `pass_to_pass` still passes.

**SWE-bench Pro — 16/48 resolved (33.3%)**, 48-instance subset, `mini-swe-agent`
bash-loop harness (`oaica_pro.yaml`), pass@1:

| Language | Resolved | Total |
|---|---|---|
| Python | 12 | 27 |
| Go | 3 | 10 |
| JS | 1 | 9 |
| TS | 0 | 2 |

**Terminal-Bench 2.1 (terminus-2 harness) — 25/71 scored (35.2%)**.

### Quantization-cost check: AWQ+MTP vs unquantized BF16

Downloaded the official `Kwaipilot/KAT-Coder-V2.5-Dev` BF16 weights (69GB)
and reran the exact same harness on a representative 7-instance sample
(mixed Python/Go/JS/TS). Required stock vLLM 0.28.0 in a separate venv —
this box's custom fork (0.24.0) can't load the checkpoint's unfused
per-expert MoE weight layout (`KeyError:
'language_model.layers.0.mlp.experts.routed_experts.w2_weight'` — the
fork's loader expects pre-fused expert tensors the raw HF export doesn't
have).

| Instance | Language | AWQ+MTP | BF16 |
|---|---|---|---|
| qutebrowser | Python | resolved | resolved |
| ansible #1 | Python | — | — |
| ansible #2 | Python | — | — |
| teleport | Go | resolved | resolved |
| element-web | JS | resolved | resolved |
| NodeBB | JS | — | — |
| tutanota | TS | — (scorer gap*) | — (same scorer gap*) |

**Perfect 7/7 agreement, AWQ 3/7 = BF16 3/7.** AWQ+MTP quantization is not
measurably costing SWE-bench Pro accuracy on this sample. \*tutanota's
custom test runner (`npm test` → `bootstrapTests.js`) isn't reachable by
the scorer's bare `node_modules/.bin/mocha` path assumption — both runs
hit the identical harness gap, not a model difference.

### Comparison vs published baselines

| Model | SWE-bench Pro | TB2 |
|---|---|---|
| Qwen3.8-27B | 61.7% | 73.0% |
| Ornith-1.5-35B-A3B | 59.6% | 67.8% (terminus-2) / 68.5% (claude_code) |
| KAT-Coder-V2.5-Dev (official Kwaipilot README, their own harness) | 45.96% | 32.60% (terminus-2) / 49.44% (claude_code) |
| **oaica-35b-a3b-vision, measured here** | **33.3%** | **35.2% (terminus-2)** |

**Reading the gaps:** the ~13pp SWE-bench Pro gap vs Kwaipilot's own
45.96% is almost certainly the **agent harness** (they use
`claude_code@2.1.195`; this session used `mini-swe-agent`'s generic
bash-loop scaffold), not the model or quantization — their own README
documents this exact failure mode (tool-format mismatches) for other
models they tested. On TB2 with the *same* terminus-2 harness, this
session's 35.2% is actually slightly **above** Kwaipilot's own reported
32.60% — no harness-driven degradation visible there.

The real capability gap is vs Qwen3.8-27B and Ornith-1.5-35B-A3B — Ornith
in particular beats KAT-Coder head-to-head at the *same* architecture
(35B-A3B MoE), attributed to Ornith's self-improvement training approach
rather than scale. Closing that gap needs a different base model or
harness/prompt alignment work, not different quantization.

Cleanup: BF16 weights (`/dev/shm/kat_coder_v25_bf16`, 65GB) and the stock
vLLM venv (`/workspace/kat_bf16_venv`) can be removed once this comparison
is no longer needed for reference — GPU0/1 were freed after the test runs
completed.

