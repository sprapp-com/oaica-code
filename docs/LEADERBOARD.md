# a100b model leaderboard — real measured numbers

All numbers below are real measurements taken on the shared a100b box
(2026-08-24), using `bench_robust.py`-family scripts: N concurrent requests,
`max_tokens=150`, `temperature=0.7`, real `usage.completion_tokens` from
responses, total wall-clock / total tokens. Box is shared with other users'
active jobs — throughput varies run to run with contention, these are single
snapshots, not averaged benchmarks. Re-run before citing for anything
load-bearing.

## Throughput comparison (N=48 concurrent, single measurement)

| Model | Port | tok/s | errors | time | Replicas live |
|---|---|---|---|---|---|
| **kat-awq** (prod, AWQ W4A16) | `:30099` (katlb) | **2983.7** | 0 | 2.4s | 1 (only 1 of 6 configured backends up at measurement time) |
| **kat-vl-mtp** (vision+MTP, BF16) | `:30140` | **183.3** | 0 | 37.8s | 1 |
| **malay35b** (gguf Q4_K_M) | `:30110` | **106.3** | 0 | 62.7s | 1 |

kat-awq is ~16x kat-vl-mtp's throughput and ~28x malay35b's — expected: it's
quantized (W4A16 vs BF16) and runs with CUDA graphs (`--enforce-eager` not
set), while kat-vl-mtp is forced into eager mode to fit unquantized on one
A100, and malay35b is a CPU-offload-capable gguf/llama.cpp serve, not vLLM.
Not apples-to-apples — different runtimes, different quant, different
purpose (vision+tool-calling vs text-only coding vs Malay-language text).

## Model profile

| | kat-awq | kat-vl-mtp | malay35b |
|---|---|---|---|
| Base | `Kwaipilot/KAT-Coder-V2.5-Dev` | same + vision/MTP splice from `Qwen/Qwen3.6-35B-A3B` | DARE-merged 35B (see model card) |
| Quant | AWQ W4A16-ASYM | none (BF16) | GGUF Q4_K_M |
| Serving engine | vLLM 0.24.0 | vLLM 0.24.0 | llama.cpp (`llama-server`) |
| VRAM/replica | ~21.65GB weights | ~67GB weights, ~73.5GB w/ KV cache | ~21GB (Q4_K_M) |
| Max context | 262,144 | 16,384 (tested; untested beyond) | 65,536 (`n_ctx`), 262,144 train |
| Modality | text + tool_calls | **vision + text + tool_calls** | text only |
| Spec-decode / MTP | not used | present, 86.4% draft acceptance, but net **-15% throughput** under `--enforce-eager` (measured, see `KAT_VL_MODEL_EVAL.md` part 4) | n/a |
| CUDA graphs | yes | no (`--enforce-eager` forced — BF16 too large for graphs + weights on 1 GPU) | n/a (llama.cpp) |
| GPU (current) | GPU0 (`:30199`), GPU5 (`:30105`) | GPU2 | GPU7 |
| Watchdog | yes (`vllm_awq_watchdog.sh`) | yes (`vllm_vlmtp_watchdog.sh`) | yes (`oaica_malay35b_serve.py`) |
| Persistent / in remotes.json | yes, all 3 machines | yes, all 3 machines (added 2026-08-23/24) | yes, all 3 machines |

## Known caveats on these numbers

- **kat-awq measurement is optimistic for "the fleet"**: only 1 of 6
  configured backends was actually up at measurement time (others show
  `DOWN` in katlb `/status` — not launched, not failed). Real fleet-aggregate
  numbers from earlier full-fleet testing: ~5,000 tok/s/replica @ N=192,
  ~27,000 tok/s aggregate across 6 replicas (see `tools/katlb/README.md`).
- **kat-vl-mtp number varies run-to-run** (144.0 tok/s in one run, 183.3 in
  another, same config, ~10 min apart) — box contention from other users'
  jobs on shared GPUs affects wall-clock even though kat-vl-mtp has its own
  dedicated GPU2, likely PCIe/host-CPU contention from neighbor jobs.
- **Not a quality/capability ranking** — kat-vl-mtp is the only one with
  vision. malay35b is the only one tuned for Malay-language text. kat-awq is
  the fastest/highest-context but text-only. Pick by capability need, not
  raw tok/s.
- Full detailed eval, quantization-blocker root cause, and MTP throughput
  A/B methodology for kat-vl-mtp: see `docs/KAT_VL_MODEL_EVAL.md`.
