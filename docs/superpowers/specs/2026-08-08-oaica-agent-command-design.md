# `oaica agent` — prime-agent-style native coding agent

Date: 2026-08-08
Status: Approved design (brainstormed, user-locked 2026-08-08)
Reference: https://github.com/PrimeIntellect-ai/prime-agent (studied for loop semantics + tool surface)

## Goal

Add a new command, `oaica agent`, that runs a **streaming coding agent** in the
current directory — a Go-native, in-process agent loop (not a wrapper that
launches an external agent binary). The feature is modeled on prime-agent's
`agent-loop.ts` semantics (stream assistant response → execute tool calls →
feed results back → repeat) and reuses oaica-code's existing routing/proxy
infrastructure so it works against **every** upstream the fork already supports:
user-defined remotes (via the Anthropic↔OpenAI translation proxy), the OAICA
cloud router, and local `oaica serve` models.

Hard constraints:
- **Additive / ollama-upgrade-safe.** The repo is a fork of
  `github.com/ollama/ollama`. Upstream `agent/`, `api/`, `anthropic/`, `server/`
  packages must remain untouched. All new code lives in a new `cmd/agent/`
  package plus one small new exported file in the fork-owned `cmd/launch/`
  package. `cmd/cmd.go` gets exactly one added line in `rootCmd.AddCommand`.
- **Implemented in an isolated git worktree** so `main` and the ollama upgrade
  path stay clean during development.

## Key discovery that shaped this design

oaica-code **already contains a complete ReAct agent engine**: the upstream
`agent/` package (`agent/session.go`) with tool registry (`agent/registry.go`),
tool implementations (`agent/tools/`: Bash, Read, Edit, WebSearch, WebFetch,
Skill), skills (`agent/skills.go`), compaction (`agent/compactor.go`), and a
tool-approval flow (`agent/approval.go`). A full-screen bubbletea chat TUI
consumes it today (`cmd/tui/chat`, entered via `cmd/agent_tui.go`).

That engine is, however, wired **only to the local Ollama server**: it uses
`api.Client.Chat` (`api.ClientFromEnvironment()`, i.e. `OLLAMA_HOST`), and the
fork's primary OAICA router path (`cmd/oaica_client.go`) is **non-streaming**
and does not implement the `ChatClient` interface. So a prime-agent-style agent
that runs against remotes / the router / local models has never existed.

The gap is therefore narrow and precisely scoped: implement a **streaming
ChatClient shim** that speaks the Anthropic Messages API to a resolved endpoint
(through the existing proxy machinery), plus a **plain-stdout renderer** and the
**command**. The engine, tools, skills, compaction, and approval flow are reused
unchanged.

## Architecture

```
cmd/agent/                     NEW package — the only new code
  agent_cmd.go                 cobra command "oaica agent" + flags + wiring
  shim.go                      agent.ChatClient impl:
                               api.ChatRequest → Anthropic MessagesRequest → POST /v1/messages
                               → parse Anthropic SSE → api.ChatResponseFunc
  sse.go                       inbound Anthropic SSE parser (event structs already in anthropic/)
  render.go                    plain-stdout agent.EventSink (streaming text + tool status lines)
  tools.go                     tool registry build (reuse agent/tools; no client.Show dep)
cmd/launch/agent_routing.go    NEW exported helper in fork-owned launch package
                               ResolveAgentModel(model) (baseURL, token, meta, error)
```

### Reused, untouched

| Piece | Source | Role |
|---|---|---|
| `agent.Session` engine | `agent/session.go` | ReAct loop: chat round → tool calls → execute → feed results → repeat |
| Tool implementations | `agent/tools/` | Bash, Read, Edit, WebSearch, WebFetch, Skill |
| Approval flow | `agent/approval.go` | per-tool ask-user gating (`ApprovalPrompter`) |
| Compaction | `agent/compactor.go` | context-window management |
| Anthropic types | `anthropic/anthropic.go` | MessagesRequest, ContentBlock, Tool, SSE event structs, Error |
| Translation proxy | `cmd/launch/anthropic_openai_proxy.go` | Anthropic `/v1/messages` ↔ OpenAI upstream for user remotes |
| Logging proxy | `cmd/launch/local_proxy.go` / `request_log.go` | cloud/local Anthropic passthrough + `~/.oaica/requests.log` |
| Model resolution | `cmd/launch/oaica_models.go`, `user_remotes.go` | host + key resolution |
| Model inventory | `cmd/launch/model_inventory.go` | capabilities, context length, tool capability |
| Sign-in precondition | `cmd/cmd.go` `oaicaEnsureSignedIn` | PreRunE |
| Streaming text render | `cmd/cmd.go` `displayResponse` + `progress` spinner | per-delta stdout |
| ANSI | `readline/types.go` | colors, cursors |

### New code, file by file

