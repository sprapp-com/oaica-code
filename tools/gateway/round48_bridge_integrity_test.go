package main

// round48_bridge_integrity_test.go — round 48's findings on the gateway leg.
//
// Every finding here was reproduced on the unmodified tree before it was fixed.
//
// C-F1: a fragment's INDEX names its call, so two fragments at two indexes that
// state one id each opened two blocks carrying it. Round 47 made the second
// RENUMBER, which is right for two different calls but wrong for the case the
// wire actually writes: the upstream restating the SAME call (same name, same
// canonical arguments) under its own id, which both other legs answer with ONE
// block. The non-stream list had the mirror defect — it emitted the restatement
// twice, running a side effect the wire never asked for — and round 48 fixed
// both to the same rule: same identity merges, different identity renumbers.
//
// B-F2/C-F2: the mint of an id-less call hashes the call's FINISHED arguments,
// but a block used to open on its first fragment. The name arrives before the
// arguments, so a wire that split the call across frames minted over the
// half-written text and gave the client a different id than the byte-identical
// unsplit wire — one call, two ids, two sessions. The block is now held until
// its arguments are a finished JSON object (or the stream ends, or another call
// starts), and released with its own identity.
//
// B-F3/C-F3: the arguments a mint hashes are the canonical re-encoding of the
// parse (canonicalCallArgs), because that is what both other legs hash. Two
// shapes reached a different id here: a repeated key ({"a":1,"a":2} is {"a":2}
// to an ordered map) and freeform text, which both other legs and this
// bridge's own block input keep as {"_raw":…}, never as the bare text.
//
// B-F4: the non-stream list reserved no stated id before it took its mints, so
// an id-less call listed BEFORE a call that stated the minted id took that id
// for itself and the second call had to be renumbered — the one id the wire
// did name, changed. Every stated id is reserved in a pre-pass.
//
// C-F5: the JSON literal null unmarshals into a nil map without an error, so
// the block's input was emitted as null while the id beside it had been minted
// from the empty object. The input of a tool_use block is a required object.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// round48DocBlock is one tool_use block of a non-stream body, as the client
// reads it: the id, the name, and the input re-encoded.
type round48DocBlock struct {
	id, name, input string
}

