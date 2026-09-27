package main

// round45_gateway_integrity_test.go — round 45's findings on the gateway leg.
//
// Every finding here is one the round-44 fixes left standing, and each was
// reproduced on the unmodified tree before it was fixed.
//
// B45-18: the non-stream arm of noAnswer() asked only whether the body PARSED,
// so a whole completion that says nothing was booked with the upstream's own
// 200 in the ledger while the client read the 502 finalize() writes for the
// same body — two readers of one turn, disagreeing again.
//
// B45-14: the round-40 guard suppressed this gateway's own ESTIMATE for a turn
// the client was told failed, but the counts the UPSTREAM stated entered the
// row unconditionally, so a 502 was booked with a positive cost_usd.
//
// B45-1: an id that arrives on the fragment AFTER the one that named the call
// opened a second block, leaving a tool_use named Bash with input {} and the
// real arguments relayed to the client as prose. The client leg merges this
// wire into one call.
//
// B45-2: nothing recorded whether the upstream ever terminated the stream, so a
// connection that closed mid-answer was relayed as a finished turn — end_turn,
// message_stop — and booked 200 with full cost. The client leg refuses it.
//
// B45-4 and B45-19: a minted id is a pure function of the call's name and
// arguments, so two identical id-less calls shared one id; and the whole
// (non-stream) document path passed an absent id through as "" at all.
//
// B45-13: adoption called toolDelta for every call in the document with no
// truncation check, so a fragment the model was still writing reached the
// client as an executable tool_use whose input_json_delta is not JSON, on a
// turn the non-stream path (and the client leg) drop.
//
// B45-3: the bridge rewrites r.URL.Path to reach the upstream's OpenAI wire, so
// every bridged turn was booked under /v1/chat/completions.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	round45AskStream = `{"model":"kat-awq","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"go"}]}`
	round45AskPlain  = `{"model":"kat-awq","max_tokens":64,"stream":false,"messages":[{"role":"user","content":"go"}]}`
)

// round45Upstream serves one fixed body with one fixed content type.
func round45Upstream(t *testing.T, ct, body string) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", ct)
		io.WriteString(w, body)
	}))
	t.Cleanup(up.Close)
	return up
}

// round45Frames serves SSE frames, one Write per frame.
func round45Frames(t *testing.T, frames ...string) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		for _, fr := range frames {
			io.WriteString(w, fr+"\n\n")
			f.Flush()
		}
	}))
	t.Cleanup(up.Close)
	return up
}

