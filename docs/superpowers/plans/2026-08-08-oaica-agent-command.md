# `oaica agent` Command Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `oaica agent [PROMPT]` — a streaming coding agent command — by reusing the existing `agent/` ReAct engine and adding a thin Anthropic-native shim that talks to every upstream the fork already supports (user remotes via the translation proxy, the OAICA router, and local `oaica serve` models).

**Architecture:** `cmd/agent/` (new package) holds the shim (`api.ChatRequest` → Anthropic `MessagesRequest` → POST `/v1/messages` → parse Anthropic SSE → `api.ChatResponse` deltas), a plain-stdout `EventSink`, a tool registry builder, and the cobra command. One new exported helper `launch.ResolveAgentModel` mirrors `claude.go`'s routing decision without launching a child process. The engine, tools, skills, compactor, and approval flow in the top-level `agent/` package are reused untouched.

**Tech Stack:** Go (fork of `github.com/ollama/ollama`), cobra, the existing `agent/` engine, the existing `anthropic/` types, the existing `cmd/launch` proxy/routing machinery.

## Global Constraints

- `agent/`, `api/`, `anthropic/`, `server/`, `cmd/tui/`, `cmd/agent_tui.go` — **untouched** (ollama-upgrade safety).
- `cmd/cmd.go` — exactly **one added line**: `agentcmd.AgentCmd(oaicaEnsureSignedIn)` in the `rootCmd.AddCommand` block.
- `cmd/launch/` — exactly **one new file** (`agent_routing.go`) plus its test file.
- All other new code lives in `cmd/agent/`.
- Module path stays `github.com/ollama/ollama`; no new dependencies.
- `cmd/agent` must NOT import `cmd` (cycle: `cmd` imports `cmd/agent` to register the command). The sign-in check is injected via `AgentCmd(checkServerHeartbeat func(cmd *cobra.Command, args []string) error)` — mirrors `launch.LaunchCmd`'s DI pattern.
- `api.Message` has no thinking-signature field and `anthropic.ContentBlock` requires one on assistant thinking blocks, so **thinking is dropped on the outbound request** (it is not needed by the model; the engine already echoes final text). Thinking deltas ARE rendered on the response side.
- The engine treats an empty `ChatResponse` frame as a stream-end sentinel (`messageEmpty`), so the shim **never emits a frame with empty Content, empty Thinking, and no ToolCalls**.
- The engine appends `ToolCalls` across frames, so the shim emits each completed `tool_use` block exactly once (at its `content_block_stop`).

---

### Task 1: `launch.ResolveAgentModel` — routing helper

**Files:**
- Create: `cmd/launch/agent_routing.go`
- Test: `cmd/launch/agent_routing_test.go`

**Interfaces:**
- Consumes (all existing, in `cmd/launch`):
  - `oaicaStripLocalTag(model string) (base string, wasLocal bool)` — `oaica_models.go:92`
  - `findUserRemoteForModel(name string) (userRemote, string, bool)` — `user_remotes.go:106`
  - `ListenAnthropicOpenAIProxy(remote userRemote, upstreamModel string) (net.Listener, int, error)` — `anthropic_openai_proxy.go:313`
  - `RunAnthropicOpenAIProxy(ln net.Listener, remote userRemote, upstreamModel string) error` — `anthropic_openai_proxy.go:326`
  - `oaicaResolveHostForModel(model string) string` — `oaica_models.go:105`
  - `ListenLocalLoggingProxy() (net.Listener, int, error)` — `request_log.go:174`
  - `RunLocalLoggingProxy(ln net.Listener, targetBaseURL string) error` — `request_log.go:113`
  - `oaicaLaunchAPIKeyForEnv() string` — `oaica_models.go:135`
  - `newModelInventory(client *api.Client) *modelInventory` — `model_inventory.go:58`
  - `(i *modelInventory) Load(ctx context.Context) ([]LaunchModel, error)` — `model_inventory.go:62`
  - `findLaunchModel(models []LaunchModel, name string) (LaunchModel, bool)` — `model_inventory.go:198`
  - `fallbackLaunchModel(name string) LaunchModel` — `model_inventory.go:200`
  - `userRemote.key() string` — env-over-file key precedence
- Produces:
  - `type AgentModelMeta struct { ToolCapable bool; ContextLength int; MaxOutputTokens int }`
  - `func ResolveAgentModel(ctx context.Context, model string) (baseURL, token, upstreamModel string, meta AgentModelMeta, err error)`

- [ ] **Step 1: Write the failing test**

`cmd/launch/agent_routing_test.go`:

```go
package launch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRemotesFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "remotes.json")
	content := `{
  "remotes": [
    { "name": "deepseek", "base_url": "https://api.deepseek.com", "api_key": "sk-static" },
    { "name": "lan",      "base_url": "http://192.168.1.50:8080", "api_key_env": "LAN_KEY" }
  ]
}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestResolveAgentModelRemote routes "<remote>/<model>" through a loopback
// translation proxy with the remote's own key, and returns the bare upstream
// model id.
func TestResolveAgentModelRemote(t *testing.T) {
	t.Setenv("OAICA_REMOTES_FILE", writeRemotesFile(t))
	t.Setenv("OAICA_API_KEY", "unused-oaica-key")

	baseURL, token, upstreamModel, meta, err := ResolveAgentModel(context.Background(), "deepseek/deepseek-chat")
	if err != nil {
		t.Fatalf("ResolveAgentModel: %v", err)
	}
	if !strings.HasPrefix(baseURL, "http://127.0.0.1:") {
		t.Errorf("baseURL = %q, want loopback proxy", baseURL)
	}
	if token != "sk-static" {
		t.Errorf("token = %q, want remote's static key", token)
	}
	if upstreamModel != "deepseek-chat" {
		t.Errorf("upstreamModel = %q, want %q", upstreamModel, "deepseek-chat")
	}
	if !meta.ToolCapable {
		t.Error("remote models should default to tool-capable")
	}
	if meta.MaxOutputTokens <= 0 {
		t.Errorf("MaxOutputTokens = %d, want positive default", meta.MaxOutputTokens)
	}
}

// TestResolveAgentModelKeyPrecedence: api_key_env wins over api_key.
func TestResolveAgentModelKeyPrecedence(t *testing.T) {
	t.Setenv("OAICA_REMOTES_FILE", writeRemotesFile(t))
	t.Setenv("LAN_KEY", "sk-from-env")

	_, token, upstreamModel, _, err := ResolveAgentModel(context.Background(), "lan/qwen3")
	if err != nil {
		t.Fatalf("ResolveAgentModel: %v", err)
	}
	if token != "sk-from-env" {
		t.Errorf("token = %q, want env-provided key (env beats file)", token)
	}
	if upstreamModel != "qwen3" {
		t.Errorf("upstreamModel = %q, want %q", upstreamModel, "qwen3")
	}
}

// TestResolveAgentModelLocalTag: ":local" is stripped and the result uses the
// OAICA key, never the remote key.
func TestResolveAgentModelLocalTag(t *testing.T) {
	t.Setenv("OAICA_REMOTES_FILE", writeRemotesFile(t))
	t.Setenv("OAICA_API_KEY", "oaica-key")
	t.Setenv("OAICA_HOST", "https://router.example.test")

	baseURL, token, upstreamModel, _, err := ResolveAgentModel(context.Background(), "llama3.1:8b:local")
	if err != nil {
		t.Fatalf("ResolveAgentModel: %v", err)
	}
	if upstreamModel != "llama3.1:8b" {
		t.Errorf("upstreamModel = %q, want tag stripped to %q", upstreamModel, "llama3.1:8b")
	}
	if token != "oaica-key" {
		t.Errorf("token = %q, want OAICA key", token)
	}
	if !strings.HasPrefix(baseURL, "http://127.0.0.1:") && baseURL != "https://router.example.test" {
		t.Errorf("baseURL = %q, want loopback logging proxy or OAICA_HOST", baseURL)
	}
}

