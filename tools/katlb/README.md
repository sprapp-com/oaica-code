# katlb

Plain reverse-proxy load balancer over a set of OpenAI-compatible backends.
Not model-specific — backend list, health-check path, and listen ports all
come from `-config <file>.json` (see `lbConfig` in main.go). Built for the
kat-awq 6-replica fleet, but any future multi-replica model gets the same
leastconn + session-hash + auto-failover by running a second instance with
its own config.

Two listeners, same backends:

- `leastconn_addr` (default `:8090`) — least-outstanding-requests, ties
  broken round-robin (so sequential, non-overlapping requests still spread
  evenly instead of always hitting backend 0).
- `session_hash_addr` (default `:8091`) — consistent hash on the
  `X-Session-Id` request header, degrading to leastconn if the hashed
  backend is unhealthy. Pins a conversation to one replica so its vLLM
  prefix cache actually gets reused turn-to-turn, instead of every turn
  landing on a cold-cache replica under round-robin. Not wired into
  production yet — the Anthropic->OpenAI proxy doesn't emit a stable
  per-conversation session ID today, so nothing currently sends
  `X-Session-Id`. Listener is up and correct, just unused until that's
  added.

Active health checks: `GET <health_path>` (default `/v1/models`) every 3s,
2 consecutive failures marks a backend down, 2 consecutive successes marks
it back up.

## Build

```bash
cd tools/katlb
GOOS=linux GOARCH=amd64 go build -o katlb-linux-amd64 main.go
```

## Real deployment (a100b, 2026-08-21)

Config at `/root/katlb-kat-awq.json` on a100b, pointing at the 6 kat-awq
vLLM replicas (ports 30099, 30101-30105). Kept alive by
`/root/katlb_watchdog.sh` (no systemd in the container — plain poll loop,
same pattern as the SSH tunnel watchdogs).

`~/.oaica/remotes.json`'s `kat-awq` entry on all 3 machines (this laptop,
lenovo.samwong.com, 192.168.0.46) points at the leastconn listener through
an SSH tunnel — local port 8890 (not 8090; that's already taken by an
unrelated service on two of the three boxes) mapped to a100b's real 8090.
The separate `kat-awq1`-`kat-awq5` picker entries were removed — `kat-awq`
alone now load-balances across all 6 replicas.

Verified: even distribution under both sequential (4/4/4/4/4/4 over 24
requests) and concurrent (10/10/10/10/10/10 over 60 concurrent requests)
load, real end-to-end `oaica launch claude --model kat-awq` on all 3
machines.
