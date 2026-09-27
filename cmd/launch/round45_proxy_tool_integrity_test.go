package launch

// round45_proxy_tool_integrity_test.go — round 45's findings on the client
// proxy (leg 2: cmd/launch/anthropic_openai_proxy.go).
//
// A45-1: a call the upstream never named reached the client as a tool_use block
// with no name — on the non-stream path and on the adopt path, the two the
// round-44 fix did not reach (it fixed flushToolCalls, whose comment claimed
// the non-stream path already answered end_turn; it did not).
//
// A45-3: an upstream id reused for the turn's second call was deduped on the id
// alone, so the second call was dropped on both of this leg's paths, silently,
// under stop_reason "tool_use".
//
// A45-5: a whole completion that says nothing was relayed as a successful
// assistant turn with zero content blocks, where the gateway leg refuses the
// identical document with 502.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// r45ToolUseBlocks pulls tool_use content blocks out of either response shape:
// a non-stream Anthropic body, or an SSE body's content_block_start events.
func r45ToolUseBlocks(t *testing.T, body string) []map[string]any {
	t.Helper()
	out := []map[string]any{}
	trimmed := strings.TrimSpace(body)
	if strings.HasPrefix(trimmed, "{") {
		var resp struct {
			Content []map[string]any `json:"content"`
		}
		if err := json.Unmarshal([]byte(trimmed), &resp); err != nil {
			t.Fatalf("decode non-stream body: %v\n%s", err, body)
		}
		for _, c := range resp.Content {
			if c["type"] == "tool_use" {
				out = append(out, c)
			}
		}
		return out
	}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		payload, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(payload)), &ev) != nil {
			continue
		}
		if ev["type"] != "content_block_start" {
			continue
		}
		cb, _ := ev["content_block"].(map[string]any)
		if cb != nil && cb["type"] == "tool_use" {
			out = append(out, cb)
		}
	}
	return out
}

// TestANamelessCallIsNotAToolUseOnEitherPath is A45-1 for this leg.
func TestANamelessCallIsNotAToolUseOnEitherPath(t *testing.T) {
	const unnamed = `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"type":"function","function":{"name":"","arguments":"{\"q\":1}"}}]},"finish_reason":"tool_use"}]}`

	t.Run("non-stream", func(t *testing.T) {
		up := jsonUpstream(t, unnamed)
		defer up.Close()
		proxy := startCalibProxy(t, up.URL, "sess-r45-unnamed-ns")
		body, status := postMessagesRaw(t, proxy, false)
		// Round 45 read this document as one that "says nothing" and refused it
		// 502 under A45-5. Round 54 overturns that reading: the arguments are
		// the model's output, the byte-identical STREAM of this same document
		// answers 200 with them relayed as text (this leg's flushToolCalls and
		// both arms of the metered gateway do exactly that, and the gateway's
		// documentSaysSomething counts a nameless call WITH arguments as
		// something), so a 502 here made the verdict depend on which shape the
		// upstream sent. A45-5's real subject — a completion with no content and
		// no calls at all — is still refused, and is pinned by
		// TestACompletionThatSaysNothingIsRefused below.
		if status != http.StatusOK {
			t.Fatalf("a document whose only payload is a nameless call WITH arguments was answered %d, want 200:\n%s", status, body)
		}
		if strings.Contains(body, `"stop_reason":"tool_use"`) {
			t.Errorf("the turn reports tool_use for a call the client cannot run: %s", body)
		}
		if !strings.Contains(body, `\"q\":1`) && !strings.Contains(body, `{"q":1}`) {
			t.Errorf("the arguments the upstream wrote did not reach the client as text: %s", body)
		}
	})

	t.Run("adopted whole completion", func(t *testing.T) {
		up := jsonUpstream(t, unnamed)
		defer up.Close()
		proxy := startCalibProxy(t, up.URL, "sess-r45-unnamed-adopt")
		body, status := postMessagesStream(t, proxy)
		for _, b := range r45ToolUseBlocks(t, body) {
			t.Errorf("a call the upstream never named reached the client as a tool_use block: %v\nthe block carries no name, so Claude Code reports it as pending forever and stop_reason tool_use stops it continuing in text", b)
		}
		// Round 45 read this arm as "a document that says nothing" and refused
		// it 502 with an error event. Round 55 overturns that the same way round
		// 54 overturned the non-stream subtest above: the document's only
		// payload is a nameless call WITH arguments, which the metered gateway
		// counts as something on both of its arms (documentSaysSomething) and
		// relays as text, so refusing it here made the verdict depend on which
		// shape the upstream sent — and left this leg's adopt arm disagreeing
		// with its own non-stream arm about one byte-identical document.
		if status != http.StatusOK {
			t.Fatalf("status=%d, want 200: the arguments ARE the model's output on this turn, and the non-stream twin of the same document answers 200 with them as text:\n%s", status, body)
		}
		if strings.Contains(body, `"error"`) {
			t.Errorf("a stream request answered with a document that relays the model's own output carried an error event:\n%s", body)
		}
		if !strings.Contains(body, `\"q\":1`) && !strings.Contains(body, `{"q":1}`) {
			t.Errorf("the arguments the upstream wrote did not reach the client as text:\n%s", body)
		}
	})

	// The control: the same wire with a name IS a call on both paths.
	const named = `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}]},"finish_reason":"tool_use"}]}`
	up := jsonUpstream(t, named)
	defer up.Close()
	proxy := startCalibProxy(t, up.URL, "sess-r45-named-ns")
	body, status := postMessagesRaw(t, proxy, false)
	if status != 200 {
		t.Fatalf("status=%d\n%s", status, body)
	}
	blocks := r45ToolUseBlocks(t, body)
	if len(blocks) != 1 || blocks[0]["name"] != "Bash" {
		t.Errorf("a NAMED call reached the client as %v, want one Bash block", blocks)
	}
}

