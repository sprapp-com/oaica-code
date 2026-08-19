# Per-model wire-protocol routing

> **Status:** design spec (v1). Companion to `SERVERLESS_ARCH.md` §0 (the
> as-built `cmd/launch/` system) and `SELF_HOST.md` §8. This doc specifies a
> **routing + capability** change to `cmd/launch/` so that which wire protocol
> a request speaks is chosen **per model**, not per integration. It is a spec
> for an implementer — the change is localized and reuses existing helpers; no
> new server-side forwarding is required.
>
> **Motivating incident:** kat-coder (a100b GPU4:30099, a Qwen3.6-35B-A3B MoE)
> only worked — badly — through Claude Code, and failed there because kat-coder
> emits tool calls as **free-form JSON text** (`{"action":"plan",...}`), not
> OpenAI `tool_calls` and not Anthropic `tool_use`. The translation proxy had
> nothing to translate, so Claude Code spiraled to 100% context. The user's
> ask: *"if katcoder is used, use the openai way… different models use different
> ways to call."*

## 1. The gap (confirmed against source)

`cmd/launch/` routes **per integration**, not per model.

- `findUserRemoteForModel` (`cmd/launch/user_remotes.go:137`) is called in
  exactly **4 non-test sites**: `claude.go:90`, `agent_routing.go:44`,
  `launch.go:1464` (`hasLocalModel` short-circuit), `models.go:275`
  (`showOrPull` short-circuit). **No OpenAI-speaking integration calls it.**
- Every OpenAI integration hardcodes `envconfig.Host()+"/v1"` (≈16 call sites
  across `opencode.go`, `codex.go`, `hermes.go`, `kimi.go`, `omp.go`,
  `cline.go`, `droid.go`, `qwen.go`, `copilot.go`, `poolside.go`, `pi.go`) and
  passes the namespaced `<remote>/<model>` id as the model to the **local oa
  daemon**.
- The daemon does **not** forward user-remote requests.
  `server/cloud_proxy.go:111` only forwards `:cloud` refs to `ollama.com`; a
  `<remote>/<model>` ref is `modelSourceUnspecified` → falls through to local
  inference → **404**. (`grep remotes.json` across `server/` = zero hits.)

**So a user-remote OpenAI model like kat-coder is NEVER routed direct-to-remote
by any integration.** Only `claude.go` + `agent_routing.go` reach user remotes,
and both force the Anthropic↔OpenAI translation proxy. That is why the user was
forced into Claude Code + kat-coder, which then failed on the tool-format
mismatch.

The Anthropic↔OpenAI proxy (`cmd/launch/anthropic_openai_proxy.go`) **already
fully translates** `tool_calls`↔`tool_use` in both directions:

| Direction | Helper | Site |
|---|---|---|
| request: `tool_use`/`tool_result`/`tools` → OpenAI | `chatRequestToOpenAI` | `:161-217` |
| response non-stream: `tool_calls` → `tool_use` | `parseOpenAIToolCalls` → `anthropic.ToMessagesResponse` | `:251-283`, `:429` |
| response stream: `tool_calls` → `tool_use` deltas | `toolAccums` + `flushToolCalls` | `:459-464`, `:553-568`, `:478-516` |

So the gap is **routing + capability**, not translation. An OpenAI model that
emits real `tool_calls` already works end-to-end through Claude Code today.
kat-coder's problem is it emits **free-form text** — there is nothing to
translate.

## 2. Goal

- **OpenAI-native model** (e.g. kat-coder, vLLM, llama-server) → OpenAI
  integrations (opencode, codex, hermes, …) talk to the remote's
  `/v1/chat/completions` **directly**, no proxy.
- **Anthropic-native model** → Anthropic integrations (claude, `cmd/agent`)
  talk to the remote's `/v1/messages` **directly**, no proxy.
- The existing translation proxy bridges **only on genuine protocol mismatch**
  (Anthropic integration ↔ OpenAI model, or the reverse).
