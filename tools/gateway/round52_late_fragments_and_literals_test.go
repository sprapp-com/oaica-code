package main

// round52_late_fragments_and_literals_test.go — round 52's findings on the
// metered gateway leg.
//
// G1: a turn's verdict (stop_reason) was taken over everything the UPSTREAM
// stated, not over the bytes the CLIENT was handed. A fragment that arrives
// after its call's block closed is logged and dropped, yet it kept appending to
// the accumulated arguments, so an unterminated input the model wrote was
// completed on paper by a fragment the client never received: the turn reported
// tool_use and the upstream's max_tokens truncation was erased with it, while
// the client held `{"path":"a` — a call no accumulator can parse, billed as a
// success. The verdict is a statement about the call the client was given.
//
// G2: whitespace the upstream stated as a call's whole argument text was
// wrapped as freeform (`{"_raw":"   "}`), so the same bytes were an empty
// object on this leg's non-stream path (callInput) and an object with a key no
// tool declared on the streamed one.
//
// G3: an integer field written as a JSON number with a fraction or an exponent
// (`max_tokens:64.0`, `top_k:1e3`, `thinking.budget_tokens:5.0`,
// `tools[0].max_uses:3.0`) is refused at decode by both sibling legs — their
// fields are Go ints — while this leg read the decoded float64, found a whole
// number, and served 200 with a different cap, sampling or budget. Only the
// literal carries the distinction, so it is read from the literal.
//
// G4: `source` stated as a number, an array or a bool was dropped and the turn
// served; the sibling's typed field cannot hold it and refused the whole body.
// A source stated as bare TEXT is a shape the sibling accepts (a reference), so
// it stays served (2026-09-27 audit, round 52).

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// round52StopReason reads the stop_reason the stream's message_delta states.
func round52StopReason(t *testing.T, stream string) string {
	t.Helper()
	got := ""
	for _, line := range strings.Split(stream, "\n") {
		body, ok := strings.CutPrefix(strings.TrimSpace(line), "data:")
		if !ok {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(body)), &ev) != nil {
			continue
		}
		if ev.Type == "message_delta" && ev.Delta.StopReason != "" {
			got = ev.Delta.StopReason
		}
	}
	return got
}

// TestALateFragmentCompletesTheCallItBelongsTo is G1 re-read by round 63. The
// upstream truncated while the model was still writing the call, and the rest
// of that call arrived after a SECOND call had opened — the same interleaving
// round 63's F63-L3-2 is about. Round 52 read the second call's start as
// closing the first call's block, so the tail was logged and dropped and the
// turn fell to max_tokens. Round 63's feed rule is that a call whose arguments
// are still an unfinished object is not closed by the next call's start, which
// would strand the bytes the client is owed: the tail is delivered, the call
// reaches the client whole, and — with it whole — the verdict follows the call
// the client was given, exactly as this bridge's own document arm answers the
// same turn (round 60's r60DocArm).
//
// What round 52 pinned is unchanged and still held: the verdict is taken over
// the bytes the CLIENT was handed, never over everything the upstream stated.
// TestATruncatedCallIsNotACall below is that property's own guard, on the
// same body without the interleaved tail.
func TestALateFragmentCompletesTheCallItBelongsTo(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		// The call opens and its arguments stop mid-object, as a token cap does.
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"path\":\"a"}}]}}]}`+"\n\n")
		f.Flush()
		// A second call opens: the first call's block is closed, so everything
		// the client is owed for it has been delivered. The second call's own
		// arguments are the text no accumulator can parse, so it is not a call
		// the client can run either.
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_2","type":"function","function":{"name":"Write","arguments":"{\"x\":1}oops"}}]}}]}`+"\n\n")
		f.Flush()
		// The rest of the FIRST call's arguments, arriving across the second
		// call's events. The index says they are that call's, so they are owed
		// to the client, and the first call's block is still open to take them.
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":".txt\"}"}}]}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"length"}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	srv := round36Gateway(t, upstream)
	stream := round36Stream(t, srv, round44AskStream)

	acc := round51StreamArgs(t, stream)
	if got, ok := acc[0]; !ok || got != `{"path":"a.txt"}` {
		t.Errorf("the first call reaches the client as %q, want {\"path\":\"a.txt\"}:\n%s\nits own fragment arrived after the second call opened; the second call's start does not close a block whose arguments are still an unfinished object, or the bytes the model wrote for that call are stranded", got, stream)
	}
	if reason := round52StopReason(t, stream); reason != "tool_use" {
		t.Errorf("the turn's stop_reason is %q, want tool_use:\n%s\nthe client was handed the first call whole, which is what this bridge's own document arm answers the same turn with", reason, stream)
	}
}

