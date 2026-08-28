# oaicalb (formerly katlb)

Plain reverse-proxy load balancer over a set of OpenAI-compatible backends.
Not model-specific — backend list, health-check path, and listen ports all
come from `-config <file>.json` (see `lbConfig` in main.go). Built for the
kat-awq 6-replica fleet (now serving as `oaica-35b-a3b-vision`, 2026-08-28),
but any future multi-replica model gets the same leastconn + session-hash +
auto-failover by running a second instance with its own config.

Two listeners, same backends:

- `leastconn_addr` (default `:8090`) — least-outstanding-requests, ties
  broken round-robin (so sequential, non-overlapping requests still spread
  evenly instead of always hitting backend 0).
- `session_hash_addr` (default `:8091`) — consistent hash on the
  `X-Session-Id` request header, degrading to leastconn if the hashed
  backend is unhealthy. Pins a conversation to one replica so its vLLM
  prefix cache actually gets reused turn-to-turn, instead of every turn
  landing on a cold-cache replica under round-robin. Wired into production
  since 2026-08-28: oaica-code's Anthropic->OpenAI proxy generates one
  random session ID per launch (`newProxySessionID`) and sends it as
  `X-Session-Id` on every request.

  **`session_overflow_factor`** (default `0`, disabled): a healthy but
  disproportionately loaded hashed backend can also trigger a reroute, not
  just an unhealthy one. When set > 0, a request reroutes to the
  least-loaded healthy backend if its hashed backend's inflight count
  exceeds `session_overflow_factor` × the average inflight across healthy
  backends. This is a per-REQUEST decision, not sticky — the same
  session's next request re-hashes to its normal backend and rejoins it
  once load drains back under the threshold, with no separate
  "un-reroute" state to manage. Exists because pure session-hash is
  sticky forever: one replica can queue a growing backlog while another
  sits idle, purely because of which session IDs happened to hash where
  (observed 2026-08-29 — a100b's two oaica-35b-a3b-vision replicas can
  end up unevenly loaded even though both are healthy). Deployed at `2.0`
  on a100b: a session reroutes once its hashed backend is carrying more
  than double the fleet's average load.

Active health checks every 3s; 2 consecutive failures marks a backend down,
2 consecutive successes marks it back up. Two probe modes:

- default: `GET <health_path>` (`/v1/models`). Cheap, but only proves the
  HTTP server is up.
- `"probe_model": "<served-model-name>"`: a real 1-token
  `POST /v1/chat/completions`. Use this in production. vLLM answers
  `GET /v1/models` 200 while every chat request 400s (e.g. tokenizer with no
  `chat_template` -- the 2026-08-25 outage), so the GET probe kept every
  backend UP while all traffic failed. `probe_timeout_sec` (default 10)
  bounds one probe so a replica still loading weights is not marked DOWN.
  A 200 whose body is not a completion (empty, truncated, no `choices`)
  counts as a failure.

Hung-replica detection (`stall_sec`, default 120; `-1` disables): oaicalb
tracks every in-flight request per backend. If at least
`stall_min_inflight` (default 1) of them are older than `stall_sec` AND the
latest probe failed or timed out, the backend is marked DOWN on that single
failure instead of waiting for two in a row -- this is the only thing that
catches a replica whose probe flaps (ok, timeout, ok, ...) while every real
request on it hangs. It is re-admitted by the normal 2-consecutive-successes
rule. Old requests alone never mark a backend DOWN (a long legitimate
generation with a passing probe is fine). `/status` on `status_addr` adds
`oldest_inflight_sec=` and `probe=ok|fail` per backend.

A malformed config is returned as an error at startup instead of
`log.Fatalf`, so a future reload path cannot kill the LB.

## Build

```bash
cd tools/oaicalb
GOOS=linux GOARCH=amd64 go build -o oaicalb-linux-amd64 main.go
```

## Real deployment (a100b, 2026-08-21; renamed katlb -> oaicalb 2026-08-28)

Originally deployed as `katlb`, config at `/root/katlb-kat-awq.json` on
a100b, pointing at the 6 kat-awq vLLM replicas. Kept alive by
`/root/katlb_watchdog.sh` (no systemd in the container — plain poll loop,
same pattern as the SSH tunnel watchdogs). The served model itself was
renamed `kat-awq` -> `oaica-35b-a3b-vision` on 2026-08-28; the LB binary,
config filename, and watchdog script are renamed to `oaicalb` accordingly
going forward, but the deployment story below (from 2026-08-21) is kept
as-written since it describes what actually happened at the time.

**`leastconn_addr` is `:30099`, not `:8090`** — deliberately took over the
port GPU0's vLLM instance used to own. GPU0 itself was moved to `:30199`
(now just another backend in the pool) so that every existing
`~/.oaica/remotes.json` `kat-awq` entry, and every ALREADY-RUNNING Claude
Code session's translation proxy, keeps working with zero relaunch and zero
config change — they were always addressing "port 30099," it just silently
became load-balanced with failover instead of being one fixed GPU. This is
the answer to "does a live session need to restart to get failover": no,
because the LB sits exactly where the single instance used to sit, not on
a new port next to it.

`session_hash_addr` stays on `:8091` (still unused in production — see
above). `status_addr` `:8092`.

`~/.oaica/remotes.json`'s `kat-awq` entry on all 3 machines (this laptop,
lenovo.samwong.com, 192.168.0.46) is just `http://127.0.0.1:30099/v1` —
the same address it always was. The separate `kat-awq1`-`kat-awq5` picker
entries were removed — `kat-awq` alone now load-balances across all 6
replicas.

Verified: even distribution under both sequential (4/4/4/4/4/4 over 24
requests) and concurrent (10/10/10/10/10/10 over 60 concurrent requests)
load; real end-to-end `oaica launch claude --model kat-awq` on all 3
machines; a request to the fixed `:30099` address transparently routed to
a different backend after GPU0 was moved, with no client-side change.
