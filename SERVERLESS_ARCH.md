# oaica-code Serverless + Registry Deployment Architecture

> **Status:** design doc (v1.1). Companion to `OAICA_FORK_PLAN.md` (which chose
> "thin-client: strip Ollama's server, talk to our OpenAI-compatible API").
> This doc adds the **deployment fabric**: how the thin client finds, auths
> to, connects to, and fails over across a fleet of serving boxes — most of
> them **serverless (scale-to-zero)** and most of them **behind NAT**.
>
> **v1.1 reality check (2026-08-19):** a lot of this is ALREADY built in
> `cmd/launch/`, in a *loopback-proxy* shape (not the "direct public IP" shape
> this doc's v1.0 assumed). Read §0 first — it reconciles this proposal with
> what `cmd/launch/` already does, so the registry/failover work below is an
> **additive extension** to the existing launch system, not a replacement.

## 0. Reality check — the as-built launch system (read this first)

`cmd/launch/` is the **single standardized launch + model-resolution system**
every integration goes through. It is NOT a menu; it is the router/registry
itself. Key pieces:

| File (`cmd/launch/`) | Role |
|---|---|
| `registry.go` | **Integration registry** — canonical table of `IntegrationSpec{Name, Runner, Aliases, Description, Install}` for claude, chatgpt, hermes, openclaw, opencode, codex, copilot, omp, cline, droid, pi, pool, qwen … |
| `launch.go` | The launch framework (LauncherState / LauncherIntegrationState / resolveRunModels) every Runner uses |
| `opencode.go`, `codex.go`, `claude.go`, `hermes.go`, `kimi.go`, … | One **Runner per integration**. `opencode.go` configures opencode by writing `~/.local/state/opencode/model.json` + emitting `OPENCODE_CONFIG_CONTENT` (JSON pointing provider `ollama` at `envconfig.Host()+"/v1"`) |
| `models.go`, `oaica_models.go`, `model_inventory.go`, `deprecated_models.go` | **Model registry/inventory** — what's available in the picker, merged with cloud + user-remote models |
| `user_remotes.go` | **Self-hosted endpoint descriptor** — `~/.oaica/remotes.json` entries (`name`, `base_url`, `api_key`/`api_key_env`) for llama-server / prism_server / vLLM / any OpenAI gateway |
| `agent_routing.go` | `ResolveAgentModel` — picks how a `<remote>/<model>` maps to `{base_url, token, upstream_model, meta}`; also resolves cloud/local models |
| `anthropic_openai_proxy.go`, `local_proxy.go` | **Loopback translation proxies** — per-remote Anthropic↔OpenAI proxy on `127.0.0.1:<port>`; + system-message normalization for local llama-server |
| `cmd/agent/` (new, additive) | **First first-party CLI consumer** of the routing — the `oaica agent` coding agent. `<remote>/<model>` → `ResolveAgentModel` → loopback proxy → Anthropic `/v1/messages` SSE shim → `agent.Session` engine (tools, approval, compaction) |

**How a request actually reaches a box today (v1.1 — corrected from v1.0):**

1. User picks `<remote>/<model>` (or a local/cloud model) in the picker.
2. For a **user remote** (from `~/.oaica/remotes.json`), the launch system does
   **NOT** point the integration directly at the box. It spawns a **loopback
   Anthropic↔OpenAI translation proxy** (`RunAnthropicOpenAIProxy`) bound to
   that remote's own API key, and hands the integration `http://127.0.0.1:<port>`
   (`agent_routing.go`). opencode is special-cased: it points at the **local oa
   daemon** `envconfig.Host()+"/v1"`.
3. The local daemon / loopback proxy forwards `baseURL/v1/chat/completions`
   with the remote's key. The box never sees the raw integration traffic; the
   client keeps a local routing layer.
4. `oaica agent` (new `cmd/agent/`) is the **first first-party CLI consumer** of
   this routing. It resolves the same way — user remote → loopback
   Anthropic↔OpenAI proxy; cloud/router/local → `ResolveAgentModel`'s
   logging-proxy path — then drives a coding loop: `api.ChatRequest` →
   Anthropic `/v1/messages` → SSE → the existing `agent.Session` engine (tools,
   approval, compaction). The agent speaks the **Anthropic** shape into the
   loopback proxy, which OpenAI-translates to the box — a new consumer without
   a new route to boxes.

So **there is no "direct-to-public-IP with JWT" today, and no failover between
boxes.** A dropped remote just fails. What exists is:
- Standardized endpoint *discovery* (inventory + picker + `remotes.json`).
- Standardized endpoint *auth* (per-remote bearer key).
- Standardized endpoint *connect* (loopback proxy / local daemon).

The registry + short-lived-token + failover + serverless parts below EXTEND this
loopback/launch model. Where each new idea lands in code is mapped in §9.

> **Per-model routing (related):** the loopback model above routes **per
> integration** — every OpenAI integration points at the local daemon, which
> does not forward user-remotes (they 404), and only claude/agent reach user
> remotes via the translation proxy. `docs/architecture/PER_MODEL_ROUTING.md`
> specifies making the wire protocol **per model** (OpenAI-native model →
> OpenAI integration direct; Anthropic-native → Anthropic integration direct;
> proxy only on mismatch) plus a capability gate so a free-form-tool model like
> kat-coder is refused behind a `tool_use` loop instead of silently spiraling.
> That doc is the spec; this doc is the fleet fabric.

> **v1.0 naming error, corrected:** `integration/` is the Go **test suite**, not
> the launcher. The launcher is `cmd/launch/`. All `integration/` mentions in
> this doc's older text were wrong and are replaced below.



1. **Standardize endpoint selection** across all integrations (replaces the
   per-integration "opencode selection" / manual `remotes.json` picker with one
   registry-backed model).
2. **Direct-IP first**: end users talk to a serving IP (or tunnel hostname)
   directly when reachable — no middleware hop.
3. **Failover**: if an endpoint drops, the client automatically asks the
   central registry for another working one and reconnects.
4. **Auth**: a way to connect to a box directly without shipping long-lived
   passwords — short-lived signed tokens issued by the central service.
5. **Serverless-native**: scale-to-zero boxes (Vast serverless, Bitdeer spot)
   join/leave the registry on demand; a cold request triggers spin-up.

## 2. Architecture overview

```
┌──────────────┐  1.discover / 2.token / 4.failover-ask
│  oaica-code   │ ─────────────────────────────────────────▶ ┌──────────────────────────┐
│  (client/CLI) │                                             │  Registry (CF Worker)    │
│  cmd/launch/  │                                            │  GET /v1/registry        │
│  user_remotes │ ◀───────────────────────────────────────────│  POST /v1/registry/token│
└──────┬───────┘     {healthy endpoints + signed JWT}         │  POST /v1/registry/request│
       │  3. loopback proxy → /v1/chat (Bearer JWT)           └──────────────────────────┘
       ▼                                                        │ heartbeat/register
   serving box (public-IP or NAT-tunneled) ◀──────────────────┘
       prism_server / oaica-code server, validates JWT,
       scale-to-zero on idle
```

Three roles:

| Role | What | Where it lives |
|---|---|---|
| **Client** | `oaica-code` CLI/GUI; discovers, tokens, proxies (loopback), fails over | `cmd/launch/` (user_remotes.go, agent_routing.go, anthropic_openai_proxy.go) |
| **Registry** | source of truth for healthy endpoints; issues short-lived tokens; boots cold serverless workers | Cloudflare Worker (extend `marketplace/`) |
| **Boxes** | serving instances (PrismX/`prism_server`, or an oaica-code server); self-register + heartbeat + validate tokens | public-IP pool, NAT pool (a100b/5090), serverless workers |

## 3. Standard endpoint descriptor (the "everything is this one shape")

Every box (public, NAT-tunneled, or serverless) is described by one JSON shape
shared between the registry, the boxes' self-report, and the client. This is
the "standardized" part — no more hand-written remotes.

```json
{
  "id": "a100b-gpu4",
  "name": "kat-a100b",
  "model": "kat-coder-i-compact.v3b",
  "base_url": "https://kat.sprapp.com/v1",
  "transport": "public|nat-tunnel",
  "region": "sea",
  "tier": "serverless|steady|spot",
  "max_context": 262144,
  "vr": 96,
  "healthy": true,
  "latency_ms": 14,
  "auth": { "mode": "jwt", "issuer": "sprapp", "public_key": "ed25519:..." },
  "models": ["v2-ic-layout-a100b_v3b"]
}
```

- `base_url` is the **direct** URL a client hits (public IP, or a stable
  tunnel hostname for NAT boxes). No central proxy in the hot path.
- `auth.mode=jwt` means the box only accepts Bearer tokens signed by the
  registry's key (see §5).

## 4. Registry (Cloudflare Worker — new `/v1/registry` routes)

### 4.1 Endpoints the client calls

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/v1/registry` | List healthy endpoints (all models or `?model=`/`?tier=` filter) |
| `GET` | `/v1/registry/servers/:id` | Single endpoint + current health |
| `POST` | `/v1/registry/token` | Issue a short-lived JWT binding (user, endpoint, model) |
| `POST` | `/v1/registry/request` | Request a serverless cold box to spin up; returns when warm |
| `POST` | `/v1/registry/register` | Box self-register + heartbeat (boxes call this) |
| `POST` | `/v1/registry/event` | Health/drop events (turned into the failover source) |

Registry does NOT proxy traffic. It only returns `base_url`s + tokens. The
client then connects directly (goal #2).

### 4.2 Server/box side (self-report, heartbeat)

Each box registers on boot and heartbeats every ~30-60s:

```
POST /v1/registry/register
Authorization: Bearer <SERVER_REGISTRATION_KEY>   # long-lived, box-only
{ "id","model","base_url","transport","region","tier","max_context","cap" }
```

- Registry keeps a `last_seen`; misses >~3 intervals → marked unhealthy →
  excluded from discovery + failover.
- Serverless boxes **deregister** on scale-down (`/v1/registry/event
  {"kind":"down"}`) so the registry never returns a cold worker as healthy.
- Warm pool (`min_load>0`) boxes stay registered and hold prefix cache.

### 4.3 Serverless cold-start flow

When a client asks for a model whose only capacity is a serverless tier with
0 warm workers:

1. `POST /v1/registry/request {model}` → Worker calls the serverless provider
   (e.g. Vastle) to boot a worker of the descriptor's image.
2. Worker returns `{"status":"pending","request_id"}`; client polls
   `GET /v1/registry/request/:id`.
3. Worker's container boots `prism_server` with the model + JWT-validate
   secret, registers via §4.2, then the request resolves to the new `base_url`.
4. Client connects directly.

This is the exact mechanism to "replace Ollama-style always-on" with
serverless economics (§ of `SELF-HOST.md` for the container).

## 5. Auth — short-lived signed token (JWT)

**Chosen:** short-lived signed token (user's decision).

### Issue (client → registry)

Client already holds a marketplace `prism_sk_` key. To connect:

```
POST /v1/registry/token
Authorization: Bearer prism_sk_...
{ "endpoint_id": "a100b-gpu4", "model": "kat-coder", "ttl": 900 }
→ { "token": "<signed-jwt>", "exp": ..., "base_url": "https://..." }
```

The JWT carries `{sub, endpoint_id, model, iat, exp, jti}` and is signed by
the registry's private key. **It only lives a few minutes.** Short TTL + `jti`
blacklist = instant revocation; no passwords go to the box.

### Validate (box side)

The box (prism-server/`/api`, or oaica-server) validates every `/v1/*` request:

- Verify signature against the registry's **public key** (embedded in the
  box's descriptor, or a configured `OAICA_REGISTRY_PUBLIC`).
- Check `exp` (server clock), `endpoint_id` matches self, `model` allowed.
- Reject expired/invalid with `401` + `WWW-Authenticate: Bearer`.

Prism note: today the a100b/GPU4 server is open (no key). For public/`tier:
serverless` boxes we **gate behind token validation**; internal dev boxes may
stay open. Gate is env: `OAICA_TOKEN_SECRET` (symmetric, fastest to ship) or
`OAICA_REGISTRY_PUBLIC` (Ed25519, production).

### Renew / refresh

Client keeps a small token cache; when `exp` is near, re-request from the
registry. Failover (next §) also re-requests a fresh token for the new endpoint
(scope moves to the new `endpoint_id`).

## 6. Direct connect (public-IP pool + NAT fallback)

Chosen: **public-IP pool + NAT-tunnel fallback** (user-confirmed).

- **Public-IP tier**: boxes rented with a real public IP (e.g. Bitdeer
  on-demand B200/H200, or vast with public ports). Registry hands the client
  `https://<ip>:<port>/v1` directly. No proxy hop.
- **NAT tier (a100b, 5090, our own cards)**: the box isn't publicly routable.
  The `nat/` piece (or a cloudflared tunnel) publishes a **stable hostname**;
  the registry stores that as `base_url`. Client still connects "directly" to
  the hostname — the tunnel is the last mile, not a proxy middleman.
- **Latency-aware discovery**: registry can return a `latency_hint` (client
  sends its region/time-of-flight); client prefers the nearest healthy box.

Transport summary (registry stores `transport`):

```
transport: "public"       → direct https://IP:port/v1
transport: "nat-tunnel"   → https://<stable-tunnel-host>/v1 (cloudflared/ wireguard)
transport: "serverless"   → issued via /v1/registry/request (may start cold)
```

## 7. Failover ("if it dropped, request another")

Client policy (in `api/client.go`, new registry-backed dialer):

1. **Probe** the chosen `base_url` (`GET /health`, short timeout, retry xN).
2. On drop/timeout/`401-expired`:
   - mark endpoint **bad** in the local registry cache (cool-down, e.g. 5 min);
   - query `GET /v1/registry?model=...&tier=...` for the **next healthy** box;
   - issue a fresh short-lived token for the new `endpoint_id`;
   - reconnect and resume the same user session (the model reloads; prefix
     cache restarts unless a warm-pool box with the prompt cached is chosen).
3. Exponential backoff + circuit breaker per endpoint; never hammer a dead box.
4. If `tier=serverless` and all are cold → call `request` (§4.3) and poll.

The registry is the single source of "what's alive". The client's job is to
keep trying until one answers, then stay pinned to the good one.

**Affinity rule:** prefer a warm serverless worker that already holds the
conversation's prefix (repeat system prompt → cache hit → cheap). Reuse of the
registry's `cache` hint (a `cache_key` the client sends) helps the box pick a
cached slot.

## 8. Standardization (as-built) + what the registry adds

**Standardization already exists and is NOT "replace opencode selection" — it
is `cmd/launch/`, and opencode is updated through it** (see §0):

- `opencode.go` configures opencode via `OpenCode.Edit()` → writes
  `~/.local/state/opencode/model.json` (so models appear in opencode's picker)
  and emits `OPENCODE_CONFIG_CONTENT` JSON: provider `ollama`, `baseURL =
  envconfig.Host()+"/v1"`, `model = ollama/<primary>`. opencode talks to the
  local daemon, not to any remote box directly.
- `user_remotes.go` already IS the endpoint descriptor: a box
  (`prism_server` / `llama-server` / vLLM / OpenAI gateway) lists in
  `~/.oaica/remotes.json` and its models appear namespaced `<remote>/<model>`
  in the SAME picker as local + cloud models. A sleeping box costs only its own
  entry (per-remote timeout, errors collected).
- `agent_routing.go` resolves `<remote>/<model>` → spawn loopback
  Anthropic↔OpenAI proxy → `{base_url, token, upstream_model}`.

**What the registry ADDS** (the real gap): this is discovery + auth + connect
today, but **no failover and no dynamic endpoint churn**. The registry work is
layered on top:

- **Auto-registration / heartbeat** → a serverless box joins/leaves without a
  human editing `remotes.json`. The registry is a *live* source of healthy
  endpoints; `remotes.json` becomes a cached snapshot the client re-syncs.
- **Short-lived-token auth** replaces static bearer keys for public/serverless
  boxes (see §5). Loopback proxy still holds the key — the token just scopes
  to one endpoint for minutes.
- **Failover** (§7): on drop, the client re-asks the registry for the next
  healthy box instead of dying. This slots into `agent_routing.go` +
  `anthropic_openai_proxy.go` — the proxy retargets to the new `base_url`,
  the integration never notices.

So "replace opencode selection" is **NOT** the goal — opencode selection is
already standardized. The goal is: *make the endpoints behind that selection
self-registering, healthy, token-auth'd, and failover-capable*.

### Environment (standardized — extends, does not replace, `remotes.json`)
```
OAICA_REGISTRY        https://registry.api.sprapp.com   # Worker base
OAICA_REGISTRY_KEY    <marketplace prism_sk_> (for token issue)
OAICA_TOKEN_CACHE     ~/.oaica/tokens.json (short-lived, chmod 600)
OAICA_REMOTES_FILE    ~/.oaica/remotes.json             # existing, kept (static base + registry sync)
OAICA_DEFAULT_MODEL   kat-a100b
OAICA_DEFAULT_TIER    auto|economia|serverless
```

## 9. Where this lands in the code (corrected — mapped onto the real tree)

| File/dir (`cmd/launch/` unless noted) | Change |
|---|---|
| `user_remotes.go` | Extend `userRemote` with `{id, transport, tier, region, healthy, latency_ms, cache_key, auth}` — the v1.1 endpoint descriptor (§3). Add `SyncFromRegistry()` mirror |
| `agent_routing.go` | `ResolveAgentModel`: after loopback-proxy bind, add failover retry loop (probe→mark-bad→re-query→re-bind) per §7 — also serves the `oaica agent` CLI (`cmd/agent/`), so failover/short-token work here benefits the agent for free |
| `anthropic_openai_proxy.go` / `local_proxy.go` | Proxy gains the failover dialer + short-token injection for JWT-gated boxes |
| `registry.go` | Integration registry unchanged. (The `/v1/registry` Worker is a NEW module, not this file) |
| `models.go` / `model_inventory.go` | Inventory pulls the registry's healthy list (live) merged over static remotes |
| new `registry_client.go` | Client Registry API: discover, token, request, events, sync |
| `nat/` | stable tunnel up for NAT boxes; expose hostname to registry |
| `discover/` | GPU probe — unchanged |
| box-side daemon (Prism container) | registration + heartbeat + JWT validation; a bare Vastle worker becomes a registered endpoint |

> **v1.0 errors corrected here:** there is no `api/registry.go`, no
> `integration/` consumer, no `server/cloud_proxy.go` replacement. The launch
> system in `cmd/launch/` is where all this lands.

## 10. Implementation phases

| Phase | Deliverable | Proves |
|---|---|---|
| **A** | Registry `/v1/registry` + `register`/`heartbeat` (Worker) | registry lists healthy; box drops → gone |
| **B** | JWT issue (`/token`) + box-side validate | direct-connect authenticated, expired rejected |
| **C** | Client dialer: discover→token→direct connect→failover | `oa` switches a live a100b↔5090 on drop |
| **D** | Serverless cold-start (`request`+poll) | Vastle worker scales in on demand |
| **E** | Registry-backed endpoint churn: inventory/`user_remotes.go` sync live registry over static `remotes.json` | serverless boxes join/leave with zero human config |

Phase A-B are the foundation; C is the visible win (auto-failover); D is the
serverless economics; E is the operational win (self-registering fleet).

## 11. Security notes (non-negotiable)

- **Secrets** (`OAICA_REGISTRY_KEY`, box `JWT_SECRET`, server registration
  tokens) live in env/secret stores — **never in code or committed files**.
- Token TTL short (minutes); `jti` deny-list; clock-sync (`NTP`) on boxes
  before exp validation matters.
- Serverless workers are ephemeral: they get an env-secret at boot
  (from the registry or the provider), never a hardcoded key.
- Rate-limit `/v1/registry/token` + `/request` (a token = billed model spend;
  abuse = burn).
- Audit: registry logs all token issues + endpoint joins/leaves.

## 12. Honest gate

This does NOT magically make NAT'd a100b public. Direct-IP is real only for
the **public-IP pool**; NAT boxes (our own cards) use the tunnel hostname
(§6). Failover and auth are real regardless. The biggest unknown is
**serverless cold-start latency** (loading a 17.45G `.prism` pack + scale
time) — measure it in Phase D before promising sub-minute failover to
scale-to-zero. If it's too slow, keep a warm pool (`min_load=1`) as the
cache-holder and let only bursts go cold, matching the split-tier model in
the operator's market notes.
