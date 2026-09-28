package launch

// round48_proxy_wire_test.go — round 48's findings on the client proxy leg.
//
// Every finding here was reproduced on the unmodified tree before it was fixed.
//
// A-F2/C-F2: after a whole completion arriving inside a frame was adopted, EVERY
// later tool fragment was dropped, on the stated ground that the gateway drops
// them too. That is only true of a fragment continuing a call the adoption
// itself wrote: an adoption that wrote nothing but text closed no call, so a
// tool fragment after it is a call the model wrote, and the gateway leg opens a
// block for exactly that wire (content_block_stop is out for the text block
// alone). Dropping it here made the turn's answer depend on whether the text
// arrived as a whole message or as deltas.
//
// C-F6: the prompt-size unit marshalled the messages and the tool schemas
// inside a synthetic {"messages":…,"tools":…} object, whose braces, key names
// and comma the gateway leg does not charge: the same documents measured 13
// bytes more here (22 with tools), a constant no calibration removes and a
// large share of a small prompt. The unit is the two arrays, each marshalled on
// its own, exactly as the gateway's messagesBytes charges them.

import (
	"strings"
	"testing"
)

// TestAnAdoptedTextTurnKeepsTheCallsThatFollow is A-F2/C-F2: the adoption wrote
// text, so the tool fragment after it is a call of the turn, not the tail of a
// call the client already holds.
func TestAnAdoptedTextTurnKeepsTheCallsThatFollow(t *testing.T) {
	up := streamUpstream(t,
		"data: {\"choices\":[{\"message\":{\"role\":\"assistant\",\"content\":\"hi there\"}}]}\n\n"+
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_X\",\"type\":\"function\",\"function\":{\"name\":\"Bash\",\"arguments\":\"{\\\"a\\\":1}\"}}]}}]}\n\n"+
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", true)
	proxy := startCalibProxy(t, up.URL, "r48-af2")
	body, status := postMessagesStream(t, proxy)
	if status != 200 {
		t.Fatalf("status %d\n%s", status, body)
	}
	if !strings.Contains(body, "hi there") {
		t.Errorf("the adopted turn's text never reached the client:\n%s", body)
	}
	blocks := r45ToolUseBlocks(t, body)
	if len(blocks) != 1 {
		t.Fatalf("the client holds %d tool_use block(s), want 1 — the tool call that followed the adopted text was dropped, so the turn's answer depends on whether the text arrived whole or as deltas:\n%s", len(blocks), body)
	}
	if blocks[0]["name"] != "Bash" {
		t.Errorf("the call's name is %v, want Bash\n%s", blocks[0]["name"], body)
	}
	if !strings.Contains(body, `{\"a\":1}`) {
		t.Errorf("the call's arguments never reached the client:\n%s", body)
	}
}

// TestAnAdoptedCallTurnDropsItsOwnTail is the other half of A-F2/C-F2: the
// adoption wrote a call itself, so the client holds its block already and a
// nameless fragment after it cannot be delivered as a second call under an id
// the client can answer once. It is relayed as the prose it is — see the
// REVERSED note on the last check below (2026-09-28 audit, round 78, F78-L2-1).
func TestAnAdoptedCallTurnDropsItsOwnTail(t *testing.T) {
	up := streamUpstream(t,
		"data: {\"choices\":[{\"message\":{\"role\":\"assistant\",\"content\":null,\"tool_calls\":[{\"id\":\"call_X\",\"type\":\"function\",\"function\":{\"name\":\"Bash\",\"arguments\":\"{\\\"a\\\":1}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n"+
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_X\",\"function\":{\"arguments\":\"{\\\"b\\\":2}\"}}]}}]}\n\n"+
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", true)
	proxy := startCalibProxy(t, up.URL, "r48-af2b")
	body, status := postMessagesStream(t, proxy)
	if status != 200 {
		t.Fatalf("status %d\n%s", status, body)
	}
	if blocks := r45ToolUseBlocks(t, body); len(blocks) != 1 {
		t.Errorf("the client holds %d tool_use block(s), want 1 — the fragment after the adopted call opened a second block under the id it was adopted from:\n%s", len(blocks), body)
	} else if blocks[0]["id"] != "call_X" {
		t.Errorf("the block's id is %v, want the stated call_X\n%s", blocks[0]["id"], body)
	}
	if !strings.Contains(body, `{\"a\":1}`) {
		t.Errorf("the adopted call's arguments never reached the client:\n%s", body)
	}
	// REVERSED by round 78 (F78-L2-1). This row held that the fragment's bytes
	// must not reach the client at all, read as "the fragment continues the
	// adopted call". The fragment states no name, so it is not a call on any arm
	// of this leg: content_block_start is the only event that carries a name, and
	// an entry the upstream never named has none to carry. What the row was
	// really pinning — that the fragment does not open a SECOND tool block under
	// the adopted id — is still checked above (one block, and it is call_X's).
	// The bytes themselves are the model's output and reach the client as text,
	// which is what the whole-list arm answers for the same body: measured
	// 2026-09-28, both arms answer the call `{"a":1}` and the prose `{"b":2}`
	// beside it. Dropping the text here made the turn's answer depend on whether
	// the body arrived as frames or as a document.
	if !strings.Contains(body, `{\"b\":2}`) {
		t.Errorf("the nameless fragment's bytes never reached the client — they are the model's prose and are relayed as text (2026-09-28 audit, round 78, F78-L2-1):\n%s", body)
	}
}

// TestThePromptUnitIsTheTwoArrays is C-F6. The expected sizes are written out
// here as the JSON text the two arrays ARE, so the pin does not ask the
// implementation what it measured: the gateway leg charges "messages" and
// "tools" each marshalled on its own, and neither is wrapped in anything.
func TestThePromptUnitIsTheTwoArrays(t *testing.T) {
	messagesJSON := `[{"role":"user","content":"go"}]`
	toolsJSON := `[{"type":"function","function":{"name":"Read","description":"read a file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]`

	withoutTools := []byte(`{"model":"kat-awq","max_tokens":64,"messages":[{"role":"user","content":"go"}]}`)
	if got, want := clientPromptBytes(withoutTools), len(messagesJSON); got != want {
		t.Errorf("a messages-only prompt measures %d bytes, want %d — the unit charges %d byte(s) the gateway leg does not (a wrapper around the arrays):\n%s", got, want, got-want, messagesJSON)
	}

	withTools := []byte(`{"model":"kat-awq","max_tokens":64,"messages":[{"role":"user","content":"go"}],` +
		`"tools":[{"name":"Read","description":"read a file","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}]}`)
	if got, want := clientPromptBytes(withTools), len(messagesJSON)+len(toolsJSON); got != want {
		t.Errorf("a prompt with one tool schema measures %d bytes, want %d (%d + %d) — the two legs' calibrated ratios differ by a constant no prompt can calibrate out", got, want, len(messagesJSON), len(toolsJSON))
	}
}
