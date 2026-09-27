package anthropic

// round45_tool_block_integrity_test.go — round 45's findings on the Anthropic
// translation site (leg 1: anthropic/anthropic.go, the local server path).
//
// A45-1: a call the upstream never named was still emitted as a tool_use block
// with an empty name on both of this leg's paths, where the client can only
// report it as a call that never resolves.
//
// A45-2: the non-stream path minted its synthesized id from the call's name and
// arguments with no within-list repeat handling, so two identical id-less calls
// produced two blocks answering to ONE id — a shape its own streaming twin
// already refuses (the "#n" suffix).
//
// A45-3: an upstream id reused for the turn's second call was deduped on the id
// alone, so the second call was thrown away after the caller had already split
// the fragments apart to recover it.
//
// A45-4: the non-stream block passed the zero-value arguments through, and that
// type marshals to NO "input" key at all (json:",omitzero") — while the stream
// converter and the client leg both emit "input":{} for the same call.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ollama/ollama/api"
)

func r45Chat(tcs ...api.ToolCall) api.ChatResponse {
	return api.ChatResponse{
		Model:      "m",
		Message:    api.Message{Role: "assistant", ToolCalls: tcs},
		Done:       true,
		DoneReason: "stop",
	}
}

func r45Call(name, args string) api.ToolCall {
	var a api.ToolCallFunctionArguments
	if args != "" {
		if err := json.Unmarshal([]byte(args), &a); err != nil {
			panic(err)
		}
	}
	return api.ToolCall{Function: api.ToolCallFunction{Name: name, Arguments: a}}
}