- **Capability gate:** when a model's tool-call format cannot satisfy the
  integration's tool loop, oaica **warns/refuses** instead of silently launching
  a session that will spiral. kat-coder via opencode reaches the remote;
  kat-coder via Claude Code is refused (unless forced) because free-form text
  can't drive a `tool_use` loop.

## 3. Per-remote protocol descriptor

Add three optional fields to `userRemote` (`user_remotes.go:36-47`, after the
existing `Version` field) and propagate them through `LaunchModel`
(`model_inventory.go`) and `AgentModelMeta` (`agent_routing.go`):

| Field | Values | Default |
|---|---|---|
| `Wire` | `"openai"` \| `"anthropic"` | `"openai"` |
| `ToolFormat` | `"tool_calls"` \| `"freeform"` \| `"xml"` \| `"none"` | inferred from `Wire` (openai→`tool_calls`, anthropic→`xml`) |
| `ToolReliable` | bool | `true` only when inferred `tool_calls`; `false` for `freeform`/`xml`/`none` unless explicitly set |

JSON (remotes.json), all `omitempty`:

```json
{ "name": "kat-a100b", "base_url": "http://192.168.0.50:8080",
  "api_key_env": "KAT_KEY", "tool_format": "freeform" }
```

- `ToolReliable` as a `*bool` so the tri-state (unset / explicitly true /
  explicitly false) is distinguishable — the default-vs-override distinction is
  what the gate keys on.
- Centralize the defaults in a new `userRemote.Descriptor()` method
  (`user_remotes.go`, right after `key()` :55). Update the remotes.json doc
  comment (`user_remotes.go:11-24`) with the three keys + the kat-coder example.
- `userRemoteLaunchModels` (`user_remotes.go:218-248`, the `LaunchModel{...}`
  build at `:241`) stamps `r.Descriptor()` onto every model from that remote.
  Local/cloud entries leave the trio zero (`Wire=""` → "ollama-native / daemon
  route").
- `applyAgentModelMeta` (`agent_routing.go`) copies the trio into
  `AgentModelMeta`. The existing `meta.ToolCapable = lm.ToolCapable || lm.Remote`
  line stays but becomes subordinate to the gate (§6): `ToolCapable` = "can emit
  tool calls in *some* form"; `ToolReliable` = "those calls satisfy this
  integration's wire loop."

## 4. Shared resolver (one place that splits a picker name)

New in `user_remotes.go` next to `findUserRemoteForModel`:

```go
type RemoteEndpoint struct {
    BaseURL       string // r.openAIBase() — includes the /v1 (or /v4) version prefix
    Token         string // r.key()
    UpstreamModel string // bare id the remote expects (part after the first "/")
    Wire, ToolFormat string
    ToolReliable  bool
}
func resolveRemoteEndpoint(model string) (RemoteEndpoint, bool)
```

`BaseURL` uses the **existing** `r.openAIBase()` helper (`user_remotes.go:172`,
already handles the `Version` field — z.ai `/v4`, everyone else `/v1`). Do **not**
hardcode `/v1`; that would break z.ai and any `version`-overridden remote.

Companion in `oaica_models.go` next to `oaicaResolveHostForModel` (`:105`):

```go
func openAIBaseURLAndKey(primary LaunchModel) (baseURL, apiKey, modelID string)
// user-remote   → ep.BaseURL, ep.Token, ep.UpstreamModel
// local/cloud/:local → envconfig.Host()+"/v1", "ollama", primary.Name   (unchanged)
```

Every OpenAI integration swaps its hardcoded `envconfig.Host()+"/v1"` +
`"ollama"` + namespaced id for `openAIBaseURLAndKey(primary)`. For non-remote
models `resolveRemoteEndpoint` returns `ok=false` and the integration falls
through to the **exact existing** triple — local/cloud launches are
byte-identical (regression guard, §8). Note `:local`/`:cloud` use `:` not `/`
at the prefix position, so `findUserRemoteForModel` (splits on first `/`)
returns `ok=false` for them.

## 5. Per-integration change pattern

Thread `primary LaunchModel` into each integration's `*BaseURL()` helper (today
zero-arg) and the model-id/key call sites. Leave the config **format**
(JSON/TOML/YAML/env) untouched — only the three values (`baseURL`, `apiKey`,
`modelID`) change.

