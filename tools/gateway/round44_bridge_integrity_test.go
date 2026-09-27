package main

// round44_bridge_integrity_test.go — round 44's findings on the gateway leg.
//
// The round-43 split rule gave a repeated id a synthetic key so two index-less
// calls would stay two calls (B43-3). B44-1: the blocks the split minted then
// inherited the repeated id anyway, so the client was handed two tool_use
// blocks that answer to one id — one tool_result for two calls.
//
// B44-2 (the gateway's half of C44-1): a call whose arguments never parsed, on
// a turn the upstream truncated, was still emitted with a fabricated
// input:{} — the client leg drops that fragment on both of its paths and
// reports max_tokens, so the two legs disagreed about whether a call happened.
//
// B44-3: a document that says nothing was adopted as a successful turn on the
// non-stream path (a fabricated empty text block) while the stream path
// answered 502 — the same upstream body read two ways.
//
// B44-4: a body whose only message was a blank system message reached the
// upstream as "messages":[].
//
// B44-5: documentSaysSomething counted a call that named nothing, so a
// document whose only call was unnamed was booked a success, bypassing the
// 502 the round-42 rule put there.
//
// C44-5 and C44-6 are the cross-leg agreements: unparseable arguments that are
// NOT truncated answer {"_raw": …} as the client leg does, and a document or
// search_result block ends with a newline as it does on the local leg.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const (
	round44AskStream = `{"model":"kat-awq","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"go"}]}`
	round44AskPlain  = `{"model":"kat-awq","max_tokens":64,"stream":false,"messages":[{"role":"user","content":"go"}]}`
	round44AskBlank  = `{"model":"kat-awq","max_tokens":8,"stream":true,"messages":[{"role":"system","content":""}]}`
)

// round44Capture is an upstream that records the request body it was sent.
type round44Capture struct {
	mu   sync.Mutex
	body string
}

func (c *round44Capture) take() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.body
}

// round44Upstream serves one fixed body and records what was asked of it.
func round44Upstream(t *testing.T, cap *round44Capture, ct, body string) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if cap != nil {
			cap.mu.Lock()
			cap.body = string(b)
			cap.mu.Unlock()
		}
		w.Header().Set("Content-Type", ct)
		io.WriteString(w, body)
	}))
	t.Cleanup(up.Close)
	return up
}

// round44Raw posts an Anthropic body and reports the status and the whole
// response, which the stream tests need in order to read the message_delta.
func round44Raw(t *testing.T, srv *httptest.Server, body string) (int, string) {
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
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(out)
}

// round44Run answers one request from one fixed upstream body.
func round44Run(t *testing.T, ct, upstreamBody, ask string) (int, string) {
	t.Helper()
	up := round44Upstream(t, nil, ct, upstreamBody)
	srv, _ := round39Gateway(t, up, nil)
	return round44Raw(t, srv, ask)
}