// TestResolveAgentModelUnknownModelFallsBackToDefaults: an unknown model
// yields positive defaults and tool-capable=true.
func TestResolveAgentModelUnknownModelFallsBackToDefaults(t *testing.T) {
	t.Setenv("OAICA_REMOTES_FILE", writeRemotesFile(t))
	t.Setenv("OAICA_API_KEY", "oaica-key")
	t.Setenv("OAICA_HOST", "https://router.example.test")

	_, _, upstreamModel, meta, err := ResolveAgentModel(context.Background(), "made-up-model")
	if err != nil {
		t.Fatalf("ResolveAgentModel: %v", err)
	}
	if upstreamModel != "made-up-model" {
		t.Errorf("upstreamModel = %q, want identity for cloud", upstreamModel)
	}
	if !meta.ToolCapable {
		t.Error("unknown models should default to tool-capable")
	}
	if meta.ContextLength <= 0 || meta.MaxOutputTokens <= 0 {
		t.Errorf("defaults not applied: ContextLength=%d MaxOutputTokens=%d", meta.ContextLength, meta.MaxOutputTokens)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/launch/ -run TestResolveAgentModel -count=1`
Expected: FAIL — undefined: `ResolveAgentModel`

- [ ] **Step 3: Write minimal implementation**

`cmd/launch/agent_routing.go`:

```go
package launch

import (
	"context"
	"fmt"

	"github.com/ollama/ollama/api"
)

// AgentModelMeta carries the model metadata the agent command needs to
// configure its shim and tool gating. Zero values mean "unknown"; consumers
// fall back to their own defaults.
type AgentModelMeta struct {
	ToolCapable     bool
	ContextLength   int
	MaxOutputTokens int
}

// Agent shim defaults when the launch inventory has no metadata for a model.
const (
	defaultAgentContextLength = 128000
	defaultAgentMaxTokens     = 4096
)

// ResolveAgentModel resolves a picker model name to the Anthropic-native
// /v1/messages endpoint the agent shim should talk to, the bearer token to
// send, and the bare upstream model id (picker tag and remote prefix
// stripped). It mirrors claude.go's routing decision without launching a
// child process:
//
//   - user-defined remote ("<remote>/<model>") → loopback Anthropic↔OpenAI
//     translation proxy bound to the remote's own key. A bind failure is a
//     hard error — never silently fall back to the OAICA router (which would
//     send the remote's model name to the wrong endpoint with the wrong key).
//   - cloud / ":local" → loopback logging proxy in front of the resolved host
//     (best-effort; the host is used directly if the proxy cannot bind).
//
// Model metadata comes from the launch inventory (tool capability, context
// length, max output tokens) with sensible defaults when the model is unknown
// or the inventory is unreachable.
func ResolveAgentModel(ctx context.Context, model string) (baseURL, token, upstreamModel string, meta AgentModelMeta, err error) {
	upstreamModel, _ = oaicaStripLocalTag(model)

	if remote, bare, ok := findUserRemoteForModel(model); ok {
		upstreamModel = bare
		ln, port, lerr := ListenAnthropicOpenAIProxy(remote, bare)
		if lerr != nil {
			return "", "", "", AgentModelMeta{}, fmt.Errorf("failed to start translation proxy for remote %q: %w", remote.Name, lerr)
		}
		go func() { _ = RunAnthropicOpenAIProxy(ln, remote, bare) }()
		baseURL = fmt.Sprintf("http://127.0.0.1:%d", port)
		token = remote.key()
	} else {
		realHost := oaicaResolveHostForModel(model)
		baseURL = realHost
		if ln, port, lerr := ListenLocalLoggingProxy(); lerr == nil {
			go func() { _ = RunLocalLoggingProxy(ln, realHost) }()
			baseURL = fmt.Sprintf("http://127.0.0.1:%d", port)
		}
		token = oaicaLaunchAPIKeyForEnv()
	}

	meta = agentModelMeta(ctx, model)
	return baseURL, token, upstreamModel, meta, nil
}

// agentModelMeta looks the model up in the launch inventory and applies
// defaults. A model found in the inventory keeps its advertised tool
// capability (false really means no tools); an unknown or unreachable model
// is assumed tool-capable — an agent without tools is useless, and a model
// that cannot actually call tools fails visibly on the first call instead.
func agentModelMeta(ctx context.Context, model string) AgentModelMeta {
	meta := AgentModelMeta{
		ToolCapable:     true,
		ContextLength:   defaultAgentContextLength,
		MaxOutputTokens: defaultAgentMaxTokens,
	}
	client, err := api.ClientFromEnvironment()
	if err != nil {
		return meta
	}
	models, err := newModelInventory(client).Load(ctx)
	if err != nil {
		return meta
	}
	if lm, ok := findLaunchModel(models, model); ok {
		meta.ToolCapable = lm.ToolCapable
		if lm.ContextLength > 0 {
			meta.ContextLength = lm.ContextLength
		}
		if lm.MaxOutputTokens > 0 {
			meta.MaxOutputTokens = lm.MaxOutputTokens
		}
		return meta
	}
	fb := fallbackLaunchModel(model)
	if fb.ContextLength > 0 {
		meta.ContextLength = fb.ContextLength
	}
	if fb.MaxOutputTokens > 0 {
		meta.MaxOutputTokens = fb.MaxOutputTokens
	}
	return meta
}
```

Note: tests pass `OAICA_REMOTES_FILE` and `OAICA_API_KEY` env vars; `agentModelMeta` calls `api.ClientFromEnvironment()` + `inventory.Load`, which will attempt a (fast, refused) connection to the default `OLLAMA_HOST` — fine in tests, falls back to defaults.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/launch/ -run TestResolveAgentModel -count=1`
Expected: PASS (all four subtests)

- [ ] **Step 5: Commit**

```bash
git add cmd/launch/agent_routing.go cmd/launch/agent_routing_test.go
git commit -m "feat(agent): add launch.ResolveAgentModel routing helper"
```

---

### Task 2: `cmd/agent` — inbound Anthropic SSE parser

**Files:**
- Create: `cmd/agent/sse.go`
- Test: `cmd/agent/sse_test.go`

**Interfaces:**
- Consumes (existing): `anthropic.ContentBlockStartEvent`, `ContentBlockDeltaEvent`, `Delta`, `ContentBlockStopEvent`, `StreamErrorEvent` (`anthropic/anthropic.go`); `api.ChatResponse`, `api.Message`, `api.ToolCall`, `api.ToolCallFunction`, `api.ToolCallFunctionArguments` (`api/types.go`).
- Produces:
  - `type anthropicSSEAccumulator struct { ... }` (unexported)
  - `func newAnthropicSSEAccumulator() *anthropicSSEAccumulator`
  - `func (a *anthropicSSEAccumulator) Feed(eventType string, data []byte) (deltas []api.ChatResponse, done bool, err error)`
    - Returns the deltas to hand to the engine; `done=true` exactly once after `message_stop`.
    - Never returns an empty delta (engine's `messageEmpty` treats empty frames as stream-end).
    - Emits each completed `tool_use` block exactly once at its `content_block_stop`, with arguments parsed from accumulated `input_json_delta` fragments.

- [ ] **Step 1: Write the failing test**

`cmd/agent/sse_test.go`:

```go
package agent

import (
	"strconv"
	"strings"
	"testing"

	"github.com/ollama/ollama/api"
)

// textDeltaFrame builds a content_block_delta frame with a text_delta for the
// given index. These fixtures exercise the parser directly.
func textDeltaFrame(index int, text string) string {
	return `{"type":"content_block_delta","index":` + strconv.Itoa(index) + `,"delta":{"type":"text_delta","text":` + strconv.Quote(text) + `}}`
}

// TestFeedTextSequence: a text block streams deltas and terminates on
// message_stop with a Done frame.
func TestFeedTextSequence(t *testing.T) {
	acc := newAnthropicSSEAccumulator()

	deltas, done, err := acc.Feed("content_block_start", []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`))
	if err != nil || len(deltas) != 0 || done {
		t.Fatalf("start: deltas=%d done=%v err=%v", len(deltas), done, err)
	}
	for _, chunk := range []string{"Hello", ", ", "world"} {
		deltas, done, err = acc.Feed("content_block_delta", []byte(textDeltaFrame(0, chunk)))
		if err != nil {
			t.Fatalf("delta %q: %v", chunk, err)
		}
		if done || len(deltas) != 1 {
			t.Fatalf("delta %q: deltas=%d done=%v", chunk, len(deltas), done)
		}
		if deltas[0].Message.Content != chunk {
			t.Errorf("delta content = %q, want %q", deltas[0].Message.Content, chunk)
		}
	}
	deltas, done, err = acc.Feed("message_stop", []byte(`{"type":"message_stop"}`))
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !done || len(deltas) != 1 || !deltas[0].Done {
		t.Fatalf("stop: done=%v deltas=%#v", done, deltas)
	}
	// A second message_stop must be inert.
	_, done, _ = acc.Feed("message_stop", []byte(`{"type":"message_stop"}`))
	if done {
		t.Fatal("second message_stop should not set done again")
	}
}

// TestFeedToolUseAccumulation: input_json_delta fragments accumulate into a
// single ToolCall emitted once at content_block_stop.
func TestFeedToolUseAccumulation(t *testing.T) {
	acc := newAnthropicSSEAccumulator()

	_, _, err := acc.Feed("content_block_start", []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"read_file","input":{}}}`))
	if err != nil {
		t.Fatalf("tool_use start: %v", err)
	}
	// Fragments deliberately split the JSON to prove accumulation.
	for _, frag := range []string{`{"pa`, `th":"/tmp/a.txt",`, `"max_chars":100}`} {
		frame := `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":` + strconv.Quote(frag) + `}}`
		deltas, _, err := acc.Feed("content_block_delta", []byte(frame))
		if err != nil || len(deltas) != 0 {
			t.Fatalf("input_json_delta %q: deltas=%d err=%v", frag, len(deltas), err)
		}
	}

	deltas, done, err := acc.Feed("content_block_stop", []byte(`{"type":"content_block_stop","index":0}`))
	if err != nil || done {
		t.Fatalf("tool stop: done=%v err=%v", done, err)
	}
	if len(deltas) != 1 {
		t.Fatalf("expected 1 delta with the tool call, got %d", len(deltas))
	}
	calls := deltas[0].Message.ToolCalls
	if len(calls) != 1 {
		t.Fatalf("ToolCalls = %d, want 1", len(calls))
	}
	call := calls[0]
	if call.ID != "toolu_1" || call.Function.Name != "read_file" {
		t.Errorf("call = %q/%q, want toolu_1/read_file", call.ID, call.Function.Name)
	}
	args := call.Function.Arguments.ToMap()
	if args["path"] != "/tmp/a.txt" || args["max_chars"] != float64(100) {
		t.Errorf("args = %#v", args)
	}
}