Representative files (line numbers from the current tree):

| File | Helper / site | Edit |
|---|---|---|
| `opencode.go` | `buildInlineConfig` `:289`, `buildModelEntries` `:344`, baseURL `:301`, model `:306` | resolved triple + per-remote provider (§7) |
| `codex.go` | `codexBaseURL()` `:339` → `(primary)`; callers `:178`, `:319`; `-m` arg | resolved triple. **Flag:** `wire_api="responses"` `:182` — see Risks |
| `hermes.go` | `hermesBaseURL()` `:528` → `(primary)`; `:294/:296/:297` | resolved triple |
| `kimi.go` | provider map `:169` | resolved triple |
| `omp.go` | `ompBaseURL()` `:373` → `(primary)`; `:366` + `auth` | resolved triple; flip `auth` none→bearer if `ep.Token != ""` |
| `cline.go` | `clineProviderBaseURL()` `:133` → `(primary)` | resolved triple |
| `droid.go` | per-model `modelEntry` `:129-134` | stamp `resolveRemoteEndpoint` **per model** (cleanest — droid already carries per-model `BaseURL`) |
| `qwen.go` | `qwenBaseURL()` `:406` → `(primary)`; `:413`, `OPENAI_API_KEY` `:407` | resolved triple |
| `copilot.go` | env vars `:66-73` (`COPILOT_PROVIDER_BASE_URL`/`_API_KEY`/`_MODEL`) | resolved triple |
| `poolside.go` | `Run` `:34-49` (`POOLSIDE_STANDALONE_BASE_URL`/`_API_KEY`, `-m` `:41`) | resolved triple |
| `pi.go` | ollama provider `:561-565` | resolved triple + per-remote provider (§7) |

### Ollama-native integrations (defer)

`openclaw.go:690-693` (`api:"ollama"`, Ollama `/api/chat`) and `vscode.go:269`
(Ollama vendor) rely on the Ollama native API the thin-client fork does **not**
serve for user-remotes — they 404 today and keep 404-ing after Phase 1.