// round48DocBlocks decodes a non-stream Anthropic body's tool_use blocks.
func round48DocBlocks(t *testing.T, body string) []round48DocBlock {
	t.Helper()
	var doc struct {
		Content []struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	var blocks []round48DocBlock
	for _, b := range doc.Content {
		if b.Type != "tool_use" {
			continue
		}
		blocks = append(blocks, round48DocBlock{id: b.ID, name: b.Name, input: string(b.Input)})
	}
	return blocks
}

// round48Doc is a whole non-stream completion carrying the given tool_calls
// entries verbatim.
func round48Doc(toolCalls string) string {
	return `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,` +
		`"tool_calls":[` + toolCalls + `]},` +
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":2,"total_tokens":13}}`
}

// TestARestatedStatedCallIsOneBlock is C-F1. The upstream states one id for the
// same call twice, at two indexes and then in a whole non-stream list: both
// other legs answer with ONE block, so a client that runs tool calls does not
// run the model's one call twice.
func TestARestatedStatedCallIsOneBlock(t *testing.T) {
	t.Run("stream_same_identity", func(t *testing.T) {
		up := round45Frames(t,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_X","type":"function","function":{"name":"a","arguments":"{}"}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_X","type":"function","function":{"name":"a","arguments":"{}"}}]}}]}`,
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
			t.Fatalf("the stream opened %d tool_use block(s), want 1 — a call restated under its own id is one call:\n%s", len(blocks), body)
		}
		if blocks[0].id != "call_X" {
			t.Errorf("the block's id is %q, want the stated call_X\n%s", blocks[0].id, body)
		}
		if blocks[0].json != "{}" {
			t.Errorf("the block's input is %q, want {}\n%s", blocks[0].json, body)
		}
	})

	t.Run("document_same_identity", func(t *testing.T) {
		doc := round48Doc(
			`{"id":"call_X","type":"function","function":{"name":"a","arguments":"{}"}},` +
				`{"id":"call_X","type":"function","function":{"name":"a","arguments":"{}"}}`)
		up := round45Upstream(t, "application/json", doc)
		srv, _ := round39Gateway(t, up, nil)
		status, body := round45Ask(t, srv, round45AskPlain)
		if status != http.StatusOK {
			t.Fatalf("status %d\n%s", status, body)
		}
		blocks := round48DocBlocks(t, body)
		if len(blocks) != 1 {
			t.Fatalf("the list carries %d tool_use block(s), want 1 — the client would run the one call twice:\n%s", len(blocks), body)
		}
		if blocks[0].id != "call_X" {
			t.Errorf("the block's id is %q, want the stated call_X\n%s", blocks[0].id, body)
		}
	})

	t.Run("document_different_identity", func(t *testing.T) {
		doc := round48Doc(
			`{"id":"call_X","type":"function","function":{"name":"a","arguments":"{}"}},` +
				`{"id":"call_X","type":"function","function":{"name":"b","arguments":"{\"z\":1}"}}`)
		up := round45Upstream(t, "application/json", doc)
		srv, _ := round39Gateway(t, up, nil)
		status, body := round45Ask(t, srv, round45AskPlain)
		if status != http.StatusOK {
			t.Fatalf("status %d\n%s", status, body)
		}
		blocks := round48DocBlocks(t, body)
		if len(blocks) != 2 {
			t.Fatalf("the list carries %d tool_use block(s), want 2 — two different calls are two calls:\n%s", len(blocks), body)
		}
		if blocks[0].id != "call_X" {
			t.Errorf("the first block's id is %q, want the stated call_X\n%s", blocks[0].id, body)
		}
		if blocks[1].id == "call_X" {
			t.Errorf("the second, different call was left under the id the upstream stated for the first — the client answers one of the two:\n%s", body)
		}
	})
}

// TestAnIDLessCallDoesNotTakeALaterCallsStatedID is B-F4. The upstream names
// its second call with the id the id-less first call mints for itself, so a
// list that minted before reserving renamed the one id the wire actually
// states.
func TestAnIDLessCallDoesNotTakeALaterCallsStatedID(t *testing.T) {
	minted := round47ToolCallIDFor("a", "{}")
	want := "call_20544379" // round47ToolCallIDFor("a", "{}\x00#0")
	if got := round47ToolCallIDFor("a", "{}\x00#0"); got != want {
		t.Fatalf("this test's own FNV copy disagrees with the recorded literal: %s != %s", got, want)
	}
	doc := round48Doc(
		`{"type":"function","function":{"name":"a","arguments":"{}"}},` +
			`{"id":"` + minted + `","type":"function","function":{"name":"b","arguments":"{}"}}`)
	up := round45Upstream(t, "application/json", doc)
	srv, _ := round39Gateway(t, up, nil)
	status, body := round45Ask(t, srv, round45AskPlain)
	if status != http.StatusOK {
		t.Fatalf("status %d\n%s", status, body)
	}
	blocks := round48DocBlocks(t, body)
	if len(blocks) != 2 {
		t.Fatalf("got %d tool_use block(s), want 2:\n%s", len(blocks), body)
	}
	if blocks[1].id != minted {
		t.Errorf("the id the upstream STATED for its second call is %s, want %s — an id-less call earlier in the same list took it:\n%s", blocks[1].id, minted, body)
	}
	if blocks[0].id != want {
		t.Errorf("the id-less call's id is %s, want the bumped %s\n%s", blocks[0].id, want, body)
	}
}

// TestASplitIDLessCallMintsItsFinishedArguments is B-F2/C-F2. The name arrives
// before the arguments on the ordinary OpenAI order, so the mint has to wait
// for the arguments the id is a function of — including when the fragment that
// names the call states NO arguments at all, which is not the mid-object text
// the earlier hold covered.
func TestASplitIDLessCallMintsItsFinishedArguments(t *testing.T) {
	want := "call_7ff51383" // round47ToolCallIDFor("Bash", `{"a":1}`)
	split := round45Frames(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"Bash"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":1}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	)
	unsplit := round45Frames(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	)
	ids := make([]string, 0, 2)
	for i, up := range []*httptest.Server{split, unsplit} {
		srv, _ := round39Gateway(t, up, nil)
		status, body := round45Ask(t, srv, round45AskStream)
		if status != http.StatusOK {
			t.Fatalf("wire %d: status %d\n%s", i, status, body)
		}
		blocks := round47Blocks(t, body)
		if len(blocks) != 1 {
			t.Fatalf("wire %d opened %d tool_use block(s), want 1:\n%s", i, len(blocks), body)
		}
		if blocks[0].json != `{"a":1}` {
			t.Errorf("wire %d: the call's input is %q, want {\"a\":1}\n%s", i, blocks[0].json, body)
		}
		ids = append(ids, blocks[0].id)
	}
	if ids[0] != want {
		t.Errorf("the split wire minted %s, want %s — the mint hashed the half-written arguments\n", ids[0], want)
	}
	if ids[0] != ids[1] {
		t.Errorf("one call minted two ids: %s split, %s unsplit", ids[0], ids[1])
	}
}

// TestFreeformArgumentsMintTheClientLegsID is B-F3/C-F3. Both other legs keep
// text that is not a JSON object as {"_raw":…} and hash that, which is also the
// input this bridge hands the client for the call.
func TestFreeformArgumentsMintTheClientLegsID(t *testing.T) {
	want := "call_66e21791" // round47ToolCallIDFor("a", `{"_raw":"x"}`)

	t.Run("document", func(t *testing.T) {
		doc := round48Doc(`{"type":"function","function":{"name":"a","arguments":"x"}}`)
		up := round45Upstream(t, "application/json", doc)
		srv, _ := round39Gateway(t, up, nil)
		status, body := round45Ask(t, srv, round45AskPlain)
		if status != http.StatusOK {
			t.Fatalf("status %d\n%s", status, body)
		}
		blocks := round48DocBlocks(t, body)
		if len(blocks) != 1 {
			t.Fatalf("got %d tool_use block(s), want 1:\n%s", len(blocks), body)
		}
		if blocks[0].id != want {
			t.Errorf("the minted id is %s, want %s — freeform arguments were hashed as bare text where the client leg hashes the _raw object:\n%s", blocks[0].id, want, body)
		}
		if blocks[0].input != `{"_raw":"x"}` {
			t.Errorf("the block's input is %s, want {\"_raw\":\"x\"}\n%s", blocks[0].input, body)
		}
	})

	t.Run("stream", func(t *testing.T) {
		up := round45Frames(t,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"a","arguments":"x"}}]}}]}`,
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
			t.Fatalf("got %d tool_use block(s), want 1:\n%s", len(blocks), body)
		}
		if blocks[0].id != want {
			t.Errorf("the streamed call's id is %s, want %s — the same wire minted two ids depending on the leg:\n%s", blocks[0].id, want, body)
		}
		if blocks[0].json != `{"_raw":"x"}` {
			t.Errorf("the call's input is %q, want {\"_raw\":\"x\"}\n%s", blocks[0].json, body)
		}
	})
}

