package main

// round43_bridge_integrity_test.go — round 43's findings on the gateway leg.
//
// A43-1: the hoist's non-text guard was reachable only for the system messages
// the scan walked before it stopped, so a text-only late system message hid a
// later one that carried an image, and the rewrite merged it into a bare string.
//
// B43-1: noAnswer's bufferedCompletion arm asked only whether the body PARSED.
// The ledger row — written before finalize, from that arm — read 200 for a
// whole completion that said nothing, while the client, after adoption had set
// startSent, was told the turn failed. One turn, two answers.
//
// B43-2: a message text block whose `text` is not a string was read with a
// comma-ok and ignored, so the client's text left the prompt in silence.
//
// B43-3: the id branch of toolKey returned before the name-change / complete-
// arguments split ran, so two index-less fragments sharing one id merged into a
// single tool_use whose partial_json held two concatenated objects.
//
// C43-2: an absent tool_use.input reached the upstream as the literal string
// "null" where the other two legs say {}.
//
// C43-3 and C43-4: this leg's non-stream stop_reason came from the finish_reason
// alone, where leg 1's mapStopReason lets the blocks the turn carries decide —
// tool_use for a turn that has a call (even a truncated one), and never tool_use
// for a turn that has none.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- A43-1 -----------------------------------------------------------------

// TestACarrierBehindATextOnlyLateSystemMessageIsNotMerged is A43-1, the twin of
// the local leg's test. The carrier here is a part array, which is what this
// leg's systemMessageIsTextOnly refuses.
func TestACarrierBehindATextOnlyLateSystemMessageIsNotMerged(t *testing.T) {
	in := []map[string]any{
		{"role": "user", "content": "hi"},
		{"role": "system", "content": "late note"},
		{"role": "system", "content": []any{
			map[string]any{"type": "text", "text": "and the picture"},
			map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "QUJD"}},
		}},
	}

	got := normalizeSystemFirst(in)

	if len(got) != 3 {
		t.Fatalf("normalizeSystemFirst() = %+v, want the three messages as the client sent them: the third carries an image part, and merging it into the leading system string drops what it carries", got)
	}
	for i := range in {
		if len(got[i]) != len(in[i]) {
			t.Errorf("message %d = %+v, want %+v untouched: the non-text guard is asked of every system message, not only of those the scan reaches before it stops", i, got[i], in[i])
		}
	}
	if _, isStr := got[2]["content"].(string); isStr {
		t.Errorf("the carrier's content was rewritten to a bare string: %+v", got[2])
	}
}

// --- B43-1 -----------------------------------------------------------------

// TestAWholeDocumentThatSaysNothingIsNotBookedAsASuccess is B43-1. Whatever the
// client is told, the ledger row for the same turn must say the same thing.
func TestAWholeDocumentThatSaysNothingIsNotBookedAsASuccess(t *testing.T) {
	upstream := round40JSONReply(`{"id":"c1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":""}}],"usage":{"prompt_tokens":12,"completion_tokens":0}}`)
	defer upstream.Close()

	srv, ledger := round39Gateway(t, upstream, nil)
	status, stream := round40Post(t, srv, round39Ask)

	rows := waitLedger(t, ledger, 1)
	if len(rows) != 1 {
		t.Fatalf("the ledger holds %d rows, want 1", len(rows))
	}
	row := rows[0]

	clientFailed := status >= 400 || strings.Contains(stream, "event: error")
	rowFailed := row.Status >= 400
	if clientFailed != rowFailed {
		t.Errorf("the two readers of one turn disagree: the client read status=%d%s while the row says status=%d with prompt=%d completion=%d. An empty whole completion is the same outcome as the `[DONE]`-only body, and both readers must call it a failure (stream=%q)",
			status, map[bool]string{true: " and an error event", false: ""}[strings.Contains(stream, "event: error")], row.Status, row.PromptTokens, row.CompletionTokens, stream)
	}
	if !rowFailed {
		t.Errorf("the ledger booked a metered success for a turn that carries no answer: %+v", row)
	}
	if strings.Contains(stream, "message_start") {
		t.Errorf("a document with nothing to adopt opened a message_start anyway:\n%s", stream)
	}
}

