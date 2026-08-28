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
  :30106 vLLM oaica-35b-a3b-vision  GPU0    :30105 vLLM oaica-35b-a3b-vision  GPU2
```

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

`REPLICAS` defaults to `2:30105 0:30106` (GPU2 + GPU0). GPU5 is held by the
malay35b-offload `prism_server` plus another session's 52 GB job, so an
oaica-35b-a3b-vision replica OOMs at startup there. A `booting()` guard
skips a port whose api_server exists but is not yet listening (vLLM takes
~100 s to load), so a slow start is not treated as a crash.

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
