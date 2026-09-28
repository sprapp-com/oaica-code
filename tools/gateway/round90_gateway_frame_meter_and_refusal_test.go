package main

// Round 90, leg 3 (tools/gateway). Three defects, one doctrine each:
//
//   - F90-L3-1: the METER and the BRIDGE read the same `data:` frame with two
//     different structs — the bridge decoded the whole frame strictly, the
//     recorder modelled only `usage` — so a frame whose other fields did not
//     fit was dropped by the one that serves the client and counted by the one
//     that books the row. The row then stated tokens the client was never told
//     and the upstream was never billed.
//
//   - F90-L3-2: the SENTENCE a refused turn is answered with depended on the
//     client's `stream` flag, because the streamed arm asked only whether the
//     body PARSED while the buffered arm named the cause. One body, one cause,
//     whichever spelling carried it.
//
//   - F90-L3-3: an Authorization value that carries no credential shadowed the
//     key the caller actually sent — including the scheme word alone, which is
//     how an empty `Bearer ` token arrives (a header value's trailing
//     whitespace is stripped on the wire).

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// r90Usage is the closing usage the client was handed: the message_delta of a
// stream, or the usage of a plain message.
func r90Usage(t *testing.T, body string) (int, int, int) {
	t.Helper()
	usage := ""
	for _, line := range strings.Split(body, "\n") {
		raw, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type  string          `json:"type"`
			Usage json.RawMessage `json:"usage"`
		}
		if json.Unmarshal([]byte(raw), &ev) == nil && ev.Type == "message_delta" && len(ev.Usage) > 0 {
			usage = string(ev.Usage)
		}
	}
	if usage == "" {
		var msg struct {
			Usage json.RawMessage `json:"usage"`
		}
		if json.Unmarshal([]byte(body), &msg) == nil {
			usage = string(msg.Usage)
		}
	}
	if usage == "" {
		// A refused turn carries no usage; the callers that read usage only do
		// so for turns that were served.
		return 0, 0, 0
	}
	var u struct {
		Input  int `json:"input_tokens"`
		Cache  int `json:"cache_read_input_tokens"`
		Output int `json:"output_tokens"`
	}
	if err := json.Unmarshal([]byte(usage), &u); err != nil {
		t.Fatalf("usage is not JSON (%v): %s", err, usage)
	}
	return u.Input, u.Cache, u.Output
}

// r90Sentence is the refusal sentence the client was handed, from an SSE error
// event or from a plain error envelope.
func r90Sentence(t *testing.T, body string) string {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		raw, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(raw), &ev) == nil && ev.Type == "error" {
			return ev.Error.Message
		}
	}
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &env) == nil && env.Error.Message != "" {
		return env.Error.Message
	}
	return ""
}

// r90Turn answers one request from one fixed upstream body and reports the
// client's status, the usage it was told, the sentence it was handed and the
// ledger row booked for the same turn.
func r90Turn(t *testing.T, ct, upstreamBody, ask string) (int, int, int, int, string, ledgerEntry) {
	t.Helper()
	up := round44Upstream(t, nil, ct, upstreamBody)
	srv, ledger := round39Gateway(t, up, nil)
	code, body := round44Raw(t, srv, ask)
	rows := waitLedger(t, ledger, 1)
	var row ledgerEntry
	if len(rows) > 0 {
		row = rows[0]
	}
	in, cache, out := r90Usage(t, body)
	return code, in, cache, out, r90Sentence(t, body), row
}

// TestTheMeterReadsOnlyTheFramesTheBridgeRead is F90-L3-1.
func TestTheMeterReadsOnlyTheFramesTheBridgeRead(t *testing.T) {
	good := `data: {"id":"c","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n" +
		`data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":1}}` + "\n\n" +
		"data: [DONE]\n\n"

	// A frame the BRIDGE drops — its own struct cannot hold these fields — whose
	// usage the METER used to count anyway. Placed LAST, so nothing a later
	// frame states can overwrite what the row booked from it.
	for _, tc := range []struct{ name, frame string }{
		{"content as an array", `{"id":"c","choices":[{"index":0,"delta":{"content":[{"type":"text","text":"hi"}]}}],"usage":{"prompt_tokens":9000,"completion_tokens":500}}`},
		{"a numeric id", `{"id":123,"choices":[{"index":0,"delta":{"content":"hi"}}],"usage":{"prompt_tokens":9000,"completion_tokens":500}}`},
		{"finish_reason as an object", `{"id":"c","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":{"why":"stop"}}],"usage":{"prompt_tokens":9000,"completion_tokens":500}}`},
	} {
		code, in, _, out, _, row := r90Turn(t, "text/event-stream", good+"data: "+tc.frame+"\n\n", round44AskStream)
		if code != http.StatusOK {
			t.Fatalf("%s: the turn was answered %d, want 200", tc.name, code)
		}
		if row.PromptTokens != in || row.CompletionTokens != out {
			t.Errorf("%s: the ledger booked %d/%d for a turn the client read as %d/%d\n"+
				"the frame this bridge dropped is not part of the answer it served, so neither is its usage: the meter and the bridge must read the frame with the same struct (2026-09-29 audit, round 90, F90-L3-1)",
				tc.name, row.PromptTokens, row.CompletionTokens, in, out)
		}
	}

	// The control: a frame the bridge DOES read is metered, on both readers.
	code, in, _, out, _, row := r90Turn(t, "text/event-stream", good, round44AskStream)
	if code != http.StatusOK || row.PromptTokens != in || row.CompletionTokens != out {
		t.Errorf("a well-formed stream was answered %d and booked %d/%d for a client told %d/%d",
			code, row.PromptTokens, row.CompletionTokens, in, out)
	}
}

