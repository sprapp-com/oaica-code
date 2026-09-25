# Claude Code model tiers with `oaica launch claude`

Claude Code picks a model *tier* per request and resolves each tier through
an env var the launcher sets:

| Request | Tier | Env var |
|---|---|---|
| Plan mode under `/model opusplan`; `/model opus`; `--model opus` | Opus | `ANTHROPIC_DEFAULT_OPUS_MODEL` |
| Execution under `/model opusplan`; `/model sonnet`; `--model sonnet` | Sonnet | `ANTHROPIC_DEFAULT_SONNET_MODEL` |
| Subagents | — | `CLAUDE_CODE_SUBAGENT_MODEL` (= the Sonnet model) |
| Quick background calls (titles, summaries) | Haiku | `ANTHROPIC_DEFAULT_HAIKU_MODEL` (= the primary unless `--haiku-model`/`haiku_model` sets a tier) |

The launcher passes `--model <primary>` to Claude Code, so **with a plain
launch every main-conversation request goes to the primary**. The
`--sonnet-model` backend is reached three ways: `/model opusplan` (plans on
the primary, executes on the secondary), `/model sonnet` / `--model sonnet`
(main conversation on the secondary), and subagents (always the secondary).
`--model opus` / `--model sonnet` pin the *main conversation* only;
subagents and Haiku calls keep their env-var tiers.

**`--haiku-model` (or the standing `haiku_model` key) is the tier worth
setting** (2026-09-25): Claude Code sends its background work there, and a
haiku tier left unset bills it at the primary's price — the primary's model
serves every title-generation and topic-detection call. With a tier split the
launch runs `--model opusplan`, in which Claude Code resolves its **opus and
haiku slots from its own built-in catalog**, ignoring
`ANTHROPIC_DEFAULT_{OPUS,HAIKU}_MODEL` entirely (probed against 2.1.282:
only the Sonnet slot's value ever reached the wire). Those requests therefore
arrive carrying real Anthropic family ids (`claude-haiku-4-5-20251001`), and
the proxy maps them onto the leg the plan owns for that tier
(`proxyRouteTable.FamilyLegs`, built by `tierFamilyRoutes`) — the opus slot to
the primary, sonnet to the secondary, haiku to the haiku leg, whether those
legs are native (`claude/haiku`) or an ordinary remote/router model
(`zai-coding-plan/glm-4.5-air`). A leg literally named for a tier claims that
family if the tier is opus/sonnet/haiku — with two limits. It must be a
genuinely distinct SONNET leg (`--sonnet-model claude/opus`), the one slot
whose env value the launcher controls, so a native primary's own tier name
cannot claim a family through the copy of itself that fills the empty sonnet
slot (`--model claude/haiku --haiku-model <remote>` leaves the haiku family to
the remote leg); and `--haiku-model claude/opus` cannot take the opus family
(the main plan-mode conversation) off the configured primary, because a leg on
the opus or haiku slot only claims its own slot's family. A tier name outside
those three slots (`--haiku-model claude/fable`) always claims, since no slot
owns that family and the alternative is the primary's model, which cannot serve
an Anthropic id at all. If a sonnet leg's claim collides with a haiku leg the plan
also configured (`--sonnet-model claude/haiku --haiku-model zai/glm-4.5-air`),
the sonnet leg wins that family — deliberately: the sonnet slot's env value is
the one producing those ids, and the alternative (the configured haiku leg
serving the execution turns Claude Code sends with them) would put your main
coding traffic on a cheap background model.

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
  explicit: `<remote>/<id>`, `<model>:local`, `router/<id>`, `ollama/<id>`,
  or a native Anthropic tier.
- `claude/<tier>` / `anthropic/<tier>` (`claude/sonnet`, `anthropic/opus`,
  `claude/haiku`) is the user's own Anthropic login, spent through the
  passthrough leg — the same reserved meaning it has as a primary. Bare tier
  names win outright (no remote serves a model named `sonnet`); the
  ambiguous `anthropic/<slug>` shape still prefers a remote that enumerates
  it, so an aggregator's own Claude slug (`anthropic/claude-sonnet-4.5` on an
  OpenRouter primary) keeps going to that remote.
- Primary on the router / daemon / `oaica serve`: the secondary resolves
  with the primary table above.

