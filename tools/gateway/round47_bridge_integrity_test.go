package main

// round47_bridge_integrity_test.go — round 47's findings on the gateway leg.
//
// Every finding here was reproduced on the unmodified tree before it was fixed.
//
// G47-1: the non-stream list recorded a STATED id only as an owner, never as an
// id this bridge had given out, so an id-less call later in the same list minted
// the very id the upstream had named for an earlier call — two blocks under one
// id, which the client can answer once.
//
// G47-2: the index-less id branch split a call on the id ALONE. The ordinary
// OpenAI order states the name first and the id on a later fragment, so a wire
// whose id happened to be the one this bridge minted for the call in progress
// was split into two blocks: one named with a half-written input, one nameless
// holding the rest of the arguments.
//
// G47-3: nothingRelayed counted a tool BLOCK rather than what the block would
// relay. A fragment that names nothing and states no arguments is held forever
// (content_block_start is the only event carrying a name), so a stream that said
// nothing read as one that had said something: an empty `message` frame set
// finished on it, and the turn was relayed as complete (200, end_turn) rather
// than as the unterminated one round 45's B45-2 refuses — while the same stream
// with no frames at all is a 502.
//
// G47-4: the id a collision bump mints is spelled with the seed "\x00#k", the
// shape the local and client legs bump with (anthropic.go's ToolCallIDFor,
// anthropic_openai_proxy.go). A different seed mints a different id for the same
// call, and the id is the promise that a tool_result written against one leg's
// answer stays valid when the retry goes through another.
//
// C-F1: a fragment's INDEX names its call, so two fragments at two indexes that
// state one id each opened two blocks carrying it — while the non-stream list and
// both other legs answered the same wire with a distinctly numbered second call.
//
// C-F5: the gateway hashed the RAW argument text where both other legs hash the
// ordered-map re-encoding, so one call minted two ids depending on the leg that
// answered it ({"z": 1, "a": 2} vs {"z":1,"a":2}, or a pretty-printed object).
//
// C-F7: message_start was emitted on the first frame to ARRIVE. An upstream that
// sends a usage-only chunk, an empty delta, or the turn's finish_reason alone and
// then ends has said nothing, and committing on it fixed the status at 200 and
// left the client reading an `error` event for a turn the ledger row — written
// from the same predicate — booked as a 502.

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// round47Block is one tool_use block as the client reads it off the stream.
type round47Block struct {
	id, name, json string
}

