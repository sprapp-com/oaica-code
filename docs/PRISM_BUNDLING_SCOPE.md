# Bundling the sprapp-prism engine (`prism_server`) in the oaica CLI

Scope study, 2026-08-30. All remote facts gathered read-only on `prism-a100b`
(shared prod box; GPU5 `:8080` untouched). Nothing here is speculation unless
labelled "open question".

## 1. Findings

### 1.1 Source tree

- `/workspace/prismx` — Rust crate `prismx` 0.1.0, edition 2021, standalone
  (empty `[workspace]` table; the parent `sprapp-prism` workspace is
  `prism-engine` + `prism-soul`). Other `/workspace/prism*` dirs are older
  CLI/server checkouts, bundles and logs (`ls -d /workspace/prism*`).
- Dependencies (`/workspace/prismx/Cargo.toml`): `memmap2`, `regex`, `rand`,
  `half`, `rayon`; optional `cudarc 0.19` (features `std,driver,nvrtc,`
  `dynamic-loading,cuda-13000`) and `tiny_http 0.12`. **No candle, no cust, no
  C++ FFI, no llama.cpp link.**
- Features: `gpu` (= cudarc), `serve` (= tiny_http, gates the HTTP server),
  `cuda-mma`, `cuda-marlin`, `cuda-gdn-triton`, `cuda-flash-attn[-embed]`,
  `ggml-ffi`. Defaults are **CPU-only and server-less** — the Cargo.toml
  comment states the free/paid split is enforced by the `serve` feature gate
  (HTTP code absent from the machine code, not runtime-disabled).
- Binaries: `prism_server` (needs `serve`), `pqm_export` (needs
  `gpu,cuda-marlin`), `tokenizer_export`, plus ~30 smoke/bench bins.
- `src/bin/prism_server.rs` is a single 9.5k-line / 478 KB file.
- Build recipe: `/workspace/prismx/mbuild.sh` —
  `cargo build --release --features gpu,serve,cuda-mma,cuda-marlin,cuda-gdn-triton`.
  It refuses to build with dirty tracked files (2026-08-10 incident noted in
  the script).
- `build.rs`: nvcc-compiles `cuda/prismx_mma.cu` and `cuda/prismx_marlin.cu` to
  **fatbins** with `-arch=$PRISMX_CUDA_ARCH` (default `sm_120`), embedded via
  `include_bytes!`. Fatbin holds SASS + PTX. Also embeds a pre-built Triton
  CUBIN `cuda/cache/gdn_decode_kernel.cubin` (sm_120 only).
  → **one arch per build today**; nvcc supports multi-`-gencode`, not yet used.
- `ggml-ffi` is OFF by default; no ggml linkage in the shipped binary.

### 1.2 The binary

- `/workspace/prism_server_deltafix_f5d269e7` = **24,983,048 bytes (25 MB)**.
- `ldd`: only `linux-vdso`, `libgcc_s`, `libm`, `libc`, `ld-linux`.
  **No `libcuda`, no `libnvrtc`, no CUDA runtime** — cudarc's
  `dynamic-loading` dlopens the driver at runtime. So bundling needs **no CUDA
  redistributables**; it needs only an NVIDIA driver on the user's box.
- Prod binary in use is `/workspace/prism_server_master_05c56f8` (md5
  `5318a6f462597666695adad949498ac3`, built sm_80 from
  `origin/master 05c56f8`), per `/workspace/launch_prod_deltafix.sh`.
- A **CPU lane exists and boots**: `/workspace/prism_cpu_baseline.log` ends
  `[prism_server] CPU lane; model loaded: 40 layers, vocab=248320, max_batch=8`
  / `listening on http://127.0.0.1:18080 (POST /v1/completions)`. No tok/s
  figure is recorded there.

### 1.3 Runtime contract

- **No `--help`.** `prism_server --help` panics at
  `src/bin/prism_server.rs:9905` (`open gguf: NotFound`) — argv[1] is
  positional model path, argv[2] is `host:port`.