// round45Ask posts one Anthropic body and reports the status and the response.
func round45Ask(t *testing.T, srv *httptest.Server, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// round45OneRow answers one request and returns the ledger row it booked.
func round45OneRow(t *testing.T, ct, upstreamBody, ask string) (int, string, ledgerEntry) {
	t.Helper()
	up := round45Upstream(t, ct, upstreamBody)
	srv, ledger := round39Gateway(t, up, nil)
	status, body := round45Ask(t, srv, ask)
	rows := waitLedger(t, ledger, 1)
	if len(rows) != 1 {
		t.Fatalf("the turn booked %d ledger rows, want 1", len(rows))
	}
	return status, body, rows[0]
}

// round45ToolUseIDs returns every tool_use block id in a stream, in order.
func round45ToolUseIDs(t *testing.T, stream string) (ids, names []string) {
	t.Helper()
	for _, line := range strings.Split(stream, "\n") {
		body, ok := strings.CutPrefix(strings.TrimSpace(line), "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type         string `json:"type"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
			Index int `json:"index"`
		}
		if json.Unmarshal([]byte(body), &ev) != nil {
			continue
		}
		if ev.Type == "content_block_start" && ev.ContentBlock.Type == "tool_use" {
			ids = append(ids, ev.ContentBlock.ID)
			names = append(names, ev.ContentBlock.Name)
		}
	}
	return ids, names
}

// --- B45-18 ----------------------------------------------------------------

// TestAWholeCompletionThatSaysNothingIsBookedAsTheFailureItIs is B45-18: the
// row and the client must read one body the same way.
func TestAWholeCompletionThatSaysNothingIsBookedAsTheFailureItIs(t *testing.T) {
	const emptyDoc = `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":""}}],"usage":{"prompt_tokens":12,"completion_tokens":0}}`
	status, _, row := round45OneRow(t, "application/json", emptyDoc, round45AskPlain)
	if status != http.StatusBadGateway {
		t.Fatalf("client status %d, want 502", status)
	}
	if row.Status != http.StatusBadGateway {
		t.Errorf("the ledger booked status=%d for a turn the client read as 502: the row is written before finalize() answers, and an arm of noAnswer() that asks only whether the body parsed told the row the upstream's own 200 — so the one record of a refused turn says it succeeded", row.Status)
	}
}

// TestARefusedTurnBillsNothing is B45-14: a turn the client was told failed is
// not a turn the overage accounting may charge for, whatever the upstream
// stated about it.
func TestARefusedTurnBillsNothing(t *testing.T) {
	const emptyDoc = `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":""}}],"usage":{"prompt_tokens":12,"completion_tokens":0}}`
	const emptyStream = `data: {"choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":0}}` + "\n\n"

	for _, tc := range []struct {
		name, ct, upstream, ask string
	}{
		{"non-stream", "application/json", emptyDoc, round45AskPlain},
		{"stream", "text/event-stream", emptyStream, round45AskStream},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, stream, row := round45OneRow(t, tc.ct, tc.upstream, tc.ask)
			// A stream request's status line is sent before the frames arrive
			// (message_start commits it), so the 502 can only reach the client
			// as the error event the wire defines for a mid-stream failure; on
			// the non-stream path it is still the status itself.
			if tc.name == "non-stream" {
				if status != http.StatusBadGateway {
					t.Fatalf("client status %d, want 502", status)
				}
			} else if !strings.Contains(stream, `"error"`) {
				t.Errorf("a stream the upstream refused without an answer carried no error event:\n%s", stream)
			}
			if row.Status != http.StatusBadGateway {
				t.Errorf("a refused turn was booked status=%d, want 502", row.Status)
			}
			if row.CostUSD != 0 {
				t.Errorf("a turn the client was told failed was booked at cost %v (prompt=%d): the overage accounting charges what this row costs, so every refused turn is charged once for nothing the client received and again for its retry, and the round-40 guard only ever suppressed this gateway's OWN estimate — the counts the upstream stated walked past it", row.CostUSD, row.PromptTokens)
			}
		})
	}
}

// --- B45-1 -----------------------------------------------------------------

// TestAnIDOnALaterFragmentContinuesTheCall is B45-1: a call is introduced by
// its NAME, so a fragment that states an id for a call no block carries yet and
// names nothing is continuing the call the previous fragment opened.
func TestAnIDOnALaterFragmentContinuesTheCall(t *testing.T) {
	up := round45Frames(t,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"function":{"name":"Bash"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_1","function":{"arguments":"{\"cmd\":\"ls\"}"}}]}}]}`,
		`data: {"choices":[{"index":0,"finish_reason":"tool_calls","delta":{}}]}`,
		"data: [DONE]",
	)
	srv, _ := round39Gateway(t, up, nil)
	_, stream := round45Ask(t, srv, round45AskStream)

	ids, names := round45ToolUseIDs(t, stream)
	if len(ids) != 1 {
		t.Fatalf("the wire opened %d tool_use blocks (%v), want 1: an id that arrives after the fragment naming the call is that call's own id, and the client leg merges the two fragments for exactly this reason — splitting here sends the arguments as prose beside a tool_use the model never wrote:\n%s", len(ids), ids, stream)
	}
	if names[0] != "Bash" {
		t.Errorf("the block is named %q, want Bash", names[0])
	}
	if !strings.Contains(stream, `"partial_json":"{\"cmd\":\"ls\"}"`) {
		t.Errorf("the call's arguments did not reach the client as its input_json_delta:\n%s", stream)
	}
	if strings.Contains(stream, `"text_delta"`) {
		t.Errorf("the call's arguments were relayed to the client as text:\n%s", stream)
	}
}

// TestANewIDBesideANewNameIsStillANewCall is B45-1's control: the merge must not
// swallow the second of two calls.
func TestANewIDBesideANewNameIsStillANewCall(t *testing.T) {
	up := round45Frames(t,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"function":{"name":"Bash"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_2","function":{"name":"Read","arguments":"{\"file\":\"a\"}"}}]}}]}`,
		`data: {"choices":[{"index":0,"finish_reason":"tool_calls","delta":{}}]}`,
		"data: [DONE]",
	)
	srv, _ := round39Gateway(t, up, nil)
	_, stream := round45Ask(t, srv, round45AskStream)

	_, names := round45ToolUseIDs(t, stream)
	if len(names) != 2 {
		t.Fatalf("two calls sharing no id produced %d block(s) (%v), want 2:\n%s", len(names), names, stream)
	}
}