// TestFeedThinkingDelta routes thinking_delta into Message.Thinking.
func TestFeedThinkingDelta(t *testing.T) {
	acc := newAnthropicSSEAccumulator()
	_, _, _ = acc.Feed("content_block_start", []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`))
	frame := `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`
	deltas, _, err := acc.Feed("content_block_delta", []byte(frame))
	if err != nil {
		t.Fatalf("thinking delta: %v", err)
	}
	if len(deltas) != 1 || deltas[0].Message.Thinking != "hmm" {
		t.Fatalf("deltas = %#v", deltas)
	}
}

// TestFeedStreamError surfaces the upstream error from the event payload.
func TestFeedStreamError(t *testing.T) {
	acc := newAnthropicSSEAccumulator()
	_, _, err := acc.Feed("error", []byte(`{"type":"error","error":{"type":"overloaded_error","message":"try later"}}`))
	if err == nil {
		t.Fatal("expected error from stream error event")
	}
	if !strings.Contains(err.Error(), "try later") {
		t.Errorf("error = %v, want upstream message embedded", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/agent/ -run TestFeed -count=1`
Expected: FAIL — package `agent` under `cmd/agent` does not compile yet (no `.go` files), or undefined symbols.

- [ ] **Step 3: Write minimal implementation**

`cmd/agent/sse.go`:

```go
package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ollama/ollama/anthropic"
	"github.com/ollama/ollama/api"
)

// anthropicSSEAccumulator turns a stream of inbound Anthropic Messages SSE
// events into api.ChatResponse deltas for the engine's chatRound callback.
//
// It owns the one piece of state the shim must carry across frames: content
// blocks accumulate by index, and a tool_use block is only complete when its
// content_block_stop arrives (input_json_delta fragments must be joined
// before the ToolCall can be emitted).
type anthropicSSEAccumulator struct {
	blocks map[int]*anthropicBlockAccum
	done   bool
}

type anthropicBlockAccum struct {
	index int
	kind  string // "text", "thinking", "tool_use"
	text  strings.Builder
	tool  *api.ToolCall
}

func newAnthropicSSEAccumulator() *anthropicSSEAccumulator {
	return &anthropicSSEAccumulator{blocks: make(map[int]*anthropicBlockAccum)}
}

// Feed processes one SSE frame (event type + raw JSON data) and returns the
// ChatResponse deltas to hand to the engine, plus done=true after
// message_stop. It never returns an empty delta — the engine's chatRound
// treats an empty message as a stream-end sentinel.
func (a *anthropicSSEAccumulator) Feed(eventType string, data []byte) (deltas []api.ChatResponse, done bool, err error) {
	switch eventType {
	case "message_start", "ping", "message_delta":
		return nil, false, nil
	case "content_block_start":
		var ev anthropic.ContentBlockStartEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, false, fmt.Errorf("parse content_block_start: %w", err)
		}
		b := &anthropicBlockAccum{index: ev.Index}
		switch ev.ContentBlock.Type {
		case "tool_use":
			b.kind = "tool_use"
			b.tool = &api.ToolCall{
				ID: ev.ContentBlock.ID,
				Function: api.ToolCallFunction{
					Name:      ev.ContentBlock.Name,
					Arguments: api.ToolCallFunctionArguments{},
				},
			}
		default:
			b.kind = ev.ContentBlock.Type // "text", "thinking", or unknown
		}
		a.blocks[ev.Index] = b
		return nil, false, nil
	case "content_block_delta":
		var ev anthropic.ContentBlockDeltaEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, false, fmt.Errorf("parse content_block_delta: %w", err)
		}
		b, ok := a.blocks[ev.Index]
		if !ok {
			return nil, false, nil // delta for a block we never saw; ignore
		}
		switch ev.Delta.Type {
		case "text_delta":
			b.text.WriteString(ev.Delta.Text)
			return []api.ChatResponse{{Message: api.Message{Content: ev.Delta.Text}}}, false, nil
		case "thinking_delta":
			b.text.WriteString(ev.Delta.Thinking)
			return []api.ChatResponse{{Message: api.Message{Thinking: ev.Delta.Thinking}}}, false, nil
		case "input_json_delta", "signature_delta":
			b.text.WriteString(ev.Delta.PartialJSON + ev.Delta.Signature)
			return nil, false, nil
		}
		return nil, false, nil
	case "content_block_stop":
		var ev anthropic.ContentBlockStopEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, false, fmt.Errorf("parse content_block_stop: %w", err)
		}
		b, ok := a.blocks[ev.Index]
		if !ok {
			return nil, false, nil
		}
		if b.kind == "tool_use" && b.tool != nil {
			if s := strings.TrimSpace(b.text.String()); s != "" {
				var args map[string]any
				if err := json.Unmarshal([]byte(s), &args); err != nil {
					return nil, false, fmt.Errorf("parse accumulated tool_use input: %w", err)
				}
				for k, v := range args {
					b.tool.Function.Arguments.Set(k, v)
				}
			}
			return []api.ChatResponse{{Message: api.Message{ToolCalls: []api.ToolCall{*b.tool}}}}, false, nil
		}
		return nil, false, nil
	case "message_stop":
		if a.done {
			return nil, false, nil
		}
		a.done = true
		return []api.ChatResponse{{Done: true}}, true, nil
	case "error":
		var ev anthropic.StreamErrorEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, false, fmt.Errorf("parse stream error: %w", err)
		}
		return nil, false, fmt.Errorf("upstream error: %s: %s", ev.Error.Type, ev.Error.Message)
	default:
		return nil, false, fmt.Errorf("unexpected SSE event type %q", eventType)
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/agent/ -run TestFeed -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add cmd/agent/sse.go cmd/agent/sse_test.go
git commit -m "feat(agent): inbound Anthropic SSE accumulator"
```

---

### Task 3: `cmd/agent` — request converter (`api.ChatRequest` → `anthropic.MessagesRequest`)

**Files:**
- Create: `cmd/agent/shim.go` (converter only for now; the `Chat` method lands in Task 4)
- Test: `cmd/agent/shim_convert_test.go`

**Interfaces:**
- Consumes:
  - `api.ChatRequest` fields used: `Model`, `Messages`, `Stream *bool`, `Tools`, `Think *api.ThinkValue` (`api/types.go`)
  - `api.Message` (`Role`, `Content`, `Images []ImageData`, `ToolCalls`, `ToolCallID`)
  - `api.ToolCall`, `api.ToolCallFunction`, `api.ToolCallFunctionArguments` (has `Set(k, v)`)
  - `anthropic.MessagesRequest`, `MessageParam`, `ContentBlock`, `Tool`, `ThinkingConfig` (`anthropic/anthropic.go`)
  - `cmd/launch.AgentModelMeta` (Task 1)
- Produces:
  - `const defaultAgentMaxTokens = 4096`
  - `func buildMessagesRequest(req *api.ChatRequest, meta launch.AgentModelMeta) (*anthropic.MessagesRequest, error)`
    - System messages → `System` (joined with `"\n\n"`), excluded from `Messages`.
    - `role:"tool"` messages → grouped into ONE `user` message of `tool_result` blocks (Anthropic requires tool results in a user-role turn). `IsError` is always false (the engine returns error text in `Content`).
    - Assistant `Thinking` is **dropped** (see Global Constraints — no signature available).
    - `ToolCalls` → `tool_use` blocks (`Input: tc.Function.Arguments`).
    - `Images` → `image` blocks with magic-byte media-type sniffing.
    - `Tools` → `anthropic.Tool{Type: "custom", ...}` with `InputSchema` = marshaled `api.ToolFunction.Parameters`.
    - `Think` truthy → `Thinking{Type: "enabled", BudgetTokens: 20000}`.
    - `MaxTokens` = `meta.MaxOutputTokens` or `defaultAgentMaxTokens`; `Stream: true`.

- [ ] **Step 1: Write the failing test**

`cmd/agent/shim_convert_test.go`:

```go
package agent

import (
	"testing"

	"github.com/ollama/ollama/anthropic"
	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/cmd/launch"
)

func thinkValue(v any) *api.ThinkValue { return &api.ThinkValue{Value: v} }

// TestBuildMessagesRequestSystemExtraction: system messages move into the
// System field and never appear as a message param.
func TestBuildMessagesRequestSystemExtraction(t *testing.T) {
	req := &api.ChatRequest{
		Model: "deepseek-chat",
		Messages: []api.Message{
			{Role: "system", Content: "You are a helpful agent."},
			{Role: "user", Content: "hi"},
		},
	}
	out, err := buildMessagesRequest(req, launch.AgentModelMeta{MaxOutputTokens: 2048})
	if err != nil {
		t.Fatalf("buildMessagesRequest: %v", err)
	}
	if out.System != "You are a helpful agent." {
		t.Errorf("System = %q", out.System)
	}
	if len(out.Messages) != 1 || out.Messages[0].Role != "user" {
		t.Fatalf("Messages = %#v", out.Messages)
	}
	if out.MaxTokens != 2048 {
		t.Errorf("MaxTokens = %d, want 2048 from meta", out.MaxTokens)
	}
	if !out.Stream {
		t.Error("Stream must be true")
	}
}

// TestBuildMessagesRequestToolResultGrouping: consecutive role:"tool" messages
// collapse into one user message of tool_result blocks.
func TestBuildMessagesRequestToolResultGrouping(t *testing.T) {
	req := &api.ChatRequest{
		Model: "m",
		Messages: []api.Message{
			{Role: "assistant", Content: "", ToolCalls: []api.ToolCall{{ID: "toolu_1", Function: api.ToolCallFunction{Name: "read_file"}}}},
			{Role: "tool", ToolCallID: "toolu_1", Content: "file contents"},
			{Role: "tool", ToolCallID: "toolu_2", Content: "second result"},
			{Role: "user", Content: "thanks"},
		},
	}
	out, err := buildMessagesRequest(req, launch.AgentModelMeta{})
	if err != nil {
		t.Fatalf("buildMessagesRequest: %v", err)
	}
	if len(out.Messages) != 3 {
		t.Fatalf("Messages = %d, want [assistant, user(tool_result), user]", len(out.Messages))
	}
	tr := out.Messages[1]
	if tr.Role != "user" {
		t.Errorf("tool result message role = %q, want user", tr.Role)
	}
	if len(tr.Content) != 2 {
		t.Fatalf("tool_result blocks = %d, want 2", len(tr.Content))
	}
	if tr.Content[0].Type != "tool_result" || tr.Content[0].ToolUseID != "toolu_1" || tr.Content[0].Content != "file contents" {
		t.Errorf("block[0] = %#v", tr.Content[0])
	}
	if tr.Content[1].ToolUseID != "toolu_2" {
		t.Errorf("block[1] ToolUseID = %q", tr.Content[1].ToolUseID)
	}
}

// TestBuildMessagesRequestToolUseBlock: assistant tool calls become tool_use
// blocks carrying the ordered arguments.
func TestBuildMessagesRequestToolUseBlock(t *testing.T) {
	args := api.ToolCallFunctionArguments{}
	args.Set("path", "/tmp/a.txt")
	req := &api.ChatRequest{
		Model: "m",
		Messages: []api.Message{
			{Role: "assistant", ToolCalls: []api.ToolCall{{ID: "toolu_9", Function: api.ToolCallFunction{Name: "read_file", Arguments: args}}}},
		},
	}
	out, err := buildMessagesRequest(req, launch.AgentModelMeta{})
	if err != nil {
		t.Fatalf("buildMessagesRequest: %v", err)
	}
	block := out.Messages[0].Content[0]
	if block.Type != "tool_use" || block.ID != "toolu_9" || block.Name != "read_file" {
		t.Fatalf("block = %#v", block)
	}
	if got := block.Input.ToMap()["path"]; got != "/tmp/a.txt" {
		t.Errorf("input path = %v", got)
	}
}

// TestBuildMessagesRequestTools: api.ToolFunction parameters marshal into the
// Anthropic input_schema.
func TestBuildMessagesRequestTools(t *testing.T) {
	req := &api.ChatRequest{
		Model: "m",
		Tools: api.Tools{{
			Function: api.ToolFunction{
				Name:        "read_file",
				Description: "Read a file",
				Parameters: api.ToolFunctionParameters{
					Type: "object",
					Required: []string{"path"},
					Properties: &api.ToolPropertiesMap{},
				},
			},
		}},
	}
	out, err := buildMessagesRequest(req, launch.AgentModelMeta{})
	if err != nil {
		t.Fatalf("buildMessagesRequest: %v", err)
	}
	if len(out.Tools) != 1 {
		t.Fatalf("Tools = %d, want 1", len(out.Tools))
	}
	tt := out.Tools[0]
	if tt.Type != "custom" || tt.Name != "read_file" || tt.Description != "Read a file" {
		t.Errorf("tool = %#v", tt)
	}
	var schema map[string]any
	if err := json.Unmarshal(tt.InputSchema, &schema); err != nil {
		t.Fatalf("InputSchema is not valid JSON: %v (%s)", err, tt.InputSchema)
	}
	if schema["type"] != "object" {
		t.Errorf("InputSchema type = %v, want object", schema["type"])
	}
	if req, ok := schema["required"].([]any); !ok || len(req) != 1 || req[0] != "path" {
		t.Errorf("InputSchema required = %#v, want [path]", schema["required"])
	}
}

// TestBuildMessagesRequestThink: a truthy Think enables Anthropic thinking.
func TestBuildMessagesRequestThink(t *testing.T) {
	req := &api.ChatRequest{Model: "m", Think: thinkValue(true)}
	out, err := buildMessagesRequest(req, launch.AgentModelMeta{})
	if err != nil {
		t.Fatalf("buildMessagesRequest: %v", err)
	}
	if out.Thinking == nil || out.Thinking.Type != "enabled" {
		t.Errorf("Thinking = %#v, want enabled", out.Thinking)
	}

	req.Think = thinkValue("high")
	out, err = buildMessagesRequest(req, launch.AgentModelMeta{})
	if err != nil {
		t.Fatalf("buildMessagesRequest: %v", err)
	}
	if out.Thinking == nil {
		t.Error("string think level should also enable thinking")
	}

	req.Think = thinkValue(false)
	out, err = buildMessagesRequest(req, launch.AgentModelMeta{})
	if err != nil {
		t.Fatalf("buildMessagesRequest: %v", err)
	}
	if out.Thinking != nil {
		t.Errorf("Thinking = %#v, want nil when disabled", out.Thinking)
	}
}

// TestBuildMessagesRequestMaxTokensDefault: no meta → 4096 floor.
func TestBuildMessagesRequestMaxTokensDefault(t *testing.T) {
	out, err := buildMessagesRequest(&api.ChatRequest{Model: "m"}, launch.AgentModelMeta{})
	if err != nil {
		t.Fatalf("buildMessagesRequest: %v", err)
	}
	if out.MaxTokens != defaultAgentMaxTokens {
		t.Errorf("MaxTokens = %d, want %d", out.MaxTokens, defaultAgentMaxTokens)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/agent/ -run TestBuildMessagesRequest -count=1`
Expected: FAIL — undefined: `buildMessagesRequest`

- [ ] **Step 3: Write minimal implementation**

Add to `cmd/agent/shim.go`:

```go
package agent

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ollama/ollama/anthropic"
	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/cmd/launch"
)

// defaultAgentMaxTokens is the fallback max_tokens when the launch inventory
// has no MaxOutputTokens for the resolved model (spec floor).
const defaultAgentMaxTokens = 4096

// thinkingBudget is the Anthropic thinking budget for enabled thinking. The
// engine passes no budget level through api.ChatRequest, so one fixed budget
// is used for every enabled level.
const thinkingBudget = 20000

// buildMessagesRequest converts the engine's api.ChatRequest into an
// Anthropic MessagesRequest. It is the reverse of anthropic.convertMessage:
// tool_use blocks become api.ToolCalls and consecutive role:"tool" messages
// are grouped into a single user message of tool_result blocks (Anthropic
// requires tool results to live in a user-role turn following the assistant's
// tool_use blocks).
//
// Assistant thinking is dropped on the way out: api.Message carries no
// thinking signature, and Anthropic rejects assistant thinking blocks without
// one. The model does not need the echo; the final text content is sent.
func buildMessagesRequest(req *api.ChatRequest, meta launch.AgentModelMeta) (*anthropic.MessagesRequest, error) {
	out := &anthropic.MessagesRequest{
		Model:     req.Model,
		MaxTokens: meta.MaxOutputTokens,
		Stream:    true,
	}
	if out.MaxTokens <= 0 {
		out.MaxTokens = defaultAgentMaxTokens
	}

	var system []string
	var messages []anthropic.MessageParam
	var pendingToolResults []anthropic.ContentBlock
	flushToolResults := func() {
		if len(pendingToolResults) == 0 {
			return
		}
		messages = append(messages, anthropic.MessageParam{Role: "user", Content: pendingToolResults})
		pendingToolResults = nil
	}

	for _, msg := range req.Messages {
		switch msg.Role {
		case "system":
			system = append(system, msg.Content)
		case "tool":
			pendingToolResults = append(pendingToolResults, anthropic.ContentBlock{
				Type:      "tool_result",
				ToolUseID: msg.ToolCallID,
				Content:   msg.Content,
			})
		default:
			flushToolResults()
			param := anthropic.MessageParam{Role: msg.Role}
			if msg.Content != "" {
				param.Content = append(param.Content, anthropic.ContentBlock{Type: "text", Text: &msg.Content})
			}
			for _, img := range msg.Images {
				data := []byte(img)
				param.Content = append(param.Content, anthropic.ContentBlock{
					Type: "image",
					Source: &anthropic.ImageSource{
						Type:      "base64",
						MediaType: imageMediaType(data),
						Data:      base64.StdEncoding.EncodeToString(data),
					},
				})
			}
			for _, tc := range msg.ToolCalls {
				param.Content = append(param.Content, anthropic.ContentBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: tc.Function.Arguments,
				})
			}
			if len(param.Content) > 0 {
				messages = append(messages, param)
			}
		}
	}
	flushToolResults()

	if len(system) > 0 {
		out.System = strings.Join(system, "\n\n")
	}
	out.Messages = messages

	if len(req.Tools) > 0 {
		out.Tools = make([]anthropic.Tool, 0, len(req.Tools))
		for _, t := range req.Tools {
			params, err := json.Marshal(t.Function.Parameters)
			if err != nil {
				return nil, fmt.Errorf("marshal tool %q parameters: %w", t.Function.Name, err)
			}
			out.Tools = append(out.Tools, anthropic.Tool{
				Type:        "custom",
				Name:        t.Function.Name,
				Description: t.Function.Description,
				InputSchema: params,
			})
		}
	}

	if thinkEnabled(req.Think) {
		out.Thinking = &anthropic.ThinkingConfig{Type: "enabled", BudgetTokens: thinkingBudget}
	}

	return out, nil
}

// thinkEnabled reports whether a ThinkValue requests thinking (bool true or a
// known level string).
func thinkEnabled(t *api.ThinkValue) bool {
	if t == nil || t.Value == nil {
		return false
	}
	switch v := t.Value.(type) {
	case bool:
		return v
	case string:
		return v == "high" || v == "medium" || v == "low" || v == "max"
	default:
		return false
	}
}

// imageMediaType sniffs image bytes to fill Anthropic's required media_type.
func imageMediaType(b []byte) string {
	switch {
	case len(b) >= 12 && bytes.Equal(b[:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return "image/webp"
	case len(b) >= 4 && bytes.Equal(b[:4], []byte{0x89, 'P', 'N', 'G'}):
		return "image/png"
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "image/jpeg"
	case len(b) >= 3 && bytes.Equal(b[:3], []byte("GIF")):
		return "image/gif"
	default:
		return "image/png"
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/agent/ -run TestBuildMessagesRequest -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add cmd/agent/shim.go cmd/agent/shim_convert_test.go
git commit -m "feat(agent): api.ChatRequest to Anthropic MessagesRequest converter"
```

---

### Task 4: `cmd/agent` — `ChatClient` shim + integration

**Files:**
- Modify: `cmd/agent/shim.go` (add `shimClient` and `Chat`)
- Test: `cmd/agent/shim_test.go` (httptest fake + `agent.Session.Run` integration)

**Interfaces:**
- Consumes:
  - `buildMessagesRequest` (Task 3), `anthropicSSEAccumulator` (Task 2)
  - `api.ChatResponseFunc` (`api/client.go:291`), `api.ChatRequest`, `api.ChatResponse`
  - `agent.ChatClient` interface (`agent/session.go:18`), `agent.Session`, `agent.Registry`, `agent.Tool`, `agent.ToolResult`, `agent.ToolContext` (`agent/registry.go`)
  - `cmd/launch.AgentModelMeta` (Task 1)
- Produces:
  - `type shimClient struct { baseURL, token, model string; meta launch.AgentModelMeta; httpClient *http.Client }`
  - `func newShimClient(baseURL, token, model string, meta launch.AgentModelMeta) *shimClient`
  - `func (s *shimClient) Chat(ctx context.Context, req *api.ChatRequest, fn api.ChatResponseFunc) error`
    - POST `baseURL + "/v1/messages"` with `Authorization: Bearer <token>`, JSON body from Task 3, 5-minute HTTP timeout (precedent `anthropic_openai_proxy.go`).
    - Non-2xx → parse `anthropic.Error` JSON → non-nil error.
    - Streams SSE via `bufio.Scanner` (16 MiB buffer cap); skips `event:` lines, reads `data:` lines; breaks on `[DONE]`; feeds frames to the accumulator and forwards deltas to `fn`.
    - Returns nil after `message_stop` (the engine terminates on `Chat` returning).

- [ ] **Step 1: Write the failing test**

`cmd/agent/shim_test.go`:

```go
package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ollama/ollama/agent"
	"github.com/ollama/ollama/anthropic"
	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/cmd/launch"
)

// fakeAnthropic serves Anthropic /v1/messages with a pluggable stream writer
// and records the requests it receives.
type fakeAnthropic struct {
	mu        sync.Mutex
	requests  []anthropic.MessagesRequest
	stream    func(w http.ResponseWriter, req anthropic.MessagesRequest)
	status    int
	errorBody string
}

func (f *fakeAnthropic) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, `{"type":"authentication_error","message":"bad token"}`, http.StatusUnauthorized)
			return
		}
		var m anthropic.MessagesRequest
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.requests = append(f.requests, m)
		f.mu.Unlock()
		if f.status != 0 {
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(f.errorBody))
			return
		}
		if f.stream != nil {
			f.stream(w, m)
		}
	})
	return mux
}

func writeSSE(w http.ResponseWriter, frames ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, frame := range frames {
		_, _ = w.Write([]byte(frame))
		_, _ = w.Write([]byte("\n\n"))
	}
}

func textStream() []string {
	return []string{
		`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"m"}}`,
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello, "}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world!"}}`,
		`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}`,
		`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
	}
}

// TestShimClientTextStream: deltas stream to the callback, Done frame arrives
// at message_stop, Chat returns nil.
func TestShimClientTextStream(t *testing.T) {
	fake := &fakeAnthropic{stream: func(w http.ResponseWriter, _ anthropic.MessagesRequest) {
		writeSSE(w, textStream()...)
	}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	shim := newShimClient(srv.URL, "test-token", "m", launch.AgentModelMeta{})
	var got []string
	var done bool
	err := shim.Chat(context.Background(), &api.ChatRequest{Model: "m"}, func(resp api.ChatResponse) error {
		if resp.Message.Content != "" {
			got = append(got, resp.Message.Content)
		}
		if resp.Done {
			done = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if strings.Join(got, "") != "Hello, world!" {
		t.Errorf("streamed content = %q", strings.Join(got, ""))
	}
	if !done {
		t.Error("expected a Done frame after message_stop")
	}
}

// TestShimClientToolUse: a tool_use block streams in and is emitted as a
// ToolCall once at content_block_stop.
func TestShimClientToolUse(t *testing.T) {
	fake := &fakeAnthropic{stream: func(w http.ResponseWriter, _ anthropic.MessagesRequest) {
		writeSSE(w,
			`event: content_block_start`+"\n"+`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"read_file","input":{}}}`,
			`event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"/tmp/a.txt\"}"}}`,
			`event: content_block_stop`+"\n"+`data: {"type":"content_block_stop","index":0}`,
			`event: message_stop`+"\n"+`data: {"type":"message_stop"}`,
		)
	}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	shim := newShimClient(srv.URL, "test-token", "m", launch.AgentModelMeta{})
	var calls []api.ToolCall
	err := shim.Chat(context.Background(), &api.ChatRequest{Model: "m"}, func(resp api.ChatResponse) error {
		calls = append(calls, resp.Message.ToolCalls...)
		return nil
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(calls) != 1 || calls[0].Function.Name != "read_file" {
		t.Fatalf("calls = %#v", calls)
	}
	if calls[0].Function.Arguments.ToMap()["path"] != "/tmp/a.txt" {
		t.Errorf("args = %#v", calls[0].Function.Arguments.ToMap())
	}
}

// TestShimClientErrorEvent: an SSE error event surfaces as a non-nil error.
func TestShimClientErrorEvent(t *testing.T) {
	fake := &fakeAnthropic{stream: func(w http.ResponseWriter, _ anthropic.MessagesRequest) {
		writeSSE(w, `event: error`+"\n"+`data: {"type":"error","error":{"type":"overloaded_error","message":"try later"}}`)
	}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	shim := newShimClient(srv.URL, "test-token", "m", launch.AgentModelMeta{})
	err := shim.Chat(context.Background(), &api.ChatRequest{Model: "m"}, func(api.ChatResponse) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "try later") {
		t.Fatalf("err = %v, want upstream message", err)
	}
}

// TestShimClientHTTPError: a non-2xx response with an Anthropic-shaped error
// body yields a descriptive error.
func TestShimClientHTTPError(t *testing.T) {
	fake := &fakeAnthropic{status: http.StatusTooManyRequests, errorBody: `{"type":"rate_limit_error","message":"slow down"}`}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	shim := newShimClient(srv.URL, "test-token", "m", launch.AgentModelMeta{})
	err := shim.Chat(context.Background(), &api.ChatRequest{Model: "m"}, func(api.ChatResponse) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "slow down") {
		t.Fatalf("err = %v, want rate-limit message", err)
	}
}

// stubTool is a minimal agent.Tool for the integration test.
type stubTool struct{}

func (stubTool) Name() string                                 { return "read_file" }
func (stubTool) Description() string                          { return "stub read_file" }
func (stubTool) Schema() api.ToolFunction                     { return api.ToolFunction{Name: "read_file", Parameters: api.ToolFunctionParameters{Type: "object"}} }
func (stubTool) Execute(ctx context.Context, tc agent.ToolContext, args map[string]any) (agent.ToolResult, error) {
	return agent.ToolResult{Content: "stub file contents"}, nil
}

// TestSessionRunToolLoop: an end-to-end agent.Session.Run over the shim. The
// fake streams a read_file tool_use on the first call, then a plain text
// answer once it sees the tool_result fed back — proving the shim's round
// trip (tool call → execute → feed result → next model call).
func TestSessionRunToolLoop(t *testing.T) {
	fake := &fakeAnthropic{stream: func(w http.ResponseWriter, req anthropic.MessagesRequest) {
		hasToolResult := false
		for _, m := range req.Messages {
			for _, b := range m.Content {
				if b.Type == "tool_result" {
					hasToolResult = true
				}
			}
		}
		if !hasToolResult {
			writeSSE(w,
				`event: content_block_start`+"\n"+`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"read_file","input":{}}}`,
				`event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"/tmp/a.txt\"}"}}`,
				`event: content_block_stop`+"\n"+`data: {"type":"content_block_stop","index":0}`,
				`event: message_stop`+"\n"+`data: {"type":"message_stop"}`,
			)
			return
		}
		writeSSE(w,
			`event: content_block_start`+"\n"+`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Done reading."}}`,
			`event: content_block_stop`+"\n"+`data: {"type":"content_block_stop","index":0}`,
			`event: message_stop`+"\n"+`data: {"type":"message_stop"}`,
		)
	}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	shim := newShimClient(srv.URL, "test-token", "m", launch.AgentModelMeta{ToolCapable: true})
	sess := &agent.Session{
		Client: shim,
		Tools:  &agent.Registry{},
	}
	sess.Tools.Register(stubTool{})

	result, err := sess.Run(context.Background(), agent.RunOptions{
		Model:    "m",
		Messages: []api.Message{{Role: "user", Content: "read /tmp/a.txt"}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Messages) == 0 {
		t.Fatal("no messages in result")
	}
	last := result.Messages[len(result.Messages)-1]
	if !strings.Contains(last.Content, "Done reading.") {
		t.Errorf("final assistant content = %q, want the tool-fed answer", last.Content)
	}

	// The second request must have carried the tool_result back to the model.
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) < 2 {
		t.Fatalf("requests = %d, want >= 2 (model → tool result → model)", len(fake.requests))
	}
	lastReq := fake.requests[len(fake.requests)-1]
	found := false
	for _, m := range lastReq.Messages {
		for _, b := range m.Content {
			if b.Type == "tool_result" && b.ToolUseID == "toolu_1" && b.Content == "stub file contents" {
				found = true
			}
		}
	}
	if !found {
		t.Error("tool_result was not fed back to the model in the final request")
	}
}
```

Note on `agent.Registry{}` vs a constructor: there is no exported constructor — `&agent.Registry{}` is the established pattern (`cmd/agent_tui.go`).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/agent/ -run TestShimClient -count=1`
Expected: FAIL — undefined: `newShimClient`

