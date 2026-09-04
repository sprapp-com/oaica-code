# Claude Code model tiers with `oaica launch claude`

Claude Code picks a model *tier* per request and resolves each tier through
an env var the launcher sets:

| Request | Tier | Env var |
|---|---|---|
| Plan mode under `/model opusplan`; `/model opus`; `--model opus` | Opus | `ANTHROPIC_DEFAULT_OPUS_MODEL` |
| Execution under `/model opusplan`; `/model sonnet`; `--model sonnet` | Sonnet | `ANTHROPIC_DEFAULT_SONNET_MODEL` |
| Subagents | — | `CLAUDE_CODE_SUBAGENT_MODEL` (= the Sonnet model) |
| Quick background calls (titles, summaries) | Haiku | `ANTHROPIC_DEFAULT_HAIKU_MODEL` (= the primary) |

The launcher always passes `--model <primary>` to Claude Code, so **with a
plain launch every main-conversation request goes to the primary**. The
`--sonnet-model` backend is reached three ways: `/model opusplan` (plans on
the primary, executes on the secondary), `/model sonnet` / `--model sonnet`
(main conversation on the secondary), and subagents (always the secondary).
Haiku-tier background calls stay on the primary in every mode. `--model
opus` / `--model sonnet` pin the *main conversation* only; subagents and
Haiku calls keep their env-var tiers.

Every request carries the resolved model id. The launcher runs ONE local
Anthropic→OpenAI translation proxy with a **routing table keyed by that id**,
so different tiers can go to different backends.

```bash
# one model everywhere (default)
oaica launch claude --model kat-awq

# plan with a cloud model, execute locally — any mix of sources
oaica launch claude --model deepseek-v4-flash:0731-cloud -- --sonnet-model kat-awq
oaica launch claude --model openrouter/anthropic/claude-sonnet-4 -- --sonnet-model bonsai:local
```

## Where a model name can come from

Resolution order for the **primary** (first match wins), `cmd/launch/tier_routing.go`:

| Name | Source | Endpoint used |
|---|---|---|
| `<remote>/<id>`, or a bare id exactly one remote serves | `~/.oaica/remotes.json` | the remote's `base_url` + key |
| `<model>:local` | a running `oaica serve` | its origin (+ its `--api-key` if it has one) |
| `router/<id>` or `oaica/<id>` — or a bare id the OAICA router lists (`OAICA_HOST`, default api.oaica.com); `<id>+<lora>[+…]` composites resolve by `<id>` and are sent upstream whole | router | `<host>/v1` + `OAICA_API_KEY` |
| `ollama/<id>` or `daemon/<id>` — or anything the local Ollama daemon answers `POST /api/show` for (pulled models **and** `:cloud` aliases) | daemon (`OLLAMA_HOST`) | `<daemon>/v1` |

A bare id that several remotes serve is refused with a hint to write
`<remote>/<id>`. A name found nowhere fails before anything starts, naming
every place that was tried (and the fix when the router rejected the key).

## `--sonnet-model` resolution

- Primary on a **user remote**: an un-namespaced secondary means *on that
  same remote* — the id is passed through even if the remote's `/models`
  does not enumerate it (`muse-spark-1.2` on opencode-go, `openai/gpt-5` on
  an OpenRouter remote). A bare id that other remotes or the router also
  serve is never silently rerouted. To leave the primary's remote, be
  explicit: `<remote>/<id>`, `<model>:local`, `router/<id>`, `ollama/<id>`.
- Primary on the router / daemon / `oaica serve`: the secondary resolves
  with the primary table above.

## Why this exists

- Before 2026-08-26 `--sonnet-model` had to be on the **same** remote as the
  primary (one proxy = one base URL + key).
- Router and daemon models bypassed the translation proxy: Claude Code was
  pointed straight at the host and expected it to speak `/v1/messages`. The
  public gateway only speaks `/v1/chat/completions`, so a fresh install's
  `launch claude --model kat-awq` died with `unrecognized_model`. Now every
  source goes through the proxy.

## Security notes

- The proxy listens on 127.0.0.1 but loopback is shared with every process
  and user on the machine, so it requires a **per-launch random token**
  (`Authorization: Bearer` / `x-api-key`). Claude Code receives that token
  as `ANTHROPIC_AUTH_TOKEN`; the real upstream keys live only inside the
  proxy and never enter the child environment.