- **Phase 1:** refuse early at launch with a clear message ("openclaw/vscode do
  not yet support user remotes; use opencode/codex/hermes/…") instead of the
  confusing daemon 404. A `supportsUserRemotes` flag on each Runner (or a switch
  in the launch dispatcher) is the clean place.
- **Phase 3:** a thin Ollama↔OpenAI shim proxy analogous to
  `anthropic_openai_proxy.go` (translate `/api/chat` ↔ `/v1/chat/completions`).

## 6. Capability gate (one shared predicate, two sites)

New in `agent_routing.go`:

```go
type ToolWire int // toolWireOpenAI | toolWireAnthropic; each integration declares which it wants
func toolGateDecision(wants ToolWire, ep RemoteEndpoint) (ok bool, reason string)
// !ep.ToolReliable → refuse:
//   "model %q emits tool calls as %q (tool_reliable=false); integration requires reliable %s tool calls"
```

- **Gate site A — Anthropic path** (`ResolveAgentModel` `:41`, `claude.go:90`):
  after resolving the remote, before starting the proxy — if `ep.Wire=="openai"`
  and `!ep.ToolReliable` (or `ToolFormat!="tool_calls"`), refuse unless
  `--force-tools`. Extract a shared `gateUserRemoteTools(remote, wants, force)`.
  Add `ResolveAgentModelWithOpts(ctx, model, ResolveOpts{ForceTools})`; keep
  `ResolveAgentModel` as a `ForceTools:false` wrapper so `cmd/agent`
  (`agent_cmd.go:64`) stays back-compatible.
- **Gate site B — OpenAI path:** each OpenAI integration's `Run` calls a shared
  `gateOpenAITools(primary, force)` after resolving; refuse (non-zero exit) or,
  with `--force-tools`, warn-and-proceed.

The warning goes to **stderr** (not stdout, so piped output is clean) and names:
the model, the remote, the observed `tool_format`, the integration's wire
requirement, and the **exact remotes.json keys** to set. `--force-tools`
downgrades refuse → warn.

**Default caution:** `ToolReliable` defaults to `true` only for inferred
`tool_calls`. So vLLM / llama-server / SGLang (which emit real `tool_calls`)
never hit the gate. kat-coder is gate-able precisely because its user sets
`tool_format:"freeform"` → `ToolReliable` stays false.

## 7. Mixed-catalog decision — per-remote provider entry (Option B)

opencode's `buildInlineConfig` registers ONE `ollama` provider with ONE
`baseURL` (`:301`) and puts every `LaunchModel` under it via `buildModelEntries`
(`:344`). Pointing that single provider at a remote breaks every other (local)
model in the list. Same shape: `droid.go`, `pi.go`, and codex's model catalog.

**Pick per-remote provider entries; keep the daemon `ollama` provider for
local/cloud.** In `buildInlineConfig`, partition `models` by endpoint: walk the
list, call `resolveRemoteEndpoint(m.Name)` per entry, group by `(BaseURL,
Token)`. Emit one provider block per group — `ollama` (daemon, key `"ollama"`)
for the no-remote group, `<remote.Name>` per remote (key `ep.Token`, baseURL
`ep.BaseURL`). `buildModelEntries` becomes provider-aware: the opencode model id
is `<providerId>/<model>`, the `models` map lives **inside** each provider block;
remote entries use `ep.UpstreamModel` (bare), local entries use `m.Name`. The
top-level `"model"` (`:306`) becomes `<primaryProviderId>/<primary.Name>`.

The `provider` map shape at `:301` strongly implies opencode's
`@ai-sdk/openai-compatible` supports multiple providers — **validate against
opencode's config schema before merge.** If it rejects multi-provider inline
configs, fall back to **Option A** (remote-only provider when primary is
remote, drop local models from the inline catalog, document the picker
regression). codex catalog: per-entry `base_url` override if the schema permits,
else a second `<remote>` profile with the root `model_provider` set to it when
primary is remote.

## 8. Anthropic path uses the descriptor

Dispatch `claude.go:90` / `agent_routing.go:44` on `ep.Wire`:

- `openai` (default, today's case): existing translation proxy — but **gate
  first** (§6). The proxy already translates `tool_calls`→`tool_use` correctly,
  so a `tool_calls`-emitting OpenAI remote works end-to-end today, unchanged
  except the pre-check.
- `anthropic` (Phase 3, future): **skip the proxy**, point
  `ANTHROPIC_BASE_URL`/`ANTHROPIC_API_KEY` direct at `ep.BaseURL`/`ep.Token`,
  pass `ep.UpstreamModel`. Direct `/v1/messages`, no loopback. Dead until a user
  sets `"wire":"anthropic"` — correct but inert.
- OpenAI-integration ↔ Anthropic-remote (reverse proxy, Phase 3): refuse in
  Phase 1 ("anthropic-native remote via openai integration not yet supported;
  use `oaica launch claude`").

## 9. Phased rollout

| Phase | Ships | Unblocks |
|---|---|---|
| **1** | descriptor fields + `Descriptor()` + JSON doc (§3); `resolveRemoteEndpoint` + `openAIBaseURLAndKey` (§4); wire the 11 OpenAI integrations (§5); per-remote provider for opencode/droid/pi/codex (§7); early-refuse openclaw/vscode | kat-coder via opencode/codex reaches the remote (**200, not 404**). Local/cloud unchanged. |
| **2** | `toolGateDecision` + `--force-tools` + `ResolveOpts` (§6); wire gate into claude/agent + each OpenAI integration | kat-coder via opencode refuses by default with the warning; `--force-tools` proceeds. No more silent spirals. |
| **3** | Anthropic-native direct branch; reverse proxy; openclaw/vscode Ollama↔OpenAI shims; codex `wire_api` chat-completions fix | completeness |

**Phase 1 alone satisfies the stated ask.**

## 10. Verification

1. **Unit tests** (`cmd/launch/*_test.go`): add a remote-primary variant to each
   integration's config-construction test asserting
   `baseURL==ep.BaseURL`, `apiKey==ep.Token`, model id == bare upstream; and
   **byte-identical** assertions for a local primary (regression guard).
   `opencode_test.go` / `codex_test.go` / `hermes_test.go` add mixed-catalog
   (one local + one remote) → two provider entries. Add a `fakeUserRemote(name,
   baseURL, opts)` helper to `test_config_helpers_test.go` writing a temp
   `OAICA_REMOTES_FILE`. `agent_routing_test.go` asserts
   `Wire/ToolFormat/ToolReliable` propagate + `--force-tools` refuse/proceed
   (Phase 2). `integrations_test.go` gets a cross-integration "user-remote
   reaches remote, not daemon" table test.
2. `go build ./...` + `go test ./cmd/launch/...` green.
3. **E2E (manual, on .47):** remotes.json entry for kat-a100b with
   `tool_format:"freeform"`; `oaica launch opencode --model kat-a100b/kat-coder`
   → Phase 1: requests hit the remote's `/v1/chat/completions`, HTTP 200 (vs
   current daemon 404). Phase 2: same command refuses with the warning unless
   `--force-tools`. Regression: `oaica launch opencode --model qwen3:32b` →
   still `envconfig.Host()+"/v1"`. `oaica launch claude --model kat-a100b/kat-coder`
   → translation proxy; Phase 2 gate refuses unless forced.
4. **Honest gate:** Phase 1 routes to the remote; whether kat-coder then
   performs well over OpenAI chat/completions is a **model-quality** question,
   separate from routing. No live-box fabrication.

## 11. Risks

1. **Breaking local-model launches** (highest): `resolveRemoteEndpoint` returns
   `ok=false` for every non-`<remote>/...` name → exact existing triple. The
   integrations test sweep must assert byte-identical local config.
2. **codex `wire_api="responses"`** (`codex.go:182`): many remotes speak
   `chat/completions` only → 404 on `/v1/responses`. Document codex+remote as
   responses-only in Phase 1; add a `responses_api` descriptor flag (default
   false → `wire_api="chat"`) in Phase 3.
3. **opencode multi-provider inline config**: if the schema rejects it, fall
   back to Option A (remote-only provider, drop local models) — validate before
   merge.
4. **`tool_reliable` defaults surprise users**: warning text names the exact
   remotes.json keys; ship `oaica remotes verify <name>` (Phase 2) that probes
   the remote with a tool-call request and reports the observed format.
5. **openclaw/vscode user-remotes still unsupported** after Phase 1 — refuse
   early, not silent 404.
6. **Remote-direct traffic unlogged** (`~/.oaica/requests.log` won't see it):
   acceptable; Phase 2 optional loopback logging-only proxy (reuse
   `local_proxy.go`) if observability is needed.

## 12. Reuse (do not rewrite)

- `remoteBaseURL` (`user_remotes.go:161`), `openAIBase` (`:172`), `key()`
  (`:55`) — already handle the `Version` prefix and env-key resolution.
- `chatRequestToOpenAI` (`anthropic_openai_proxy.go:161`),
  `parseOpenAIToolCalls` (`:251`), `flushToolCalls` (`:478`) — the translation
  the proxy already does; the gate just decides whether to trust it.
- `oaicaResolveHostForModel` (`oaica_models.go:105`) — local/cloud resolution,
  unchanged; `openAIBaseURLAndKey` sits beside it and only diverges for
  user-remotes.

## Cross-references

- `SERVERLESS_ARCH.md` §0 — the as-built `cmd/launch/` loopback-proxy model
  this extends.
- `SELF_HOST.md` §8 — serving kat-coder to other machines (OpenAI-compatible
  API), the box side of a user remote.
- `user_remotes.go` file doc comment — the remotes.json schema this adds
  `wire`/`tool_format`/`tool_reliable` to.