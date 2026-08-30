# a100b — reproducible serving stack

Everything needed to rebuild the oaica-35b-a3b-vision (renamed from kat-awq,
2026-08-28) public-inference stack on the a100b box (vast.ai, 8x A100 80GB,
no systemd, 16G root overlay that is 100% full). Before this directory
existed, the stack lived only as ad-hoc files under `/root/` and `/tmp/` on
the box; when `/dev/shm/kat_awq` was deleted by another session's cleanup
the watchdog crash-looped ~49k times and the only fix was a from-memory
re-download that silently lost the chat-template patch.

## Layout on the box

All state lives under `/workspace/` (the only writable, exec-capable disk
with headroom). Never write to `/root/` or `/tmp/` -- `cp`/`install` there
fails with ENOSPC on the full overlay.

| Path | What | Source in repo |
|---|---|---|
| `/dev/shm/kat_awq/` | model weights (volatile tmpfs, shared) | HF `Ar4ikov/KAT-Coder-V2.5-Dev-AWQ-W4A16-ASYM` @ `446ea8c6` |
| `/workspace/kat_awq.tokenizer_config.json` | chat_template patch | `kat_awq.tokenizer_config.json` |
| `/workspace/vllm_awq_watchdog.sh` | fleet watchdog (preflight + restore + backoff + stall-kill) | `vllm_awq_watchdog.sh` |
| `/workspace/oaicalb-linux-amd64` + `oaicalb.json` | LB with chat-aware probe (formerly katlb) | `../oaicalb/`, `oaicalb.json` |
| `/workspace/gatekeeper` + `/workspace/gatekeeper.json` (0600, plaintext keys) | per-key concurrency tiers | `../gatekeeper/`, `gatekeeper.json` |
| `/workspace/oaica-gateway` + `oaica-gateway.json` | public OpenAI gateway (metering, hashed keys) | `../gateway/`, `gateway.json` |
| `/workspace/oaica-gateway-ledger.jsonl` | billing ledger (append-only) | -- |
| `/workspace/*-swap.sh` | detached restart scripts (see gotchas) | -- |

Durable off-box copy of the weights: `lenovo.samwong.com:/mnt/ext4/models/kat_awq/`
(rsync mirror, ~21 GB). Restoring from it is faster than HF and keeps the
tokenizer patch.

## meterhub (central metering + entitlement, added 2026-08-28)

`/workspace/meterhub-linux-amd64` (source: `tools/meterhub/`, pure-Go
sqlite via `modernc.org/sqlite`, no cgo), config `/workspace/meterhub.json`,
db `/workspace/meterhub.db`, listens `:8095` (loopback only — not exposed
through the cloudflare tunnel). The gateway (`oaica-gateway.json`'s
`meterhub_addr`/`meterhub_token`) fire-and-forget reports every completion
to `POST /ingest` after writing its own local ledger row; meterhub is pure
aggregation and is never on the request-critical path — an unreachable
meterhub cannot slow or fail a real completion.

Endpoints (all require `Authorization: Bearer <plaintext report token>`,
sha256 of which is in `meterhub.json`'s `report_tokens`):
- `GET /usage`, `GET /usage/summary` — billing/usage queries
- `GET /subscribers/get?key=<label>` — the fast check the gateway's
  entitlement cache polls (TTL 60s by default); returns `{"status":"unknown"}`
  (200, not 404) for a key with no row — fail-open/fail-closed policy lives
  entirely in gateway config (`entitlement_fail_open`), not in HTTP status
- `POST /subscribers/set` — manual control surface: `{"key_label","status"
  (active|past_due|canceled|suspended),"plan","note"}`. This is how to
  block/unblock a key TODAY, no Stripe account needed.
- `GET /subscribers/list?status=X` — audit view
- `POST /subscribers/webhook` — Stripe-event-shaped receiver, NOT yet safe
  to point a real Stripe webhook at: auth is still the same bearer report
  token, not Stripe's per-endpoint signing-secret verification. Swap that
  in before connecting a live Stripe account.

Gateway-side entitlement enforcement (`entitlement_enabled` in
`oaica-gateway.json`, currently **unset/false** — capability is deployed
but not turned on) blocks a key with a non-`active`/`past_due` status with
`403 subscription_required` from `completionHandler`. Restart either
service with the box's swap-script pattern (find listener pid by `ss -ltnp`,
never `pgrep -f`): `/workspace/gw-swap.sh` for the gateway,
`/workspace/meterhub-swap.sh` for meterhub.