- [ ] **Step 3: Write minimal implementation**

Append to `cmd/agent/shim.go`:

```go
import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ollama/ollama/cmd/launch"
)

// shimTimeout matches the translation proxy's upstream timeout so a hung
// upstream fails the run instead of stalling it forever.
const shimTimeout = 5 * time.Minute

// maxSSEFrameSize bounds a single SSE data line (input_json_delta fragments
// are the only long lines; tool schemas are small).
const maxSSEFrameSize = 16 * 1024 * 1024

// shimClient implements agent.ChatClient by speaking the Anthropic Messages
// API to a resolved endpoint: for user remotes the loopback translation proxy,
// for cloud/local the OAICA router or local server (behind the logging proxy).
// The endpoint always presents Anthropic-native /v1/messages, so this shim is
// the one protocol the agent engine sees regardless of upstream.
type shimClient struct {
	baseURL   string // loopback proxy or router; shim appends "/v1/messages"
	token     string // bearer token
	model     string // bare upstream model id
	meta      launch.AgentModelMeta
	httpClient *http.Client
}

func newShimClient(baseURL, token, model string, meta launch.AgentModelMeta) *shimClient {
	return &shimClient{
		baseURL:    baseURL,
		token:      token,
		model:      model,
		meta:       meta,
		httpClient: &http.Client{Timeout: shimTimeout},
	}
}

func (s *shimClient) Chat(ctx context.Context, req *api.ChatRequest, fn api.ChatResponseFunc) error {
	mreq, err := buildMessagesRequest(req, s.meta)
	if err != nil {
		return err
	}
	body, err := json.Marshal(mreq)
	if err != nil {
		return fmt.Errorf("marshal messages request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Authorization", "Bearer "+s.token)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return parseAPIError(resp)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxSSEFrameSize)
	acc := newAnthropicSSEAccumulator()

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "event:") {
			continue // the event type is also in the JSON payload
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(data), &envelope); err != nil {
			return fmt.Errorf("parse SSE frame: %w", err)
		}
		deltas, done, err := acc.Feed(envelope.Type, []byte(data))
		if err != nil {
			return err
		}
		for _, d := range deltas {
			if err := fn(d); err != nil {
				return err
			}
		}
		if done {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read SSE stream: %w", err)
	}
	return nil
}

// parseAPIError extracts an Anthropic-shaped error body from a non-2xx
// response, falling back to the raw body.
func parseAPIError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var e anthropic.Error
	if err := json.Unmarshal(body, &e); err == nil && e.Message != "" {
		return fmt.Errorf("upstream %s: %s", resp.Status, e.Message)
	}
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = resp.Status
	}
	return fmt.Errorf("upstream error: %s", msg)
}
```