- Prod invocation (`/workspace/launch_prod_deltafix.sh`):
  `prism_server /dev/shm/malay35b.pqm 0.0.0.0:8080 --n-cpu-moe 36
  --moe-cache-experts 2048 --max-batch 1 --max-seq 8192`.
- Flags found in source: `--cpu-moe --n-cpu-moe --moe-cache-experts
  --max-batch --max-seq --max-tokens --max-prefill-chunk --max-prefill-tokens`.
- Env (~150 `PRISMX_*` vars in `src/`). Prod-relevant: `PRISMX_PQM`,
  `PRISMX_PQM_STANDALONE`, `PRISMX_TOKENIZER`, `PRISMX_MIN_TEMPERATURE`,
  `PRISMX_REPEAT_PENALTY`, `PRISMX_STRIP_THINK_OPENAI`,
  `PRISMX_VISION_BRIDGE_SOCK`, `PRISMX_API_KEY`, `PRISMX_CUDA_ARCH`,
  `PRISMX_THREADS`, `PRISMX_PREFIX_CACHE`.
- **Auth already exists**: `PRISMX_API_KEY` = opt-in bearer token
  (`prism_server.rs:8657`); unset means no auth.
- Routes (`prism_server.rs:410-414`, `:8641`): `/v1/messages`,
  `/v1/chat/completions`, `/v1/completions`, `/v1/models`.
  `GET /health` returns plain text `prismx continuous-batching server. POST
  /v1/completions`; `GET /healthz` → 200.
- `GET /v1/models` on prod returns
  `{"object":"list","data":[{"id":"oaica-35b-malay-260827","object":"model",
  "owned_by":"prismx"}]}` — the id is the **`file_stem()` of the `.pqm` path**
  (`prism_server.rs:9291`), i.e. naming the file names the model.
- Streaming: real SSE, `chat.completion.chunk` + `choices[].delta.content`,
  `[DONE]` terminator (`:3656-3706`); a gate run recorded "SSE 32 chunks +
  [DONE]" (gpus.md, 2026-08-26).
- `usage` with `prompt_tokens` / `completion_tokens` / `total_tokens` is
  emitted on both streaming and non-streaming paths (`:3646`, `:3750`, `:3789`).
- `tool_calls`: supported, parsed out of Qwen-style `<tool_call>` /
  `<tool_calls>` XML (`parse_qwen_tool_calls`, `src/tool_grammar.rs`), with a
  large hardened wrapper-guard test suite.
- Chat template is **not read from the model**: it is hardcoded ChatML,
  looking up `<|im_start|>`/`<|im_end|>` in the vocab and bailing out if
  absent (`prism_server.rs:661`).

### 1.4 `.pqm` format

- `src/pqm_format.rs`: magic `PRISMXQM`, `PQM_FORMAT_VERSION = 1`, 256-byte
  header, 128-byte extent rows, ≤ 8192 extents, 4 KiB alignment,
  `PQM_PATH_ENV = "PRISMX_PQM"`. Format flags encode the quant lanes:
  `FLAG_MARLIN`, `FLAG_MARLIN_Q3K`, `FLAG_MARLIN_KU3`, `FLAG_MARLIN_Q3BP`,
  `FLAG_MARLIN_OWN_Q4K`, `FLAG_SHARED_U8Z`, `FLAG_LMHEAD_U8`,
  `FLAG_LMHEAD_ZPKILL`, `FLAG_DENSE_MARLIN{,2}`.
- A `.pqm` is a **dump of post-repack GPU-ready buffers** produced by running
  the normal GGUF load path once (`src/bin/pqm_export.rs`), so the pipeline is
  **GGUF → pqm**, and the exporter requires features `gpu,cuda-marlin` — i.e.
  **conversion needs an NVIDIA GPU**, it is not a pure host transform.