## Rebuild from nothing

```bash
# 1. weights (watchdog does this itself on first tick if missing)
python3 -c "from huggingface_hub import snapshot_download; snapshot_download(
  repo_id='Ar4ikov/KAT-Coder-V2.5-Dev-AWQ-W4A16-ASYM',
  revision='446ea8c64909baff9a94c627b25765915b2c211d',
  local_dir='/dev/shm/kat_awq', allow_patterns=['*.safetensors','*.json','*.txt'])"
install -m 0644 kat_awq.tokenizer_config.json /dev/shm/kat_awq/tokenizer_config.json

# 2. binaries (build on the laptop, scp to /workspace)
(cd ../oaicalb && GOOS=linux GOARCH=amd64 go build -o oaicalb-linux-amd64 main.go)
(cd ../gateway && GOOS=linux GOARCH=amd64 go build -o oaica-gateway main.go)

# 3. configs: fill secrets, then
#    gateway.json  -> api_keys[].sha256 = printf '%s' "$KEY" | sha256sum
#    gatekeeper.json -> keys: plaintext (gatekeeper does not hash yet)
#    the gateway's upstream credential is one of gatekeeper's keys on the
#    "openrouter" tier, passed as OAICA_GATEWAY_UPSTREAM_KEY in gw-swap.sh

# 4. start (order matters: replicas -> oaicalb -> gatekeeper -> gateway)
nohup /workspace/vllm_awq_watchdog.sh > /workspace/vllm_awq_watchdog.out 2>&1 &
nohup /workspace/oaicalb-linux-amd64 -config /workspace/oaicalb.json > /workspace/oaicalb.log 2>&1 &
nohup /workspace/gatekeeper -config /workspace/gatekeeper.json > /workspace/gatekeeper.log 2>&1 &
OAICA_GATEWAY_UPSTREAM_KEY=<gatekeeper openrouter key> \
  nohup /workspace/oaica-gateway --config /workspace/oaica-gateway.json > /workspace/oaica-gateway.log 2>&1 &
```

## Request path

```
Cloudflare (api.oaica.com) -> cloudflared ON a100b (/workspace/cf/run.sh) -> :8081
  :8081 oaica-gateway   auth (sha256 key), model rewrite, usage injection, ledger
  :30098 gatekeeper     per-key concurrency ("openrouter" tier = 32)
  :30099 oaicalb        leastconn across replicas, chat-aware health probe (:8091 session-hash)
  :30106 vLLM oaica-35b-a3b-vision  GPU0    :30108 vLLM oaica-35b-a3b-vision  GPU1
```

GPU7 (`:30107`, `oaica-nemotron-30b-a3b`) sits behind its own oaicalb
(`:30120`) and IS in the gateway (`oaica-gateway.json` model entry with a
per-model `upstream_addr`), but bypasses gatekeeper on purpose — the gateway
already authenticates the caller and `:30120` binds loopback only. See
"Watchdog behaviour" below for the replica, and "Second model pool" for the
gateway/LB wiring.

The tunnel is the `oaica-api` tunnel in the Cloudflare account that owns
oaica.com (unisqu, `125f3856…`), run directly on the box with a tunnel token
(`/workspace/cf/token`, 0600) -- no .91 hop. `oaica.samwong.com` (samwong
account, via .91) still resolves but is no longer the published URL.

## Health semantics (why the probe is a chat call)

`GET /v1/models` on vLLM returns 200 while every completion 400s (seen when
the tokenizer had no `chat_template`) -- and, separately (2026-08-28), while
a wedged scheduler queues real requests behind ~10 concurrent long-context
ones with generation throughput near zero. oaicalb's probe is therefore a
real 1-token `POST /v1/chat/completions` for the served model
(`probe_model` in `oaicalb.json`). A replica is UP only if a customer
request would succeed. The gateway's `/health` in turn asks gatekeeper, so
an external monitor on `https://api.oaica.com/health` sees the whole chain.