(Note: the imports at the top of `shim.go` are the union of Task 3's and Task 4's `import` blocks — keep them merged into one `import (...)` at the top of the file.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/agent/ -count=1`
Expected: PASS (all shim + integration + SSE + converter tests)

- [ ] **Step 5: Commit**

```bash
git add cmd/agent/shim.go cmd/agent/shim_test.go
git commit -m "feat(agent): streaming Anthropic ChatClient shim + session integration"
```

---

### Task 5: `cmd/agent` — plain-stdout renderer (`EventSink`)

**Files:**
- Create: `cmd/agent/render.go`
- Test: `cmd/agent/render_test.go`

**Interfaces:**
- Consumes (existing): `agent.EventSink`, `agent.Event`, event types `EventMessageDelta`, `EventThinkingDelta`, `EventToolCallDetected`, `EventToolFinished`, `EventRunFinished`, `EventError` (`agent/events.go`); `readline.ColorGrey/ColorDefault/ColorBold` (`readline/types.go`).
- Produces:
  - `type stdoutSink struct { out io.Writer; errOut io.Writer }`
  - `func newStdoutSink() *stdoutSink` (writes to `os.Stdout`/`os.Stderr`)
  - `func (s *stdoutSink) Emit(ev agent.Event) error`
    - `EventMessageDelta` → raw streaming text (no newline; the model's own newlines land naturally).
    - `EventThinkingDelta` → grey `┄ <line>` per line.
    - `EventToolCallDetected` → `  ◆ <bold>name</bold> <compact args>`.
    - `EventToolFinished` → `  ✓ <name>`; on error `  ✗ <name> <error>`; indented, truncated tool output below.
    - `EventRunFinished` → blank line (final answer already streamed).
    - `EventError` → **returns** `fmt.Errorf(ev.Error)` without printing — cobra prints the single "Error:" line and the exit code stays non-zero.

- [ ] **Step 1: Write the failing test**

`cmd/agent/render_test.go`:

```go
package agent

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/ollama/ollama/agent"
	"github.com/ollama/ollama/api"
)

