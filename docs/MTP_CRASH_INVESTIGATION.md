# MTP replica crashes on a100b — investigation and outcome (2026-08-29)

Status of the "oaica-35b-a3b-vision fleet keeps dying since the MTP swap"
incident. Written at the end of the investigation; every number below is
measured, and what is *not* proven is said so.

## What actually happened (two unrelated failures)

**1. Silent, simultaneous, idle deaths — cgroup OOM kills, not MTP.**
The 05:19–06:16 restart loop, the 12:12 double death and the 18:42 triple
death all had the same shape: every replica died in the same second, with
`Running: 0 reqs, Waiting: 0 reqs`, no traceback, only a health probe in
flight. The container's memory cgroup explains it:

```
/sys/fs/cgroup/memory.events   max 182159   oom 420   oom_kill 15
memory.max  = 2 065 597 202 432   (2 TB)
memory.peak = 2 065 597 231 104   (== max: the limit was hit)
memory.stat  shmem 1024 GB  anon 45 GB  file 1156 GB
```

`/dev/shm` holds ~1 TB of model dumps (tmpfs = unreclaimable, counted
against the cgroup); when anything on the box spikes RAM the kernel
OOM-kills, and a SIGKILL is exactly a silent idle death. These would have
happened on the previous AWQ model too. Two limits of this container:
`dmesg` is capability-restricted (no post-hoc proof of *which* kill), and
there is no `CAP_SYS_RESOURCE`, so `oom_score_adj` cannot be lowered
(verified: `Permission denied`). The watchdog therefore logs every
`oom_kill` delta (`ALERT: cgroup OOM killer fired ...`) so the next death
is attributed within one 15 s tick. The only real fix is housekeeping in
`/dev/shm`; oaica's own idle share was `nemotron_bf16` (61 GB) and
`nemotron35_lightning_30b_a3b` (17 GB); the rest belongs to other sessions
(`deepseek-v4-flash-0731` 155 GB, `dsv4_0731_fixed` 92 GB, five
`kat_malay35b*` copies ≈ 270 GB, `hfcheck` 37 GB).

**2. One genuine crash — the MTP draft/GDN race (upstream vllm #53726).**
18:24 on GPU1, full traceback:
`torch.AcceleratorError: CUDA error: an illegal memory access was
encountered` from `gpu_model_runner._to_list -> transfer_event.synchronize()`
inside `sample_tokens`. It cascaded: one session's request killed :30110,
its retry killed :30108, and the same request then succeeded on :30106 —
a non-deterministic race, not a poison payload. Upstream root cause: the
MTP draft's CUDA-graph replay racing the GDN/Mamba recurrent-state
copies on hybrid models. Fixes: PR #50729 (merged to `main` 2026-08-17,
in no tagged release) and PR #53613 (open). `num_speculative_tokens` 3→1
did not help (the second "crash" after that change was an OOM kill).

## What was tried (all on GPU7, isolated, port 30130, prod flags)

| run | traffic | result |
|---|---|---|
| V0 | 100 independent requests, 6 min, c=8 | survived |
| V0b | + 35 % client aborts, 25 % image requests, 8 min, c=12 | survived (45 aborts, 39 images) |
| V0c | multi-turn sessions (40k→190k tokens), aborts, images, 10 min, c=10 | survived (497 turns, 0 errors) |
| V1 | V0c load with draft `enforce_eager:true` | survived, 172 vs 182 tok/s, acceptance 1.74–1.83 vs 1.75 |
| main | V0c load on vLLM nightly `0.28.1rc1.dev101+g6cddad414` (has both fixes), draft CUDA graphs ON | survived, 433 turns, 0 errors, 176 tok/s, acceptance 1.89 |

The 18:24 crash could **not** be reproduced on demand in ~60 min of
production-shaped load, so nothing here *proves* a fix; the harness lives
in `tools/a100b/mtp_bisect/` for the next attempt.

## What is deployed

* `--speculative-config '{"method":"mtp","num_speculative_tokens":1,"enforce_eager":true}'`
  on all three replicas since 20:26 UTC. `enforce_eager` inside the
  speculative config applies to the **draft head only**; the target
  model keeps torch.compile and PIECEWISE CUDA graphs
  (`SpeculativeConfig.enforce_eager` →
  `llm_base_proposer.initialize_cudagraph_keys` → `CUDAGraphMode.NONE`
  for the drafter in vLLM 0.24.0). This is the workaround the upstream
  reporters confirmed. Cost measured: none (KV cache size identical,
  throughput within run-to-run variance).
* Watchdog: crash-log preservation (`*.prev_crash`), OOM attribution,
  one-time notice that `oom_score_adj` cannot be lowered.
* `tools/a100b/rolling_restart.sh` — restart one replica at a time
  (waits for the old pid to die and a new pid + `probe=ok`); the manual
  loop used on 2026-08-29 restarted all three at once (≈2.5 min outage).
* `tools/a100b/patches/` — backport of #50729's barrier to 0.24.0's fused
  postprocess kernel. Analysis says the intra-chunk overlap it guards is
  not reachable with `COPY_BLOCK_SIZE=1024` and ≥16 KB per-token shifts on
  this model, so it is kept as belt-and-braces and is **not** applied to
  the running install.

## How to judge it, and the upgrade path

Judge at **72 h** (from 2026-08-29 20:26 UTC): the pre-mitigation rate
was ~1 IMA per ~6 h per replica; three replicas × 72 h with zero
`AcceleratorError`/`illegal memory` lines and zero non-OOM deaths is
"fixed in practice". OOM kills are reported separately by the watchdog
and do not count against MTP.

The nightly build is the real fix and is the upgrade path once the soak
is judged: `tools/a100b/vllm_main/launch_main.sh` documents the exact
venv/launch used. Before running it in production, install a torch that
matches the wheel's pin (the venv was installed `--no-deps` on the
system torch 2.11 and only works because the wheel uses the stable
libtorch ABI), re-run the harness, and switch the watchdog's interpreter.
`/workspace` has ~2.4 GB free; a from-source build is not possible there.
