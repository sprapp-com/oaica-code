package launch

// proxy_stream_whole_frame_integrity_test.go — three ways a streaming turn was
// reported to the client as a success with nothing in it (2026-09-26 audit,
// fifteenth round).
//
//  1. A whole completion INSIDE one SSE frame. An upstream that emulates
//     streaming around a non-streaming backend answers stream:true with
//     `data: {"choices":[{"message":{...},"finish_reason":"stop"}]}`. The
//     frame reader models only `delta`, so the frame parsed cleanly, produced
//     nothing, and the finish_reason it carried completed the turn: 200, a
//     plausible usage line, EMPTY content, the answer discarded, and the leg
//     recorded healthy. One "data: " wrapper away from the body round 14
//     fixed.
//
//  2. A stream terminated by `data: [DONE]` that never carried a choice at
//     all. The sentinel says the stream ENDED, not that it ever held a turn;
//     believing it alone reported a successful empty turn (and recorded the
//     leg healthy), while both sibling paths refuse the same upstream
//     condition — handleNonStreamResponse's `len(Choices) == 0` and
//     adoptNonSSECompletion.
//
//  3. Two distinct id-less AND index-less tool calls merged into one
//     fabricated tool_use. The accumulator is keyed on tc.Index, which is
//     Go's zero value when the upstream omits it, so both calls landed in one
//     accumulator and the client got ONE block named after the last call with
//     the two calls' arguments concatenated. The non-streaming path emits two
//     blocks for the same body.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// sseToolUseBlocks returns the tool_use blocks a streaming Anthropic response
// emitted, each with its accumulated input_json_delta partial_json decoded.
func sseToolUseBlocks(t *testing.T, body string) []map[string]any {
	t.Helper()
	type block struct {
		meta map[string]any
		json strings.Builder
	}
	blocks := map[int]*block{}
	var order []int
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev); err != nil {
			continue
		}
		idx := -1
		if f, ok := ev["index"].(float64); ok {
			idx = int(f)
		}
		switch ev["type"] {
		case "content_block_start":
			cb, _ := ev["content_block"].(map[string]any)
			if cb == nil || cb["type"] != "tool_use" {
				continue
			}
			blocks[idx] = &block{meta: cb}
			order = append(order, idx)
		case "content_block_delta":
			d, _ := ev["delta"].(map[string]any)
			if d == nil || d["type"] != "input_json_delta" {
				continue
			}
			if b := blocks[idx]; b != nil {
				if s, ok := d["partial_json"].(string); ok {
					b.json.WriteString(s)
				}
			}
		}
	}
	out := make([]map[string]any, 0, len(order))
	for _, idx := range order {
		b := blocks[idx]
		args := map[string]any{}
		if raw := strings.TrimSpace(b.json.String()); raw != "" {
			if err := json.Unmarshal([]byte(raw), &args); err != nil {
				args = map[string]any{"_undecodable": raw}
			}
		}
		out = append(out, map[string]any{"name": b.meta["name"], "id": b.meta["id"], "input": args})
	}
	return out
}

// F1: a whole completion inside one SSE frame is the turn, not an empty one.
func TestProxyStream_WholeCompletionInsideAnSSEFrameIsRelayed(t *testing.T) {
	const answer = "hello from a frame-wrapped whole completion"
	up := streamUpstream(t, "data: "+
		`{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"`+answer+`"},"finish_reason":"stop"}],"usage":{"prompt_tokens":50000,"completion_tokens":7}}`+
		"\n\ndata: [DONE]\n\n", true)
	defer up.Close()

	proxy := startCalibProxy(t, up.URL, "sess-frame-whole")
	body, status := postMessagesStream(t, proxy)
	events := sseEvents(body)

	if status != http.StatusOK {
		t.Errorf("a frame-wrapped whole completion got status %d, want 200 (events: %v)\nbody:\n%s", status, events, body)
	}
	if hasEvent(events, "error") {
		t.Errorf("a frame-wrapped whole completion was reported as an error (events: %v)\nbody:\n%s", events, body)
	}
	if !strings.Contains(body, answer) {
		t.Errorf("the answer never reached the client — the frame carried it in `message`, which the delta-only reader does not model, so the turn completed on its finish_reason and relayed EMPTY content:\n%s", body)
	}
	if !hasEvent(events, "message_stop") {
		t.Errorf("the relayed completion did not end with message_stop (events: %v)\nbody:\n%s", events, body)
	}
}