// --- B45-2 -----------------------------------------------------------------

// TestAnUnterminatedStreamIsNotAFinishedTurn is B45-2: a stream is complete
// only when the upstream said so, and one that ends in the middle of an answer
// must not be relayed as a turn that finished.
func TestAnUnterminatedStreamIsNotAFinishedTurn(t *testing.T) {
	up := round45Frames(t,
		`data: {"choices":[{"index":0,"delta":{"content":"The answer is"}}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":50}}`,
	)
	srv, ledger := round39Gateway(t, up, nil)
	_, stream := round45Ask(t, srv, round45AskStream)

	if strings.Contains(stream, `"stop_reason":"end_turn"`) || strings.Contains(stream, "message_stop") {
		t.Errorf("a stream that never sent a finish_reason or [DONE] was relayed as a finished turn:\n%s\nthe client leg reports this as a failure and does not flush the calls it accumulated, and a caller reading a clean end never retries a truncated answer", stream)
	}
	if !strings.Contains(stream, `"error"`) {
		t.Errorf("the stream carried no error event for an answer that was cut off:\n%s", stream)
	}
	rows := waitLedger(t, ledger, 1)
	if rows[0].Status != http.StatusBadGateway {
		t.Errorf("an unterminated stream was booked status=%d, want 502", rows[0].Status)
	}
}

// TestATerminatedStreamIsStillAFinishedTurn is B45-2's control, on both
// terminators the OpenAI wire uses.
func TestATerminatedStreamIsStillAFinishedTurn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		frames []string
	}{
		{"finish_reason", []string{
			`data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
			`data: {"choices":[{"index":0,"finish_reason":"stop","delta":{}}]}`,
		}},
		{"done sentinel", []string{
			`data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
			"data: [DONE]",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := round45Frames(t, tc.frames...)
			srv, ledger := round39Gateway(t, up, nil)
			status, stream := round45Ask(t, srv, round45AskStream)
			if status != http.StatusOK {
				t.Fatalf("status %d, body:\n%s", status, stream)
			}
			if !strings.Contains(stream, "message_stop") {
				t.Errorf("a stream the upstream terminated was not closed with message_stop:\n%s", stream)
			}
			rows := waitLedger(t, ledger, 1)
			if rows[0].Status != http.StatusOK {
				t.Errorf("a terminated stream was booked status=%d", rows[0].Status)
			}
		})
	}
}

// --- B45-4 and B45-19 ------------------------------------------------------