Stall detection (`stall_sec`, deployed as 300 with `stall_min_inflight` 2):
oaicalb tracks every in-flight request per backend. If at least
`stall_min_inflight` of them are older than `stall_sec` AND the latest probe
failed or timed out, the replica is marked DOWN on that single failure --
not the usual two in a row -- and gets no new requests until the probe
passes twice again. A 200 probe with an empty or non-completion body also
counts as a failure. What this does NOT catch: a replica whose 1-token probe
keeps passing while real generations sit forever is never marked DOWN by
this rule -- the probe remains the gate; stall only lowers the trip count
from two failures to one when old requests are also piling up. Old requests
alone (a long legitimate generation with a passing probe) never mark a
replica DOWN. The deployed 300 s / 2 defaults are deliberately conservative:
under saturation, streams > 120 s are routine and a single 10 s probe timeout
must not shrink capacity at peak. `stall_sec: -1` disables the check.
`:8092/status` shows `oldest_inflight_sec=` and `probe=ok|fail` per backend.

## Watchdog behaviour

`vllm_awq_watchdog.sh` per replica in `REPLICAS` ("gpu:port"):

1. if the port is listening, run the **stall probe**: a real 1-token chat
   completion (`STALL_PROBE_MODEL`, default `oaica-35b-a3b-vision`), 15 s
   timeout. Success resets the failure streak and does nothing else. 3
   consecutive failures (`STALL_FAIL_THRESHOLD`) -- while the port is still
   LISTENING, so the crash path below never sees it -- triggers
   `force_kill_replica`: SIGTERM, 5 s grace, SIGKILL if still alive, then
   hunts and SIGKILLs any `VLLM::EngineCore` process reparented to PID 1
   (vLLM's V1 engine runs generation in a separate process that outlives
   `api_server`'s death and keeps holding GPU memory -- exactly what turned
   a 2026-08-28 stall into hours of stray-process confusion before this was
   automated). The replica is then relaunched on the next tick.
2. else **preflight**: weights present (re-download from the pinned revision
   if not) and tokenizer patch sha256-matches (re-apply if not);
3. launch; if the process dies within 60 s, double the retry delay (cap
   10 min) and write to `/workspace/vllm_awq_watchdog.ALERT`.

A stall-kill also writes to `/workspace/vllm_awq_watchdog.ALERT`. Poll the
ALERT file from a monitor; a restore, a backoff, or a stall-kill is the
signal that something on the box (or under real load) changed under us.

**2026-08-28 recurring stall-kills, root-caused:** both replicas were
stall-killed and relaunched repeatedly, every 2-4 minutes, with ZERO
external client traffic in the gateway ledger during the window (confirmed
via `oaica-gateway-ledger.jsonl` — request logging stopped at 20:13, crashes
kept happening at 20:39/20:40/20:43/20:44/20:46/20:48) and no established
connections to oaicalb's public ports. Self-inflicted, not load-driven.
Cause: vLLM 0.24.0 auto-enables async scheduling by default; combined with
the experimental Mamba `align`-mode caching this model's architecture
requires when `--enable-prefix-caching` is on (`Qwen3_5MoeForConditionalGeneration`
does not support the non-experimental `all` mode — vLLM falls back to
`align` unconditionally, see `vllm/model_executor/models/config.py`'s
`MambaModelConfig`), the combination hangs the engine internally within
minutes of a clean boot. Fix: added `--no-async-scheduling` to `launch()`'s
vLLM command — prefix caching (and its `align`-mode Mamba caching) stays on
unchanged, only the async scheduler is disabled. Verified stable afterward:
zero new ALERT entries and both replicas served real 200s well past the
previous 2-4 min crash window. `--no-async-scheduling` cannot be removed
without reintroducing this, and `--enable-prefix-caching` must NOT be
disabled to "fix" this — that was explicitly ruled out; the actual fix is
the async-scheduling flag.

`REPLICAS` defaults to `0:30106 1:30108` (GPU0 + GPU1). GPU2 was released
back to other tenants 2026-08-29 22:42Z. GPU5 is held by the
malay35b-offload `prism_server` plus another session's 52 GB job, so an
oaica-35b-a3b-vision replica OOMs at startup there. A `booting()` guard
skips a port whose api_server exists but is not yet listening (vLLM takes
~100 s to load), so a slow start is not treated as a crash.

## Second model pool: oaica-nemotron-30b-a3b (GPU7)