// TestATruncatedCallIsNotACall is the guard round 52 left behind, and the
// property its G1 finding is really about: a call the model was cut off inside
// is not a call. Nothing completes it — no later fragment carries the rest, and
// nothing follows it — so the client holds an unterminated input and the turn
// is the upstream's truncation. The verdict is taken over the bytes the client
// was HANDED; a fragment that arrives after its block closed, or that never
// arrives, cannot promote it to tool_use.
func TestATruncatedCallIsNotACall(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"path\":\"a"}}]}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"length"}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	srv := round36Gateway(t, upstream)
	stream := round36Stream(t, srv, round44AskStream)

	if got, ok := round51StreamArgs(t, stream)[0]; !ok || got != `{"path":"a` {
		t.Errorf("the truncated call reaches the client as %q, want the unterminated {\"path\":\"a the model wrote", got)
	}
	if reason := round52StopReason(t, stream); reason != "max_tokens" {
		t.Errorf("the turn's stop_reason is %q, want max_tokens:\n%s\nthe upstream truncated mid-call and nothing completed it; reporting tool_use promises the client a call the model never finished writing", reason, stream)
	}
}

// TestTheVerdictIsTheCallTheClientWasGiven is the other side of G1: a fragment
// that IS delivered still completes the call, and the turn is a tool_use.
func TestTheVerdictIsTheCallTheClientWasGiven(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"path\":\"a"}}]}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":".txt\"}"}}]}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	srv := round36Gateway(t, upstream)
	stream := round36Stream(t, srv, round44AskStream)

	if got := round51StreamArgs(t, stream)[0]; got != `{"path":"a.txt"}` {
		t.Errorf("a delivered call accumulated as %q, want {\"path\":\"a.txt\"}", got)
	}
	if reason := round52StopReason(t, stream); reason != "tool_use" {
		t.Errorf("stop_reason %q over a call the client was handed whole:\n%s", reason, stream)
	}
}

// TestWhitespaceArgumentsArriveAsTheEmptyObject is G2: the same bytes are an
// empty object on this leg's non-stream path, and must be on the streamed one.
func TestWhitespaceArgumentsArriveAsTheEmptyObject(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Ping","arguments":"   "}}]}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	srv := round36Gateway(t, upstream)
	stream := round36Stream(t, srv, round44AskStream)

	got, ok := round51StreamArgs(t, stream)[0]
	if !ok {
		t.Fatalf("the call reached the client with no arguments at all:\n%s", stream)
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(got), &value); err != nil {
		t.Fatalf("the client accumulated %q, which is not JSON: %v", got, err)
	}
	if len(value) != 0 {
		t.Errorf("whitespace arguments reached the client as %q, want {}\nthe non-stream path hands the same bytes an empty object, and a key no tool declared is input the model never wrote", got)
	}
}

