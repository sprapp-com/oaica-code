# gatekeeper

Per-key auth + concurrency gate. Sits in front of `katlb` (or any single
OpenAI-compatible upstream), adds the one thing katlb deliberately doesn't
do: customer identity and per-tier concurrency limits.

- `Authorization: Bearer <key>` required — missing/unknown key -> 401.
- Each key belongs to a tier (`free`/`pro`/`team`/... — config-defined, not
  hardcoded) with a `max_concurrent`. A request over that key's current
  limit gets 429 immediately, no queueing.
- Concurrency, not requests-per-second: matches how these tiers are meant
  to be sold ("N simultaneous sessions"), simpler to reason about and audit
  than a token-bucket rate.
- Config reloads on `SIGHUP` — add/revoke keys without dropping in-flight
  requests or restarting.

## Build

```bash
cd tools/gatekeeper
GOOS=linux GOARCH=amd64 go build -o gatekeeper-linux-amd64 main.go
```

## Config

```json
{
  "tiers": {"free": 2, "pro": 10, "team": 50},
  "keys":  {"sk-abc123": "pro", "sk-def456": "free"},
  "upstream_addr": "http://127.0.0.1:30099",
  "listen_addr": ":30098"
}
```

## Real deployment (a100b, 2026-08-21)

`/root/gatekeeper.json`, listening on `:30098`, forwarding to katlb on
`:30099`. Kept alive by `/root/gatekeeper_watchdog.sh` (same poll-loop
pattern as katlb's and the SSH tunnel watchdogs — no systemd in the
container).

**Deployed additively, not in place of `:30099`.** Forcing auth onto the
port every existing session already hits would 401 all 3 client machines
immediately (their `remotes.json` currently sends no `Authorization`
header at all) — that's a breaking rollout, not something to do silently.
`:30099` (katlb, unauthenticated) stays as-is for internal/current use;
`:30098` (gatekeeper) is the gated entrypoint for when real customer
key issuance and billing are ready. `tiers.internal: 200` and 3 generous
internal keys were minted for this session's own use/testing — real
customer-facing key issuance (signup flow, billing tie-in) is separate,
unbuilt work.

Verified: valid key -> 200 through the full gatekeeper -> katlb -> vLLM
chain; no key -> 401; concurrency correctly enforced (free tier limit=2,
3 concurrent -> exactly 1x 429; pro tier limit=10, 12 concurrent -> exactly
2x 429) against a synthetic slow upstream before deploying.
