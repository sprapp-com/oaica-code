# Porting opencode's model catalog (models.dev) into oaica-code

**Date:** 2026-09-25
**Status:** Approved design, ready for implementation planning

## Problem

oaica-code's provider directory is hand-maintained. `cmd/launch/providers/providers.json`
holds 19 rows with hand-guessed base URLs, `cmd/launch/cloud_limits/cloud_limits.json`
holds 21 more hand-written context/output limits, and both must be edited by hand
whenever a provider ships a new model, reprices, or changes an endpoint. Every such
change needs a human to notice it, edit JSON, and push.

opencode does not have this problem because it does not maintain a catalog at all: it
reads **models.dev**, an MIT-licensed community database that sst (opencode's own
organisation) maintains and consumes. models.dev's README states it plainly: "We also
use it internally in opencode."

The goal is to adopt the same source, so that when models.dev is updated we port the
update by syncing a file, with no code change and no manual editing.

## Goal

- Provider rows, endpoints, credentials, model lists, context limits, and pricing all
  come from models.dev, ported verbatim.
- oaica's own first-party models (which models.dev does not know about) remain
  first-class picker entries.
- A "frequently used" section surfaces what this machine actually runs.
- Porting an upstream update is one command with zero edits to ported data.

## Decisions

Settled during design discussion; binding on the implementation.

1. **Show everything.** All 223 models.dev providers are reachable in the picker, not a
   curated allowlist.
2. **Frequently used is auto-tracked locally.** Launch counts per machine; no synced
   pin list, no manual configuration.
3. **First-party models come from an oaica overlay**, with their endpoint *resolved* at
   launch rather than hardcoded.
4. **The model list comes from models.dev**, intersected with a live `/v1/models` sweep
   when a remote is reachable, unfiltered when it is not.

## Verified against opencode's implementation

The mapping below was checked against opencode's actual source, not the published
schema docs: `packages/core/src/models-dev.ts` (the fetcher and the authoritative
`Model`/`Provider` schema) and `packages/opencode/src/provider/provider.ts` (the
`fromModelsDevModel` / `fromModelsDevProvider` mapping and the per-provider auth
loaders), both at branch `dev`, version 1.18.32.

Four findings changed this design and are incorporated below:

- The provider `env` field is an **array that is scanned in order, first *set* variable
  winning** — `provider.env.map((item) => envs[item]).find(Boolean)`. Taking `env[0]` would
  miss any provider whose first listed variable is the less common one.
- `tool_call` **defaults to true** when absent, not false
  (`toolcall: model.tool_call ?? true`).
- Cost carries **context-tiered pricing** (`cost.tiers[]`) and a second price band above
  200k tokens (`cost.context_over_200k`). Displaying only the base input/output rate
  would misprice long Anthropic contexts.
- Wire selection is **per-model before per-provider**: `model.provider.npm ?? provider.npm
  ?? "@ai-sdk/openai-compatible"`, and likewise `model.provider.api ?? provider.api`.

Also confirmed: opencode's default source is its own mirror `https://models.opencode.ai`,
overridable by `OPENCODE_MODELS_URL`. That mirror is **byte-identical** to
`https://models.dev/api.json` — both 4,927,248 bytes, the same 223 providers and the same
8177 models. This design uses models.dev as the default because it is the canonical,
MIT-licensed upstream the mirror copies, and accepts the mirror via `--url`.

## Non-goals

- Replacing `remotes.json`, `aliases.json`, or the local daemon model path. These keep
  their current semantics and keep winning over catalog data.
- Mirroring models.dev into our own hosted file. That would reintroduce the pipeline
  dependency this design exists to remove.
- **`experimental.modes` expansion.** opencode synthesises extra models from this field
  (`<id>-<mode>`, with mode-specific cost and request body). oaica reads and ignores it.
- **Reasoning variants.** opencode derives provider-specific reasoning option sets
  (`ProviderTransform.reasoningVariants`). Out of scope; oaica reads `reasoning` as a
  boolean capability mark only.
- **Signed-request providers.** Bedrock, Vertex, Azure and similar cannot work through a
  plain base URL; they are listed but hidden by default. See "Provider coverage".

## Architecture

Three layers, lowest precedence first. A higher layer always wins.

### Layer 1 — models.dev catalog (ported verbatim)

