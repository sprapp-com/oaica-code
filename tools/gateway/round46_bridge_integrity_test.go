package main

// round46_bridge_integrity_test.go — round 46's findings on the gateway leg.
//
// Every finding here was reproduced on the unmodified tree before it was fixed.
//
// G45-1: the id this bridge mints for an id-less call is a pure function of
// that call, so a call the upstream NAMED could be handed the same id — two
// blocks the client can answer only once. The non-stream list minted without
// looking at the ids its own siblings stated, and the stream took a stated id
// its own mint had already given away.
//
// G45-5: the "#n" suffix the repeat rule appends lives inside the string the id
// hashes, so an upstream whose second call really states the arguments "x#1" and
// one that merely repeats "x" hashed to one id.
//
// G45-2: an EMPTY `message` frame marked the stream finished. A stream that then
// ended with no finish_reason and no [DONE] was relayed as a complete turn (200,
// end_turn) instead of the unterminated one round 45's B45-2 refuses — the same
// verdict the client leg gives that wire.
//
// G45-3: the frame reader completes a LINE, so a terminal frame arriving without
// a trailing newline was never parsed and the turn read as unterminated. Worse,
// the ledger asks the same predicate FIRST, so the reading it recorded was the
// error the client was then told.
//
// G45-4: a fragment stating a FRESH id over a block whose id the upstream had
// already stated must begin a new call, not grow the running call's input. The
// gateway gets this right by the round-45 rekey guard (`!cur.statedID`) alone:
// the id-less fragment case that a round-46 candidate branch was written for is
// handled by that guard, and the branch itself was removed after it failed its
// revert-check — the suite was green with it reverted, and on the one wire it
// did change it diverged from the client leg. The test below pins the rule
// against the guard being relaxed, which is the defect the wire can actually
// see (one tool_use whose input is two calls' arguments run together).

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// round46ToolUseIDs returns the ids of every tool_use block in a stream.
func round46ToolUseIDs(t *testing.T, stream string) []string {
	t.Helper()
	ids, _ := round45ToolUseIDs(t, stream)
	return ids
}