// TestAnIntegerFieldIsIntegerInItsLiteral is G3: the four fields both siblings
// decode as Go ints, written as numbers with a fraction or an exponent.
func TestAnIntegerFieldIsIntegerInItsLiteral(t *testing.T) {
	for _, tc := range []struct {
		name string
		// field is the field under test, written as the client wrote it.
		field string
		want  string
	}{
		{"max-tokens-fraction", `"max_tokens":64.0`, "max_tokens is required and must be a positive integer"},
		{"max-tokens-exponent", `"max_tokens":1e3`, "max_tokens is required and must be a positive integer"},
		{"top-k-fraction", `"max_tokens":64,"top_k":3.0`, "top_k must be an integer"},
		{"top-k-exponent", `"max_tokens":64,"top_k":1e3`, "top_k must be an integer"},
		{"budget-fraction", `"max_tokens":64,"thinking":{"type":"enabled","budget_tokens":5.0}`, "thinking.budget_tokens must be an integer"},
		{"max-uses-fraction", `"max_tokens":64,"tools":[{"name":"a","max_uses":3.0}]`, "tools[0].max_uses must be an integer"},
		{"max-uses-exponent", `"max_tokens":64,"tools":[{"name":"a","max_uses":2e0}]`, "tools[0].max_uses must be an integer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"kat-awq","stream":false,"messages":[{"role":"user","content":"go"}],` + tc.field + `}`
			cap := &round44Capture{}
			up := round44Upstream(t, cap, "application/json", `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			srv, _ := round39Gateway(t, up, nil)

			status, out := round44Raw(t, srv, body)
			if status != http.StatusBadRequest {
				t.Fatalf("status %d for %s, body:\n%s\nthe sibling legs decode this field into an int and refuse the whole body at decode", status, tc.field, out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("the refusal does not name the field (%q):\n%s", tc.want, out)
			}
			if asked := cap.take(); asked != "" {
				t.Errorf("the body reached the upstream anyway:\n%s", asked)
			}
		})
	}
}

// TestAnIntegerLiteralIsStillServed is the other side of G3: the same fields
// written as integer literals — and the plain whole-number floats a body this
// process built itself carries — still reach the upstream.
func TestAnIntegerLiteralIsStillServed(t *testing.T) {
	cap := &round44Capture{}
	up := round44Upstream(t, cap, "application/json", `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	srv, _ := round39Gateway(t, up, nil)

	body := `{"model":"kat-awq","max_tokens":64,"top_k":40,"stream":false,` +
		`"thinking":{"type":"enabled","budget_tokens":1024},` +
		`"tools":[{"name":"a","max_uses":3}],` +
		`"messages":[{"role":"user","content":"go"}]}`
	status, out := round44Raw(t, srv, body)
	if status != http.StatusOK {
		t.Fatalf("integer literals were refused with %d:\n%s", status, out)
	}
	asked := cap.take()
	if !strings.Contains(asked, `"max_tokens":64`) || !strings.Contains(asked, `"top_k":40`) {
		t.Errorf("an integer field did not reach the upstream:\n%s", asked)
	}
}

// TestASourceOfTheWrongKindIsRefusedInWords is G4.
func TestASourceOfTheWrongKindIsRefusedInWords(t *testing.T) {
	const head = `{"model":"kat-awq","max_tokens":64,"stream":false,"messages":[{"role":"user","content":`
	for _, tc := range []struct {
		name  string
		block string
	}{
		{"number", `{"type":"image","source":7}`},
		{"array", `{"type":"image","source":[]}`},
		{"bool", `{"type":"image","source":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cap := &round44Capture{}
			up := round44Upstream(t, cap, "application/json", `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			srv, _ := round39Gateway(t, up, nil)

			status, out := round44Raw(t, srv, head+`[`+tc.block+`]}]}`)
			if status != http.StatusBadRequest {
				t.Fatalf("status %d for a source of this kind, body:\n%s\nthe sibling's typed field cannot hold it and refused the whole request", status, out)
			}
			if !strings.Contains(out, ".source must be an object") {
				t.Errorf("the refusal does not name the field:\n%s", out)
			}
			if asked := cap.take(); asked != "" {
				t.Errorf("the body reached the upstream anyway:\n%s", asked)
			}
		})
	}
}

// TestABareStringSourceIsStillServed is the other side of G4: a source stated
// as bare TEXT is the reference shape the sibling accepts, and a source stated
// as an object is the ordinary image.
func TestABareStringSourceIsStillServed(t *testing.T) {
	cap := &round44Capture{}
	up := round44Upstream(t, cap, "application/json", `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	srv, _ := round39Gateway(t, up, []string{"text", "image"})

	body := `{"model":"kat-awq","max_tokens":64,"stream":false,` +
		`"messages":[{"role":"user","content":[{"type":"search_result","source":"https://example.com/a"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}]}]}`
	status, out := round44Raw(t, srv, body)
	if status != http.StatusOK {
		t.Fatalf("a text source and an object source were refused with %d:\n%s", status, out)
	}
}