// F1, tool variant: the same frame carrying tool_calls must reach the client as
// a tool_use block, or the agent's tool call vanishes and stop_reason end_turn
// tells it the model finished.
func TestProxyStream_WholeCompletionInsideAFrameKeepsItsToolCalls(t *testing.T) {
	up := streamUpstream(t, "data: "+
		`{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"/tmp/x\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":3}}`+
		"\n\ndata: [DONE]\n\n", true)
	defer up.Close()

	proxy := startCalibProxy(t, up.URL, "sess-frame-tools")
	body, status := postMessagesStream(t, proxy)

	if status != http.StatusOK {
		t.Fatalf("status %d, want 200\nbody:\n%s", status, body)
	}
	blocks := sseToolUseBlocks(t, body)
	if len(blocks) != 1 {
		t.Fatalf("got %d tool_use block(s), want 1 — the client's tool call was dropped and stop_reason said end_turn, so the agent stops instead of running the tool:\n%s", len(blocks), body)
	}
	if blocks[0]["name"] != "Read" {
		t.Errorf("tool_use name = %v, want Read\n%s", blocks[0]["name"], body)
	}
	if !strings.Contains(body, `"tool_use"`) {
		t.Errorf("no tool_use stop_reason reached the client:\n%s", body)
	}
}

// F2: a stream that ends at the sentinel without ever carrying a choice is not
// a turn, and must not be recorded as a healthy leg.
func TestProxyStream_ASentinelWithNoChoiceIsNotASuccessfulTurn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
	}{
		{"nothing but the sentinel", "data: [DONE]\n\n"},
		{"a usage-only frame then the sentinel", "data: {\"id\":\"x\",\"choices\":[],\"usage\":{\"prompt_tokens\":23,\"completion_tokens\":0}}\n\ndata: [DONE]\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := streamUpstream(t, tc.script, true)
			defer up.Close()

			proxy := startCalibProxy(t, up.URL, "sess-sentinel-no-choice")
			body, status := postMessagesStream(t, proxy)
			events := sseEvents(body)

			// The client must be able to tell that no turn arrived: either a
			// status it can retry, or an error event mid-stream.
			if status == http.StatusOK && !hasEvent(events, "error") {
				t.Errorf("a stream that terminated without a single choice was relayed as a SUCCESSFUL turn (status %d, events %v) — the client sees an empty message and bills prompt tokens for a turn that produced nothing, and cannot tell it apart from a real empty answer:\n%s",
					status, events, body)
			}
			if hasEvent(events, "message_stop") && !hasEvent(events, "error") && status == http.StatusOK {
				t.Errorf("the empty stream was closed with message_stop as though it were complete:\n%s", body)
			}
		})
	}
}