- Source: `https://models.dev/api.json` (~5.0 MB, 223 providers, 8177 models).
  Not `models.json` (397 KB) — that file carries model facts but no endpoints.
- Cache: `~/.oaica/cache/catalog/modelsdev.json`.
- Fetched by `oaica model catalog sync`.
- **Never edited by us.** The overlay corrects it at read time, so re-syncing can never
  lose a local fix and an upstream update can never be blocked.
- **Not embedded in the binary.** 5 MB of embedded catalog would dwarf the binary and go
  stale between releases. The cost is that a first run with no network shows no external
  providers; see "Offline behaviour".

### Layer 2 — oaica overlay (embedded + synced)

One file, `cmd/launch/providers/oaica.json`, embedded via `go:embed` and overridable by a
synced copy at `~/.oaica/cache/providers/oaica.json`. Three sections:

- `providers` — base URLs for providers models.dev omits, corrections to upstream data,
  and `hidden` flags.
- `models` — oaica's first-party models (`oaica-default`, `oaica-35b-a3b-1M`) with their
  display metadata.
- `limits` — context/output limits for ids models.dev does not carry.

Shipped ahead of this plan, as the first slice of the overlay: `providers.json` rows now
carry an optional `models` map (bare upstream id → context/output) for providers whose
`/v1/models` sweep cannot answer for them. z.ai's Coding Plan and MiniMax's serve no model
list at all on their Anthropic-compatible endpoints, and a key-gated sweep can be refused,
so without a declared list a billed plan had no rows in the picker at all. Providers that
do answer `/v1/models` declare nothing and are unaffected — swept ids win any collision.

This file supersedes both `providers/providers.json` and `cloud_limits/cloud_limits.json`.
Because all three sources are embedded, the merge is a one-time content move, not a
migration users have to perform — no compatibility shim is needed and no user loses data.
The old cache paths are no longer read.

Why `limits` must survive rather than being deleted: verified against upstream,
models.dev's `ollama-cloud` carries 24 models while our file lists 21, and of ours
`cogito-2.1`, `deepseek-v3.1`, `deepseek-v3.2`, `glm-4.6`, `glm-4.7`, `qwen3-coder`, and
`qwen3-next` are absent upstream entirely. Our `glm-5` says 202752 context; upstream says
1000000. Neither file is a superset of the other, so the overlay keeps the entries
upstream lacks and the merge decides per id.

### Layer 3 — user configuration (unchanged)

`~/.oaica/remotes.json`, `aliases.json`, and `local_servers.json` keep their present
meaning and take precedence over both catalog layers. A user who overrides a provider's
URL or pins a key gets exactly what they configured.

## Mapping models.dev onto oaica

Per provider:

| models.dev | oaica | Rule |
|---|---|---|
| `api` | `base_url` | 197 of 223 providers carry it; overridden per model (below) |
| `env[]` | `api_key_env` | scanned in order, **first set wins** |
| `name` | picker section title | e.g. "Z.AI Coding Plan" |
| `npm` | `wire` | `@ai-sdk/anthropic` → `anthropic`; everything else → `openai` |
| `id` | picker id prefix | |
| `models` | model rows | keyed by id |

Per model, with `provider` meaning the enclosing provider's values:

| models.dev | oaica | Rule |
|---|---|---|
| `id` | picker id | authoritative identifier |
| `name` | display label | |
| `limit.context` | `ContextLength` | |
| `limit.input` | input limit | kept separately from context |
| `limit.output` | `MaxOutputTokens` | |
| `cost.input` / `cost.output` | per-M pricing | |
| `cost.cache_read` / `cost.cache_write` | cache pricing | |
| `cost.tiers[]` | tiered pricing | context-band pricing, shown when present |
| `cost.context_over_200k` | second band | shown when present |
| `tool_call` | `ToolFormat = tool_calls` | **defaults true when absent** |
| `attachment` + `modalities.input` | vision capability | |
| `modalities.output` | output modalities | |
| `reasoning` | reasoning mark | |
| `temperature` | temperature control | |
| `release_date` | ordering/sort key | |
| `status` | `alpha` / `beta` / `deprecated` | **defaults `active`**; deprecated rows are marked, not hidden |
| `provider.npm` | wire override | **wins over** provider `npm` |
| `provider.api` | base-URL override | **wins over** provider `api` |