// round47Blocks reassembles the tool_use blocks of an SSE stream: the id and
// name from content_block_start, the input from the input_json_delta events that
// follow it.
func round47Blocks(t *testing.T, stream string) []round47Block {
	t.Helper()
	var blocks []round47Block
	idx := map[int]int{}
	for _, line := range strings.Split(stream, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var ev struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock *struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
			Delta *struct {
				Type        string `json:"type"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "content_block_start":
			if ev.ContentBlock == nil || ev.ContentBlock.Type != "tool_use" {
				continue
			}
			idx[ev.Index] = len(blocks)
			blocks = append(blocks, round47Block{id: ev.ContentBlock.ID, name: ev.ContentBlock.Name})
		case "content_block_delta":
			if ev.Delta == nil || ev.Delta.Type != "input_json_delta" {
				continue
			}
			if i, ok := idx[ev.Index]; ok {
				blocks[i].json += ev.Delta.PartialJSON
			}
		}
	}
	return blocks
}

// round47ToolCallIDFor is the documented cross-leg mint (anthropic.go's
// ToolCallIDFor, FNV-1a over "\x00"+name+"\x00"+arguments), written out here so
// a pin can state the id it expects as a literal rather than asking the
// implementation what it produced.
func round47ToolCallIDFor(name, argsJSON string) string {
	var h uint32 = 2166136261
	for _, c := range []byte("\x00" + name + "\x00" + argsJSON) {
		h ^= uint32(c)
		h *= 16777619
	}
	return "call_" + strconv.FormatUint(uint64(h), 16)
}

// TestAStatedIDIsRecordedBeforeTheNextMint is G47-1. The upstream names its
// first call with the id an id-less call of the same identity mints — the wire
// the stream path and both other legs answer with two ids.
func TestAStatedIDIsRecordedBeforeTheNextMint(t *testing.T) {
	minted := gatewayToolCallIDFor("a", "{}")
	doc := `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,` +
		`"tool_calls":[{"id":"` + minted + `","type":"function","function":{"name":"a","arguments":"{}"}},` +
		`{"type":"function","function":{"name":"a","arguments":"{}"}}]},` +
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":2,"total_tokens":13}}`
	up := round45Upstream(t, "application/json", doc)
	srv, _ := round39Gateway(t, up, nil)
	status, body := round45Ask(t, srv, round45AskPlain)
	if status != http.StatusOK {
		t.Fatalf("status %d\n%s", status, body)
	}
	ids := round46BlockIDs(t, body)
	if len(ids) != 2 {
		t.Fatalf("got %d tool_use block(s), want 2:\n%s", len(ids), body)
	}
	if ids[0] == ids[1] {
		t.Errorf("the id-less call was minted the id %q the upstream had already NAMED for another call — the client answers one of the two:\n%s", ids[0], body)
	}
}

// TestAnIDAfterTheNameContinuesItsCall is G47-2: the id arrives on the fragment
// AFTER the name, and it is the id this bridge minted for the call in progress.
// That is the call continuing, not a second call.
func TestAnIDAfterTheNameContinuesItsCall(t *testing.T) {
	minted := gatewayToolCallIDFor("Bash", `{"a"`)
	up := round45Frames(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"Bash","arguments":"{\"a\""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"`+minted+`","function":{"arguments":":1}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	)
	srv, _ := round39Gateway(t, up, nil)
	status, body := round45Ask(t, srv, round45AskStream)
	if status != http.StatusOK {
		t.Fatalf("status %d\n%s", status, body)
	}
	blocks := round47Blocks(t, body)
	if len(blocks) != 1 {
		t.Fatalf("the stream opened %d tool_use block(s), want 1 — an id stated after the name split the call in two:\n%s", len(blocks), body)
	}
	if blocks[0].name != "Bash" {
		t.Errorf("the block's name is %q, want Bash\n%s", blocks[0].name, body)
	}
	if blocks[0].json != `{"a":1}` {
		t.Errorf("the call's input is %q, want {\"a\":1} — the arguments of the call in progress were relayed to the client as prose beside a tool_use with an empty input\n%s", blocks[0].json, body)
	}
}

// TestANamelessFragmentIsNotAnAnswer is G47-3. The fragment names nothing and
// states no arguments, so it relays nothing whatever — the client is shown no
// block for it, and content_block_start, the only event that carries a tool
// name, can never correct it. The whole completion that follows IS the turn
// (round 39's B-F1), and it must reach the client.
func TestANamelessFragmentIsNotAnAnswer(t *testing.T) {
	up := round45Frames(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"type":"function","id":"","function":{"name":"","arguments":""}}]}}]}`,
		`data: {"choices":[{"message":{"role":"assistant","content":"hi there"}}]}`,
	)
	srv, _ := round39Gateway(t, up, nil)
	status, body := round45Ask(t, srv, round45AskStream)
	if status != http.StatusOK {
		t.Fatalf("status %d\n%s", status, body)
	}
	if !strings.Contains(body, "hi there") {
		t.Errorf("the whole completion the upstream sent inside the frame was dropped, because a tool block that names nothing and states no arguments counted as an answer the stream had already given:\n%s", body)
	}
	if !strings.Contains(body, `"end_turn"`) {
		t.Errorf("the relayed turn carries no stop_reason:\n%s", body)
	}
}

// TestACollisionBumpMintsTheCrossLegID is G47-4. Three id-less calls — "x",
// "x", "x#1" — force a bump: the third call's own id lands on the second's, so
// it is minted again from the seed "\x00#0". The literal is what the local leg's
// ToolCallIDFor produces for the same call.
func TestACollisionBumpMintsTheCrossLegID(t *testing.T) {
	want := "call_be2c8409" // anthropic.ToolCallIDFor("a", "x#1\x00#0")
	if got := round47ToolCallIDFor("a", "x#1\x00#0"); got != want {
		t.Fatalf("this test's own FNV copy disagrees with the recorded literal: %s != %s", got, want)
	}
	doc := `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,` +
		`"tool_calls":[{"type":"function","function":{"name":"a","arguments":"x"}},` +
		`{"type":"function","function":{"name":"a","arguments":"x"}},` +
		`{"type":"function","function":{"name":"a","arguments":"x#1"}}]},` +
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":2,"total_tokens":13}}`
	up := round45Upstream(t, "application/json", doc)
	srv, _ := round39Gateway(t, up, nil)
	status, body := round45Ask(t, srv, round45AskPlain)
	if status != http.StatusOK {
		t.Fatalf("status %d\n%s", status, body)
	}
	ids := round46BlockIDs(t, body)
	if len(ids) != 3 {
		t.Fatalf("got %d tool_use block(s), want 3:\n%s", len(ids), body)
	}
	if ids[2] != want {
		t.Errorf("the bumped id is %s, want %s — a tool_result written against the local leg's answer does not name this call when the retry comes through the gateway:\n%s", ids[2], want, body)
	}
}