// round44ToolUseIDs returns the id of every tool_use content_block_start in a
// stream, in the order the client reads them.
func round44ToolUseIDs(t *testing.T, stream string) []string {
	t.Helper()
	var ids []string
	for _, line := range strings.Split(stream, "\n") {
		body, ok := strings.CutPrefix(strings.TrimSpace(line), "data: ")
		if !ok {
			continue
		}
		var event struct {
			Type         string `json:"type"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
			} `json:"content_block"`
		}
		if json.Unmarshal([]byte(body), &event) != nil {
			continue
		}
		if event.Type == "content_block_start" && event.ContentBlock.Type == "tool_use" {
			ids = append(ids, event.ContentBlock.ID)
		}
	}
	return ids
}

// --- B44-1 -----------------------------------------------------------------

// TestTwoSplitCallsDoNotShareAnID is B44-1. The split rule mints a fresh key
// for the block it opens; that block must not then adopt the id the fragment
// repeated, which another block already carries.
func TestTwoSplitCallsDoNotShareAnID(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}]}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"file\":\"a\"}"}}]}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"choices":[{"index":0,"finish_reason":"tool_calls","delta":{}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	srv, _ := round39Gateway(t, upstream, nil)
	stream := round36Stream(t, srv, round39Ask)

	ids := round44ToolUseIDs(t, stream)
	if len(ids) != 2 {
		t.Fatalf("two index-less calls sharing one id produced %d tool_use block(s) (ids %v):\n%s", len(ids), ids, stream)
	}
	if ids[0] == "" || ids[1] == "" {
		t.Errorf("a tool_use block reached the client with no id at all (ids %v): Claude Code answers a call by its id, so a block without one can never be answered:\n%s", ids, stream)
	}
	if ids[0] == ids[1] {
		t.Errorf("both tool_use blocks carry the id %q: the block the split rule minted inherited the id the fragment repeated, so one tool_result answers two calls and the OpenAI wire cannot key two tool_results by one tool_call_id:\n%s", ids[0], stream)
	}
}

// --- B44-2 and C44-1 -------------------------------------------------------

const (
	// A call whose arguments never parse, on a turn the upstream truncated.
	round44TruncatedCall = `{"choices":[{"finish_reason":"length","message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls"}}]}}],"usage":{"prompt_tokens":12,"completion_tokens":3}}`
	// The same call on a turn that finished normally.
	round44UnparseableCall = `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"Bash","arguments":"not json at all"}}]}}],"usage":{"prompt_tokens":12,"completion_tokens":3}}`
)

// TestATruncatedUnparseableCallIsNotACall is B44-2 on the non-stream path: no
// fabricated input, and no tool_use.
func TestATruncatedUnparseableCallIsNotACall(t *testing.T) {
	status, body := round44Run(t, "application/json", round44TruncatedCall, round44AskPlain)
	if status != http.StatusOK {
		t.Fatalf("status %d, body:\n%s", status, body)
	}
	if strings.Contains(body, `"tool_use"`) {
		t.Errorf("a call whose arguments never parsed on a truncated turn was reported as tool_use:\n%s\nthe client leg drops that fragment on both of its paths and reports max_tokens, so a tool_use here tells Claude Code to wait for a call the other leg never delivered", body)
	}
	if !strings.Contains(body, `"max_tokens"`) {
		t.Errorf("the truncated turn's stop_reason is not max_tokens:\n%s", body)
	}
	if strings.Contains(body, `"input":{}`) {
		t.Errorf("the unparseable call was emitted with a fabricated empty input:\n%s\nthe model never wrote that, and a parser handed {} will run the call anyway", body)
	}
	if !strings.Contains(body, `"content":[]`) {
		t.Errorf("the turn does not carry an empty content list:\n%s", body)
	}
}

// TestATruncatedUnparseableCallDoesNotClaimATool is B44-2 on the stream path.
// The block is already on the wire and cannot be un-sent, so the verdict is
// what has to be right.
func TestATruncatedUnparseableCallDoesNotClaimATool(t *testing.T) {
	status, body := round44Run(t, "text/event-stream", "data: "+round44TruncatedCall+"\n\ndata: [DONE]\n\n", round44AskStream)
	if status != http.StatusOK {
		t.Fatalf("status %d, body:\n%s", status, body)
	}
	if !strings.Contains(body, `"stop_reason":"max_tokens"`) {
		t.Errorf("the stream's stop_reason is not max_tokens:\n%s", body)
	}
}

// TestAnUnparseableCallThatIsNotTruncatedKeepsItsRawArguments is C44-5: the
// client leg hands an upstream parser {"_raw": …} for this body rather than an
// empty object, and the two legs must say the same thing.
func TestAnUnparseableCallThatIsNotTruncatedKeepsItsRawArguments(t *testing.T) {
	status, body := round44Run(t, "application/json", round44UnparseableCall, round44AskPlain)
	if status != http.StatusOK {
		t.Fatalf("status %d, body:\n%s", status, body)
	}
	if !strings.Contains(body, `"tool_use"`) {
		t.Errorf("a call on a turn that finished normally was not reported as tool_use:\n%s", body)
	}
	if !strings.Contains(body, `"_raw":"not json at all"`) {
		t.Errorf("the unparseable arguments are not kept under _raw:\n%s\nthe client leg answers this body with {\"_raw\": …}, and an upstream parser handed {} would run a call whose arguments were never understood", body)
	}
}

// --- B44-3 -----------------------------------------------------------------

// TestAnEmptyCompletionIsRefusedOnBothPaths is B44-3: one upstream body, two
// paths, one answer.
func TestAnEmptyCompletionIsRefusedOnBothPaths(t *testing.T) {
	const emptyDoc = `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":""}}],"usage":{"prompt_tokens":12,"completion_tokens":0}}`

	t.Run("non-stream", func(t *testing.T) {
		status, body := round44Run(t, "application/json", emptyDoc, round44AskPlain)
		if status != http.StatusBadGateway {
			t.Errorf("a whole document that says nothing answered %d, want 502:\n%s\nthe stream path answers 502 for the same body, and a 200 carrying a fabricated empty text block books a metered turn the model never wrote", status, body)
		}
	})

	t.Run("stream", func(t *testing.T) {
		status, body := round44Run(t, "application/json", emptyDoc, round44AskStream)
		if status != http.StatusBadGateway {
			t.Errorf("a whole document that says nothing answered %d on the stream path, want 502:\n%s", status, body)
		}
	})
}

// --- B44-4 -----------------------------------------------------------------

// TestABlankSystemMessageStillLeavesATurn is B44-4: the upstream must never be
// asked with an empty messages array.
func TestABlankSystemMessageStillLeavesATurn(t *testing.T) {
	cap := &round44Capture{}
	up := round44Upstream(t, cap, "text/event-stream",
		`data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}`+"\n\n"+
			`data: {"choices":[{"index":0,"finish_reason":"stop","delta":{}}]}`+"\n\n"+
			"data: [DONE]\n\n")
	srv, _ := round39Gateway(t, up, nil)

	if status, body := round44Raw(t, srv, round44AskBlank); status != http.StatusOK {
		t.Fatalf("status %d, body:\n%s", status, body)
	}
	sent := cap.take()
	if strings.Contains(sent, `"messages":[]`) {
		t.Errorf("the upstream was asked with an empty messages array:\n%s\na body whose only message is a blank system message still has a turn to answer, and a strict template raises on an empty conversation", sent)
	}
	if !strings.Contains(sent, `"messages":[`) {
		t.Errorf("the upstream body carries no messages key at all:\n%s", sent)
	}
}