- Load is validated against `PqmArchFingerprint` (d_model, n_layer, recurrent/
  attn layer counts, vocab, plus `EXPERT_COUNT`/`EXPERT_FF`/`EXPERT_TOPK` and
  GDN/attn dims taken from **compile-time constants**, `GpuGdnCfg::QWEN35MOE`)
  and a `gguf_header_fingerprint`. Mismatch on either = hard load failure
  (`pqm_format.rs:974`).
  → **The engine is hardcoded to one architecture (Qwen-35B-A3B-class MoE +
  GDN).** It is not a general runtime; it runs the models it was compiled for.
- Tokenizer is a **sidecar**, not embedded: `.tok` is a flat binary of
  tokens/token_type/merges/bos/eos exported from a GGUF by
  `src/bin/tokenizer_export.rs`; no chat template inside.
- Sizes (`ls -la /dev/shm/*.pqm *.tok`): `kat_v25.pqm` 39,870,623,744 B
  (37.1 GiB), `oaica-35b-a3b-vision-260827.pqm` 40,763,801,600 B (38.0 GiB),
  `v2-ic-layout-a100b_v3b.pqm` 18,740,461,568 B (17.5 GiB); `.tok` files are
  8.94 MB.
- Vision needs a **second process**: `PRISMX_VISION_BRIDGE_SOCK` +
  `/workspace/prismx/vision_bridge/` (`docs/VISION_BRIDGE_PROTOCOL.md`).

### 1.5 Hardware envelope (from `/dev/shm/gpus.md`, GPU5 prod section)

- Config: `--n-cpu-moe 36 --moe-cache-experts 2048 --max-batch 1 --max-seq 8192`.
- **VRAM 10,854 MiB (10.85 GB)** for the 35B MoE (was 10,502 MiB at
  `--max-seq 1024`; +352 MiB measured for 8192).
- **Host RAM: 15 GiB page-locked** at boot, plus the ~38 GiB `.pqm` mapped
  from `/dev/shm`.
- **Boot ≈ 108 s** (40 blocks + the 15 GiB page-locked alloc); full
  kill→serving restart measured at 113 s.
- Decode, batch-1, same-GPU A/B (`/workspace/ab_gpu6/`, 2026-08-26):
  **57 tok/s** (binary a4b0cac5) vs **35 tok/s** (f5d269e7, 34 with prod env)
  — a known ~39% regression from the offload overflow-scratch change.
- No CPU-only or small-GPU throughput figure exists in the notes.

### 1.6 Licensing

- `grep -ri "license\|entitle" /workspace/prismx/src/` → **0 matches**. No
  `LICENSE` or `NOTICE` file in the crate root. The only access control is
  `PRISMX_API_KEY`. The free/paid split is expressed purely as the `serve`
  Cargo feature.

### 1.7 What oaica has today

- `cmd/oaica_pull_serve.go`: `oaica pull` → `~/.oaica/models/<m>.gguf`
  (`oaicaModelPath`, hardcoded `.gguf` suffix, size-based skip);
  `ServeHandler` finds `llama-server` via `OAICA_LLAMA_SERVER` or `$PATH`
  (`findLlamaServer:588` — **oaica ships no engine at all today**, it tells the
  user to build llama.cpp), picks a free internal port, spawns it with
  `-ngl 999 -c <ctx> -t <threads> -fa on -ctk/-ctv q8_0 -cmoe|-ncmoe`, runs
  `launch.RunNormalizingProxyOn` in front of it, and registers
  `~/.oaica/local_servers.json` via `oaicaRegisterLocalServer`, unregistering
  on every exit path.
- `tools/gateway/pull.go`: `/v1/manifest` entries carry `size_bytes`,
  `sha256`, `license_required`; license keys are stored SHA-256 hashed
  (`sha256.Sum256([]byte(key))`).
- `cmd/launch/entitlement.go`: an **inert** hook — `entitlementCheckEnabled`
  false by default (env `OAICA_ENTITLEMENT_CHECK=1`), `entitlementCheckFn`
  defaults to `allowAllEntitlementCheck`, called in
  `RunAnthropicOpenAIProxyRoutes` after routing, before upstream.