GPU7 was swapped 2026-08-29 from a third oaica-35b-a3b-vision replica to a
new, unrelated pool: `oaica-nemotron-30b-a3b`
(NVIDIA-Nemotron-3.5-Lightning-30B-A3B, W4A16 compressed-tensors, HF
`useful-quants/NVIDIA-Nemotron-3.5-Lightning-30B-A3B-W4A16`, arch
`NemotronHForCausalLM`, hybrid Mamba/MoE, ~3B active, native
`max_position_embeddings` 262144). One vLLM 0.24.0 replica on GPU7 `:30107`
(GPU7 is shared with a ~6 GB co-tenant; the replica uses ~67 GB at
`--gpu-memory-utilization 0.82`, KV cache 1.63M tokens).

Launch flags: `--quantization compressed-tensors --dtype bfloat16
--max-model-len 262144 --max-num-batched-tokens 8192 --max-num-seqs 8
--gpu-memory-utilization 0.82 --enforce-eager --enable-prefix-caching
--enable-chunked-prefill --enable-auto-tool-choice --tool-call-parser
qwen3_xml --reasoning-parser nemotron_v3 --enable-prompt-tokens-details
--trust-remote-code`. Why `qwen3_xml`/`nemotron_v3`: the model's chat
template emits `<think>…</think>` reasoning (thinking ON by default via
`enable_thinking=True`) and Qwen3-Coder-style XML tool calls
(`<tool_call><function=name><parameter=key>value</parameter></function></tool_call>`);
with the `hermes` parser, tool_calls came back null and reasoning leaked
into content — verified fixed with this pair (content clean, reasoning in
`reasoning_content`, tool_calls parsed). Thinking is on by default, so a
short `max_tokens` can be consumed entirely by reasoning (a 120-token
stream test produced 0 content chars, 366 reasoning chars); clients can
pass `chat_template_kwargs: {"enable_thinking": false}` to suppress it.

`--enforce-eager` is kept because torch.compile's cache needs an
exec-capable filesystem with room and `/workspace` is 96% full (2.2 GB
free) / the root overlay has 528 MB free — the vendor's example says not
to use enforce-eager; single-stream decode measured ~18 tok/s with it on.
Fix: free `/workspace` space, then drop the flag.

Supervised by `/workspace/nemotron_watchdog.sh` (source
`tools/a100b/nemotron_watchdog.sh`, log `/workspace/nemotron_watchdog.log`,
replica log `/workspace/vllm_nemotron_gpu7.log`) — a sibling of
`vllm_awq_watchdog.sh` with the same OOM attribution / orphan sweep /
backoff. Since the watchdog persists crash-backoff state, killing the
replica right after restarting its watchdog delays the relaunch (observed
7 min) — to change launch flags: edit the script, kill the watchdog AND
the replica, then start the watchdog.

**Its own load balancer**, same reasoning as the main fleet's chat-aware
probe: `oaicalb` on `:30120` (leastconn) / `:8191` (session-hash) /
`:8192` (status), config `/workspace/nemotron-oaicalb.json` (repo copy
`tools/a100b/nemotron-oaicalb.json`, `probe_model
oaica-nemotron-30b-a3b`), same binary as the main fleet's — see oaicalb's
own file header comment: "any future multi-replica model... gets the same
leastconn + session-hash load balancing by running a second oaicalb with
its own `-config`, not by touching this code". A separate LB means a hung
Nemotron replica can never be picked for an oaica-35b request and vice
versa. Reports to meterhub the same way the main fleet does. Now
supervised by `stack_watchdog.sh` (`listening 30120 || start_nemotron_lb`).
Launch: `nohup /workspace/oaicalb-linux-amd64 -config
/workspace/nemotron-oaicalb.json > /workspace/nemotron-oaicalb.log 2>&1 &`.

**In the gateway** via a per-model `upstream_addr`
(`http://127.0.0.1:30120`) pointing at its own LB instead of the shared
`upstream_addr` gatekeeper path — see "Layout on the box" / `gateway.json`
for the model entry shape. Verified end-to-end: `POST
https://api.oaica.com/v1/chat/completions` with model
`oaica-nemotron-30b-a3b` → 200, ledger row carries model/upstream_model,
gateway log "2 models … (2 distinct upstreams)".

## Control-plane supervisor + reboot

`stack_watchdog.sh` keeps oaicalb (:30099, formerly katlb) and the gateway
(:8081) alive, detecting each by LISTENING PORT (never `pgrep -f`). The
gateway's upstream credential is read from `/workspace/gateway_upstream.key`
(0600). This replaced the legacy `/root/katlb_watchdog.sh` loop, which
relaunched the OLD binary from `/root` and fought the v2 process
("bind: address already in use" in /tmp/katlb.log -- the log itself is now
named oaicalb.log, kept as-written here for the historical incident).