#### `cmd/launch/agent_routing.go` (exported helper)

`ResolveAgentModel(model string) (baseURL, token string, meta AgentModelMeta, err error)`

Reimplements `claude.go`'s routing decision (`claude.go:90-137`) as a
programmatic seam, so the agent command resolves an endpoint without launching a
child process:

1. Strip the `:local` picker tag (`oaicaStripLocalTag`).
2. If `findUserRemoteForModel(model)` matches a configured remote →
   - start `ListenAnthropicOpenAIProxy(remote, bare)` in a goroutine
     (`go RunAnthropicOpenAIProxy(ln, remote, bare)`), exactly as
     `claude.go:92-96`;
   - `baseURL = "http://127.0.0.1:<port>"`; `token = remote.key()`.
   - Proxy bind failure → **hard error** (precedent `claude.go:98-103`).
3. Else →
   - `host := oaicaResolveHostForModel(model)`; start `ListenLocalLoggingProxy`
     (best-effort, falls back to `host` directly — precedent `claude.go:121-128`);
   - `baseURL` = loopback proxy (or `host`); `token = oaicaLaunchAPIKeyForEnv()`.
4. Look up model metadata from the launch inventory (`launcherClient` /
   `model_inventory.go`) and return `AgentModelMeta{Capabilities,
   ContextLength, MaxOutputTokens, ToolCapable}`; fall back to sensible defaults
   when the name is absent (mirror `fallbackLaunchModel`).

`AgentModelMeta` is a new exported struct in `cmd/launch`.

Note: `ResolveAgentModel` returns an Anthropic-native endpoint (`/v1/messages`).
For user remotes this is the loopback translation proxy; for cloud/local it is
the router/local server's Anthropic endpoint (or the logging proxy in front).
The agent shim therefore always speaks Anthropic — one uniform protocol.

#### `cmd/agent/shim.go` — `agent.ChatClient` implementation

```go
type shimClient struct {
    baseURL string   // loopback proxy or router; shim appends "/v1/messages"
    token   string   // bearer token
    model   string   // bare model id the upstream expects
}
func (s *shimClient) Chat(ctx context.Context, req *api.ChatRequest, fn api.ChatResponseFunc) error
```

Conversion (new, ~150-250 LOC):
- `api.ChatRequest` → `anthropic.MessagesRequest`: map messages
  (`api.Message` → `anthropic.MessageParam`, tool-call content blocks to
  `tool_use` / `tool_result`), tools (`api.Tool` → `anthropic.Tool`), system
  prompt, `Think` → `ThinkingConfig`, `MaxTokens` (from `Options` or a default),
  `Stream = true`.
- POST `baseURL + "/v1/messages"` with `Authorization: Bearer <token>`,
  `Content-Type: application/json`, streaming SSE response.
- Parse inbound Anthropic SSE events (see `sse.go`) and, for each delta, feed an
  `api.ChatResponse` to `fn` so the engine renders incrementally.
- Terminal events: `message_stop` (or stream error) → finish; surface
  Anthropic-shaped `error` JSON via a non-nil return.
- HTTP timeout: 5 minutes (precedent `anthropic_openai_proxy.go:390`).

#### `cmd/agent/sse.go` — inbound Anthropic SSE parser

Parse `event: <type>` / `data: <json>` frames into the existing
`anthropic.*Event` structs (`MessageStartEvent`, `ContentBlockStartEvent`,
`ContentBlockDeltaEvent` with `text_delta`/`input_json_delta`/`thinking_delta`,
`ContentBlockStopEvent`, `MessageDeltaEvent`, `MessageStopEvent`,
`PingEvent`, `StreamErrorEvent` — `anthropic/anthropic.go:227-294`). New code:
no inbound Anthropic SSE parser exists anywhere in the repo today (verified;
the `StreamConverter` only *writes* Anthropic SSE server-side).

Accumulate content blocks by index; when a `tool_use` block completes, emit the
corresponding `api.ToolCall`; when the message stops, emit final
`api.ChatResponse` with `Done: true`.

#### `cmd/agent/render.go` — plain-stdout `EventSink`

Implement `agent.EventSink` (`Emit(agent.Event) error`,
`agent/events.go:82-93`) — the one consumer today is the bubbletea TUI; a
plain-stdout sink is fresh work:

- `EventMessageDelta` → `displayResponse`-style streaming text (reset
  `displayResponseState` per turn; `cmd.go:1699` precedent).
- `EventThinkingDelta` → grey thinking block delimiters
  (`thinkingOutputOpeningText/ClosingText`, `cmd.go:1621/1631`).
- `EventToolCallDetected` / `EventToolStarted` → tool header line.
- `EventToolFinished` → status line + truncated output (stdout/stderr), ANSI
  from `readline/types.go`.