func newTestSink() (*stdoutSink, *bytes.Buffer, *bytes.Buffer) {
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	return &stdoutSink{out: out, errOut: errOut}, out, errOut
}

func TestSinkMessageDelta(t *testing.T) {
	s, out, _ := newTestSink()
	_ = s.Emit(agent.Event{Type: agent.EventMessageDelta, Content: "Hello"})
	_ = s.Emit(agent.Event{Type: agent.EventMessageDelta, Content: ", world"})
	if out.String() != "Hello, world" {
		t.Errorf("stdout = %q, want continuous stream", out.String())
	}
}

func TestSinkToolCallAndFinished(t *testing.T) {
	s, out, _ := newTestSink()
	_ = s.Emit(agent.Event{
		Type: agent.EventToolCallDetected,
		ToolCalls: []api.ToolCall{{Function: api.ToolCallFunction{Name: "read_file"}}},
	})
	_ = s.Emit(agent.Event{Type: agent.EventToolFinished, ToolName: "read_file", Content: "line1\nline2"})
	got := out.String()
	for _, want := range []string{"read_file", "line1", "line2"} {
		if !strings.Contains(got, want) {
			t.Errorf("stdout missing %q:\n%s", want, got)
		}
	}
}

func TestSinkToolFinishedTruncatesLongOutput(t *testing.T) {
	s, out, _ := newTestSink()
	long := strings.Repeat("x", 5000)
	_ = s.Emit(agent.Event{Type: agent.EventToolFinished, ToolName: "bash", Content: long})
	if !strings.Contains(out.String(), "… (truncated)") {
		t.Error("long tool output should be truncated with an ellipsis marker")
	}
}

func TestSinkErrorReturnsError(t *testing.T) {
	s, _, errOut := newTestSink()
	err := s.Emit(agent.Event{Type: agent.EventError, Error: "boom"})
	if err == nil || err.Error() != "boom" {
		t.Errorf("err = %v, want the event error", err)
	}
	if errOut.Len() != 0 {
		t.Errorf("EventError must not print itself (cobra prints it): %q", errOut.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/agent/ -run TestSink -count=1`
Expected: FAIL — undefined: `stdoutSink`

- [ ] **Step 3: Write minimal implementation**

`cmd/agent/render.go`:

```go
package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ollama/ollama/agent"
	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/readline"
)

// maxToolOutput caps the tool output echoed to the terminal so a runaway
// command cannot flood the transcript.
const maxToolOutput = 4000

// stdoutSink renders engine events as plain streaming text on stdout — the
// non-TUI counterpart of the bubbletea chat renderer (cmd/tui/chat). Writers
// are fields so tests can capture output without touching os.Stdout.
type stdoutSink struct {
	out    io.Writer // streaming assistant text + tool status
	errOut io.Writer // errors (unused by Emit — see EventError case)
}

func newStdoutSink() *stdoutSink {
	return &stdoutSink{out: os.Stdout, errOut: os.Stderr}
}

func (s *stdoutSink) Emit(ev agent.Event) error {
	switch ev.Type {
	case agent.EventMessageDelta:
		fmt.Fprint(s.out, ev.Content)

	case agent.EventThinkingDelta:
		fmt.Fprintf(s.out, "%s┄ %s%s\n", readline.ColorGrey, strings.TrimRight(ev.Thinking, "\n"), readline.ColorDefault)

	case agent.EventToolCallDetected:
		for _, tc := range ev.ToolCalls {
			fmt.Fprintf(s.out, "\n  ◆ %s%s%s %s\n", readline.ColorBold, tc.Function.Name, readline.ColorDefault, compactToolArgs(tc))
		}

	case agent.EventToolStarted:
		// The call was already announced on EventToolCallDetected.

	case agent.EventToolFinished:
		if ev.Error != "" {
			fmt.Fprintf(s.out, "  ✗ %s %s\n", ev.ToolName, ev.Error)
			return nil
		}
		fmt.Fprintf(s.out, "  ✓ %s\n", ev.ToolName)
		if content := strings.TrimSpace(ev.Content); content != "" {
			fmt.Fprintln(s.out, indentBlock(truncateToolOutput(content), "    "))
		}

	case agent.EventRunFinished:
		fmt.Fprintln(s.out)

	case agent.EventError:
		// Do not print here: Session.Run returns the error and cobra prints
		// the single "Error: …" line. Returning the error keeps the exit
		// code non-zero.
		return fmt.Errorf("%s", ev.Error)
	}
	return nil
}

// compactToolArgs renders a tool call's arguments as a single JSON line,
// capped for the status line.
func compactToolArgs(tc api.ToolCall) string {
	b, err := json.Marshal(tc.Function.Arguments.ToMap())
	if err != nil {
		return ""
	}
	s := string(b)
	const max = 80
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

func truncateToolOutput(s string) string {
	if len(s) > maxToolOutput {
		return s[:maxToolOutput] + "\n… (truncated)"
	}
	return s
}

func indentBlock(s, indent string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = indent + line
	}
	return strings.Join(lines, "\n")
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/agent/ -run TestSink -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add cmd/agent/render.go cmd/agent/render_test.go
git commit -m "feat(agent): plain-stdout EventSink renderer"
```

---

### Task 6: `cmd/agent` — tool registry + approval prompter

**Files:**
- Create: `cmd/agent/tools.go`, `cmd/agent/approval.go`
- Test: `cmd/agent/tools_test.go`

**Interfaces:**
- Consumes (existing): `agent.Registry` (`&agent.Registry{}` — no constructor), `agent/tools` constructors `&agenttools.Bash{}`, `&agenttools.Read{}`, `&agenttools.Edit{}`, `&agenttools.Skill{Catalog: skills}`, `&agenttools.WebSearch{}`, `&agenttools.WebFetch{}`; `agent.SkillCatalog` (`List() []Skill`); `agent.ApprovalPrompter`, `agent.Approval`, `agent.ApprovalRequest` (`agent/approval.go`); env vars `OLLAMA_AGENT_DISABLE_SHELL`, `OLLAMA_AGENT_DISABLE_WEBSEARCH` (precedent `cmd/agent_tui.go:257`); `cmd/launch.AgentModelMeta` (Task 1).
- Produces:
  - `func agentRegistry(skills *agent.SkillCatalog, meta launch.AgentModelMeta) *agent.Registry` — nil when `!meta.ToolCapable`; Bash gated on `OLLAMA_AGENT_DISABLE_SHELL`; WebSearch/WebFetch gated on `OLLAMA_AGENT_DISABLE_WEBSEARCH`; Skill only when the catalog lists skills.
  - `type terminalApprovalPrompter struct { in io.Reader; out io.Writer }`
  - `func newTerminalApprovalPrompter(in io.Reader, out io.Writer) *terminalApprovalPrompter`
  - `func (p *terminalApprovalPrompter) PromptApproval(ctx context.Context, req agent.ApprovalRequest) (agent.Approval, error)`
    - `y` → `Approval{Allow: true, AllowScopes: [scope]}` (approve call + remember scope for the run); `a` → `Approval{Allow: true, AllowAll: true}`; anything else (default `n`) → `Approval{Reason: "Denied by user."}`. If the batch has multiple calls, all must approve or the batch is denied.
  - `type autoApprovePrompter struct{}` — always `Approval{Allow: true, AllowAll: true}`.

- [ ] **Step 1: Write the failing test**

`cmd/agent/tools_test.go`:

```go
package agent

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/ollama/ollama/agent"
	"github.com/ollama/ollama/cmd/launch"
)

// TestAgentRegistryEnvGating: OLLAMA_AGENT_DISABLE_SHELL / _WEBSEARCH prune
// the corresponding tools.
func TestAgentRegistryEnvGating(t *testing.T) {
	reg := agentRegistry(nil, launch.AgentModelMeta{ToolCapable: true})
	for _, want := range []string{"bash", "read", "edit", "web_search", "web_fetch"} {
		if !regHas(reg, want) {
			t.Errorf("registry missing %q", want)
		}
	}

	t.Setenv("OLLAMA_AGENT_DISABLE_SHELL", "1")
	t.Setenv("OLLAMA_AGENT_DISABLE_WEBSEARCH", "1")
	reg = agentRegistry(nil, launch.AgentModelMeta{ToolCapable: true})
	if regHas(reg, "bash") {
		t.Error("bash should be disabled by OLLAMA_AGENT_DISABLE_SHELL")
	}
	if regHas(reg, "web_search") || regHas(reg, "web_fetch") {
		t.Error("web tools should be disabled by OLLAMA_AGENT_DISABLE_WEBSEARCH")
	}
	for _, keep := range []string{"read", "edit"} {
		if !regHas(reg, keep) {
			t.Errorf("registry should still have %q", keep)
		}
	}
}

// TestAgentRegistryToolIncapableModel: a model that cannot call tools yields a
// nil registry (the engine then finalizes text-only).
func TestAgentRegistryToolIncapableModel(t *testing.T) {
	if reg := agentRegistry(nil, launch.AgentModelMeta{ToolCapable: false}); reg != nil {
		t.Error("registry should be nil for a non-tool-capable model")
	}
}

func regHas(reg *agent.Registry, name string) bool {
	if reg == nil {
		return false
	}
	_, ok := reg.Get(name)
	return ok
}

// TestTerminalApprovalPrompter: y approves (and remembers the scope), a
// grants all, n denies, and the batch is denied if any call is refused.
func TestTerminalApprovalPrompter(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantAllow bool
	}{
		{"yes", "y\n", true},
		{"always", "a\n", true},
		{"no", "n\n", false},
		{"default no", "\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTerminalApprovalPrompter(strings.NewReader(tt.input), &bytes.Buffer{})
			req := agent.ApprovalRequest{}
			req.AddToolCall("t1", "bash", "bash", map[string]any{"command": "ls"})
			got, err := p.PromptApproval(context.Background(), req)
			if err != nil {
				t.Fatalf("PromptApproval: %v", err)
			}
			if got.Allow != tt.wantAllow {
				t.Errorf("Allow = %v, want %v (reason=%q)", got.Allow, tt.wantAllow, got.Reason)
			}
		})
	}
}