// TestRepeatedArgumentKeysTakeTheLastValue is B-F3. An ordered map keeps the
// last value written for a repeated key, and that is the object both other legs
// hash.
func TestRepeatedArgumentKeysTakeTheLastValue(t *testing.T) {
	want := "call_50bc2d18" // round47ToolCallIDFor("t", `{"a":2}`)
	doc := round48Doc(`{"type":"function","function":{"name":"t","arguments":"{\"a\":1,\"a\":2}"}}`)
	up := round45Upstream(t, "application/json", doc)
	srv, _ := round39Gateway(t, up, nil)
	status, body := round45Ask(t, srv, round45AskPlain)
	if status != http.StatusOK {
		t.Fatalf("status %d\n%s", status, body)
	}
	blocks := round48DocBlocks(t, body)
	if len(blocks) != 1 {
		t.Fatalf("got %d tool_use block(s), want 1:\n%s", len(blocks), body)
	}
	if blocks[0].id != want {
		t.Errorf("the minted id is %s, want %s — the repeated key was hashed twice instead of keeping its last value:\n%s", blocks[0].id, want, body)
	}
	if blocks[0].input != `{"a":2}` {
		t.Errorf("the block's input is %s, want {\"a\":2}\n%s", blocks[0].input, body)
	}
}

// TestNullArgumentsAreTheEmptyObject is C-F5. The JSON literal null is not an
// object, and the tool_use contract requires one — but the ID beside it is the
// one the other two legs mint, and they mint it from the text they were handed:
// an ordered map re-encodes null as `null`, so legs 1 and 2 hash "null"
// (call_8dd75358) while this round's first reading folded the literal into {} and
// numbered the call call_ab56f21 — a gateway id neither sibling would mint for
// the same call (2026-09-27 audit, round 49, correcting round 48's C-F5).
func TestNullArgumentsAreTheEmptyObject(t *testing.T) {
	want := "call_8dd75358" // round47ToolCallIDFor("n", `null`)
	if got := round47ToolCallIDFor("n", "null"); got != want {
		t.Fatalf("this test's own FNV copy disagrees with the recorded literal: %s != %s", got, want)
	}
	doc := round48Doc(`{"type":"function","function":{"name":"n","arguments":"null"}}`)
	up := round45Upstream(t, "application/json", doc)
	srv, _ := round39Gateway(t, up, nil)
	status, body := round45Ask(t, srv, round45AskPlain)
	if status != http.StatusOK {
		t.Fatalf("status %d\n%s", status, body)
	}
	blocks := round48DocBlocks(t, body)
	if len(blocks) != 1 {
		t.Fatalf("got %d tool_use block(s), want 1:\n%s", len(blocks), body)
	}
	if blocks[0].id != want {
		t.Errorf("the minted id is %s, want %s (the literal both other legs hash; only the INPUT folds to the empty object):\n%s", blocks[0].id, want, body)
	}
	if blocks[0].input != `{}` {
		t.Errorf("the block's input is %s, want {} — the tool_use contract requires an object\n%s", blocks[0].input, body)
	}
}