// r45NonStreamBlocks marshals a non-stream response and decodes its blocks.
func r45NonStreamBlocks(t *testing.T, r api.ChatResponse) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(ToMessagesResponse("msg_1", r))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc struct {
		Content    []map[string]any `json:"content"`
		StopReason string           `json:"stop_reason"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, raw)
	}
	t.Logf("non-stream body: %s", raw)
	return doc.Content
}

// r45StreamBlocks collects the tool_use blocks a stream converter emits.
func r45StreamBlocks(t *testing.T, r api.ChatResponse) []map[string]any {
	t.Helper()
	conv := NewStreamConverter("msg_1", "m", 10)
	var out []map[string]any
	for _, ev := range conv.Process(r) {
		if ev.Event != "content_block_start" {
			continue
		}
		b, ok := ev.Data.(ContentBlockStartEvent)
		if !ok || b.ContentBlock.Type != "tool_use" {
			continue
		}
		raw, _ := json.Marshal(b.ContentBlock)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		out = append(out, m)
	}
	return out
}

// TestANamelessCallIsNeverAToolUseBlock is A45-1: a call the upstream never
// named cannot be dispatched, so it must not reach the client as one.
func TestANamelessCallIsNeverAToolUseBlock(t *testing.T) {
	r := r45Chat(r45Call("", `{"cmd":"ls"}`))

	for _, b := range r45NonStreamBlocks(t, r) {
		if b["type"] == "tool_use" {
			t.Errorf("the non-stream path emitted a tool_use block with no name: %v — the block carries the name and there is no second event that does, so the client can only report a call it can never run", b)
		}
	}
	for _, b := range r45StreamBlocks(t, r) {
		t.Errorf("the stream converter emitted a tool_use block with no name: %v", b)
	}

	// The control: a call that IS named is still a block on both paths.
	named := r45Chat(r45Call("Bash", `{"cmd":"ls"}`))
	if got := len(r45NonStreamBlocks(t, named)); got != 1 {
		t.Errorf("a named call produced %d non-stream blocks, want 1", got)
	}
	if got := len(r45StreamBlocks(t, named)); got != 1 {
		t.Errorf("a named call produced %d stream blocks, want 1", got)
	}
}

// TestTwoIdenticalIdlessCallsGetTwoIDs is A45-2: a synthesized id is a function
// of the call, so two identical calls must be told apart the way the streaming
// converter already tells them apart.
func TestTwoIdenticalIdlessCallsGetTwoIDs(t *testing.T) {
	blocks := r45NonStreamBlocks(t, r45Chat(r45Call("do_thing", `{}`), r45Call("do_thing", `{}`)))
	var ids []string
	for _, b := range blocks {
		if b["type"] != "tool_use" {
			continue
		}
		id, _ := b["id"].(string)
		ids = append(ids, id)
	}
	if len(ids) != 2 {
		t.Fatalf("%d tool_use block(s) (%v), want 2", len(ids), ids)
	}
	if ids[0] == ids[1] {
		t.Errorf("both blocks carry the id %q: the client runs one call, sends one tool_result, and the second block can never be satisfied — the streaming converter gives the repeat its own id, this path did not", ids[0])
	}
	for _, id := range ids {
		if id == "" {
			t.Errorf("a tool_use block reached the client with no id at all: %v", blocks)
		}
	}

	// The control: two calls that differ in arguments are two calls, and the
	// streams this leg emits for them agree with the non-stream answer.
	other := r45Chat(r45Call("do_thing", `{"q":"a"}`), r45Call("do_thing", `{"q":"b"}`))
	if got := len(r45NonStreamBlocks(t, other)); got != 2 {
		t.Errorf("two differing calls produced %d non-stream blocks, want 2", got)
	}
	if got := len(r45StreamBlocks(t, other)); got != 2 {
		t.Errorf("two differing calls produced %d stream blocks, want 2", got)
	}
}

// TestTwoCallsUnderOneStatedIDKeepBoth is A45-3: the id is not the call.
func TestTwoCallsUnderOneStatedIDKeepBoth(t *testing.T) {
	a := r45Call("read_file", `{"p":"a"}`)
	b := r45Call("write_file", `{"p":"a"}`)
	a.ID, b.ID = "call_shared", "call_shared"
	r := r45Chat(a, b)

	blocks := r45StreamBlocks(t, r)
	if len(blocks) != 2 {
		t.Fatalf("an upstream id reused for two calls produced %d stream block(s), want 2", len(blocks))
	}
	if blocks[0]["id"] == blocks[1]["id"] {
		t.Errorf("both blocks carry the id %q: one tool_result answers both calls", blocks[0]["id"])
	}
	if blocks[0]["name"] != "read_file" || blocks[1]["name"] != "write_file" {
		t.Errorf("the calls reached the client as %v/%v, want read_file/write_file", blocks[0]["name"], blocks[1]["name"])
	}
	if blocks[0]["id"] != "call_shared" {
		t.Errorf("the first call's own id is %v, want the id the upstream stated (call_shared)", blocks[0]["id"])
	}

	// The control: the same call restated under its own id is still one call —
	// the dedup that exists for a repeat must survive this change.
	same := r45Chat(r45Call("read_file", `{"p":"a"}`), r45Call("read_file", `{"p":"a"}`))
	same.Message.ToolCalls[0].ID = "call_one"
	same.Message.ToolCalls[1].ID = "call_one"
	if got := len(r45StreamBlocks(t, same)); got != 1 {
		t.Errorf("a call restated under its own id produced %d blocks, want 1", got)
	}
}

// TestAnArgumentlessCallStillCarriesItsInput is A45-4: "input" is required.
func TestAnArgumentlessCallStillCarriesItsInput(t *testing.T) {
	blocks := r45NonStreamBlocks(t, r45Chat(r45Call("get_time", "")))
	if len(blocks) != 1 {
		t.Fatalf("%d blocks, want 1", len(blocks))
	}
	if _, ok := blocks[0]["input"]; !ok {
		t.Errorf("the block has no \"input\" key: %v — an SDK unmarshalling it reads a nil argument map where the streaming converter and the client leg both send {}", blocks[0])
	}

	// The stream twin, for the comparison the finding is about: the same call
	// with the same absent argument string carries "input":{}.
	sblocks := r45StreamBlocks(t, r45Chat(r45Call("get_time", "")))
	if len(sblocks) != 1 {
		t.Fatalf("%d stream blocks, want 1", len(sblocks))
	}
	if _, ok := sblocks[0]["input"]; !ok {
		t.Errorf("the streaming block has no \"input\" key: %v", sblocks[0])
	}
}

// TestAToolTurnKeepsItsStopReasonWhenTheCallIsNamed is the control for the
// stop_reason the nameless rule changes: dropping a nameless call must not
// turn a real call's turn into end_turn.
func TestAToolTurnKeepsItsStopReasonWhenTheCallIsNamed(t *testing.T) {
	raw, err := json.Marshal(ToMessagesResponse("msg_1", r45Chat(r45Call("Bash", `{"cmd":"ls"}`))))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"stop_reason":"tool_use"`) {
		t.Errorf("a turn carrying a named call reports: %s", raw)
	}
}

// TestANamelessCallDoesNotClaimAToolTurn is the other half of A45-1: the
// blocks the turn actually carries decide its stop_reason, not the upstream's
// stated call list — an agent that reads tool_use here waits for a call it
// will never receive, on a turn that is over.
func TestANamelessCallDoesNotClaimAToolTurn(t *testing.T) {
	raw, err := json.Marshal(ToMessagesResponse("msg_1", r45Chat(r45Call("", `{"cmd":"ls"}`))))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), `"stop_reason":"tool_use"`) {
		t.Errorf("a turn whose only call was never named reports tool_use: %s", raw)
	}
}