// TestTwoIndexesStatingOneIDAreTwoCalls is C-F1: the upstream states one id for
// two calls at two indexes. The client can answer a tool_use only by its id, so
// the second call is numbered, exactly as the non-stream list and both other
// legs number it.
func TestTwoIndexesStatingOneIDAreTwoCalls(t *testing.T) {
	up := round45Frames(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_X","type":"function","function":{"name":"a","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_X","type":"function","function":{"name":"b","arguments":"{\"z\":1}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	)
	srv, _ := round39Gateway(t, up, nil)
	status, body := round45Ask(t, srv, round45AskStream)
	if status != http.StatusOK {
		t.Fatalf("status %d\n%s", status, body)
	}
	ids := round46ToolUseIDs(t, body)
	if len(ids) != 2 {
		t.Fatalf("the stream opened %d tool_use block(s), want 2:\n%s", len(ids), body)
	}
	if ids[0] == ids[1] {
		t.Errorf("both blocks carry the id %q the upstream stated for the first:\n%s", ids[0], body)
	}
	if ids[0] != "call_X" {
		t.Errorf("the first block's id is %q, want the stated call_X\n%s", ids[0], body)
	}
}

// TestCanonicalArgumentsMintTheCrossLegID is C-F5. The upstream writes the same
// object with different whitespace; both other legs hash the re-encoding of the
// parsed arguments, which is the literal below.
func TestCanonicalArgumentsMintTheCrossLegID(t *testing.T) {
	want := "call_b57129c2" // anthropic.ToolCallIDFor("a", `{"z":1,"a":2}`)
	doc := `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,` +
		`"tool_calls":[{"type":"function","function":{"name":"a","arguments":"{\"z\": 1, \"a\": 2}"}}]},` +
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":2,"total_tokens":13}}`
	up := round45Upstream(t, "application/json", doc)
	srv, _ := round39Gateway(t, up, nil)
	status, body := round45Ask(t, srv, round45AskPlain)
	if status != http.StatusOK {
		t.Fatalf("status %d\n%s", status, body)
	}
	ids := round46BlockIDs(t, body)
	if len(ids) != 1 {
		t.Fatalf("got %d tool_use block(s), want 1:\n%s", len(ids), body)
	}
	if ids[0] != want {
		t.Errorf("the minted id is %s, want %s — the gateway hashed the raw text where both other legs hash the parsed arguments:\n%s", ids[0], want, body)
	}
}

// TestADegenerateStreamIsNotACommittedTurn is C-F7. The upstream's only frame
// states the turn's finish_reason (or a usage count) and nothing else, then
// sends [DONE]: the stream has relayed nothing, so it is the empty stream the
// ledger row records as a 502 — and a message_start emitted on the first frame
// to ARRIVE fixed the client's status at 200, leaving it to read an `error`
// event for a turn the row booked as a failure.
func TestADegenerateStreamIsNotACommittedTurn(t *testing.T) {
	t.Run("finish_reason_only", func(t *testing.T) {
		up := round45Frames(t,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		)
		srv, _ := round39Gateway(t, up, nil)
		status, body := round45Ask(t, srv, round45AskStream)
		if status == http.StatusOK {
			t.Errorf("a stream that relayed nothing but its finish_reason was answered 200:\n%s", body)
		}
	})

	t.Run("usage_only", func(t *testing.T) {
		up := round45Frames(t,
			`data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":0,"total_tokens":11}}`,
			`data: [DONE]`,
		)
		srv, _ := round39Gateway(t, up, nil)
		status, body := round45Ask(t, srv, round45AskStream)
		if status == http.StatusOK {
			t.Errorf("a stream that relayed nothing but a usage chunk was answered 200:\n%s", body)
		}
	})

	t.Run("content_still_commits", func(t *testing.T) {
		up := round45Frames(t,
			`data: {"choices":[{"delta":{"content":"hi"}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		)
		srv, _ := round39Gateway(t, up, nil)
		status, body := round45Ask(t, srv, round45AskStream)
		if status != http.StatusOK {
			t.Fatalf("a stream carrying text was not committed: status %d\n%s", status, body)
		}
		if !strings.Contains(body, `"end_turn"`) {
			t.Errorf("the committed turn carries no stop_reason:\n%s", body)
		}
	})
}