gatekeeper (:30098) runs from `/workspace` under `stack_watchdog.sh` since
2026-08-26; the legacy `/root/gatekeeper_watchdog.sh` loops were stopped and
the script renamed `.disabled`. The gateway's upstream key (a gatekeeper
key on the `openrouter` tier) was rotated the same day; rotate again with:
add the new key to `/workspace/gatekeeper.json` AND `/root/gatekeeper.json`
(kept in sync) -> `kill -HUP` the :30098 listener -> write it to
`/workspace/gateway_upstream.key` -> `/workspace/gw-swap.sh` -> remove the
old key -> HUP again. `cloudflared` (the
`oaica-api` tunnel, `/workspace/cf/run.sh`) IS supervised since
2026-08-26: `stack_watchdog.sh` detects it by `pgrep -x cloudflared -a |
grep -- --token` (other sessions' `--url` quick tunnels are deliberately
not matched) and relaunches `run.sh` when absent.

There is no systemd in the container and `/etc/rc.local` is empty. Root's
crontab now carries two `@reboot` lines (added 2026-08-26) that start
`vllm_awq_watchdog.sh` and `stack_watchdog.sh` after a 30 s delay; the
stack watchdog then brings up katlb, the gateway and cloudflared. Verify
with `crontab -l | grep @reboot`. If they are missing, run by hand in order:

```bash
nohup /workspace/vllm_awq_watchdog.sh > /workspace/vllm_awq_watchdog.out 2>&1 &
nohup /workspace/stack_watchdog.sh    > /workspace/stack_watchdog.out    2>&1 &
```

The ledger is rotated monthly on the box (`/workspace/ledger-rotate.sh`,
cron `5 0 1 * *`: `mv` to `oaica-gateway-ledger.YYYY-MM.jsonl` then `kill
-HUP` the :8081 listener — the gateway reopens the path on reload) and pulled
to the laptop every 30 min by cron (`~/.oaica/ledger-backup/pull.sh`, rsync
of `oaica-gateway-ledger*.jsonl` plus dated daily copies); the box is the
only other copy. Monthly totals: `jq -s 'map(select(.status==200)) |
{prompt: map(.prompt_tokens)|add, completion: map(.completion_tokens)|add}'
oaica-gateway-ledger.2026-09.jsonl`.

Reboots of a rented vast.ai instance also wipe /dev/shm (the weights); the
vLLM watchdog's preflight re-downloads them from the pinned revision, or
restore faster from the lenovo mirror.

## Gotchas (each cost real time)

- `pgrep -f <pattern>` matches the ssh shell running it; killing that drops
  the session mid-script and leaves a half-applied swap. Find listeners by
  port (`ss -ltnp | grep :PORT`) and do multi-step restarts from a detached
  on-box script: `setsid nohup /workspace/x-swap.sh > out 2>&1 < /dev/null &`.
- The old katlb watchdog (`/root/katlb_watchdog.sh`) relaunches the OLD
  binary from `/root` if you kill katlb without killing it first.
- Heredocs over ssh: `$$` and nested quotes get expanded by the local shell;
  write the script to the box first, then run it.
- vLLM refuses to start if free VRAM < `gpu_memory_utilization * total`
  (default 0.9-0.92 -> ~73 GiB). A "free" GPU with 20 GB used is not free.
- Ledger rows with `usage_seen=false` on a 200 mean vLLM sent no usage; do
  not invoice from those zeros.
- `scp` with more than one "src dst" pair silently does a remote→remote
  copy and hangs — pass one pair per invocation.
- `ps | grep <script path>` self-matches the invoking ssh shell; kill
  supervisors by argv-exact match instead, e.g.
  `ps -eo pid,args | awk '$2=="/bin/bash" && $3=="/workspace/stack_watchdog.sh"{print $1}'`.

## Gateway: per-model upstream_addr (2026-08-29)

