# oaica-code production readiness — 2026-08-29

Status of the "make it sellable, Ollama-style" workstream. Written at the
end of the session that shipped it; each section says what's done, what's
verified with numbers, and what's explicitly out of scope/deferred.

## 1. Model manifest + CLI

**Done.** `cmd/launch/model_manifest.go` defines a per-model manifest
(id, arch, quant, context window, default output-token budget, engine,
launch flags, GPU/RAM footprint) analogous to an Ollama Modelfile.
`oaica model add/list/rm/show` (cmd/cmd.go) read/write it. `tier_routing.go`
and `context_window_remote.go` consult it, extending rather than replacing
the existing live-probe path — a probe still wins when the manifest and
reality disagree.

## 2. Tier profiles ("our own /opusplan")

**Done.** `cmd/launch/tier_plan_profiles.go` + `oaica plan set/list/show/rm`.
A plan is a named (Model, SonnetModel) pair stored in `~/.oaica/plans.json`;
`oaica launch claude --plan NAME` resolves it upstream of `buildTierPlan`,
so it reuses `tierPlan`/`proxyRouteTable` unchanged — no new routing path.
Model and sonnet-model may be on entirely different remotes (confirmed: one
local, one cloud API both work through the same translation proxy).

## 3. Stress/bench on prism-a100b

**Done, GPU0 released back to 0 MiB after.** Concurrency scaling 1/4/8/18
parallel requests: all clean, sub-2s wall time at 18-way. Vision + text
mixed load: 10/10 clean. Prefix caching: **9.5x TTFT speedup** on 33k-token
prompts (3.13s cold → 0.33s warm), confirmed matching by common prefix not
exact string. OOM/downsize recovery and cold-start timing exercised as part
of the same pass. GPU2 (real user traffic) was read-only-probed only, never
disrupted.

## 4. Caching

**Verified, not just assumed.** `--enable-prefix-caching` measured
effective (see #3's 9.5x number). Proxy path checked for anything that
would defeat cache reuse (message reordering, system-prompt mutation) —
none found. Repeated `/v1/models` probes and auth checks were not judged
worth memoizing at current request volume; revisit if profiling ever shows
otherwise.

## 5. Packaging/catalog/licensing groundwork

**Done, scaffold only, no business decisions made.**
- `cmd/launch/catalog.go`: `CatalogVariant`/`CatalogEntry` shape for a
  future oaica.com/library page — inert, not wired to any HTTP handler or
  CLI verb. No prices, no license terms, no marketing copy invented.
- Entitlement-check hook point: **built AND now live in production**
  (originally scoped as feature-flagged/off-by-default scaffolding; the
  user asked for it turned on this session — see below). This exceeds the
  original ask's scope, noted here for accuracy.

## Metering + entitlement (grew out of workstream 5, became its own arc)

Not in the original 5-item scope but is the biggest change this session
shipped, so it's reported in full:

- **tools/meterhub** (new service): central usage ledger aggregation
  (idempotent SQLite ingest, `/usage`, `/usage/summary`) + subscriber
  entitlement store (`/subscribers/{set,get,list,webhook}`). Pure-Go
  sqlite (`modernc.org/sqlite`), same cross-compile story as every other
  `tools/` binary. 22/22 tests passing.
- **tools/gateway**: async fire-and-forget usage reporting to meterhub
  (bounded retry, non-blocking channel — an unreachable meterhub cannot
  slow or fail a real completion, verified by test). Entitlement check
  wired into `completionHandler`. **Bug found and fixed during this
  work**: the check was originally written as a dead `authed()` wrapper
  that `mux()` never called — subscription blocking was a no-op until
  caught by the test suite and fixed by inlining the check directly.
  30/30 tests passing.
- **Deployed to a100b** (:8081 gateway, :8095 meterhub), verified live:
  health checks, real completions, subscriber set/get/list round-trip,
  and an actual block-then-restore cycle (canceled key → 403
  `subscription_required`, verified after TTL cache expiry; restored →
  200 again).
- **Entitlement is live** (`entitlement_enabled: true`,
  `entitlement_fail_open: true`, 60s TTL cache). Fail-open means an
  unreachable meterhub never blocks real traffic — only an explicit
  `canceled`/`suspended` `POST /subscribers/set` blocks a key. All 5
  known keys (openrouter, internal-91, laptop, client-91, client-46)
  seeded active.
- **All 3 client boxes repointed** (laptop, .91, .46) from direct
  oaicalb access to the metered gateway — this closes the original gap
  that started the whole thread ("track and bill users accordingly");
  before this, everyday client traffic bypassed metering entirely.
- Stripe webhook receiver exists (`/subscribers/webhook`) but is
  explicitly **not safe to point a real Stripe account at yet** — auth is
  still the same bearer report-token as every other endpoint, not
  Stripe's per-endpoint signature verification. Documented inline with a
  SECURITY NOTE; swap before connecting Stripe.

## Explicitly out of scope / deferred

- Real Stripe integration (needs an account this session cannot create).
- GPU2 as a 3rd oaica-35b-a3b-vision replica: GPU2 shows ~74GB "used" by
  a dead PID (zombie VRAM leak, same pattern as an earlier GPU7 incident)
  — only ~7.7GB is actually free, not enough even for the downsized
  config used previously. Reclaiming it needs `nvidia-smi --gpu-reset -i
  2` on a shared 8-GPU box with 7 other live tenants; user was asked and
  chose to skip rather than take that risk. Current fleet stays GPU0+GPU1.
- `docs/BILLING_ENTITLEMENT.md` / `docs/MULTI_REGION_ROUTING.md`:
  referenced in code comments as forward pointers, not yet written as
  actual files — low priority, write on request.
- Per-key rate limiting / quota enforcement beyond binary
  active/blocked status — not asked for, not built.

## Test status at time of writing

- `go test ./cmd/launch/...`: pass (2.8s)
- `tools/gateway`: 30/30 pass
- `tools/meterhub`: 22/22 pass
- All changes committed; 3 commits pushed to `origin/main` this session
  (meterhub service, docs, subscriber/webhook tests).