## Why this exists

- Before 2026-08-26 `--sonnet-model` had to be on the **same** remote as the
  primary (one proxy = one base URL + key).
- Router and daemon models bypassed the translation proxy: Claude Code was
  pointed straight at the host and expected it to speak `/v1/messages`. The
  public gateway spoke only `/v1/chat/completions` then (it has since added
  `/v1/messages` on the Anthropic wire plus the `/v1/manifest/`, `/v1/pull/`
  and `/v1/catalog` pull routes, and `kat-awq` still does not serve the
  Anthropic wire on its own), so a fresh install's
  `launch claude --model kat-awq` died with `unrecognized_model`. Now every
  source goes through the proxy, except a native `claude/*` / `anthropic/*`
  leg, which bypasses it entirely by design.

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
  a warning). It is a launcher-level flag but is NOT registered as a
  top-level one, so it must come after `--`:
  `oaica launch claude --model X -- --force-tools` (a bare `--force-tools`
  errors with `unknown flag`). `--brief-mode` is the same, and is otherwise
  undocumented.
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

`--oversize none` drops the leg for one launch — the way to shed an
oversize model a plan stored, without editing the plan. The value is
spelled because an *empty* one is not a drop: `--oversize=` (what an unset
shell variable expands to) is refused by name, same as `--route-policy=`,
rather than silently reading as "not passed" and letting the plan refill
the stored leg. Drop the flag to keep the stored value.

remotes.json now accepts `"route_policy": "local-first|remote-first|auto|
local-only|remote-only|weighted"` per remote as the default for launches
using it, plus `"weight": N` (see `weighted`, above) to opt that remote
into consistent-hash traffic distribution; the `--route-policy` flag wins
over the remote's own default, and `--shard` overrides `weight` for a
single launch without editing the file. `oaica doctor` prints every
remote's reachability + wire + route_policy and the daemon leg — exit 1 when
a configured remote's `/models` probe fails or its `route_policy` is invalid,
so cron/scripts can grep. An unreachable local daemon does NOT fail: it
prints "unreachable (fine ...)".

`oaica doctor --report` prints the same checks plus an environment section
(version, platform, `~/.oaica` paths with their file modes, which credential
environment variables are set, how many providers are in `auth.json`) and
refuses to print at all if any credential value would appear in the output —
the text is scanned against every key the client holds before it is written.
Credential-embedded remote URLs (`https://KEY@host/v1`) print redacted in both
modes, including inside transport error messages.

## Interactive launch wizard (2026-08-31)

A plain interactive `oaica launch claude` (no tier/policy/oversize/plan/shard
flag passed) now walks the remaining tiers after the picker:

1. **Saved plans** — offered whenever `~/.oaica/plans.json` has entries:
   reuse the plan last launched from this directory (Enter), pick another
   saved plan, or start from scratch. A reused plan supplies every tier, so
   the tier steps below are skipped.
2. **Primary model** — the existing picker (unchanged).
3. **Sonnet/subagent tier** — same picker list. It leads with a "keep your
   tier for this launch" row when a standing `~/.oaica/config.json`
   `sonnet_model` or a typed `--sonnet-model` exists (Enter keeps it), then
   `auto`, then `(same as primary)`, then the rest; with nothing standing to
   keep, `(same as primary)` leads and Enter keeps the single-model launch.
4. **Haiku tier** — the cheap leg for Claude Code's background work; same
   picker list, same "keep" row for a standing `haiku_model` or a typed
   `--haiku-model`, then `(same as primary)` default (no `auto`: there is no
   "best recommended model" concept for cheap/background requests). Setting
   this is the one step that removes real spend (see the tier table above),
   so it is worth a deliberate choice.
5. **Compaction/oversize model** — only models whose PROBED context window
   (the same 2s `/models` probe the proxy uses) is at least the primary's
   qualify (`>=` is deliberate: an equal-window independent backend can still
   take over when the primary fails near the ceiling, even though the size
   crossover itself only fires for a strictly larger one — see below).
   The step opens with `(none — fail honestly at the ceiling)` (Enter) and a
   "probe the catalog for a larger-context fallback" row: the catalog is not
   scanned on the launch path (hundreds of remotes, seconds each), so
   discovery is that explicit opt-in. After a scan the candidates replace the
   menu; with no probe answer and no qualifying model it says so.
