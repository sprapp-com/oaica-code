# a100b — reproducible serving stack

Everything needed to rebuild the kat-awq public-inference stack on the a100b
box (vast.ai, 8x A100 80GB, no systemd, 16G root overlay that is 100% full).
Before this directory existed, the stack lived only as ad-hoc files under
`/root/` and `/tmp/` on the box; when `/dev/shm/kat_awq` was deleted by
another session's cleanup the watchdog crash-looped ~49k times and the only
fix was a from-memory re-download that silently lost the chat-template patch.

## Layout on the box

All state lives under `/workspace/` (the only writable, exec-capable disk
with headroom). Never write to `/root/` or `/tmp/` -- `cp`/`install` there
fails with ENOSPC on the full overlay.

| Path | What | Source in repo |
|---|---|---|
| `/dev/shm/kat_awq/` | model weights (volatile tmpfs, shared) | HF `Ar4ikov/KAT-Coder-V2.5-Dev-AWQ-W4A16-ASYM` @ `446ea8c6` |
| `/workspace/kat_awq.tokenizer_config.json` | chat_template patch | `kat_awq.tokenizer_config.json` |
| `/workspace/vllm_awq_watchdog.sh` | fleet watchdog (preflight + restore + backoff) | `vllm_awq_watchdog.sh` |
| `/workspace/katlb-linux-amd64` + `katlb-kat-awq.json` | LB with chat-aware probe | `../katlb/`, `katlb.json` |
| `/root/gatekeeper` + `/root/gatekeeper.json` | per-key concurrency tiers | `../gatekeeper/`, `gatekeeper.json` |
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
(cd ../katlb   && GOOS=linux GOARCH=amd64 go build -o katlb-linux-amd64 main.go)
(cd ../gateway && GOOS=linux GOARCH=amd64 go build -o oaica-gateway main.go)

# 3. configs: fill secrets, then
#    gateway.json  -> api_keys[].sha256 = printf '%s' "$KEY" | sha256sum
#    gatekeeper.json -> keys: plaintext (gatekeeper does not hash yet)
#    the gateway's upstream credential is one of gatekeeper's keys on the
#    "openrouter" tier, passed as OAICA_GATEWAY_UPSTREAM_KEY in gw-swap.sh

# 4. start (order matters: replicas -> katlb -> gatekeeper -> gateway)
nohup /workspace/vllm_awq_watchdog.sh > /workspace/vllm_awq_watchdog.out 2>&1 &
nohup /workspace/katlb-linux-amd64 -config /workspace/katlb-kat-awq.json > /workspace/katlb.log 2>&1 &
nohup /root/gatekeeper -config /root/gatekeeper.json > /root/gatekeeper.log 2>&1 &
OAICA_GATEWAY_UPSTREAM_KEY=<gatekeeper openrouter key> \
  nohup /workspace/oaica-gateway --config /workspace/oaica-gateway.json > /workspace/oaica-gateway.log 2>&1 &
```

## Request path

```
Cloudflare (oaica.samwong.com) -> .91 cloudflared -> ssh tunnel -> a100b
  :8081 oaica-gateway   auth (sha256 key), model rewrite, usage injection, ledger
  :30098 gatekeeper     per-key concurrency ("openrouter" tier = 32)
  :30099 katlb          leastconn across replicas, chat-aware health probe
  :30199 vLLM kat-awq   GPU0
```

## Health semantics (why the probe is a chat call)

`GET /v1/models` on vLLM returns 200 while every completion 400s (seen when
the tokenizer had no `chat_template`). katlb's probe is therefore a real
1-token `POST /v1/chat/completions` for the served model (`probe_model` in
`katlb.json`). A replica is UP only if a customer request would succeed.
The gateway's `/health` in turn asks gatekeeper, so an external monitor on
`https://oaica.samwong.com/health` sees the whole chain.

## Watchdog behaviour

`vllm_awq_watchdog.sh` per replica in `REPLICAS` ("gpu:port"):

1. if the port is listening, do nothing;
2. else **preflight**: weights present (re-download from the pinned revision
   if not) and tokenizer patch sha256-matches (re-apply if not);
3. launch; if the process dies within 60 s, double the retry delay (cap
   10 min) and write to `/workspace/vllm_awq_watchdog.ALERT`.

Poll the ALERT file from a monitor; a restore or a backoff is the signal
that something on the box changed under us.

`REPLICAS` defaults to `0:30199` only. GPU5 is held by the malay35b-offload
`prism_server` plus another session's 52 GB job, so a second kat-awq replica
OOMs at startup there. Add `5:30105` back once GPU5 is actually free.

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
