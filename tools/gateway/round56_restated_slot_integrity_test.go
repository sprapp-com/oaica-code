package main

// Round 56, leg 3 (the metered gateway). Two wires in which an upstream lists
// the call it has already stated once, and this bridge appended the second
// listing to the first's arguments: the client then accumulated
// `{"a":1}{"a":1}` as the input of one tool_use it could not parse, under a
// stop_reason of tool_use. The recognition of a restated call is keyed on the
// SLOT the upstream itself stated, which is what both wires below have in
// common — the ordinary chunk order states the id on a chunk of its own, so a
// restatement never repeats it (round 56, F1), and one delta can list one slot
// twice, the second entry carrying no id at all (round 56, F2).

import (
	"encoding/json"
	"testing"
)

// r56DocumentToolUseIDs is round44ToolUseIDs for the arms that answer a whole
// completion instead of frames.
func r56DocumentToolUseIDs(t *testing.T, body string) []string {
	t.Helper()
	var resp struct {
		Content []struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("answer is not a Messages body: %v\n%s", err, body)
	}
	var ids []string
	for _, c := range resp.Content {
		if c.Type == "tool_use" {
			ids = append(ids, c.ID)
		}
	}
	return ids
}

const (
	// The ordinary OpenAI chunk order: the call whole, then its id, then its
	// name, then its arguments again.
	r56SlotHead = `data: {"id":"c","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}]},"finish_reason":null}]}`
	r56SlotID   = `data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","id":"call_1"}]},"finish_reason":null}]}`
	r56SlotName = `data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"Bash"}}]},"finish_reason":null}]}`
	r56SlotArgs = `data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"arguments":"{\"a\":1}"}}]},"finish_reason":null}]}`
	// One delta with two entries at the SAME index: the second states no id.
	r56SlotDup  = `data: {"id":"c","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}},{"index":0,"type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}]},"finish_reason":null}]}`
	r56SlotFin  = `data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`
	r56SlotDone = `data: [DONE]`
)

// TestACallRestatedInALaterChunkOfItsSlotIsOneCall is F1: the id arrives on a
// chunk of its own, so the fragment that restates the arguments carries none —
// and the guard that only fired on a fragment repeating the id did not see it.
func TestACallRestatedInALaterChunkOfItsSlotIsOneCall(t *testing.T) {
	up := round45Frames(t, r56SlotHead, r56SlotID, r56SlotName, r56SlotArgs, r56SlotFin, r56SlotDone)
	g, _ := round39Gateway(t, up, nil)
	code, out := round45Ask(t, g, round45AskStream)

	if code != 200 {
		t.Fatalf("status %d\n%s", code, out)
	}
	if ids := round44ToolUseIDs(t, out); len(ids) != 1 {
		t.Errorf("the model's one call reached the client as %d blocks (%v)", len(ids), ids)
	}
	inputs := r53ToolInput(t, out)
	if len(inputs) != 1 || inputs[0] != `{"a":1}` {
		t.Errorf("the client accumulated %v as this call's input, want [{\"a\":1}]\n"+
			"the arguments were restated by a later chunk of the same slot and were appended as if they were more of the object: two finished argument objects do not concatenate into JSON, so the client held an input it could not parse, and the leg's own document arm answers the same call once\n%s", inputs, out)
	}
	if sr := r53StopReason(t, "true", out); sr != "tool_use" {
		t.Errorf("stop_reason %q, want tool_use", sr)
	}
}

// TestASlotListedTwiceInOneDeltaIsOneCall is F2: the second entry of the same
// delta belongs to the slot the upstream already stated, so it is that slot's
// call restated. The document arm below keeps two calls for the same body, and
// deliberately — a `tool_calls` LIST has no slot identity, and each entry of it
// is a call the upstream named in the list's own order — but no arm may
// concatenate them into input no one can run.
func TestASlotListedTwiceInOneDeltaIsOneCall(t *testing.T) {
	up := round45Frames(t, r56SlotDup, r56SlotFin, r56SlotDone)
	g, _ := round39Gateway(t, up, nil)
	code, out := round45Ask(t, g, round45AskStream)

	if code != 200 {
		t.Fatalf("status %d\n%s", code, out)
	}
	if ids := round44ToolUseIDs(t, out); len(ids) != 1 {
		t.Errorf("the slot the upstream listed twice reached the client as %d blocks (%v), want 1", len(ids), ids)
	}
	inputs := r53ToolInput(t, out)
	if len(inputs) != 1 || inputs[0] != `{"a":1}` {
		t.Errorf("the client accumulated %v as this call's input, want [{\"a\":1}]\n%s", inputs, out)
	}

	doc := `{"id":"c","object":"chat.completion","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}},{"type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`
	upDoc := round45Upstream(t, "application/json", doc)
	gDoc, _ := round39Gateway(t, upDoc, nil)
	dcode, dout := round45Ask(t, gDoc, round45AskPlain)
	if dcode != 200 {
		t.Fatalf("document arm status %d\n%s", dcode, dout)
	}
	if ids := r56DocumentToolUseIDs(t, dout); len(ids) != 2 {
		t.Errorf("the document arm answered a two-entry tool_calls list with %d calls (%v): each entry of a list is a call the upstream named, and this arm's reading is the one that keeps them apart", len(ids), ids)
	}
}