// The control: a whole completion WITH an answer is still adopted and booked as
// the 200 it was.
func TestAWholeDocumentWithAnAnswerIsStillAdopted(t *testing.T) {
	upstream := round40JSONReply(`{"id":"c1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"the answer"}}],"usage":{"prompt_tokens":12,"completion_tokens":3}}`)
	defer upstream.Close()

	srv, ledger := round39Gateway(t, upstream, nil)
	status, stream := round40Post(t, srv, round39Ask)
	if status != 200 || !strings.Contains(stream, "the answer") {
		t.Fatalf("premise: the whole completion was not adopted (status=%d):\n%s", status, stream)
	}
	rows := waitLedger(t, ledger, 1)
	if len(rows) != 1 || rows[0].Status != 200 {
		t.Fatalf("the row for an adopted answer reads %+v, want a 200", rows)
	}
}

// A call with no arguments is still an answer: its block opens either way, so a
// document carrying one must not be read as empty.
func TestAWholeDocumentCarryingOnlyACallIsAnAnswer(t *testing.T) {
	upstream := round40JSONReply(`{"id":"c1","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"Bash","arguments":""}}]}}],"usage":{"prompt_tokens":12,"completion_tokens":1}}`)
	defer upstream.Close()

	srv, ledger := round39Gateway(t, upstream, nil)
	status, stream := round40Post(t, srv, round39Ask)
	if status != 200 || !strings.Contains(stream, `"name":"Bash"`) {
		t.Fatalf("a whole completion carrying a call was refused (status=%d):\n%s", status, stream)
	}
	rows := waitLedger(t, ledger, 1)
	if len(rows) != 1 || rows[0].Status != 200 {
		t.Fatalf("the row for an adopted call reads %+v, want a 200", rows)
	}
}

// --- B43-2 -----------------------------------------------------------------

// TestANonStringMessageTextIsRefusedNotEmptied is B43-2. The sibling legs both
// refuse this body — anthropic.ContentBlock.Text is a *string — so the same
// bytes cannot quietly become an empty prompt here.
func TestANonStringMessageTextIsRefusedNotEmptied(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{"a number", `42`},
		{"a boolean", `true`},
		{"an object", `{"a":1}`},
		{"an array", `["a"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := round43Convert(t, `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[{"type":"text","text":`+tc.value+`}]}]}`); err == "" {
				t.Errorf("a text block whose `text` is %s was accepted: the field is a string on this wire and on both sibling legs, and reading it with a comma-ok wrote an empty string into the prompt the model was asked about", tc.name)
			}
		})
	}
}

// The control: an ABSENT text is a text block with no text, which the sibling
// legs keep as an empty block, and null is the same body to them.
func TestAnAbsentMessageTextIsAnEmptyBlock(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"absent", `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[{"type":"text"}]}]}`},
		{"null", `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[{"type":"text","text":null}]}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := round43Convert(t, tc.body)
			if err != "" {
				t.Fatalf("an empty text block was refused (%s): the sibling legs convert it to an empty block", err)
			}
			// The empty block is the message's whole content, so it reaches the
			// wire as an empty string rather than as a part array.
			if !strings.Contains(out, `"content":""`) {
				t.Errorf("the converted body is %s, want an empty user turn", out)
			}
		})
	}
}

// --- B43-3 -----------------------------------------------------------------