- `scripts/build_oaica.sh`: `CGO_ENABLED=0`, 5 targets (linux amd64/arm64,
  darwin amd64/arm64, windows amd64), archive layout `bin/oaica` only, ~5 MB.

## 2. Gap analysis vs Ollama's runner model

| Ollama capability | oaica today | prism gap |
|---|---|---|
| Single install ships the runner | ✗ — user must build llama.cpp | shipping *any* engine is new ground |
| Runtime GPU backend selection (CUDA/ROCm/CPU variants dlopen'd) | ✗ | prism already dlopens libcuda (good), but kernels are baked per-`sm_` at build time and the Triton CUBIN is sm_120-only |
| Subprocess runner per model, health-probed | ✓ `ServeHandler` + normalizing proxy + `local_servers.json` | needs a second engine branch; prism health is `GET /health` (plain text) / `/healthz` |
| Content-addressed pull with sha256 + size | ✓ `tools/gateway/pull.go` | needs a `format` field, a `.tok` sidecar, and 38 GB single-blob download UX (resume, disk check) |
| Modelfile / templates / params | ✗ (oaica has `template/`, unused here) | prism hardcodes ChatML; sampling comes from `PRISMX_*` env |
| Multi-architecture model support | n/a (llama.cpp is generic) | **prism is one architecture, compile-time constant** |

Already there: subprocess supervision, port allocation, proxy normalization,
registry, manifest + hashed license keys, entitlement hook. Missing: engine
artifact distribution, GPU/driver detection, a non-`.gguf` model path, sidecar
files, and a real entitlement implementation.

## 3. Proposed design

### (a) Artifact layout — **separate `oaica engine pull prism`, not bundled**

The engine is only 25 MB, which argues for bundling; everything else argues
against it. Bundling into all five archives adds 25 MB × 5 to a repo that
currently tracks its archives under `site/download/`, and 4 of the 5 targets
(darwin ×2, windows, linux-arm64) can never run it. Recommendation:

```
~/.oaica/engines/prism/<version>/prism_server      # downloaded on demand
~/.oaica/engines/prism/<version>/VERSION.txt       # sha256 + build arch list
```

`oaica serve <m> --engine prism` downloads it on first use (manifest entry,
sha256-verified, `license_required` respected), exactly like a model pull.
`OAICA_PRISM_SERVER=/path` overrides, mirroring `OAICA_LLAMA_SERVER`. No CUDA
libraries ship: `ldd` proves the binary needs only libc — the NVIDIA driver
supplies `libcuda.so.1`/`libnvrtc`.

### (b) `oaica serve <model> --engine prism`

Reuse `ServeHandler` wholesale; branch only on engine:

1. Resolve `~/.oaica/models/<m>.pqm` + `<m>.tok` (see (c)); model id served
   will be the file stem, so name the file with the catalog name.
2. Detect GPU: `nvidia-smi --query-gpu=name,compute_cap,memory.total
   --format=csv,noheader`. Refuse if no CUDA device or compute cap outside the
   built fat-binary set; refuse if free VRAM < ~12 GB for a 35B-class `.pqm`.
3. Flags: `--max-batch 1 --max-seq <ctx, default 8192>`; `--n-cpu-moe` derived
   from free VRAM (prod value 36 of 40 layers at 10.85 GB); `--moe-cache-experts`
   from free host RAM (prod 2048 → 15 GiB page-locked).
4. Env: `PRISMX_PQM_STANDALONE=1 PRISMX_PQM=… PRISMX_TOKENIZER=…`,
   `PRISMX_STRIP_THINK_OPENAI=1`, and `PRISMX_API_KEY` when `--api-key` given
   (the proxy can then stop being the only auth).
5. Health probe: poll `GET /healthz` (200) then `GET /v1/models` for the
   expected id, with a **timeout ≥ 180 s** — boot is ~108 s, far longer than
   llama-server, so the current readiness expectations must be relaxed.
6. Same `RunNormalizingProxyOn` front end and the same
   `oaicaRegisterLocalServer` / unregister-on-exit lifecycle. Whether the
   normalizing proxy is still needed (prism serves `/v1/messages` natively) is
   an open question — keep it initially for uniform auth and model naming.

### (c) Catalog / manifest changes

Add to the `/v1/manifest` entry struct in `tools/gateway/pull.go`:

- `format`: `"gguf"` (default when absent) | `"pqm"`.
- `engine`: `"llama"` | `"prism"`.
- `files[]`: `{name, size_bytes, sha256}` so the `.tok` sidecar (8.9 MB) is a
  first-class second file rather than a special case.
- `min_vram_bytes`, `min_ram_bytes`, `pqm_format_version` (1), and the arch
  fingerprint fields, so the CLI can refuse a mismatch before a 38 GB download
  instead of after a 108 s boot.
- `license_required: true` for every `.pqm` (Pro tier).

`oaicaModelPath` must stop hardcoding `.gguf` and take the format from the
manifest (`cmd/oaica_pull_serve.go:111`).

### (d) Installer / build changes

- `scripts/build_oaica.sh`: unchanged for `bin/oaica`. Add a separate
  `scripts/build_prism_engine.sh` run **on a CUDA box** (cross-compiling
  fatbins needs nvcc), producing `prism-linux-amd64-cuda.tar.zst` +
  `VERSION.txt`, built with `--features gpu,serve,cuda-mma,cuda-marlin` and a
  multi-`-gencode` nvcc line for `sm_80,86,89,90`.
- `scripts/install.sh` / `install.ps1`: unchanged — the engine is not in the
  CLI archive.
- **Windows / macOS: not supported initially.** macOS has no CUDA at all and
  the engine has no Metal backend; Windows would need an MSVC toolchain build
  plus a driver-detection path, for a runtime whose only shipping model is
  38 GB. linux-arm64 is also out: the embedded Triton CUBIN and the
  `-arch=sm_*` fatbins are built per-arch and the only aarch64 target
  mentioned in the crate is a DGX Spark GB10 (sm_121), not a user platform.

### (e) Licensing hook

`cmd/launch/entitlement.go` is the right seam and is already wired into
`RunAnthropicOpenAIProxyRoutes`. For Pro:

- Ship an ed25519 public key in the binary; `oaica license activate <key>`
  stores an **offline signed entitlement** (model set, expiry, machine hint)
  in `~/.oaica/license.json`.
- Replace `entitlementCheckFn` with a verifier that checks signature + expiry
  + requested model against the entitlement, and set
  `entitlementCheckEnabled` true when a `.pqm` engine is in play.
- **Grace period**: accept an expired entitlement for N days (14 suggested)
  with a warning, so a lapsed renewal never bricks a running server.
- Gate the *pull* too (`license_required` + hashed key already exist in
  `tools/gateway/pull.go`), so the 38 GB blob is not served unlicensed.
- Note: prism itself has zero license code (§1.6); all enforcement is oaica's.

## 4. Work breakdown

| # | Item | Size | Eng-days |
|---|---|---|---|
| 1 | Multi-arch fat build of `prism_server` (nvcc `-gencode` for sm_80/86/89/90; Triton CUBIN currently sm_120-only — needs regeneration or runtime opt-out) | L | 4–7 |
| 2 | `scripts/build_prism_engine.sh` + CUDA build host / CI runner | M | 2–3 |
| 3 | `oaica engine pull prism` (download, sha256, versioned dir, `OAICA_PRISM_SERVER`) | M | 2–3 |
| 4 | Manifest `format`/`engine`/`files[]`/VRAM fields + gateway + `oaicaModelPath` de-hardcoding | M | 2–3 |
| 5 | Multi-file pull with resume + free-disk precheck (38 GB) | M | 2–3 |
| 6 | `--engine prism` serve path: GPU detect, flag derivation, env, 180 s health probe, registration | M | 3–4 |
| 7 | Entitlement: ed25519 offline key, `oaica license activate`, grace period, pull gating | M | 3–4 |
| 8 | Model publishing: host a 38 GB `.pqm` + `.tok` on the CDN, decide naming (file stem = model id) | M | 2–3 |
| 9 | Docs, `SELF_HOST.md`, error messages, e2e test on a real consumer GPU | M | 2–3 |
| 10 | Vision bridge (second process) — **out of scope for v1** | L | — |

**Total (1–9): ~22–33 engineer-days.**

### Risks

- **CUDA arch matrix.** One fatbin arch per build today (`PRISMX_CUDA_ARCH`,
  default sm_120); prod runs sm_80. The embedded Triton CUBIN
  (`cuda/cache/gdn_decode_kernel.cubin`) is sm_120 + Triton 3.6.0 — a fat
  binary for consumer cards is unproven work, not a flag change.
- **Driver minimum.** cudarc is pinned to `cuda-13000` bindings and dlopens
  `libnvrtc.so.13`; unclear what the oldest working driver is. Users on
  535-era drivers may fail at `cuInit`.
- **CPU-only performance is unknown.** The CPU lane boots
  (`prism_cpu_baseline.log`) but no tok/s figure exists anywhere in
  `/workspace`. Cannot be promised.
- **Decode throughput is in flux**: 57 → 35 tok/s regression between two
  builds a day apart, unresolved.
- **Model size.** 38 GiB per `.pqm` vs 16–20 GB for the equivalent GGUF. CDN
  egress, resumable download, and disk precheck are mandatory, not nice-to-have.
- **`.pqm` stability.** `PQM_FORMAT_VERSION = 1` with a hard-fail arch
  fingerprint built from **compile-time constants** — an engine rebuild that
  touches `EXPERT_FF`/GDN dims invalidates every published `.pqm`. Engine and
  model versions must be pinned to each other in the manifest.
- **Single architecture.** Every additional model family is an engine code
  change, not a new download.
- **Boot time.** 108 s cold start is a UX cliff vs llama-server's seconds.
- **No `--help`, positional argv** — the CLI contract is undocumented and
  will drift; pin the engine version the CLI knows how to invoke.

### Minimum viable slice

**linux-amd64, CUDA fat binary sm_80/86/89/90, one `.pqm` model,
`license_required=true`, downloaded engine (not bundled), no vision, no CPU
fallback.** Items 1–9 minus the resume/disk polish and with a single hardcoded
flag profile (`--max-batch 1 --max-seq 8192 --n-cpu-moe 36
--moe-cache-experts 2048`, matching prod) instead of derived flags:

**≈ 15–20 engineer-days**, of which the multi-arch build (item 1) is the only
item with real technical uncertainty and could alone slip a week.

## 5. Open questions for the owner

1. Which GPUs must v1 support? sm_86 (3090) / sm_89 (4090) / sm_80 (A100) —
   and is a 12 GB card the floor, given prod measures 10.85 GB VRAM?
2. Can the sm_120 Triton CUBIN (`cuda/cache/gdn_decode_kernel.cubin`) be
   regenerated per-arch, or is `PRISMX_GDN_TRITON_FFI` off in prod anyway and
   the lane droppable for the shipped build?
3. Is the CPU-only lane a product promise? If so it needs a tok/s measurement
   before anything is announced — none exists.
4. What is the oldest NVIDIA driver we support, given the `cuda-13000`
   cudarc bindings and the `libnvrtc.so.13` dlopen?
5. Do we ship a 38 GiB `.pqm`, or invest in a smaller export first? This is
   the single biggest UX risk vs the GGUF path.
6. What licence does `prismx` ship under? There is no `LICENSE`/`NOTICE` file
   in the crate, and the free/paid split currently exists only as the `serve`
   Cargo feature.