// --- B44-5 -----------------------------------------------------------------

// TestADocumentWhoseOnlyCallIsUnnamedSaysNothing is B44-5: a call with no name
// is not something the client can act on, so it does not make a document an
// answer.
func TestADocumentWhoseOnlyCallIsUnnamedSaysNothing(t *testing.T) {
	const unnamedCall = `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"","arguments":""}}]}}],"usage":{"prompt_tokens":12,"completion_tokens":0}}`

	t.Run("stream", func(t *testing.T) {
		status, body := round44Run(t, "application/json", unnamedCall, round44AskStream)
		if status != http.StatusBadGateway {
			t.Errorf("a document whose only call names nothing answered %d, want 502:\n%s\na call the client cannot dispatch is not an answer, and reporting success books a metered turn for a stream that relayed nothing", status, body)
		}
	})

	t.Run("non-stream", func(t *testing.T) {
		status, body := round44Run(t, "application/json", unnamedCall, round44AskPlain)
		if status != http.StatusBadGateway {
			t.Errorf("a document whose only call names nothing answered %d on the non-stream path, want 502:\n%s", status, body)
		}
	})
}

// --- C44-2, C44-3 and C44-6 -------------------------------------------------

// TestAContentlessAnthropicTurnReachesTheUpstream is C44-2 and C44-3 on this
// leg, where the padding already lived: the turn is kept, with its own role.
func TestAContentlessAnthropicTurnReachesTheUpstream(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{
			name: "an empty content array",
			body: `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"a"},{"role":"user","content":[]},{"role":"user","content":"b"}]}`,
			want: `"messages":[{"content":"a","role":"user"},{"content":"","role":"user"},{"content":"b","role":"user"}]`,
		},
		{
			name: "a replayed redacted thinking turn",
			body: `{"model":"m","max_tokens":8,"messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"AAAA"}]},{"role":"user","content":"q"}]}`,
			want: `"messages":[{"content":"","role":"assistant"},{"content":"q","role":"user"}]`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := round43Convert(t, tc.body)
			if err != "" {
				t.Fatalf("the body was refused: %s", err)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("the body converted to %s, want it to contain %s: the local leg pads the same turn with its own role, and rebuilding a contentless assistant turn as a user turn puts two user turns in a row", out, tc.want)
			}
		})
	}
}

// TestAWhitespacePromptIsNotCollapsed is the whitespace half of B44-4: the
// collapse above replaces a conversation that carries nothing, and a text made
// of whitespace is not nothing — the local leg collapsed this body too, and its
// calibration unit measured 66 bytes for a hundred-kilobyte prompt.
func TestAWhitespacePromptIsNotCollapsed(t *testing.T) {
	body := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"` +
		strings.Repeat(`\n`, 1000) + `"}]}`
	out, err := round43Convert(t, body)
	if err != "" {
		t.Fatalf("the body was refused: %s", err)
	}
	if strings.Contains(out, `"content":""`) || !strings.Contains(out, `"role":"user"`) {
		t.Errorf("a turn whose text is only whitespace converted to %s: the upstream must be asked the question the client wrote — collapsing it here asks a bare empty turn instead, and the two other legs keep the text", out)
	}
}

// TestAWhitespaceCompletionIsStillAnAnswer is the completion half: relayDelta
// relays a content of whitespace as a text block, so a document holding one is
// an answer and not the empty completion the 502 above is for.
func TestAWhitespaceCompletionIsStillAnAnswer(t *testing.T) {
	const spaces = `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"   "}}],"usage":{"prompt_tokens":12,"completion_tokens":1}}`
	for _, tc := range []struct {
		name, path, ask string
	}{
		{"non-stream", "application/json", round44AskPlain},
		{"stream", "application/json", round44AskStream},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := round44Run(t, tc.path, spaces, tc.ask)
			if status != http.StatusOK {
				t.Errorf("a completion whose content is whitespace answered %d, want 200:\n%s\nthe stream relays that content as a text block, so it is an answer — refusing it would make the two decisions about one document disagree, and the other two legs serve it", status, body)
			}
		})
	}
}

// TestADocumentBlockEndsWithANewline is C44-6: the local leg terminates the
// block it renders, so a document followed by a text part does not glue the two
// sentences together.
func TestADocumentBlockEndsWithANewline(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{
			name: "a document alone",
			body: `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"text","data":"D"}}]}]}`,
			want: `"content":"D\n"`,
		},
		{
			name: "a search_result alone",
			body: `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[{"type":"search_result","title":"T","source":"https://e.com","content":[{"type":"text","text":"p"}]}]}]}`,
			want: `"content":"T\nhttps://e.com\np\n"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := round43Convert(t, tc.body)
			if err != "" {
				t.Fatalf("the body was refused: %s", err)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("the body converted to %s, want it to contain %s: the local leg ends the rendered block with a newline, and the two legs must render the same document the same way", out, tc.want)
			}
		})
	}
}