// TestTwoSameIDIndexlessCallsStayTwoCalls is B43-3. A repeated id is not proof
// that a fragment continues the same call: the client leg's startsANewToolCall
// splits this wire, and the gateway must answer it the same way.
func TestTwoSameIDIndexlessCallsStayTwoCalls(t *testing.T) {
	for _, second := range []string{"Bash", "Read"} {
		t.Run("second call to "+second, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				f := w.(http.Flusher)
				io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`+"\n\n")
				f.Flush()
				io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"`+second+`","arguments":"{\"b\":2}"}}]}}]}`+"\n\n")
				f.Flush()
				io.WriteString(w, `data: {"choices":[{"index":0,"finish_reason":"tool_calls","delta":{}}]}`+"\n\n")
				f.Flush()
				io.WriteString(w, "data: [DONE]\n\n")
				f.Flush()
			}))
			defer upstream.Close()

			srv, _ := round39Gateway(t, upstream, nil)
			stream := round36Stream(t, srv, round39Ask)

			if !strings.Contains(stream, `"name":"`+second+`"`) {
				t.Errorf("the second fragment's name %s reached the client in no block:\n%s\nthe id branch of toolKey returned before the split rule ran, so the second call's name was discarded", second, stream)
			}
			if got := strings.Count(stream, `"type":"tool_use"`); got != 2 {
				t.Errorf("two index-less calls sharing one id produced %d tool_use block(s):\n%s\nthe single block's partial_json holds two concatenated objects, which is not JSON, and only one of the two calls can be executed", got, stream)
			}
		})
	}
}

