package anthropic

// round46_stated_id_test.go — round 46's finding on the Anthropic translation
// site (leg 1: anthropic/anthropic.go).
//
// A46-1: round 45's A45-3 rule ("the id is not the call") was added to the
// streaming converter and not to ToMessagesResponse, so one upstream document
// answered two ways depending on `stream`: the streaming twin minted a
// distinct id for a call whose stated id the turn had already given away,
// while the non-stream path emitted two tool_use blocks under that one id —
// and, for a bare restatement of the same call, two blocks where the streaming
// twin drops the repeat. This is the fourth translation site and the only one
// that still lacked the rule; the client proxy's parser and the gateway's
// bridge both mint on a reused id.

import (
	"encoding/json"
	"testing"

	"github.com/ollama/ollama/api"
)

// r46NonStreamToolUse marshals a non-stream response and returns its tool_use
// blocks in order.
func r46NonStreamToolUse(t *testing.T, r api.ChatResponse) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(ToMessagesResponse("msg_1", r))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc struct {
		Content []map[string]any `json:"content"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, raw)
	}
	var out []map[string]any
	for _, b := range doc.Content {
		if b["type"] == "tool_use" {
			out = append(out, b)
		}
	}
	return out
}

// TestTheTwoPathsAnswerAReusedStatedIDTheSameWay is A46-1: two calls under one
// upstream-stated id are two calls on BOTH paths, with two ids.
func TestTheTwoPathsAnswerAReusedStatedIDTheSameWay(t *testing.T) {
	a := r45Call("read_file", `{"p":"a"}`)
	b := r45Call("write_file", `{"p":"a"}`)
	a.ID, b.ID = "call_shared", "call_shared"
	r := r45Chat(a, b)

	nonStream := r46NonStreamToolUse(t, r)
	if len(nonStream) != 2 {
		t.Fatalf("the non-stream path emitted %d tool_use block(s) for two calls, want 2: one of the model's two calls was never handed to the agent\n%v", len(nonStream), nonStream)
	}
	if nonStream[0]["id"] == nonStream[1]["id"] {
		t.Errorf("both non-stream blocks carry the id %v: one tool_result answers both calls, and the streaming twin mints a distinct id for the same body\n%v", nonStream[0]["id"], nonStream)
	}
	if nonStream[0]["id"] != "call_shared" {
		t.Errorf("the first call's own id is %v, want the id the upstream stated (call_shared)", nonStream[0]["id"])
	}
	if nonStream[0]["name"] != "read_file" || nonStream[1]["name"] != "write_file" {
		t.Errorf("the calls reached the client as %v/%v, want read_file/write_file", nonStream[0]["name"], nonStream[1]["name"])
	}

	// The same list, both paths, must produce the same ids: a body that answers
	// differently depending on `stream` is the defect this campaign keeps
	// finding.
	stream := r45StreamBlocks(t, r)
	if len(stream) != len(nonStream) {
		t.Fatalf("the paths disagree on the block count: stream %d, non-stream %d", len(stream), len(nonStream))
	}
	for i := range stream {
		if stream[i]["id"] != nonStream[i]["id"] {
			t.Errorf("block %d is %v on the stream path and %v on the non-stream one", i, stream[i]["id"], nonStream[i]["id"])
		}
	}
}

// TestARestatementUnderItsOwnIDIsStillOneCall is A46-1's other half: the
// restatement the streaming twin drops must not become a second block here.
func TestARestatementUnderItsOwnIDIsStillOneCall(t *testing.T) {
	a := r45Call("read_file", `{"p":"a"}`)
	b := r45Call("read_file", `{"p":"a"}`)
	a.ID, b.ID = "call_one", "call_one"
	r := r45Chat(a, b)

	if got := len(r46NonStreamToolUse(t, r)); got != 1 {
		t.Errorf("a call restated under its own id produced %d non-stream block(s), want 1: the streaming twin answers this same body with 1", got)
	}
	if got := len(r45StreamBlocks(t, r)); got != 1 {
		t.Errorf("the streaming twin produced %d block(s), want 1", got)
	}
}

// TestAMintedIDNeverLandsOnAStatedOne is A46-4: the id this leg synthesizes
// for an id-less call is a pure function of that call, and an upstream may
// STATE that same string for another call in the same list. Minting without
// looking at the list's stated ids put two different calls under one id, and
// the restatement rule below (which A45-3 and A46-1 both require) then reads
// the pair as one call — so the call the upstream NAMED was dropped and the
// agent ran one of the model's two calls.
func TestAMintedIDNeverLandsOnAStatedOne(t *testing.T) {
	name, args := "a", `{}`
	a := r45Call(name, args)
	b := r45Call(name, args)
	b.ID = ToolCallIDFor(name, args)
	r := r45Chat(a, b)

	nonStream := r46NonStreamToolUse(t, r)
	if len(nonStream) != 2 {
		t.Fatalf("the non-stream path emitted %d tool_use block(s), want 2: an id-less call and a call the upstream named reached the client under one id, and this loop reads that as one call restated\n%v", len(nonStream), nonStream)
	}
	if nonStream[0]["id"] == nonStream[1]["id"] {
		t.Errorf("both blocks carry id %v, want two ids: the synthesized one is bumped off the stated one\n%v", nonStream[0]["id"], nonStream)
	}
	stream := r45StreamBlocks(t, r)
	if len(stream) != len(nonStream) {
		t.Fatalf("the paths disagree: stream %d block(s), non-stream %d", len(stream), len(nonStream))
	}
	for i := range stream {
		if stream[i]["id"] != nonStream[i]["id"] {
			t.Errorf("block %d is %v on the stream path and %v on the non-stream one", i, stream[i]["id"], nonStream[i]["id"])
		}
	}
}

// TestAnIdLessCallBesideAStatedOneKeepsBothIDs is the control for the minting
// branch: the id-less call still gets its own synthesized id, and the stated
// one is untouched.
func TestAnIdLessCallBesideAStatedOneKeepsBothIDs(t *testing.T) {
	a := r45Call("read_file", `{"p":"a"}`)
	a.ID = "call_stated"
	r := r45Chat(a, r45Call("write_file", `{"p":"b"}`))

	blocks := r46NonStreamToolUse(t, r)
	if len(blocks) != 2 {
		t.Fatalf("%d block(s), want 2", len(blocks))
	}
	if blocks[0]["id"] != "call_stated" {
		t.Errorf("the stated id became %v, want call_stated", blocks[0]["id"])
	}
	if id, _ := blocks[1]["id"].(string); id == "" || id == "call_stated" {
		t.Errorf("the id-less call's synthesized id is %q", id)
	}
}