// TestTwoIdenticalCallsDoNotShareAMintedID is B45-4: two calls the model asked
// for twice are two blocks, and two blocks answering to one id can be answered
// only once.
func TestTwoIdenticalCallsDoNotShareAMintedID(t *testing.T) {
	frag := `{"function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}`
	up := round45Frames(t,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[`+frag+`]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[`+frag+`]}}]}`,
		`data: {"choices":[{"index":0,"finish_reason":"tool_calls","delta":{}}]}`,
		"data: [DONE]",
	)
	srv, _ := round39Gateway(t, up, nil)
	_, stream := round45Ask(t, srv, round45AskStream)

	ids, _ := round45ToolUseIDs(t, stream)
	if len(ids) != 2 {
		t.Fatalf("%d tool_use block(s), want 2:\n%s", len(ids), stream)
	}
	if ids[0] == ids[1] {
		t.Errorf("both blocks carry the id %q: a minted id is a pure function of the call's name and arguments, so the model asking for the same tool twice produced two blocks the client can answer only once, and the OpenAI wire cannot key two tool_results by one tool_call_id:\n%s", ids[0], stream)
	}
}

// TestAnUnnumberedCallIsStillGivenAnID is B45-19: the whole-document path used
// to pass an absent id through as "".
func TestAnUnnumberedCallIsStillGivenAnID(t *testing.T) {
	const doc = `{"choices":[{"finish_reason":"tool_calls","message":{"content":"","tool_calls":[{"function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}},{"function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}]}}],"usage":{"prompt_tokens":9,"completion_tokens":2}}`

	status, body, _ := round45OneRow(t, "application/json", doc, round45AskPlain)
	if status != http.StatusOK {
		t.Fatalf("status %d, body:\n%s", status, body)
	}
	var msg struct {
		Content []struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, body)
	}
	var ids []string
	for _, b := range msg.Content {
		if b.Type == "tool_use" {
			ids = append(ids, b.ID)
		}
	}
	if len(ids) != 2 {
		t.Fatalf("%d tool_use block(s) in the whole-document answer (%v), want 2:\n%s", len(ids), ids, body)
	}
	for _, id := range ids {
		if id == "" {
			t.Errorf("a tool_use block reached the client with no id at all: Claude Code answers a call by its id, and a tool_result written against it then carries tool_use_id \"\" — the frame path mints one for the same upstream body, which is why this only ever happened on the whole-document path\n%s", body)
		}
	}
	if ids[0] == ids[1] {
		t.Errorf("two blocks share the id %q: one tool_result answers both:\n%s", ids[0], body)
	}
}

// --- B45-13 ----------------------------------------------------------------

// TestATruncatedCallIsNotAdopted is B45-13: adoption must drop the fragment the
// non-stream path drops, rather than hand the client an executable call whose
// input can never be parsed.
func TestATruncatedCallIsNotAdopted(t *testing.T) {
	const doc = `{"choices":[{"message":{"content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":"}}]},"finish_reason":"length"}],"usage":{"prompt_tokens":9,"completion_tokens":2}}`

	// A stream request the upstream answered with a whole document: adoption.
	status, stream, _ := round45OneRow(t, "text/event-stream", doc, round45AskStream)
	if strings.Contains(stream, `"tool_use"`) {
		t.Errorf("a truncated call was adopted and reached the client as a tool_use (status %d):\n%s\nthe model was still writing those arguments when it hit the token limit, and the non-stream path drops the same call", status, stream)
	}
	if strings.Contains(stream, "input_json_delta") {
		t.Errorf("the adopted turn carries an input_json_delta for arguments that are not JSON:\n%s", stream)
	}
}

// TestACompleteCallOnATruncatedTurnIsStillAdopted is B45-13's control: the
// check is on the ARGUMENTS' completeness, not on finish_reason, so a call the
// model finished writing on a turn it hit the token limit for is still a call.
func TestACompleteCallOnATruncatedTurnIsStillAdopted(t *testing.T) {
	const complete = `{"choices":[{"message":{"content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}]},"finish_reason":"length"}],"usage":{"prompt_tokens":9,"completion_tokens":2}}`
	if _, cstream, _ := round45OneRow(t, "text/event-stream", complete, round45AskStream); !strings.Contains(cstream, `"tool_use"`) {
		t.Errorf("a COMPLETE call on a truncated turn was dropped rather than adopted:\n%s", cstream)
	}
}

// --- C45-4 -----------------------------------------------------------------

// TestAWhitespaceCompletionIsStillATextBlock is C45-4 on the bridge: a text is
// content whatever it says, so a completion the path has already decided is an
// answer must not lose its text to a trim.
func TestAWhitespaceCompletionIsStillATextBlock(t *testing.T) {
	const doc = `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":" "}}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`
	status, body, _ := round45OneRow(t, "application/json", doc, round45AskPlain)
	if status != http.StatusOK {
		t.Fatalf("status %d, body:\n%s", status, body)
	}
	if !strings.Contains(body, `"text":" "`) {
		t.Errorf("the answer's text did not reach the client:\n%s\nthe turn is charged output tokens for a body the client was never shown", body)
	}
}

// --- B45-3 -----------------------------------------------------------------

// TestABridgedTurnIsBookedUnderThePathTheClientAskedFor is B45-3.
func TestABridgedTurnIsBookedUnderThePathTheClientAskedFor(t *testing.T) {
	up := round45Frames(t,
		`data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		`data: {"choices":[{"index":0,"finish_reason":"stop","delta":{}}]}`,
		"data: [DONE]",
	)
	srv, ledger := round39Gateway(t, up, nil)
	if status, _ := round45Ask(t, srv, round45AskStream); status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	rows := waitLedger(t, ledger, 1)
	if rows[0].Path != "/v1/messages" {
		t.Errorf("an Anthropic-wire turn was booked under path %q: the bridge rewrites the path it forwards upstream, and the ledger recorded the rewritten one, so every bridged turn in the metering table is indistinguishable from an OpenAI-wire one — and any per-path rate card or accounting split reads a value this leg always overwrites", rows[0].Path)
	}
}