// The control: a fragment repeating the same id AND the same name over
// arguments that are not yet complete still continues that call, which is what
// keeps a call whose arguments arrive in pieces whole.
func TestASameIDSameNameContinuationStaysOneCall(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":"}}]}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"Bash","arguments":"\"ls\"}"}}]}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"choices":[{"index":0,"finish_reason":"tool_calls","delta":{}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	srv, _ := round39Gateway(t, upstream, nil)
	stream := round36Stream(t, srv, round39Ask)

	if got := strings.Count(stream, `"type":"tool_use"`); got != 1 {
		t.Fatalf("one call whose arguments arrived in two fragments produced %d tool_use block(s):\n%s", got, stream)
	}
	args := round43PartialJSON(t, stream)
	if !json.Valid([]byte(args)) || !strings.Contains(args, `"cmd":"ls"`) {
		t.Errorf("the block's partial_json deltas concatenate to %q, want one complete JSON object {\\\"cmd\\\":\\\"ls\\\"}: a fragment that repeats the call's id and name over arguments that are not yet complete is a continuation, and splitting it would hand the client two calls where the model made one", args)
	}
}

// round43PartialJSON concatenates the input_json_delta partials of a stream, in
// the order the client reads them.
func round43PartialJSON(t *testing.T, stream string) string {
	t.Helper()
	var out strings.Builder
	for _, line := range strings.Split(stream, "\n") {
		body, ok := strings.CutPrefix(strings.TrimSpace(line), "data: ")
		if !ok {
			continue
		}
		var event struct {
			Delta struct {
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(body), &event) == nil {
			out.WriteString(event.Delta.PartialJSON)
		}
	}
	return out.String()
}

// --- C43-2 -----------------------------------------------------------------

// TestAnAbsentToolInputIsAnEmptyObject is C43-2, with the explicit null beside
// it: the other two legs distinguish the two bodies, and so must this one.
func TestAnAbsentToolInputIsAnEmptyObject(t *testing.T) {
	out, err := round43Convert(t, `{"model":"m","max_tokens":8,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"f"}]}]}`)
	if err != "" {
		t.Fatalf("an absent tool_use.input was refused: %s", err)
	}
	if !strings.Contains(out, `"arguments":"{}"`) {
		t.Errorf("an absent tool_use.input converted to %s, want \"{}\": the local leg's nil argument map marshals to {}, and the client leg emits the same string — an upstream tool parser must not be handed the literal None for a call made with no arguments", out)
	}
}

func TestAnExplicitlyNullToolInputStaysNull(t *testing.T) {
	out, err := round43Convert(t, `{"model":"m","max_tokens":8,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"f","input":null}]}]}`)
	if err != "" {
		t.Fatalf("an explicit null tool_use.input was refused: %s", err)
	}
	if !strings.Contains(out, `"arguments":"null"`) {
		t.Errorf("an explicit tool_use.input null converted to %s, want \"null\": all three legs say null for this body, and only the absent key is the empty object", out)
	}
}

// --- C43-3 and C43-4 --------------------------------------------------------

// TestANonStreamTurnWithNoCallIsNotToolUse is C43-3.
func TestANonStreamTurnWithNoCallIsNotToolUse(t *testing.T) {
	upstream := round40JSONReply(`{"id":"c1","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"I am done."}}],"usage":{"prompt_tokens":11,"completion_tokens":3}}`)
	defer upstream.Close()

	srv, _ := round39Gateway(t, upstream, nil)
	status, out := round40Post(t, srv, `{"model":"kat-awq","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, out)
	}
	if strings.Contains(out, `"stop_reason":"tool_use"`) {
		t.Errorf("a turn whose body carries no tool_use block was reported as tool_use: %s\ntool_use promises the client a call it can execute, and an agent that reads it waits for one that is not in the message — the stalled turn the stream path has guarded against since round 39", out)
	}
	if !strings.Contains(out, `"stop_reason":"end_turn"`) {
		t.Errorf("the turn's stop_reason is neither tool_use nor end_turn: %s", out)
	}
}

// TestATruncatedTurnThatCarriesACallIsToolUse is C43-4: the other two legs rank
// a surviving call above the finish_reason, and this one must agree.
func TestATruncatedTurnThatCarriesACallIsToolUse(t *testing.T) {
	upstream := round40JSONReply(`{"id":"c1","choices":[{"index":0,"finish_reason":"length","message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]}}],"usage":{"prompt_tokens":11,"completion_tokens":4}}`)
	defer upstream.Close()

	srv, _ := round39Gateway(t, upstream, nil)
	status, out := round40Post(t, srv, `{"model":"kat-awq","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, out)
	}
	if !strings.Contains(out, `"type":"tool_use"`) {
		t.Fatalf("premise: the call did not reach the client's body: %s", out)
	}
	if !strings.Contains(out, `"stop_reason":"tool_use"`) {
		t.Errorf("a body carrying a complete tool_use block was reported as %s\nthe local and client legs both report tool_use for this body, whatever the upstream's finish_reason — the call is in the body, so the client can execute it", out)
	}
}

// The stream arm of the same rule: a truncated turn whose call reached the
// client is tool_use there too.
func TestATruncatedStreamThatCarriesACallIsToolUse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"choices":[{"index":0,"finish_reason":"length","delta":{}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	srv, _ := round39Gateway(t, upstream, nil)
	stream := round36Stream(t, srv, round39Ask)
	if !strings.Contains(stream, `"type":"tool_use"`) {
		t.Fatalf("premise: no call reached the client:\n%s", stream)
	}
	if !strings.Contains(stream, `"stop_reason":"tool_use"`) {
		t.Errorf("a stream whose call reached the client was closed with a stop_reason other than tool_use:\n%s", stream)
	}
}

// The stream control C43-3's guard was written for: a turn that stopped to call
// a tool whose fragments never named one is not tool_use.
func TestAStreamStoppingForAnUnnamedCallIsNotToolUse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"arguments":"{\"a\":1}"}}]}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"choices":[{"index":0,"finish_reason":"tool_calls","delta":{}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	srv, _ := round39Gateway(t, upstream, nil)
	stream := round36Stream(t, srv, round39Ask)
	if strings.Contains(stream, `"stop_reason":"tool_use"`) {
		t.Errorf("a turn whose only call fragments were unnamed was reported as tool_use:\n%s", stream)
	}
}

// --- helper -----------------------------------------------------------------

// round43Convert runs the request converter and returns the wire body it built
// beside the refusal, if any.
func round43Convert(t *testing.T, body string) (string, string) {
	t.Helper()
	var req map[string]any
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal %s: %v", body, err)
	}
	out, errStr := anthropicToOpenAI(req, false)
	if errStr != "" {
		return "", errStr
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw), ""
}
