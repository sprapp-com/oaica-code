# Editions, and where the free/paid line runs

This document is the decision of record for the licence boundary. It says which
editions exist, which parts of a launch each one unlocks, and why the line is
where it is. The licence texts it summarises are `EULA.md` (proprietary) and
`LICENSE` (upstream Ollama's MIT, unaffected); the plain-language summary for
buyers is `docs/LICENSING.md`.

## The rule

**Free = it works. Paid = you're optimising.**

That one line decides every case below. Anything a launch needs to *run at all*
is free, with no key and no network call. Anything that makes a launch *better
than it has to be* — cheaper, longer-context, finer-grained, guided — is paid.

It replaced an earlier rule ("any tier-routing flag at all is paid") that
wallpapered off the secondary tier, so a fresh install had no subagent model and
`claude` launched degraded. Gating the thing that makes the product work is the
fastest way to make nobody buy the thing that makes it better.

## The editions

| Edition | Who | Cost | Key | oaica's own models | Ends |
|---|---|---|---|---|---|
| **Community** | anyone | free | none | **no** (`--sonnet-model` etc. point at *your* models) | never |
| **Trial** | a fresh install | free | none | yes, everything | 14 days after first run |
| **Education** | students, staff, teaching labs | free | yes (institutional) | **no** — same as Community | yearly renewal |
| **Personal** | one developer | paid | yes | yes | while the subscription is paid (see below); a version licence is perpetual |
| **Team** | a company | paid, per seat | yes | yes | as Personal, plus org limits |

### New sales are subscriptions; perpetual is now the version licence

A Personal licence bought today is a subscription: the paid features, updates
and standard support last while it is paid, and lapse to the free capability set
when it ends. The perpetual shape now exists only as the **version licence** —
capped, Phase 3, off by default: USD 299 once, or a founder version licence at
USD 249 once, each capped at 50 sales. It grants perpetual use of the eligible
versions bought, with 12 months of eligible updates and support; never describe
it as lifetime updates or lifetime support.

Anyone who bought a perpetual licence earlier keeps it exactly as sold. Those
licences are never converted into a subscription, in code or in copy.

Trial and Education are the two free doors. They differ in what they unlock:
Trial is *everything for 14 days* so a buyer can evaluate the paid features on
real work; Education is *Community, permanently, with a key* so a lab does not
have to reinstall every term and does not silently depend on a feature that
lapses mid-course.

**Education gets exactly Community's capability set, and nothing more.** Not a
narrowed trial, not a discount on paid — the same free tier, with a
non-expiring key and an institutional identity on it. The reasoning: an
education licence is a distribution and goodwill channel, not a revenue line,
and the moment it unlocks oaica's first-party models it stops being a gift and
becomes a way to consume our margin for free. Keeping oaica's models out of
Education also keeps the incentive honest — a student who wants them can buy
Personal at the same price as anyone.

## What each flag/route costs

The gate is `requiredLaunchTier` in `cmd/launch/launch_tier.go`, consulted once
per launch at `gateLaunch`. The free/paid split for the launcher flags
(`flagTier`):

| Flag | Tier | Why |
|---|---|---|
| *(none)* | free | one primary model, local or your own remote |
| `--sonnet-model` | **free** | the secondary/subagent tier; without it a launch has no second tier at all |
| `--plan <name>` | **free** | a saved bundle — but see "plans" below, its legs are judged |
| `--haiku-model` | paid | the cheap background tier: an optimisation, not a requirement |
| `--oversize` | paid | context escalation past the primary's window |
| `--shard` | paid | traffic weighting within a route |
| `--wizard` | paid | guided setup — the natural upsell |
| `--route-policy local-first` (default), `local-only`, `remote-only` | **free** | the policies that *pin* routing; local-first is the default, so naming it must not cost what omitting it does not |
| `--route-policy remote-first`, `auto`, `weighted` | paid | the policies that choose a leg for you or split traffic |
| `--route-policy <anything else>` | paid | an unrecognised value falls on the paid side, so a typo cannot smuggle one through |

Models are judged separately, by `launchModelTier`: the user's own Claude login
forwarded untouched is free; a vendor provider row the catalog marks paid
(MiniMax, Z.AI, OpenAI, Anthropic's API, …) is paid; a free model *on* a paid
provider (`:free`/`-free`, or an explicit zero cost in the catalog) stays free,
because the user is not spending our money; and oaica's first-party router
models are paid on every model.

### Plans carry the paid legs inside them

`--plan` is free as a flag, but a `TierPlanProfile` can store `HaikuModel`,
`OversizeModel`, a non-default `RoutePolicy`, and `ToolModelDefault` — and
`resolvePlanModels`/`resolvePlanTier` hand those straight to the runner
**without ever putting them in `args`**. Scanning `args` for flags alone would
therefore let a free `--plan` smuggle every paid tier. `planTier` resolves the
named plan with `PlanGet` and judges its stored legs against the same rules;
`argsTierRoutingTier` calls it. An unknown or unreadable plan stays *free* here
— whether the plan exists is the runner's error to report, not the gate's to
invent a paid verdict for.

### Standing tiers are judged too

`oaica config set haiku-model …`, `config set tool-model-default tertiary` and
`config set tool-model <name> tertiary` write `~/.oaica/config.json`, which every
launch reads as its default tier split — again without anything appearing in
`args`. One `config set haiku-model` would otherwise buy the paid background
tier permanently. `standingConfigTier` reads them, and a standing `sonnet_model`
stays free exactly as `--sonnet-model` does.

The standing check is scoped: it applies **only** when the launch names an
integration that consumes the tier split (Claude Code; `tierFlagsConsumer`).
Every other integration — and a plain `oaica run <model>` — never reads the
split, so a `haiku_model` sitting in `config.json` must not block them. Free is
everything a launch needs to run; blocking a single-model local run over a
setting that run never consults is the failure this tiering exists to avoid.

## Compared with Ollama

Ollama is the upstream this is forked from, and the honest comparison is on two
axes only — the local CLI, and the hosted service. Cells marked `?` are ones we
could not verify from a primary source; they are not estimates.

| | Ollama (local CLI) | oaica |
|---|---|---|
| Licence of the code | MIT | proprietary (EULA), upstream MIT part retained |
| Source available | yes | no (private repo; a published public surface of config + binaries) |
| Local models | free, uncapped | free, uncapped |
| Account needed | no | no |
| Multi-tier launch (primary/secondary/background/oversize) | built in | free for one primary + one secondary; paid beyond |
| Sharding / weighted routing | n/a | paid |
| Hosted models bundled | no (local only) | no — oaica ships no hosted service of its own |
| Price of the hosted cloud | ? | n/a (not offered) |

The difference that matters: Ollama gives you the engine and stops. oaica gives
you a *launcher* that arranges several models and several endpoints around a
task, and it is that arrangement — not the inference — that is licensed. Nothing
here takes away an upstream MIT right: the Ollama code in this repo stays MIT,
and anyone who prefers it unlicensed can build Ollama itself.

## Licence-key schema (planned, not yet implemented)

Today a licence file carries `key`, `instance_id`, and a `tier` of `"paid"` or
`"free"` (`cmd/launch/license.go`). That is enough for the split above, since
everything free is also key-less. Two additions are wanted for the editions in
this document, and neither exists yet — they need a matching change on the
licence server before any client reads them:

- `edition`: `community` | `trial` | `education` | `personal` | `team`. Decides
  labelling and the renewal reminder; it does **not** by itself unlock anything.
- `capabilities`: an explicit list, because a boolean per feature does not
  survive the third feature. The one that matters now is
  `first_party_models: false` on an Education key, which is what makes
  "Education = Community" enforceable rather than a sentence in this file.
  `currentLicenceTier` would treat a key whose capabilities lack
  `first_party_models` as `licenceFree` for oaica's own router regardless of its
  edition — narrowing only, never widening (`tier` keeps its
  paid-unless-explicitly-`free` default).

Until that ships, an Education key is a `paid`-tier key issued on trust, and the
rule lives in this document and in whoever issues them.

## Education verification (planned)

- Verify an institutional email, or a domain on a curated allowlist of
  universities and research institutes. A personal address (gmail, qq, …) is
  not enough on its own.
- Issue a key valid for a year; remind before it lapses. On lapse it degrades to
  **Community** — the same capability set — not to nothing. A course that ends
  mid-term must not break a student's working setup.
- No per-seat counting for students. A lab or a whole department is one
  institution, not a seat count.

## See also

- `docs/LICENSING.md` — what a buyer is told, plain language
- `EULA.md` — the proprietary terms the installer asks a user to accept
- `cmd/launch/launch_tier.go` — the rule, in code