Absent optional fields degrade rather than reject: `cost` defaults every component to 0,
`limit` is required by opencode's schema and is present on all 8177 entries upstream, and
a missing `release_date` sorts last.

Two upstream quirks the overlay corrects, both found while writing this spec:

- `opencode-go` upstream is `https://opencode.ai/zen/go/v1` with `OPENCODE_API_KEY`.
  oaica's hand-written row says `https://opencode.ai/zen/go` — the version path is missing
  — and is marked "reference only — no api_key_env", so it stays unselectable for no
  reason. Porting fixes both.
- `zai-coding-plan` upstream reuses `ZHIPU_API_KEY`; oaica's key is
  `Z_AI_CODING_PLAN_API_KEY`. Overlay overrides the env name.

## Model id translation

Two rules that are not visible in the data and would otherwise cause silent 404s:

- **Anthropic dotted ids.** models.dev lists Anthropic models with dotted versions
  (`claude-haiku-4.5`). The Anthropic Messages API expects dashed native slugs
  (`claude-haiku-4-5`). Translate dots to dashes when speaking the Anthropic wire. This is
  lossless — no native Anthropic slug contains a dot. **The same substitution must not be
  applied to OpenAI ids**, whose native names legitimately keep dots (`gpt-5.4`).
- **Aggregator vendor prefixes.** For providers that proxy other vendors' models, the
  leading `openai/` or `anthropic/` segment selects the wire for that model, and
  `anthropic/` ids get the dashed translation. Everything else is OpenAI-compatible with
  the id passed through unchanged. This matters for the OpenRouter-shaped providers, whose
  ids keep their `vendor/model` form.

## Provider coverage

models.dev omits `api` for 26 providers, because their first-party SDK hardcodes the
endpoint or signs requests itself:

`anthropic`, `openai`, `google`, `groq`, `mistral`, `xai`, `cerebras`, `deepinfra`,
`togetherai`, `perplexity`, `cohere`, `venice`, `aihubmix`, plus SDK-signed providers
`amazon-bedrock`, `google-vertex`, `google-vertex-anthropic`, `azure`,
`azure-cognitive-services`, `watsonx`, `sap-ai-core`, `gitlab`, `cloudflare-ai-gateway`,
`vercel`, `v0`, `qvac`, `salad-cloud`.

The overlay supplies `base_url` for the first group — they are ordinary OpenAI- or
Anthropic-wire endpoints, so roughly 13 lines make them work. The SDK-signed group is
marked `hidden: true`: they cannot work through a plain base URL, so listing them would
offer the user a provider that cannot succeed. They remain reachable if a user adds an
explicit `remotes.json` entry, which always wins.

## Picker behaviour

Sections, in order:

1. **Frequently used** — up to 8 entries, auto-ranked.
2. **OAICA** — first-party models from the overlay.
3. **Providers** — all providers as sections, each listing its models inline. Providers
   with a detected credential sort first, then the rest, alphabetically within each
   group. A credential-detected provider is marked `●`, using opencode's rule: configured
   if **any** variable in `env[]` is set.

Section 3 is thousands of rows and is not collapsed, paged, or virtualised — the picker's
existing filter is the navigation mechanism, exactly as it already is for the full
local-model inventory. Typing narrows across every section at once, so a user reaches any
of the 8177 models by typing rather than scrolling. The ordering exists so the first
screen is useful without typing at all.

Each model row shows context length and input/output pricing per million, plus marks for
tools, reasoning, vision, and a `deprecated` marker where upstream sets that status.
Selecting a provider with no credential invokes the existing `ensureRemoteAPIKeyForModel`
prompt-and-persist path unchanged.

## Model list resolution

Per provider:

1. If the remote is reachable, intersect the models.dev list with its `/v1/models`
   response. This hides models a key cannot actually reach.
2. If the remote is unreachable, show the full models.dev list, each entry marked
   `unverified`.
3. A remote with no credential mechanism at all is treated as genuinely unauthenticated
   and is not swept (existing `remote_key_prompt.go` behaviour).

Inverting the current order means the common path no longer pays the per-remote sweep
timeout, and an offline machine still shows a full catalog.

## First-party endpoint resolution

oaica's own models have no fixed endpoint — the gateway lives on a different port on each
machine (8081 here, others on .46/.128). Resolution order, first hit wins:

1. `OAICA_GATEWAY_URL` environment variable.
2. A `local_servers.json` entry matching the model id. `oaica serve` already writes this
   file and entries are already `/health`-probed, so a stale entry is skipped rather than
   offered.
3. A `remotes.json` entry or alias, i.e. today's path.
4. Otherwise the model is listed but marked unavailable, with the reason.

No ports or hosts are hardcoded anywhere in the shipped overlay.

## Frequently used

`~/.oaica/usage.json`, machine-local, never synced:

```json
{
  "version": 1,
  "usage": {
    "oaica-default": {"n": 88, "last": "2026-09-25T11:20:04Z"},
    "anthropic/claude-opus-4-5": {"n": 41, "last": "2026-09-25T09:02:11Z"}
  }
}
```

Written on a successful launch. Ranked by count descending, most-recent as tiebreak. An
entry needs `n >= 2` to appear, so a single experiment never displaces the section. A
corrupt or unreadable file degrades to an empty section, never an error.

## Sync and offline behaviour

`oaica model catalog sync` follows `model_sync.go` exactly: `If-None-Match` with the
stored ETag, a 304 reuses the cached body, and a network failure falls back to the last
good copy. `--url` is accepted, and `file://` paths work for air-gapped hosts and tests.
`oaica remote sync` keeps its role but now targets `oaica.json`. (That is the
implemented command name — it was written as `oaica provider sync` earlier in
this document's drafting, which is not a command that exists; `oaica provider`
has no `sync` subcommand, `launch.ProviderSync` is wired to `remoteSyncCmd`.)

**Divergence from opencode, deliberately.** opencode treats the catalog as live
infrastructure: it re-fetches whenever the cached file is older than 5 minutes, guards the
cache with a cross-process file lock against racing CLIs, and refreshes in the background
every 60 minutes. oaica is a one-shot CLI with no resident process, so a background
refresher has nothing to live in, and paying a 5 MB fetch on a 5-minute timer would tax
launches that are already network-bound. oaica therefore keeps an explicit `sync` command
plus ETag revalidation, and shows the cache's age in the picker when it exceeds 30 days so
staleness is visible rather than silent.

Offline or first-run-with-no-network behaviour: the embedded overlay still provides our
own models and the provider corrections, local models still come from the daemon, and
explicit `remotes.json` entries still work. External providers simply do not appear, with
a single line saying so and naming the sync command. Nothing errors.

## Format-drift detection and repair

models.dev is a live third-party database. A silent upstream reformat would corrupt the
picker with no warning, so the sync path is built to notice, preserve evidence, refuse
anything it cannot read, and hand a repair path to an agent.

### Raw archive

Every distinct payload is kept verbatim, content-addressed, at
`~/.oaica/cache/catalog/archive/modelsdev-<sha256-12>.json`, indexed by
`archive/index.json` (sha, fetched_at, source URL, ETag, provider and model counts).
Identical re-syncs do not duplicate. Retention: the first-ever baseline plus the last four
distinct payloads, ~25 MB worst case, local only and never committed. This guarantees the
exact bytes any build ran against remain recoverable for a diff.

### Shape signature

`shape.json` stores the sorted set of provider-level and model-level keys with each key's
JSON type, hashed, plus the counts. It catches added, removed, and retyped fields cheaply,
without a per-field diff of 8177 entries.

### Contract check

A validator for every field oaica actually reads, mirroring opencode's own schema so that
we fail where opencode would: `env` an array of strings, `models` an object,
`limit.context` and `limit.output` finite numbers, `cost.*` numbers or absent, `tool_call`
a bool or absent, `id` and `name` non-empty strings. The contract is the set of fields
listed in the mapping tables above; anything not read is not constrained.

### Sync verdict

| Condition | Action |
|---|---|
| Bytes unchanged | no-op |
| Contract passes, shape unchanged | adopt silently |
| Contract passes, shape changed | adopt, print drift report, save new shape |
| Contract fails | **refuse**, keep last-good, write drift artifact, non-zero exit |

`--force` adopts a failing payload for inspection. `--accept-drift` records the current
shape as accepted, and is the only way to clear a refusal.

### Drift artifact

A refusal writes `~/.oaica/cache/catalog/drift/<ts>-<sha>.json` — the delta only, small
enough to read whole, never a pointer to two 5 MB blobs:

```json
{
  "source": "https://models.dev/api.json",
  "sha256": "9c11…", "prev_sha256": "4f2a…", "fetched_at": "2026-09-25T12:04:11Z",
  "verdict": "refused",
  "contract_failures": [
    {"path": "providers.groq.models.llama-x.limit.context",
     "expected": "number", "actual": "string", "sample": "163840"}
  ],
  "shape_changes": [
    {"path": "model.cost.tiers", "kind": "added"},
    {"path": "model.status", "kind": "retyped", "before": "string", "after": "null"}
  ],
  "counts": {"providers": {"before": 223, "after": 231},
             "models": {"before": 8177, "after": 8402}},
  "affected": {"providers": ["groq"], "models": 3},
  "opencode_schema": {"pinned_sha": "b71c…", "live_sha": "b71c…", "changed": false},
  "next_steps": ["oaica model catalog diff 4f2a…",
                 "oaica model catalog sync --accept-drift",
                 "docs/CATALOG_DRIFT.md"]
}
```

`affected` names which providers genuinely break rather than only that a field moved
somewhere. This artifact is the interface an agent acts on.

### Whose problem it is

The artifact records the sha256 of opencode's own `packages/core/src/models-dev.ts` and
refetches it on drift. If that schema changed too, upstream refactored and we follow it.
If it did not, models.dev shipped something opencode has not absorbed, which is likely an
upstream defect to report rather than chase. This distinguishes "our turn" from "theirs"
before any code is touched.

The refetch is best-effort. Offline, or if the file moves, the artifact reports
`"changed": null` rather than guessing, and the drift is still fully actionable from
`contract_failures` alone.

### Three remediation paths

The refusal is never a dead end; the artifact names the field, shows a sample, and the
agent picks one of three, each with an explicit command:

1. **Benign or additive** → `oaica model catalog sync --accept-drift`.
2. **Mapping needs a Go change** → edit the mapping, add the case to the frozen fixture
   and its test, then `--accept-drift` once tests pass.
3. **Upstream is wrong for us** → add an overlay entry in `oaica.json`, no Go change.

The loop is documented in `docs/CATALOG_DRIFT.md` and referenced from `AGENTS.md`, so an
agent working in this repo finds it without being told.

### Developer notification

- **Exit code and stderr summary** on every refusal.
- **A `DRIFT` marker** in the catalog cache directory, so any later `oaica` invocation —
  including a plain `oaica launch` — prints one line pointing at the artifact. Advisory,
  never blocking.
- **A generated, ready-to-run `gh issue create -F …` line** in that output.
- **A daily scheduled workflow on the fork** runs `oaica model catalog check` against
  upstream from our CI and opens or updates the issue itself. Drift therefore reaches the
  developer with no user involvement.

No user data is uploaded from user machines; the CI check is what provides fleet-wide
notice. This is a deliberate choice over shipping catalog state to the ledger.

### Commands

| Command | Purpose |
|---|---|
| `oaica model catalog sync` | fetch, validate, adopt or refuse |
| `oaica model catalog check` | fetch and validate, adopt nothing; `--json` for machines |
| `oaica model catalog diff <sha\|baseline>` | compare stored archives |
| `oaica model catalog drift --latest` | print the newest artifact and its next steps |
| `oaica model catalog sync --accept-drift` | clear a refusal, recording the new shape |
| `oaica model catalog status` | provenance: source, fetched_at, sha, counts, pinned opencode version and commit |

## Code impact

- New: catalog fetch/parse/merge, the overlay parser, usage tracking, the picker sections,
  id translation, and first-party endpoint resolution.
- New (drift): the archive writer and index, the shape signature, the contract validator,
  the drift artifact, `check` / `diff` / `drift` / `status` commands, the `DRIFT` marker,
  and `docs/CATALOG_DRIFT.md` with its `AGENTS.md` pointer.
- New (CI): a daily workflow on the fork running `oaica model catalog check --json` and
  opening or updating a drift issue. No change to the existing release workflow, since the
  notification path chosen is CI-side rather than a release gate.
- Replaced: `providerCatalog()` reads the overlay instead of `providers.json`;
  `cloudModelLimits` merges from the catalog rather than `cloud_limits.json`;
  `builtinRemotes()` sources its env-var gating from the catalog's `env[]` arrays.
