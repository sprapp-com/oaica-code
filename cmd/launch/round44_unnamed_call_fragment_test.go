package launch

// round44_unnamed_call_fragment_test.go — round 44's finding on the client leg,
// C44-4.
//
// An upstream that streams a tool call's arguments but never names the function
// — a truncated or malformed stream, or a provider that puts the name in a
// place this leg does not read — reached the flush with an empty name. The
// block was emitted anyway: a content_block_start carrying no name at all,
// which is not a shape the Anthropic wire allows, followed by a tool_use block
// the client cannot dispatch. The turn then advertised tool_use while carrying
// a call that named nothing.
//
// A call with no name is not a call. Its arguments are still the model's
// output, so they are relayed as TEXT — the same choice the non-stream path
// makes, and what keeps the payload visible instead of silently discarded — and
// the turn's stop_reason follows the blocks that were actually emitted.

import (
	"net/http"
	"strings"
	"testing"
)

// TestANamelessCallFragmentIsRelayedAsText is C44-4. The upstream names no
// function, so nothing on this turn may be called a tool call.
func TestANamelessCallFragmentIsRelayedAsText(t *testing.T) {
	up := streamUpstream(t, strings.Join([]string{
		`data: {"id":"x","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"arguments":"{\"a\":1}"}}]}}]}`,
		"",
		`data: {"id":"x","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n"), true)
	defer up.Close()

	proxy := startCalibProxy(t, up.URL, "sess-r44-nameless")
	body, status := postMessagesStream(t, proxy)
	if status != http.StatusOK {
		t.Fatalf("status %d, body:\n%s", status, body)
	}

	for _, b := range sseToolUseBlocks(t, body) {
		t.Errorf("a tool_use block was emitted for a call that named no function: %+v\nwhole stream:\n%s\na content_block_start with an empty name is not a shape the Anthropic wire allows, and the client has no function to dispatch — the fragment's arguments belong in the turn as text", b, body)
	}
	if strings.Contains(body, `"tool_use"`) {
		t.Errorf("the turn's stop_reason is tool_use although no call was emitted:\n%s", body)
	}
	if !strings.Contains(body, `"end_turn"`) {
		t.Errorf("the turn's stop_reason is neither tool_use nor end_turn:\n%s", body)
	}
	if !strings.Contains(body, `{\"a\":1}`) && !strings.Contains(body, `{"a":1}`) {
		t.Errorf("the fragment's arguments are nowhere in the stream: the model produced them, and a call that cannot be named is relayed as text rather than dropped:\n%s", body)
	}
}

// TestAWhitespaceOnlyCallNameIsNoName is round 46's A46-5: the nameless rule
// above tested the name against "" exactly, so an upstream that sent the name
// as whitespace (" ", "\t") was treated as a NAMED call — the fragment was not
// relayed as text, and the turn ended as an empty end_turn with the model's
// arguments nowhere in it. Every other site of this leg and both paths of the
// local server read the name with strings.TrimSpace, and the client proxy's
// non-streaming parser does too, so one body had two answers depending on the
// bytes of a name nobody can dispatch.
func TestAWhitespaceOnlyCallNameIsNoName(t *testing.T) {
	// The name is inserted as JSON ESCAPE TEXT, so a tab is "\t" on the wire
	// and decodes to whitespace — a raw tab inside a JSON string is not valid
	// JSON, and the frame would never reach the converter at all.
	for _, tc := range []struct{ label, enc string }{
		{"space", " "},
		{"tab", `\t`},
		{"newline", `\n`},
	} {
		up := streamUpstream(t, strings.Join([]string{
			`data: {"id":"x","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"` + tc.enc + `","arguments":"{\"a\":1}"}}]}}]}`,
			"",
			`data: {"id":"x","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			"",
			"data: [DONE]",
			"",
		}, "\n"), true)
		proxy := startCalibProxy(t, up.URL, "sess-r46-ws-name")
		body, status := postMessagesStream(t, proxy)
		up.Close()
		if status != http.StatusOK {
			t.Fatalf("%s: status %d, body:\n%s", tc.label, status, body)
		}
		for _, b := range sseToolUseBlocks(t, body) {
			t.Errorf("%s: a tool_use block was emitted for a call whose name is whitespace: %+v\n%s", tc.label, b, body)
		}
		if !strings.Contains(body, `{\"a\":1}`) && !strings.Contains(body, `{"a":1}`) {
			t.Errorf("%s: the fragment's arguments are nowhere in the stream — a name of whitespace names nothing, so the fragment belongs in the turn as text:\n%s", tc.label, body)
		}
	}
}

// TestANamedCallIsStillAToolUseBlock is the control: the rule above is about a
// call with no name, not about tool calls in general.
func TestANamedCallIsStillAToolUseBlock(t *testing.T) {
	up := streamUpstream(t, strings.Join([]string{
		`data: {"id":"x","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"get_weather","arguments":"{\"a\":1}"}}]}}]}`,
		"",
		`data: {"id":"x","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n"), true)
	defer up.Close()

	proxy := startCalibProxy(t, up.URL, "sess-r44-named")
	body, status := postMessagesStream(t, proxy)
	if status != http.StatusOK {
		t.Fatalf("status %d, body:\n%s", status, body)
	}
	blocks := sseToolUseBlocks(t, body)
	if len(blocks) != 1 {
		t.Fatalf("the named call produced %d tool_use block(s), want one:\n%s", len(blocks), body)
	}
	if name, _ := blocks[0]["name"].(string); name != "get_weather" {
		t.Errorf("the block's name is %q, want get_weather: %+v", name, blocks[0])
	}
	if !strings.Contains(body, `"tool_use"`) {
		t.Errorf("the turn's stop_reason is not tool_use though it carries a named call:\n%s", body)
	}
}