6. **Route policy** — five of the six `--route-policy` values
   (`auto` first and the default, then `local-first`, `remote-first`,
   `local-only`, `remote-only`). `weighted` is not offered: the wizard has no
   step for setting per-leg weights, so use `--shard` /
   `remotes.json` `"weight"` to pick it.

A one-line preview prints (e.g. `fallback: a <-> b · oversize: c (256k) ·
policy: remote-first`) and the choice can be saved as a named plan: Enter at
the save prompt overwrites the last-used plan name, blank skips, and a write
failure warns and continues rather than losing the launch. `oaica plan list` /
`oaica plan show NAME` show the oversize + policy columns, `oaica plan set
NAME --model a --sonnet-model b --oversize c --route-policy remote-first`
builds one by hand.

Precedence for both non-primary tiers is the same ladder: **flag >
`--plan`'s stored field > wizard > `~/.oaica/config.json`
(`sonnet_model`/`haiku_model`, set with `oaica config set`; a key is cleared
with `oaica config set <key> -`) > same-as-primary**. An ANSWERED wizard step
is a per-launch choice and outranks the standing config; a step left at Enter
(its leading "keep" row, or `(same as primary)`) keeps the standing tier.

A saved value that fails to RESOLVE is reported and ignored for that launch
rather than failing it — a stale key must not break every launch in the fleet
— and only the failing tier is dropped: the other saved key still applies
(2026-09-25; dropping both would re-bill Claude Code's background work at the
primary's price, the cost `haiku_model` exists to remove). "Fails to resolve"
means what `buildTierPlan` can detect before anything starts: a name no
remote/router/daemon/local serve answers for. A saved value that resolves to a
real backend but the backend does not actually serve is NOT detected here — an
un-namespaced id is passed to the primary's remote on purpose (see
`--sonnet-model` resolution above), so a typo inside an existing remote fails
at the first request instead. Check the launch's `tiers:` line, which prints
what each tier actually resolved to. `oaica config show` prints both keys and
the file path.

An unattributed failure — one carrying no tier prefix at all, which means it is
the primary's own error, the only kind `buildTierPlan` does not wrap and the one
no saved value can influence — does NOT drop either saved key. It retries the
identical plan once (resolution reaches the network, so a transient failure is
possible) and, if the retry fails the same way, fails the launch on the real
error. Only a failure whose message carries the `--sonnet-model:` or
`--haiku-model:` prefix drops that one key: stripping a standing tier for an
error it did not cause would silently turn the launch single-model at the
primary's price, the exact cost `haiku_model` exists to remove.

The tier is identified by the flag **prefix** on the error, not by the flag text
appearing anywhere in it: the leg's own message quotes the model name it could
not resolve, so a botched value that merely reads like a flag
(`--haiku-model ghost--sonnet-model:x`) must not make a haiku failure look like
it named the sonnet tier as well — that would cost the healthy `sonnet_model`
for that launch.

Plan > remotes.json `route_policy` > local-first also still holds for the
route policy. Old plans.json files missing `oversize_model`/`route_policy`
load unchanged (missing = empty = today's defaults).

Any interactive, non-restore launch walks the wizard — a plain interactive
`--model` included, whose Enter-key defaults keep what was typed — and it is
skipped only when a tier/policy/oversize/plan/shard flag was passed. It never
runs non-interactively: scripts and cron see no wizard at all, so their behavior
is byte-identical. `--wizard` forces the steps past that gate and errors with
`launch wizard: --wizard requires an interactive session` when non-interactive.

One flag it does not force past is `--plan`, and that combination is refused
rather than ignored: `launch wizard: --wizard cannot be combined with --plan
<name>`. A plan already supplies the tiers, and two of the wizard's own
behaviours would turn "adjust this plan" into "replace it" — its first step
offers the plan used last *from this directory* (Enter there would swap the
typed plan out), and its tier steps lead with the standing
`~/.oaica/config.json` tiers rather than the plan's, so their Enter-key defaults
would drop the plan's tiers. Drop `--plan` to walk the tiers from scratch, or
run without `--wizard` to use the plan as saved.