Each entry in `models[]` may set its own `upstream_addr`, overriding the
top-level one; entries that omit it still inherit the top-level default.
The gateway runs one reverse proxy per distinct upstream, but its health
probe only checks `models[0]` — `/health`'s body is now `{"status":"ok",
"upstreams":N}` and the startup/reload log says "(N distinct upstreams)".
This is what lets `oaica-nemotron-30b-a3b` point at its own oaicalb
(`:30120`) while `oaica-35b-a3b-vision` keeps going through gatekeeper —
see "Second model pool" above for why that model bypasses gatekeeper.

## sshd MaxStartups exhaustion (2026-08-30)

Symptom: every new ssh to the box died with `kex_exchange_identification:
read: Connection reset by peer` (TCP connected, reset at the banner) while
already-open sessions kept working. Cause: a scanner (`45.198.224.218`)
held ~45 half-open sessions, exhausting sshd's default `MaxStartups
10:30:100`, so new connections were randomly dropped — including ours (our
NAT `60.50.88.231` legitimately holds ~50 tunnel/check sessions).

Fix applied inside the container (`/etc/ssh/sshd_config`, backup
`sshd_config.bak-20260830`, validated with `sshd -t`, applied with `kill
-HUP <sshd pid>` — existing sessions survive a HUP):

```
MaxStartups 100:30:300
PerSourceMaxStartups 15
LoginGraceTime 20
```

Root login is key-only (password auth off), so the scanner cannot get in;
the per-source cap just stops it from starving everyone else. If it
recurs from a new IP, the same three lines are the fix; a host-level
firewall is not available from inside the container. Check with:
`for p in $(pgrep -f "sshd: unknown|sshd: \[accepted\]"); do ss -tnp | grep "pid=$p," | awk '{print $5}'; done | sed 's/:[0-9]*$//' | sort | uniq -c`.

### Gotcha: a long-lived shell can block the watchdog's relaunch (2026-08-30)

`vllm_awq_watchdog.sh` decides "a replica for :PORT is already booting" with
`pgrep -f "api_server.*--port $port "`. Any *other* long-lived process whose
argv contains that text matches too — e.g. an `ssh box 'bash -c "...
pgrep -f ... api_server.*--port 30108 ..."'` deploy shell that is still
waiting on a rolling restart. Symptom: the old replica exits cleanly, the
watchdog logs nothing for minutes, `:PORT` stays DOWN. Fix: kill the
offending shell (it is yours); the next tick launches. Prevention: keep
test/validation logic in a script file on the box (`validate_replica.sh`)
instead of inline `bash -c` text, and never leave a shell whose argv
mentions `api_server` + the port alive across a restart.

## Production checkpoint + launch tiers (2026-08-30)

Fleet checkpoint: **`sprappcom/oaica-35b-a3b-awq-mtp-vision-260830`**
(a100b: `/dev/shm/oaica-35b-a3b-awq-mtp-vision`) — KAT-Coder-V2.5 AWQ W4A16
text backbone + bf16 vision tower + bf16 MTP head. One checkpoint, two
launch configs (owner's A100 SXM4 80 GB bench, 8k ctx / 400 gen, greedy):

| A100 tier         | Launch                        | Result                                   |
|-------------------|-------------------------------|------------------------------------------|
| 8k / 16k + vision | MTP2 on, `--kv-cache-dtype auto` | 198 tok/s single, 99/user @N=32, 166 users @≥30 tok/s |
| **256k (prod)**   | **MTP off, `--kv-cache-dtype fp8`** | **95 users @≥30 tok/s, 2110 tok/s aggregate** |

Production (GPU0/1/2, `vllm_awq_watchdog.sh`) runs the 256k row: no
`--speculative-config`, fp8 KV, 262144 max-model-len, 18 seqs, async
scheduling on (the `--no-async-scheduling` + eager-draft pair existed only
for the MTP CUDA-graph race, which has no draft to race without MTP).
Always: `VLLM_DISABLED_KERNELS=HummingLinearKernel` and
`--gdn-prefill-backend triton` (FlashInfer GDN prefill deadlocks).

Why this beats what ran before: the previous prod (slopops int4 AutoRound
+ fp8 KV + MTP1) sat in the worst quadrant — fp8 KV halves MTP verify
speed on Ampere, MTP1 < MTP2, and int4 scales worse at N≥32. The AWQ file
wins at every N≥32, carries vision, and is our own weights. Better exists
only on other hardware: H100 FP8 (`…-fp8-mtp-vision-260830`) ≈ 2× per-user
and 1.6× aggregate at 256k; Blackwell NVFP4 pending a native bench.

Not an HF snapshot on the box: the dir is a symlink graft (`SHASUMS` inside
verifies it), so the watchdog's `HF_REPO` is empty and a missing-weights
preflight alerts instead of downloading anything over it.