// TestOneBodyStatesOneCauseWhicheverSpelling is F90-L3-2.
func TestOneBodyStatesOneCauseWhicheverSpelling(t *testing.T) {
	for _, tc := range []struct{ name, ct, body string }{
		{"a 200 that is not JSON", "text/plain", "not json at all"},
		{"a 200 with no choices", "application/json", `{"id":"c","choices":[]}`},
		{"an empty body", "application/json", ""},
		{"a bare error object", "application/json", `{"error":{"message":"boom"}}`},
		{"a whole completion that says nothing", "application/json", `{"id":"c","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":""}}]}`},
		{"a body that is a whole completion cut short", "application/json", `{"id":"c","choices":[{"finish_reason":"length","message":{"role":"assistant","content":"","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"echo hi"}}]}}],"usage":{"prompt_tokens":12,"completion_tokens":3}}`},
	} {
		sc, _, _, _, sentence, srow := r90Turn(t, tc.ct, tc.body, round44AskStream)
		pc, _, _, _, psentence, prow := r90Turn(t, tc.ct, tc.body, round44AskPlain)
		if sc != pc || sentence != psentence {
			t.Errorf("%s: the streamed client read %d %q and the buffered client %d %q\n"+
				"one upstream body states one cause, whichever spelling the client asked for (2026-09-29 audit, round 90, F90-L3-2)",
				tc.name, sc, sentence, pc, psentence)
		}
		if srow.Status != prow.Status {
			t.Errorf("%s: the ledger booked %d for the streamed turn and %d for the buffered one",
				tc.name, srow.Status, prow.Status)
		}
	}

	// The control: a body whose FRAMES said nothing keeps the stream arm's own
	// sentence — that reading is this stream's, and the buffered arm never sees
	// frames at all.
	_, _, _, _, sentence, _ := r90Turn(t, "text/event-stream",
		`data: {"id":"c","choices":[{"index":0,"delta":{}}]}`+"\n\n"+"data: [DONE]\n\n", round44AskStream)
	if sentence != "upstream returned an empty stream" {
		t.Errorf("a stream of frames that said nothing was refused with %q, want the empty STREAM sentence: a parsed frame is what tells this reading from the buffered arm's (2026-09-29 audit, round 90, F90-L3-2)", sentence)
	}

	// RECORDED, held here so a later fix has to confront the measurement: an SSE
	// body answered to a NON-streaming client is decoded as a raw document, so
	// this pairing states "unparseable upstream response" where the streamed
	// client reads "upstream returned an empty stream". No live producer: this
	// gateway mirrors the client's `stream` flag upstream (`stream, _ :=
	// req["stream"].(bool)`, main.go), so a client that asks for a buffered
	// answer never meets a body of frames — only an upstream that ignores the
	// flag produces it.
	_, _, _, _, framedPlain, _ := r90Turn(t, "text/event-stream",
		`data: {"id":"c","choices":[{"index":0,"delta":{}}]}`+"\n\n"+"data: [DONE]\n\n", round44AskPlain)
	if framedPlain != "unparseable upstream response" {
		t.Errorf("a body of frames answered to a non-streaming client reads %q, want the recorded decode sentence — if this changed, the note above must be rewritten rather than the assertion loosened (2026-09-29 audit, round 90, F90-L3-2)", framedPlain)
	}
}

// TestAnAuthorizationThatCarriesNoCredentialFallsThrough is F90-L3-3.
func TestAnAuthorizationThatCarriesNoCredentialFallsThrough(t *testing.T) {
	reply := `{"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`
	for _, tc := range []struct{ name, auth string }{
		{"an empty Bearer token", "Bearer "},
		{"the scheme word alone", "Bearer"},
		{"whitespace only", "   "},
		{"a scheme this gateway does not read", "Basic xxx"},
	} {
		up := round44Upstream(t, nil, "application/json", reply)
		srv, _ := round39Gateway(t, up, nil)
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(round44AskPlain))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", tc.auth)
		req.Header.Set("X-Api-Key", "sk")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		out := new(strings.Builder)
		buf := make([]byte, 4096)
		for {
			n, rerr := resp.Body.Read(buf)
			out.Write(buf[:n])
			if rerr != nil {
				break
			}
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: %d %s\n"+
				"an Authorization header that carries no credential is not the caller's key, and reading it as one made an intermediary's header shadow the X-Api-Key the caller did send (2026-09-29 audit, round 90, F90-L3-3)",
				tc.name, resp.StatusCode, out.String())
		}
	}

	// The controls, so the fall-through cannot become "Authorization is ignored":
	// a valid Bearer is still the credential, and a WRONG one still refuses even
	// beside a valid X-Api-Key — the header that names a credential is read first
	// and answered on its own terms.
	for _, tc := range []struct {
		name string
		auth string
		code int
	}{
		{"a valid Bearer", "Bearer " + "sk", http.StatusOK},
		{"a Bearer this gateway does not know", "Bearer nope", http.StatusUnauthorized},
	} {
		up := round44Upstream(t, nil, "application/json", reply)
		srv, _ := round39Gateway(t, up, nil)
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(round44AskPlain))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", tc.auth)
		req.Header.Set("X-Api-Key", "sk")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.code {
			t.Errorf("%s: answered %d, want %d", tc.name, resp.StatusCode, tc.code)
		}
	}
}
