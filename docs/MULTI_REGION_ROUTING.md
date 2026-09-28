# Multi-region routing — where a request actually runs

Forward pointer from `tools/meterhub/main.go`'s package doc ("the first
piece of a real multi-region answer"). This describes what exists today,
what is deliberately *not* built, and how the pieces that do exist are
expected to line up once there is more than one region.

## Today: one region, named

`gwConfig.Region` labels each gateway ("a100b" today, "a real region name
once there's more than one"), and that label rides every usage row from
the box's JSONL ledger all the way into meterhub's `usage` table. Nothing
about the label is used for routing yet — it exists so that the query
"how many tokens has key X used across every region" is a `GROUP BY`
rather than an ssh-and-sum exercise. Getting the labelling right first is
what makes a second region a rollout instead of a migration.

## The three routing layers

Routing happens in three distinct places, and they answer different
questions. Confusing them is the main hazard here.

**1. Which endpoint (client side) — `cmd/launch`'s route plan.**
A launch builds a *plan*: a primary endpoint, distinct secondary
endpoints, and a Haiku-tier endpoint, each tagged with a source
(`sourceLocal`, `sourceUserRemote`, `sourceRouter`). `route_policy.go`
decides what happens when the selected route's upstream is failing, using
the plan's other legs as fallbacks (`RouteLocalFirst` by default). This
layer chooses among *endpoints the user configured* — a local Ollama, a
remote the user added with `oaica provider login`, or the metered router.
It knows nothing about regions.

**2. Which replica (load balancer) — affinity, not selection.**
`cmd/launch` generates ONE `SessionID` per launch (not per request) and
sends it as `X-Session-Id` on every request that launch's proxy
forwards. A session-hash-aware LB in front of the backend (`oaicalb`'s
`session_hash_addr`) uses it to pin the whole conversation to one
replica, so that replica's prefix cache is actually reused turn-to-turn
instead of every turn risking a `leastconn` hop to a cold-cache replica.
A backend with no such LB sees and ignores the extra header.

This is the layer where cross-region routing would live, and the reason
the session id is per-launch is exactly that: **cache affinity beats
load balancing for chat traffic.** A fresh `oaica launch claude` (a
`resume` included) starts a fresh proxy, and therefore a fresh session
id — the boundary is the launched session, because that is the unit whose
prefix is worth keeping warm.

**3. Which model backend — gateway fan-out.**
One `oaica-gateway` can front models that live behind *different*
upstreams: `gwModel.UpstreamAddr` overrides `gwConfig.UpstreamAddr`,
which is how one gateway serves two vLLM instances on two ports.
`distinctUpstreams(cfg)` counts the fan-out so "this gateway fronts two
upstreams" is visible without diffing JSON. Routing here is static
per-model: the model id in the request selects the backend, there is no
dynamic choice.

## Usage attribution: region × backend

The ledger row carries both, and they answer different questions:

| Field | Set by | Answers |
|---|---|---|
| `region` | gateway config | which box/region served this |
| `backend` | `oaicalb`'s `X-Katlb-Backend` header, captured into `ctxKeyBackend` and stripped before the response leaves the gateway | which replica, i.e. which GPU — e.g. `http://127.0.0.1:30106` = GPU0 |
| `session_id` | caller's `X-Session-Id` | which session, so concurrent sessions under one API key are told apart without issuing separate keys |

`backend` is empty when the request never reached a backend (blocked
before proxying, or the upstream error path never set the header).
`GET /usage/summary` in meterhub groups by `(region, backend)`, which is
what makes per-GPU load visible instead of aggregate fleet totals.

## What is deliberately not built

- **No cross-region failover.** The route plan fails over between
  *endpoints the user configured*, not between regions of our own fleet.
  A region being down is not currently something the client can route
  around.
- **No geo-selection.** Nothing picks a region by latency, cost, or
  data-residency. The region label is metadata, not a lever.
- **No distributed billing state.** meterhub is a single instance with a
  local SQLite file; each region reports to it. If usage ever outgrows
  one SQLite file, the ingest/query API does not change — only the
  storage backend would, the same "flat JSON now, could be a DB later"
  note that gateway's own ledger carries.

## The invariant that makes a second region safe

**meterhub is never on the request's critical path.** A region keeps
serving even if the aggregation layer is unreachable; it just reports
late (the reporter retries, and nothing in meterhub can fail a chat
completion). This is what allows a region to be added, moved, or taken
down without touching inference availability — and it is why the
per-box JSONL ledger, not meterhub, is the durable audit trail. Any
future routing layer must preserve this: **a routing decision may depend
on remote state, but serving may not depend on a remote service being
up.**

Entitlement and cap checks follow the same rule through a short-TTL
read-through cache — see `docs/BILLING_ENTITLEMENT.md`.
