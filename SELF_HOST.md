# Running `kat-coder-i-compact` locally with OAICA

Self-host our 35B-A3B MoE coding model on your own machine. No cloud
calls, works offline, your prompts never leave the box.

Every number and command below was verified on real hardware — an RTX 4060
Laptop (8 GB VRAM) with a Ryzen 5 7640HS (6 cores / 12 threads) and 61 GB
RAM. Where something is a projection rather than a measurement, it says so.

---

## 1. What you need

| | Minimum | Why |
|---|---|---|
| **VRAM** | 8 GB | The model is 16.5 GB but only attention + a few expert layers live on the GPU; the rest streams from system RAM. 8 GB is genuinely enough — measured 2.1–4.6 GB actually used. |
| **System RAM** | 32 GB (48 GB+ comfortable) | This is the real constraint, not VRAM. The MoE experts (~15.4 GB) sit here. Measured: freeing RAM roughly **doubled** throughput. |
| **Disk** | ~17 GB free | For the model file. |
| **GPU** | Any CUDA card | Also runs CPU-only, just slower. |

`llama-server` must be available — OAICA spawns it, it isn't bundled.

## 2. Install the CLI

```bash
curl -fsSL https://oaica.com/install.sh | bash
oaica --version      # need 0.3.0-oaica or newer
```

## 3. Build `llama-server`

Adjust `CMAKE_CUDA_ARCHITECTURES` for your GPU
(89 = RTX 40-series, 86 = RTX 30-series, 80 = A100, 90 = H100):

```bash
git clone https://github.com/ggml-org/llama.cpp && cd llama.cpp
cmake -B build -DGGML_CUDA=ON -DCMAKE_CUDA_ARCHITECTURES=89 -DCMAKE_BUILD_TYPE=Release
cmake --build build --config Release -j$(nproc) --target llama-server
export OAICA_LLAMA_SERVER=$PWD/build/bin/llama-server
```

## 4. Get a licence key

`kat-coder-i-compact` needs one (the small demo models don't). Ask us for
a key, then:

```bash
mkdir -p ~/.oaica && echo '<your-key>' > ~/.oaica/license_key
```

## 5. Download the weights

```bash
# optional: put the 16.5 GB somewhere other than ~/.oaica/models
export OAICA_MODELS_DIR=/big/disk/oaica-models

oaica pull kat-coder-i-compact
```

Downloads directly from a public HuggingFace mirror and decrypts as it
streams. The weights are encrypted at rest, so the public mirror does not
expose them — the key only reaches you after your licence checks out.
Verified byte-identical (matching MD5) against the source on two separate
machines.

If you have a HuggingFace account, `export HF_TOKEN=hf_...` first —
unauthenticated downloads are rate-limited and slower.

## 6. Serve it

```bash
oaica serve kat-coder-i-compact --ncmoe 34 --threads 6
```

Leave it running. It prints the port it bound.

### Tuning `--ncmoe` and `--threads` (this matters a lot)

**`--threads`: use your PHYSICAL core count, not logical.** Measured on a
6-core/12-thread CPU:

| `--threads` | tok/s |
|---|---|
| 6 (physical) | **51.7** |
| 8 | 51.2 |
| 12 (all SMT) | 38.7 |

SMT *hurts*. This workload is memory-bandwidth-bound, so hyperthreads just
contend for the same cache and bandwidth. OAICA already defaults to
`NumCPU()/2` for this reason.

**`--ncmoe N`: keep the first N layers' experts on CPU.** The model has 40
layers. The relationship is **not monotonic** — measured:

| `--ncmoe` | VRAM | tok/s |
|---|---|---|
| 40 (all on CPU, the `-cmoe` default) | 2.1 GB | 4.85 |
| 36 | 3.8 GB | 4.83 |
| **34** | **4.6 GB** | **6.05** ← best |
| 30 | 5.9 GB | 4.76 |
| 20 | — | out of memory |

More GPU-resident layers is *not* better. Every CPU↔GPU layer boundary
costs a transfer per token, so a few contiguous GPU layers beats moderate
mixing. **Re-sweep this for your own hardware** — 34 is right for a 40-layer
model on 8 GB, not a universal constant.

(Those figures were taken under memory pressure. On a quiet box the same
config measured **51.7 tok/s**, so treat the table as *relative* ordering,
not absolute speed.)

**Big context needs full CPU offload.** `--ctx-size 65536` plus
`--ncmoe 34` will OOM on 8 GB — KV cache competes with the GPU-resident
expert layers. For long contexts drop `--ncmoe` (full `-cmoe`) and trade
some tok/s for headroom.

## 7. Use it

In another shell:

```bash
oaica launch claude
```

Pick **`kat-coder-i-compact:local`** under the **Local** section. No
`OAICA_HOST`, no env vars — the CLI discovers the running server itself.

The bare name `kat-coder-i-compact` (no `:local`) means the **cloud**
model. Both appear in the picker; the tag is what disambiguates them.

Headless:

```bash
oaica launch claude --model kat-coder-i-compact:local -- -p "your prompt"
```

---

## Troubleshooting

**No "Local" section in the picker.** The server has to be on the *same
machine* as the CLI. Check it's alive: `curl 127.0.0.1:<port>/health`.
Also note: if `OAICA_HOST` is set, local discovery is deliberately skipped
(you've pinned to one host on purpose).

**`llama-server binary not found on PATH`.** Set `OAICA_LLAMA_SERVER` to
its absolute path (step 3).

**Pull dies with "no space left on device".** Set `OAICA_MODELS_DIR` to a
bigger disk *before* pulling. Requires 0.3.0-oaica or newer — older builds
ignored it.

**`exceeds the available context size`.** Raise `--ctx-size`. Claude Code's
system prompt alone is 10–70K tokens. Note `-np N` slots *divide* the total
context, so `-c 16384 -np 2` gives only 8192 per request.

**Painfully slow (single-digit tok/s).** Almost always RAM contention, not
the GPU. Check `free -h` — you want 20 GB+ *available*. Closing a browser
took one measurement from 2.4 to 4.7 tok/s. `vmstat 1` during generation:
high `wa` and large `bi` means it's re-reading expert weights from disk
because they won't stay cached.