- `EventRunFinished` → final message; `EventError` → error to stderr + exit.
- SIGINT → cancel ctx (precedent `cmd.go:1656-1662`).

#### `cmd/agent/tools.go` — registry

Build `*agent.Registry` mirroring `agentToolsRegistry` (`cmd/agent_tui.go:257`)
but WITHOUT the `client.Show` capability probe: register Bash, Read, Edit,
Skill, and WebSearch/WebFetch (the latter gated on the same cloud-status env
vars). Tool capability gating uses `AgentModelMeta.ToolCapable` from the launch
inventory instead. `OLLAMA_AGENT_DISABLE_SHELL` / `OLLAMA_AGENT_DISABLE_WEBSEARCH`
env vars respected as today.

#### `cmd/agent/agent_cmd.go` — command

```go
var agentCmd = &cobra.Command{
    Use:   "agent [PROMPT]",
    Short: "Run a streaming coding agent",
    Args:  cobra.ArbitraryArgs,
    PreRunE: oaicaEnsureSignedIn,
    RunE:   runAgent,
}
```

Flags: `--model` (picker if unset), `--system`, `--max-turns`, `--yes`,
`--cwd`. Registered in `NewCLI()` at `cmd/cmd.go:2420-2445` (one added line).

`runAgent`:
1. Resolve model (flag or `launch.ResolveRunModel` picker path).
2. `launch.ResolveAgentModel(model)` → baseURL, token, meta.
3. Build `shimClient`, registry, renderer sink.
4. `s.Session.Run(ctx, agent.RunOptions{Model, SystemPrompt, Messages,
   MaxToolRounds})` with `WorkingDir = cwd`.
5. Exit non-zero on `EventError`; else 0.

### Tool surface (v1)

Bash, Read, Edit, WebSearch, WebFetch, Skill — all from `agent/tools/` (already
exist). git operations run through Bash (`bash: git status/...`), matching
prime-agent's "model gets a shell" philosophy; no dedicated git tool in v1.
Ask-user / interrupt = existing `ApprovalPrompter` (stdin confirm) + SIGINT
cancel. **Phase 2 (not in this spec):** a persistent Python/IPython subprocess
tool registered through the same `agent.Tool` interface.

## Data flow

```
picker --model → model name
  → launch.ResolveAgentModel(model)
      → (loopback proxy URL OR router URL) + bearer token + metadata
  → cmd/agent shimClient.Chat(ctx, api.ChatRequest, fn)
      → Anthropic MessagesRequest → POST /v1/messages (SSE)
      → proxy → OpenAI upstream (user remotes) OR router/local (cloud/local)
  → Anthropic SSE → sse.go → api.ChatResponse → fn
  → agent.Session engine: tool calls → tools.go registry → results → loop
  → render.go EventSink → stdout
```

## Error handling

| Case | Behavior |
|---|---|
| Proxy bind failure (user remote) | Hard error, non-zero exit — never silently fall back (claude.go:98-103) |
| No credential | `oaicaEnsureSignedIn` PreRunE fails before run |
| Upstream HTTP/API error | Engine `EventError` → renderer prints → non-zero exit |
| Tool execution failure | Engine returns error `tool_result` to model (existing behavior); loop continues |
| SIGINT | Cancel ctx → engine finalizes aborted message → clean exit |
| SSE parse error | Non-nil error from `Chat`; engine surfaces as `EventError` |

## Testing

All new tests live under `cmd/agent/` + `cmd/launch/` (additive; existing tests
untouched).

1. `shim_test.go` — table tests: `api.ChatRequest` → `anthropic.MessagesRequest`
   (messages, tool_use/tool_result blocks, tools, system, thinking).
2. `sse_test.go` — canned Anthropic SSE frames → expected `api.ChatResponse`
   sequence (text delta, thinking delta, tool_use accumulation, message_stop,
   error event).
3. `render_test.go` — EventSink emit → captured stdout lines.
4. `agent_routing_test.go` — remote vs cloud routing; proxy bind failure;
   key precedence (env over file); `:local` tag stripping.
5. Integration (`shim_test.go`): `httptest` fake Anthropic `/v1/messages` server
   → `agent.Session.Run` with shim + a stub tool → assert tool result fed back
   and loop terminates.

Verification before completion: `go build ./...`, `go test ./cmd/agent/...
./cmd/launch/...`, and a live smoke `oaica agent "..."` against a configured
remote and against the OAICA router.

## Ollama-upgrade safety checklist

- [ ] `agent/`, `api/`, `anthropic/`, `server/`, `cmd/tui/`, `cmd/agent_tui.go` — untouched
- [ ] `cmd/cmd.go` — exactly one added `AddCommand` line
- [ ] `cmd/launch/` — one new file (`agent_routing.go`) + one new exported struct
- [ ] All new code in `cmd/agent/`
- [ ] Implemented in isolated worktree; merge via PR-like review of the diff