- 2026-08-28 audit: every bind site for this proxy (`ListenAnthropicOpenAIProxy`,
  `ServeAnthropicProxyForRemote`) hardcodes `127.0.0.1` — there is no flag or
  parameter anywhere in this codebase that can bind it to a non-loopback
  address. Unlike `oaica serve` (which CAN bind `0.0.0.0` and is gated by
  `--api-key`/`--insecure`), this proxy structurally cannot be exposed to the
  network without a code change first.
- The proxy writes a local-only request log (`~/.oaica/requests.log`:
  model, backend label + redacted URL, sizes, status — never content).
  Backend labels: `daemon:ollama …`, `remote:<name> …`, `router:oaica …`,
  `local-serve:local …`.
- Each launch generates a random `X-Session-Id` (`newProxySessionID`, 2026-08-28)
  sent upstream on every request that launch's proxy forwards — one per
  launched conversation, not per request. A consistent-hash load balancer in
  front of a multi-replica backend (e.g. `tools/oaicalb`'s
  `session_hash_addr`) can use this to pin the whole conversation to one
  replica, so that replica's own prefix cache actually gets reused
  turn-to-turn instead of scattering across replicas under plain leastconn.
  A backend with no such LB just ignores the header.
- Tool calling is gated per endpoint (`--force-tools` downgrades refusal to
  a warning).
- Upstream streaming is bounded only by connection setup and by Claude Code
  disconnecting — a slow local model may stream as long as it needs.
- Ollama cloud models (`…:cloud`) need `ollama signin` on the daemon.

## Route policies + fallback (2026-08-31)

`oaica launch claude --route-policy <p>` decides what the launch proxy does
when the selected backend starts failing. Only relevant when two legs are on
DIFFERENT base URLs (a cross-remote/daemon `--sonnet-model` split, or future
multi-remote plans); a single-remote launch builds no fallbacks and behaves
byte-identically no matter the policy.

| policy | when the selected leg's breaker is OPEN |
|---|---|
| `local-first` (default) | fail over to the local leg; else any healthy alternate |
| `remote-first` | fail over to the remote leg; else any healthy alternate |
| `auto` | like `local-first`, PLUS session escalation — see below |
| `local-only` | never leave local — request fails visibly rather than crossing |
| `remote-only` | never leave remote — same |
| `weighted` | splits HEALTHY traffic across every leg carrying a `Weight` — see below; not a failure policy |

### `auto`: session escalation (2026-08-31, v1.1)

No longer an alias of `local-first`. Under `auto` the proxy additionally
counts consecutive failed requests per session (the same signals that feed
the breaker: 5xx after the proxy's retry budget, or transport error — 4xx
and 429 never count). After 2 consecutive failures (`autoEscalateAfterFails`)
that session's NEW requests skip straight to the strongest healthy secondary
leg — the largest-ContextWindow fallback, with the `--oversize` leg included
when larger — without waiting for the breaker to open. Escalation persists
through a lucky success (so the session isn't bounced back onto a flapping
primary) and decays 10 minutes after the last failure
(`autoEscalateHoldFor`, "minutes of healthy service"). If the escalation
target's own breaker is OPEN, or a pinned locality forbids it, the request
stays on the base route and fails normally — escalation degrades, never
crosses. Like the breaker, it is only consulted in `selectRoute`: an
in-flight response is never re-routed mid-stream, and `X-Oaica-Route`
always names the leg that actually served.

Breaker mechanics (`cmd/launch/route_policy.go`): 3 consecutive failures
(5xx after the proxy's retry budget, or transport error — 4xx/429 don't
count) open the circuit for 90 s; any success or a healthy `/models` probe
(30 s poll of every distinct base URL) closes it. Never re-routed mid-stream
— only a NEW request picks the other leg. Every response carries
`X-Oaica-Route: <label>` naming the leg that actually served it, so a silent
failover is diagnosable and (at the gateway) attributable.

### `weighted`: consistent-hash traffic split (2026-09-04)

Every other policy above is failure-driven: `Fallbacks` sit idle serving
nothing until the selected leg's breaker opens. `weighted` is different —
it can steer HEALTHY traffic away from the base leg on purpose, splitting
it across every route (base + `Fallbacks` + `Oversize`) that carries a
nonzero `Weight`, in proportion to that weight.