// round46BlockIDs decodes a non-stream Anthropic body's tool_use ids.
func round46BlockIDs(t *testing.T, body string) []string {
	t.Helper()
	var doc struct {
		Content []map[string]any `json:"content"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	var ids []string
	for _, b := range doc.Content {
		if b["type"] == "tool_use" {
			id, _ := b["id"].(string)
			ids = append(ids, id)
		}
	}
	return ids
}

// TestAMintedIDNeverLandsOnAStatedOne is G45-1.
func TestAMintedIDNeverLandsOnAStatedOne(t *testing.T) {
	minted := gatewayToolCallIDFor("a", "{}")
	doc := `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,` +
		`"tool_calls":[{"type":"function","function":{"name":"a","arguments":"{}"}},` +
		`{"id":"` + minted + `","type":"function","function":{"name":"a","arguments":"{}"}}]},` +
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":2,"total_tokens":13}}`

	t.Run("non-stream", func(t *testing.T) {
		up := round45Upstream(t, "application/json", doc)
		srv, _ := round39Gateway(t, up, nil)
		status, body := round45Ask(t, srv, round45AskPlain)
		if status != http.StatusOK {
			t.Fatalf("status %d\n%s", status, body)
		}
		ids := round46BlockIDs(t, body)
		if len(ids) != 2 {
			t.Fatalf("got %d tool_use block(s), want 2 — an id-less call and a call the upstream NAMED were handed one id:\n%s", len(ids), body)
		}
		if ids[0] == ids[1] {
			t.Errorf("both blocks carry id %q: one tool_result answers both calls\n%s", ids[0], body)
		}
	})

	t.Run("stream", func(t *testing.T) {
		up := round45Frames(t,
			`data: {"choices":[{"delta":{"tool_calls":[{"function":{"name":"a","arguments":"{}"}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"id":"`+minted+`","function":{"name":"a","arguments":"{}"}}]}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		)
		srv, _ := round39Gateway(t, up, nil)
		_, stream := round45Ask(t, srv, round45AskStream)
		ids := round46ToolUseIDs(t, stream)
		if len(ids) != 2 {
			t.Fatalf("the stream opened %d tool_use block(s), want 2:\n%s", len(ids), stream)
		}
		if ids[0] == ids[1] {
			t.Errorf("both stream blocks carry id %q\n%s", ids[0], stream)
		}
	})
}

// TestAMintedSuffixDoesNotCollideWithStatedArguments is G45-5: three id-less
// calls — "x", "x", "x#1" — must reach the client as three distinctly keyed
// calls.
func TestAMintedSuffixDoesNotCollideWithStatedArguments(t *testing.T) {
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
	seen := map[string]bool{}
	for _, id := range ids {
		seen[id] = true
	}
	if len(seen) != 3 {
		t.Errorf("three calls share %d distinct id(s) %v — the \"#n\" suffix the repeat rule appends is inside the string the id hashes, so it collides with an upstream whose own arguments end that way\n%s", len(seen), ids, body)
	}
}

// TestAStatedIDStatedTwiceIsTwoCalls is G45-1's other half: an id the upstream
// states for ONE call is not a licence to name a second call with it. Two
// blocks under one id reach the client as two tool_use blocks it can answer
// once — and only the first answer names the call it belongs to.
func TestAStatedIDStatedTwiceIsTwoCalls(t *testing.T) {
	doc := `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,` +
		`"tool_calls":[{"id":"call_X","type":"function","function":{"name":"a","arguments":"{}"}},` +
		`{"id":"call_X","type":"function","function":{"name":"b","arguments":"{\"z\":1}"}}]},` +
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
		t.Errorf("two calls the upstream named differently share the id %q it stated for the first:\n%s", ids[0], body)
	}
}

// TestAnEmptyMessageFrameDoesNotFinishTheStream is G45-2.
func TestAnEmptyMessageFrameDoesNotFinishTheStream(t *testing.T) {
	up := round45Frames(t,
		`data: {"choices":[{"message":{"role":"assistant","content":""}}]}`,
		`data: {"choices":[{"delta":{"content":"hi"}}]}`,
	)
	srv, _ := round39Gateway(t, up, nil)
	status, body := round45Ask(t, srv, round45AskStream)
	if status == http.StatusOK && !strings.Contains(body, `"error"`) {
		t.Errorf("a stream that ended with no finish_reason and no [DONE] was relayed as a finished turn, because an earlier EMPTY message frame set finished:\n%s", body)
	}
}

// TestATerminalFrameWithoutANewlineIsStillParsed is G45-3.
func TestATerminalFrameWithoutANewlineIsStillParsed(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		f.Flush()
		// The upstream's own completion marker, with no trailing newline — a
		// connection that closes right after it, or a proxy that trims it.
		io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`)
		f.Flush()
	}))
	t.Cleanup(up.Close)
	srv, _ := round39Gateway(t, up, nil)
	status, body := round45Ask(t, srv, round45AskStream)
	if status != http.StatusOK || strings.Contains(body, `"error"`) {
		t.Errorf("the upstream's own finish_reason, arriving without a trailing newline, was never parsed — the turn reads as unterminated:\n%s", body)
	}
	if !strings.Contains(body, `"end_turn"`) {
		t.Errorf("the turn carries no stop_reason:\n%s", body)
	}
}

// TestAFreshIDBesideAKeyedBlockIsANewCall is G45-4's rule, pinned against the
// rekey guard that enforces it.
func TestAFreshIDBesideAKeyedBlockIsANewCall(t *testing.T) {
	up := round45Frames(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_A","function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_B","function":{"arguments":"{\"b\":2}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	)
	srv, _ := round39Gateway(t, up, nil)
	status, body := round45Ask(t, srv, round45AskStream)
	if status != http.StatusOK {
		t.Fatalf("status %d\n%s", status, body)
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "partial_json") && strings.Contains(line, `b\":2}`) {
			t.Errorf("a fresh id over a block whose id the upstream stated was folded into that call: the client reads one tool_use whose input is two calls' arguments run together\n%s", body)
		}
	}
	if !strings.Contains(body, `{\"b\":2}`) && !strings.Contains(body, `{"b":2}`) {
		t.Errorf("the second call's arguments are nowhere in the stream — a new call's arguments belong to that call, or to the turn as text, never to another call's input:\n%s", body)
	}
}