- Removed: `cloud_limits_catalog.go`, `cloud_limits_sync.go`, and the two JSON files they
  serve, their content folded into `oaica.json`.
- Unchanged: `userRemote`, `RemoteDescriptor`, `resolveLaunchEndpoint`, the alias
  mechanism, and the daemon/local model path.
- The refactor landed on 2026-09-17 (uncommitted) — data-driven providers, cloud limits,
  and `remote_key_prompt.go` — is the foundation this builds on, and its tests
  (`clearCatalogKeys`, `stubUserRemoteModels`, `stubBareIndex`) continue to apply.

## Error handling

- Unparseable or absent catalog → overlay only, one notice, no error. This is distinct
  from a contract failure: JSON that does not parse is not adopted and not archived, while
  JSON that parses but breaks the contract is archived for diffing and refused.
- Unparseable overlay → embedded overlay, since the embedded copy is always present.
- A provider entry missing both `base_url` and a resolvable upstream `api` → skipped
  silently rather than listed as broken.
- A model missing `cost` → all components 0, so the pricing column reads free rather than
  absent; a missing `limit` cannot occur upstream (opencode's schema requires it) but
  degrades to `ctx ?` if it does.
- An id that fails Anthropic translation, or a vendor prefix we do not recognise → passed
  through unchanged, never dropped.

## Testing

Hermetic, following the existing seams:

- models.dev fixture covering: a provider with `api`, one without, one with a multi-value
  `env` where only the second variable is set, a vendor-prefixed OpenRouter id, a model
  with no `cost`, a model with `tiers`, a model with `context_over_200k`, a model with
  `status: deprecated`, and a model with a per-model `provider.api`/`provider.npm`
  override.
- Mapping assertions pinned to opencode's rules: `tool_call` absent → tool-capable;
  `env[]` first-set-wins; per-model `provider.npm` beats provider `npm`; cost components
  default to 0.
- Id translation: `claude-haiku-4.5` → `claude-haiku-4-5` on the Anthropic wire;
  `gpt-5.4` untouched on the OpenAI wire; `anthropic/...` prefix translated through an
  aggregator; an unknown prefix passed through.
- Overlay precedence: overlay `limits` beats upstream; overlay `base_url` fills a missing
  `api`; overlay corrects an env name; user `remotes.json` beats all three.
- Offline: no catalog on disk → picker shows overlay + local models, no error.
- Model list: reachable remote intersects, unreachable remote shows the full list marked
  unverified, keyless remote is not swept.
- Usage: ranking, `n >= 2` floor, tiebreak by recency, corrupt file → empty section.
- Endpoint resolution: env var beats `local_servers.json` beats `remotes.json`; a dead
  `local_servers.json` entry is skipped; nothing resolves → marked unavailable.
- Regression: `zai-coding-plan` env override and `opencode-go` corrected URL asserted
  directly, since both are live bugs the port fixes.
- Drift, each driven by a synthetic payload rather than a network call: unchanged bytes →
  no-op; additive shape change → adopted with a report; retyped `limit.context` → refused
  and last-good retained; a second provider breaking the same field → both named in
  `affected`; `--force` adopts a failing payload; `--accept-drift` clears the refusal and
  the next identical sync is a no-op. The frozen fixture's shape hash is asserted, so a
  mapping change that silently stops reading a field fails CI.
- Drift artifact: parses as JSON, contains only the delta, and `next_steps` commands all
  exist in the command surface.

## Rollout

`go test ./...` green, `oaica model catalog sync` run against the live endpoint on this
box, and a picker smoke test confirming: first-party models present with resolved
endpoints, an external provider listed with upstream context and pricing, the frequently
used section populated after two launches, and `--url file://` working for the air-gapped
path.

The drift path is exercised against the live archive: sync twice (second is a no-op),
then run `oaica model catalog check` and confirm it reports no drift, and finally validate
a deliberately corrupted copy via a `file://` URL to confirm the refusal, the artifact,
and the `DRIFT` marker all fire without touching the adopted catalog.

Rollback is `git revert` — the embedded overlay means a reverted binary is immediately
self-consistent, with no cache or user-file migration to undo. The archive and drift
directories are pure cache and can be deleted at any time without loss.