Session-sticky: the proxy hashes each launch's `SessionID` onto a
consistent-hash ring built from the weighted, currently-healthy legs (an
`open` breaker removes a leg from the ring, same as ordinary failover), so
one conversation always lands on the same replica for the life of the
session — its prefix cache keeps getting reused turn-to-turn, exactly like
plain `X-Session-Id` pinning above. Only the split ACROSS DIFFERENT
sessions follows the weights. A route with `Weight` 0 (every route, unless
opted in) is excluded from the ring; with fewer than 2 weighted legs the
policy has nothing to split and silently falls through to ordinary
failover-only behavior — so turning `weighted` on is never a regression by
itself, it only changes anything once ≥2 legs are actually weighted.

**Setting weights** — two ways, flag wins for the launch it's given on:

- `remotes.json` per remote: `"weight": 3` (0/omitted = opt-out, the
  default for every existing remote).
- `--shard <model>:<weight>` (repeatable, same picker vocabulary as
  `--sonnet-model` — `<remote>/<id>`, `router/<id>`, bare id): resolves
  `<model>` to its `BaseURL` and stamps `<weight>` onto whichever existing
  route (base or a fallback) already sits on that URL, for THIS launch
  only. Does not create a new leg — a `--shard` id that doesn't match any
  existing base/fallback route is a silent no-op, same as "fewer than 2
  weighted legs" above. Malformed entries (no `:weight`, non-positive,
  non-integer) are also dropped rather than failing the launch.

```
oaica launch claude \
  --sonnet-model gateway46/oaica-35b-a3b-vision \
  --shard gateway46/oaica-35b-a3b-vision:3 \
  --shard <other-remote>/<model>:1 \
  --route-policy weighted -- --dangerously-skip-permissions
```

Requires at least 2 genuinely healthy backends on different base URLs to
have any effect — `oaica doctor` shows which remotes are actually
reachable before you weight them.

### Oversize crossover + remotes.json defaults (2026-08-31, v0.5.0)

`--oversize <model>` (same picker vocabulary as `--sonnet-model`): the
larger-context leg that serves any request the current leg cannot hold —
the auto-compaction call near a 262k ceiling being the canonical case.
Size-based (no compaction prompt sniffing): decided inside the context-fit
clamp, when the current leg's fit budget is exhausted, and only if the
oversize leg's probed window is strictly larger, breaker-healthy, and on
the permitted side of a pinned policy. Pinned policies (`local-only`,
`remote-only`) fail honestly instead of crossing. The oversize leg also
serves as a breaker fallback leg and gets the 30s health probe.
`X-Oaica-Route` always names the leg that actually served.

remotes.json now accepts `"route_policy": "local-first|remote-first|auto|
local-only|remote-only|weighted"` per remote as the default for launches
using it, plus `"weight": N` (see `weighted`, above) to opt that remote
into consistent-hash traffic distribution; the `--route-policy` flag wins
over the remote's own default, and `--shard` overrides `weight` for a
single launch without editing the file. `oaica doctor` prints every
remote's reachability + wire + route_policy and the daemon leg — exit 1 on any
failing probe, so cron/scripts can grep.

## Interactive launch wizard (2026-08-31)

A plain interactive `oaica launch claude` (no explicit `--model`, no
tier/policy/oversize/plan flags) now walks the remaining tiers after the
picker:

1. **Primary model** — the existing picker (unchanged).
2. **Sonnet/subagent tier** — same picker list, `(same as primary)` first and
   the default (Enter keeps the single-model launch).
3. **Compaction/oversize model** — only models whose PROBED context window
   (the same 2s `/models` probe the proxy uses) is strictly larger than the
   primary's qualify; `(none — fail honestly at the ceiling)` is the default.
   With no probe answer and no larger model, the step offers nothing.
4. **Route policy** — the six `--route-policy` values, `local-first` default
   (`weighted` is available here too, but the wizard has no step for setting
   per-leg weights — use `--shard`/`remotes.json` `weight` for that).

A one-line preview prints (e.g. `fallback: a <-> b · oversize: c (256k) ·
policy: remote-first`) and the choice can be saved as a named plan (blank
skips): `oaica plan list` / `oaica plan show NAME` show the oversize +
policy columns, `oaica plan set NAME --model a --sonnet-model b --oversize c
--route-policy remote-first` builds one by hand.

Precedence is unchanged and now covers the stored fields: **flag >
plan > remotes.json `route_policy` > local-first**. Old plans.json files
missing `oversize_model`/`route_policy` load unchanged (missing = empty =
today's defaults).

Flag-only and non-interactive launches (`--model`, `--yes`, scripts, cron)
never see the wizard — their behavior is byte-identical.