// The same shape must fail the LEG, not just the client: an upstream that
// answers 200-with-[DONE] and never a turn would otherwise keep a perfect
// health record, so its breaker never opens and `auto` never escalates off it.
func TestProxyStream_ASentinelWithNoChoiceFailsTheLeg(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	table, proxy := translatedLegProxy(t, "sess-sentinel-health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})
	table.breakers.recordFail(table.Default.BaseURL)
	table.escalations.recordFail(table.SessionID, table.Default.BaseURL)

	postStreamMessage(t, proxy, "glm-5.3", true)

	if fails := breakerFails(table, table.Default.BaseURL); fails == 0 {
		t.Errorf("a leg that answered 200 with only the [DONE] sentinel was recorded as a SUCCESS — %d of these (breakerFailsToOpen) never open its circuit, and under `auto` two never escalate the session off a leg that cannot complete a turn", breakerFailsToOpen)
	}
	if esc := escalationFails(table, table.SessionID); esc == 0 {
		t.Errorf("the same empty turn did not count toward the `auto` escalation of session %q", table.SessionID)
	}
}

// F3: two id-less, index-less tool calls are two calls, on both paths.
func TestProxyStream_TwoIndexLessCallsDoNotMerge(t *testing.T) {
	up := streamUpstream(t, strings.Join([]string{
		`data: {"id":"x","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"function":{"name":"Read","arguments":"{\"p\":1}"}}]}}]}`,
		`data: {"id":"x","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"function":{"name":"Write","arguments":"{\"p\":2}"}}]}}]}`,
		`data: {"id":"x","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n\n")+"\n\n", true)
	defer up.Close()

	proxy := startCalibProxy(t, up.URL, "sess-indexless-tools")
	body, status := postMessagesStream(t, proxy)

	if status != http.StatusOK {
		t.Fatalf("status %d, want 200\nbody:\n%s", status, body)
	}
	blocks := sseToolUseBlocks(t, body)
	if len(blocks) != 2 {
		t.Fatalf("got %d tool_use block(s) for two distinct upstream calls, want 2 — an upstream that omits `index` put both in the same accumulator (Go's zero value), so the client is handed ONE call named after the last one with both argument objects concatenated:\n%s", len(blocks), body)
	}
	if blocks[0]["name"] != "Read" || blocks[1]["name"] != "Write" {
		t.Errorf("tool_use names = %v, %v; want Read, Write\n%s", blocks[0]["name"], blocks[1]["name"], body)
	}
	for i, want := range []map[string]any{{"p": float64(1)}, {"p": float64(2)}} {
		got, _ := blocks[i]["input"].(map[string]any)
		if got == nil || got["p"] != want["p"] {
			t.Errorf("tool_use[%d] input = %v, want %v — the two calls' arguments were concatenated into one block\n%s", i, blocks[i]["input"], want, body)
		}
	}
}

// The control: a single index-less call whose arguments arrive as fragments
// must still be ONE block, so the fix above cannot be passed by splitting every
// call.
func TestProxyStream_OneIndexLessCallWithFragmentedArgumentsStaysOne(t *testing.T) {
	up := streamUpstream(t, strings.Join([]string{
		`data: {"id":"x","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"function":{"name":"Read","arguments":"{\"file"}}]}}]}`,
		`data: {"id":"x","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"_path\":\"/tmp/x\"}"}}]}}]}`,
		`data: {"id":"x","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n\n")+"\n\n", true)
	defer up.Close()

	proxy := startCalibProxy(t, up.URL, "sess-fragmented-tool")
	body, status := postMessagesStream(t, proxy)

	if status != http.StatusOK {
		t.Fatalf("status %d, want 200\nbody:\n%s", status, body)
	}
	blocks := sseToolUseBlocks(t, body)
	if len(blocks) != 1 {
		t.Fatalf("got %d tool_use block(s) for ONE call whose arguments arrived in fragments, want 1:\n%s", len(blocks), body)
	}
	got, _ := blocks[0]["input"].(map[string]any)
	if got == nil || got["file_path"] != "/tmp/x" {
		t.Errorf("tool_use input = %v, want {file_path: /tmp/x} — the fragments were not reassembled\n%s", blocks[0]["input"], body)
	}
}

// F5: the clamp target is itself unvalidated, so a malformed NEGATIVE
// prompt_tokens produced a negative cache-read count nobody stated. The reader
// is statedCacheHit() now (round 42 removed the clamped one), and the guard is
// the same: a prompt count that is no measurement cannot become a cache read.
func TestCachedTokensNeverGoesNegativeWithAMalformedPromptCount(t *testing.T) {
	u := &openAIUsage{PromptTokens: -5}
	if got := u.statedCacheHit(); got != 0 {
		t.Errorf("statedCacheHit() = %d for prompt_tokens=-5 with no hit stated, want 0: a malformed negative prompt count is no measurement, and it must not reach the client as a cache_read_input_tokens the upstream never stated", got)
	}
}

// Guard the timing helper use above so the file compiles against the shared
// harness even if the helpers move.
var _ = time.Second