// TestTerminalApprovalPrompterDeniesPartialBatch: a "no" on the second of two
// calls denies the whole batch.
func TestTerminalApprovalPrompterDeniesPartialBatch(t *testing.T) {
	p := newTerminalApprovalPrompter(strings.NewReader("y\nn\n"), &bytes.Buffer{})
	req := agent.ApprovalRequest{}
	req.AddToolCall("t1", "bash", "bash", map[string]any{})
	req.AddToolCall("t2", "bash", "bash", map[string]any{})
	got, err := p.PromptApproval(context.Background(), req)
	if err != nil {
		t.Fatalf("PromptApproval: %v", err)
	}
	if got.Allow {
		t.Error("batch with a denied call must be denied")
	}
}

// TestAutoApprovePrompter: always grants everything.
func TestAutoApprovePrompter(t *testing.T) {
	got, err := (autoApprovePrompter{}).PromptApproval(context.Background(), agent.ApprovalRequest{})
	if err != nil {
		t.Fatalf("PromptApproval: %v", err)
	}
	if !got.Allow || !got.AllowAll {
		t.Errorf("got = %#v, want blanket approval", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/agent/ -run 'TestAgentRegistry|TestTerminalApproval|TestAutoApprove' -count=1`
Expected: FAIL — undefined: `agentRegistry`, `terminalApprovalPrompter`, `autoApprovePrompter`

- [ ] **Step 3: Write minimal implementation**

`cmd/agent/tools.go`:

```go
package agent

import (
	"os"

	"github.com/ollama/ollama/agent"
	agenttools "github.com/ollama/ollama/agent/tools"
	"github.com/ollama/ollama/cmd/launch"
)

// agentRegistry builds the tool registry for an agent run. Unlike the TUI's
// registry (cmd/agent_tui.go) there is no local server to probe for
// capabilities, so gating is driven by env vars (the user's explicit control)
// plus AgentModelMeta.ToolCapable from the launch inventory. A nil return
// tells the engine to run text-only.
func agentRegistry(skills *agent.SkillCatalog, meta launch.AgentModelMeta) *agent.Registry {
	if !meta.ToolCapable {
		return nil
	}
	registry := &agent.Registry{}
	if os.Getenv("OLLAMA_AGENT_DISABLE_SHELL") == "" {
		registry.Register(&agenttools.Bash{})
	}
	registry.Register(&agenttools.Read{})
	registry.Register(&agenttools.Edit{})
	if skills != nil && len(skills.List()) > 0 {
		registry.Register(&agenttools.Skill{Catalog: skills})
	}
	if os.Getenv("OLLAMA_AGENT_DISABLE_WEBSEARCH") == "" {
		registry.Register(&agenttools.WebSearch{})
		registry.Register(&agenttools.WebFetch{})
	}
	return registry
}
```

`cmd/agent/approval.go`:

```go
package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/ollama/ollama/agent"
)

// terminalApprovalPrompter asks for tool approval on the terminal, one
// question per pending call. It implements agent.ApprovalPrompter.
//
//   - "y" approves the call and remembers its scope for the rest of the run
//   - "a" approves all future calls
//   - anything else (default, "n") denies
//
// A denied call denies the whole batch: the engine sends one Approval result
// for all pending calls.
type terminalApprovalPrompter struct {
	in  io.Reader
	out io.Writer
}

func newTerminalApprovalPrompter(in io.Reader, out io.Writer) *terminalApprovalPrompter {
	return &terminalApprovalPrompter{in: in, out: out}
}

func (p *terminalApprovalPrompter) PromptApproval(ctx context.Context, req agent.ApprovalRequest) (agent.Approval, error) {
	if len(req.Calls) == 0 {
		return agent.Approval{Allow: true}, nil
	}
	reader := bufio.NewReader(p.in)
	allowed := make([]string, 0, len(req.Calls))
	for _, call := range req.Calls {
		fmt.Fprintf(p.out, "\n  Run %s with %s?\n", call.ToolName, compactMap(call.Args))
		fmt.Fprint(p.out, "  [y]es / [a]lways / [n]o (default no): ")
		line, err := reader.ReadString('\n')
		if err != nil {
			if ctx.Err() != nil {
				return agent.Approval{Reason: "Tool approval canceled."}, nil
			}
			return agent.Approval{}, err
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			allowed = append(allowed, call.ApprovalScope)
		case "a", "always":
			return agent.Approval{Allow: true, AllowAll: true}, nil
		default:
			return agent.Approval{Reason: "Denied by user."}, nil
		}
	}
	return agent.Approval{Allow: true, AllowScopes: allowed}, nil
}

func compactMap(m map[string]any) string {
	if len(m) == 0 {
		return "(no args)"
	}
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}
	return strings.Join(parts, " ")
}

// autoApprovePrompter approves every tool call without asking (--yes).
type autoApprovePrompter struct{}

func (autoApprovePrompter) PromptApproval(context.Context, agent.ApprovalRequest) (agent.Approval, error) {
	return agent.Approval{Allow: true, AllowAll: true}, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/agent/ -run 'TestAgentRegistry|TestTerminalApproval|TestAutoApprove' -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add cmd/agent/tools.go cmd/agent/approval.go cmd/agent/tools_test.go
git commit -m "feat(agent): tool registry builder + terminal approval prompter"
```

---

### Task 7: `cmd/agent` — the `oaica agent` cobra command

**Files:**
- Create: `cmd/agent/agent_cmd.go`
- Test: `cmd/agent/agent_cmd_test.go`

**Interfaces:**
- Consumes:
  - `launch.ResolveRunModel(ctx, launch.RunModelRequest{}) (string, error)` — `cmd/launch/launch.go:465` (picker when `--model` unset)
  - `launch.ResolveAgentModel` (Task 1), `newShimClient` (Task 4), `newStdoutSink` (Task 5), `agentRegistry` + `newTerminalApprovalPrompter` + `autoApprovePrompter` (Task 6)
  - `agent.Session`, `agent.RunOptions`, `agent.LoadDefaultSkills(projectDir) (*agent.SkillCatalog, error)` (`agent/skills.go`)
  - `api.Message`
- Produces:
  - `func AgentCmd(checkServerHeartbeat func(cmd *cobra.Command, args []string) error) *cobra.Command`
    - `Use: "agent [PROMPT]"`, `Short: "Run a streaming coding agent"`, `Args: cobra.ArbitraryArgs`, `PreRunE: checkServerHeartbeat`.
    - Flags: `--model` (string), `--system` (string), `--max-turns` (int, 0), `--yes` (bool), `--cwd` (string).
  - `func runAgent(ctx context.Context, opts *agentOptions, prompt string) error`

- [ ] **Step 1: Write the failing test**

`cmd/agent/agent_cmd_test.go`:

```go
package agent

import (
	"errors"
	"testing"

	"github.com/spf13/cobra"
)

// TestAgentCmdFlagRegistration: the command exposes the documented flags with
// the right defaults, accepts arbitrary positional args, and wires the
// injected PreRunE. It deliberately avoids cmd.Execute(), which would drive
// runAgent into a real model resolve + network call.
func TestAgentCmdFlagRegistration(t *testing.T) {
	preRunCalled := false
	cmd := AgentCmd(func(c *cobra.Command, args []string) error {
		preRunCalled = true
		return nil
	})

	if cmd.Use != "agent [PROMPT]" {
		t.Errorf("Use = %q", cmd.Use)
	}
	if cmd.Short != "Run a streaming coding agent" {
		t.Errorf("Short = %q", cmd.Short)
	}

	for _, name := range []string{"model", "system", "max-turns", "yes", "cwd"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("--%s flag missing", name)
		}
	}
	if cmd.Flags().Lookup("max-turns").DefValue != "0" {
		t.Errorf("--max-turns default = %q, want 0", cmd.Flags().Lookup("max-turns").DefValue)
	}
	if cmd.Flags().Lookup("yes").DefValue != "false" {
		t.Errorf("--yes default = %q, want false", cmd.Flags().Lookup("yes").DefValue)
	}

	// Args accepts any positional arguments.
	if err := cmd.Args(cmd, []string{"read", "the", "repo"}); err != nil {
		t.Errorf("Args with arbitrary positions should pass: %v", err)
	}

	// PreRunE runs the injected sign-in check.
	if err := cmd.PreRunE(cmd, nil); err != nil {
		t.Fatalf("PreRunE: %v", err)
	}
	if !preRunCalled {
		t.Error("injected checkServerHeartbeat was not called")
	}
}

// TestAgentCmdPreRunEPropagatesError: the injected check's error surfaces.
func TestAgentCmdPreRunEPropagatesError(t *testing.T) {
	cmd := AgentCmd(func(c *cobra.Command, args []string) error {
		return errors.New("not signed in")
	})
	if err := cmd.PreRunE(cmd, nil); err == nil || err.Error() != "not signed in" {
		t.Errorf("err = %v, want the injected error", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/agent/ -run TestAgentCmd -count=1`
Expected: FAIL — undefined: `AgentCmd`

- [ ] **Step 3: Write minimal implementation**

`cmd/agent/agent_cmd.go`:

```go
package agent

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/ollama/ollama/agent"
	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/cmd/launch"
)

type agentOptions struct {
	model    string
	system   string
	maxTurns int
	yes      bool
	cwd      string
}

// AgentCmd returns the "oaica agent" cobra command. checkServerHeartbeat is
// injected (the cmd package's oaicaEnsureSignedIn) to avoid an import cycle:
// cmd imports cmd/agent to register the command, so cmd/agent cannot import
// cmd. This mirrors launch.LaunchCmd's dependency-injection pattern
// (cmd/launch/launch.go).
func AgentCmd(checkServerHeartbeat func(cmd *cobra.Command, args []string) error) *cobra.Command {
	opts := &agentOptions{}
	cmd := &cobra.Command{
		Use:     "agent [PROMPT]",
		Short:   "Run a streaming coding agent",
		Args:    cobra.ArbitraryArgs,
		PreRunE: checkServerHeartbeat,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAgent(cmd.Context(), opts, strings.Join(args, " "))
		},
	}
	cmd.Flags().StringVar(&opts.model, "model", "", "model to use (interactive picker if unset)")
	cmd.Flags().StringVar(&opts.system, "system", "", "system prompt for the agent")
	cmd.Flags().IntVar(&opts.maxTurns, "max-turns", 0, "maximum consecutive tool rounds (0 = model-specific default)")
	cmd.Flags().BoolVar(&opts.yes, "yes", false, "auto-approve all tool calls without prompting")
	cmd.Flags().StringVar(&opts.cwd, "cwd", "", "agent working directory (default: current directory)")
	return cmd
}

func runAgent(ctx context.Context, opts *agentOptions, prompt string) error {
	model := opts.model
	if model == "" {
		var err error
		model, err = launch.ResolveRunModel(ctx, launch.RunModelRequest{})
		if err != nil {
			return fmt.Errorf("resolve model: %w", err)
		}
	}

	baseURL, token, upstreamModel, meta, err := launch.ResolveAgentModel(ctx, model)
	if err != nil {
		return err
	}

	cwd, err := agentWorkingDir(opts.cwd)
	if err != nil {
		return err
	}

	client := newShimClient(baseURL, token, upstreamModel, meta)

	var approval agent.ApprovalPrompter = newTerminalApprovalPrompter(os.Stdin, os.Stdout)
	if opts.yes {
		approval = autoApprovePrompter{}
	}

	skills, err := agent.LoadDefaultSkills(cwd)
	if err != nil {
		return fmt.Errorf("load agent skills: %w", err)
	}

	sess := &agent.Session{
		Client:           client,
		EventSinks:       []agent.EventSink{newStdoutSink()},
		Tools:            agentRegistry(skills, meta),
		Skills:           skills,
		ApprovalPrompter: approval,
		WorkingDir:       cwd,
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	var messages []api.Message
	if strings.TrimSpace(prompt) != "" {
		messages = append(messages, api.Message{Role: "user", Content: prompt})
	}

	_, err = sess.Run(ctx, agent.RunOptions{
		Model:         upstreamModel,
		SystemPrompt:  opts.system,
		Messages:      messages,
		MaxToolRounds: opts.maxTurns,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil // interrupted — the engine already finalized the aborted turn
		}
		return err
	}
	return nil
}

// agentWorkingDir resolves the run's working directory, defaulting to the
// process's current directory.
func agentWorkingDir(cwd string) (string, error) {
	if strings.TrimSpace(cwd) == "" {
		return os.Getwd()
	}
	info, err := os.Stat(cwd)
	if err != nil {
		return "", fmt.Errorf("cwd: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("cwd %q is not a directory", cwd)
	}
	return cwd, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/agent/ -count=1`
Expected: PASS (all `cmd/agent` tests)

- [ ] **Step 5: Commit**

```bash
git add cmd/agent/agent_cmd.go cmd/agent/agent_cmd_test.go
git commit -m "feat(agent): oaica agent cobra command"
```

---

### Task 8: Wire into `cmd/cmd.go` and verify

**Files:**
- Modify: `cmd/cmd.go` (one added line in the `rootCmd.AddCommand` block)
- Verify: `go build ./...`, `go test ./cmd/...`, live smoke (manual)

**Interfaces:**
- Consumes: `agentcmd.AgentCmd` (Task 7) — imported under alias `agentcmd "github.com/ollama/ollama/cmd/agent"`; `oaicaEnsureSignedIn` (existing, `cmd/cmd.go:934`).

- [ ] **Step 1: Add the import**

In `cmd/cmd.go`, add to the import block (alphabetical, alongside the existing `cmd/launch` import):

```go
	agentcmd "github.com/ollama/ollama/cmd/agent"
```

- [ ] **Step 2: Register the command**

In the `rootCmd.AddCommand(...)` block (`cmd/cmd.go:2420-2445`), add exactly one line:

```go
		agentcmd.AgentCmd(oaicaEnsureSignedIn),
```

- [ ] **Step 3: Build**

Run: `go build ./...`
Expected: no errors (in particular no import cycle: `cmd` → `cmd/agent` → `cmd/launch`, with `cmd/launch` not importing `cmd`)

- [ ] **Step 4: Full test run**

Run: `go test ./cmd/agent/... ./cmd/launch/... ./agent/... -count=1`
Expected: PASS (new tests) and no regressions in existing packages (the diff touches only `cmd/cmd.go`, which has no unit tests to break).

- [ ] **Step 5: Manual smoke (documented, not automated)**

```bash
# Against a configured remote (user remote "deepseek" in ~/.oaica/remotes.json):
./oaica agent --model deepseek/deepseek-chat "what is the capital of France?"

# Against the OAICA router (needs OAICA_API_KEY):
./oaica agent "list the files in this repo"

# Local model (needs `oaica serve` running with a pulled model):
./oaica agent --model llama3.1:8b:local "say hi"

# Non-interactive tool approval:
./oaica agent --yes --model deepseek/deepseek-chat "create a file named hello.txt with 'hi'"
```

Expected: streaming text on stdout; tool calls announced with `◆`, results with `✓`; `--yes` never prompts; SIGINT aborts cleanly with exit 0.

- [ ] **Step 6: Commit**

```bash
git add cmd/cmd.go
git commit -m "feat(agent): register oaica agent command"
```

---

## Self-Review (checked against the spec)

**Spec coverage:**
- `cmd/launch/agent_routing.go` `ResolveAgentModel` → Task 1 (mirrors `claude.go:90-137`; remote → translation proxy with hard bind error; cloud/local → logging proxy fallback; key precedence env-over-file; `:local` tag stripped).
- `cmd/agent/shim.go` `api.ChatRequest` → `anthropic.MessagesRequest` + POST `/v1/messages` + SSE → `api.ChatResponse` → Task 3 + Task 4 (5-minute timeout, `message_stop` terminal, Anthropic `error` JSON surfaced).
- `cmd/agent/sse.go` inbound parser → Task 2 (event structs already in `anthropic/`).
- `cmd/agent/render.go` plain-stdout `EventSink` → Task 5 (streaming text, thinking grey delimiters, tool status lines, SIGINT via `signal.NotifyContext` in Task 7).
- `cmd/agent/tools.go` registry without `client.Show` → Task 6 (env-var gating only; `AgentModelMeta.ToolCapable` replaces the capability probe).
- `cmd/agent/agent_cmd.go` command with DI `AgentCmd(checkServerHeartbeat)` → Task 7 (`--model/--system/--max-turns/--yes/--cwd`; `--max-turns` → `MaxToolRounds`; `--yes` → `autoApprovePrompter`).
- `cmd/cmd.go` one added `AddCommand` line → Task 8.
- Test plan (shim/sse/render/routing/integration) → Tasks 1-8 step 1 of each.
- Error-handling table → proxy bind failure (Task 1 hard error), no credential (PreRunE `oaicaEnsureSignedIn` — verified lenient, never hard-blocks), upstream error → `EventError` → non-zero exit (Task 5 `EventError` returns the error), SIGINT → clean exit (Task 7).

**Ollama-upgrade safety:** upstream packages untouched; `cmd/launch` gains exactly one file; all new code in `cmd/agent/`; no new dependencies; module path unchanged.

**Placeholder scan:** every step carries real, compiling code and an exact test command. No "TBD"/"add error handling" placeholders. The one deliberately deferred item is a dedicated Python/IPython tool — spec-designated Phase 2.

**Type consistency:** `AgentModelMeta` fields used identically in Tasks 1, 3, 6; `buildMessagesRequest` signature matches Tasks 3/4; `Feed` returns `(deltas, done, err)` consistently; `agentRegistry`, `newTerminalApprovalPrompter`, `autoApprovePrompter` names stable across Tasks 6/7; `AgentCmd` signature matches Task 8's single registration line.