// TestTwoCallsUnderOneStatedIDReachTheClient is A45-3 for this leg.
func TestTwoCallsUnderOneStatedIDReachTheClient(t *testing.T) {
	const shared = `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_shared","type":"function","function":{"name":"read_file","arguments":"{\"p\":\"a\"}"}},{"id":"call_shared","type":"function","function":{"name":"write_file","arguments":"{\"p\":\"a\"}"}}]},"finish_reason":"tool_use"}]}`

	t.Run("non-stream", func(t *testing.T) {
		up := jsonUpstream(t, shared)
		defer up.Close()
		proxy := startCalibProxy(t, up.URL, "sess-r45-shared-ns")
		body, status := postMessagesRaw(t, proxy, false)
		if status != 200 {
			t.Fatalf("status=%d\n%s", status, body)
		}
		blocks := r45ToolUseBlocks(t, body)
		if len(blocks) != 2 {
			t.Fatalf("one upstream id reused for two calls reached the client as %d block(s): %v\nthe model asked for two calls and the agent was handed one, silently, under stop_reason tool_use", len(blocks), blocks)
		}
		if blocks[0]["id"] == blocks[1]["id"] {
			t.Errorf("both blocks carry the id %v: one tool_result answers both calls", blocks[0]["id"])
		}
	})

	t.Run("stream", func(t *testing.T) {
		up := streamUpstream(t, strings.Join([]string{
			`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_shared","function":{"name":"read_file","arguments":"{\"p\":\"a\"}"}}]}}]}`,
			"",
			`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_shared","function":{"name":"write_file","arguments":"{\"p\":\"a\"}"}}]}}]}`,
			"",
			`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			"",
			"data: [DONE]",
			"",
		}, "\n"), true)
		defer up.Close()
		proxy := startCalibProxy(t, up.URL, "sess-r45-shared-st")
		body, status := postMessagesStream(t, proxy)
		if status != 200 {
			t.Fatalf("status=%d\n%s", status, body)
		}
		blocks := r45ToolUseBlocks(t, body)
		if len(blocks) != 2 {
			t.Fatalf("the stream carried %d tool_use block(s), want 2:\n%s", len(blocks), body)
		}
		if blocks[0]["id"] == blocks[1]["id"] {
			t.Errorf("both blocks carry the id %v", blocks[0]["id"])
		}
	})
}

// TestTwoIdenticalIdlessCallsAreTwoCalls is A45-2 for this leg: the model
// asked twice, so two blocks — on the non-stream path too, which used to
// collapse the pair while its own streaming path emitted two.
func TestTwoIdenticalIdlessCallsAreTwoCalls(t *testing.T) {
	const twice = `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"type":"function","function":{"name":"do_thing","arguments":"{}"}},{"type":"function","function":{"name":"do_thing","arguments":"{}"}}]},"finish_reason":"tool_use"}]}`
	up := jsonUpstream(t, twice)
	defer up.Close()
	proxy := startCalibProxy(t, up.URL, "sess-r45-twice")
	body, status := postMessagesRaw(t, proxy, false)
	if status != 200 {
		t.Fatalf("status=%d\n%s", status, body)
	}
	blocks := r45ToolUseBlocks(t, body)
	if len(blocks) != 2 {
		t.Fatalf("two identical calls reached the client as %d block(s): %v\nthe model asked for two calls and the agent was handed one, silently, under stop_reason tool_use", len(blocks), blocks)
	}
	if blocks[0]["id"] == blocks[1]["id"] {
		t.Errorf("both blocks carry the id %v: one tool_result answers both calls", blocks[0]["id"])
	}

	// The control: a call RESTATED under the same stated id is still one call —
	// the id-keyed dedup this change narrows must survive it.
	const restated = `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_one","type":"function","function":{"name":"do_thing","arguments":"{}"}},{"id":"call_one","type":"function","function":{"name":"do_thing","arguments":"{}"}}]},"finish_reason":"tool_use"}]}`
	up2 := jsonUpstream(t, restated)
	defer up2.Close()
	proxy2 := startCalibProxy(t, up2.URL, "sess-r45-restated")
	body2, status2 := postMessagesRaw(t, proxy2, false)
	if status2 != 200 {
		t.Fatalf("status=%d\n%s", status2, body2)
	}
	if got := len(r45ToolUseBlocks(t, body2)); got != 1 {
		t.Errorf("a call restated under its own id produced %d block(s), want 1: %s", got, body2)
	}
}

// TestACompletionThatSaysNothingIsRefused is A45-5: the client must be told the
// turn failed rather than handed an empty assistant turn it can never continue.
func TestACompletionThatSaysNothingIsRefused(t *testing.T) {
	const empty = `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`
	up := jsonUpstream(t, empty)
	defer up.Close()
	proxy := startCalibProxy(t, up.URL, "sess-r45-empty")
	body, status := postMessagesRaw(t, proxy, false)
	if status != 502 {
		t.Fatalf("a completion that says nothing was answered status=%d:\n%s\na session that receives an empty end_turn simply stops where it should have surfaced the upstream failure", status, body)
	}

	// The control: a one-character answer is an answer.
	const answer = `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`
	up2 := jsonUpstream(t, answer)
	defer up2.Close()
	proxy2 := startCalibProxy(t, up2.URL, "sess-r45-answer")
	body2, status2 := postMessagesRaw(t, proxy2, false)
	if status2 != 200 {
		t.Fatalf("an answer was refused with status=%d:\n%s", status2, body2)
	}
	if !strings.Contains(body2, `"hi"`) {
		t.Errorf("the answer did not reach the client: %s", body2)
	}
}
