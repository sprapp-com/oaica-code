package main

// round41_gateway_body_and_cache_integrity_test.go — round 41's half of the
// gateway's contract, one test per finding, plus the cross-leg divergences
// auditor C found in this leg:
//
//   - B41-1: the stated cache hit was clamped down to a stated prompt inside
//     cachedTokens(), which ran BEFORE the ledger row's own raise — the raise
//     was unreachable for exactly the shape it was written for, so the client
//     read 5000 while the row for the same turn recorded 1000.
//   - B41-2: the estimate for an answer adopted from a whole completion counted
//     only the message's content, so a tool-only call (or an answer carrying
//     reasoning) was booked at output_tokens:0 while the client was told the
//     real size.
//   - B41-3: the two spellings of the cache hit were resolved last-writer-wins,
//     so a stream that stated the hit as details in one chunk and as the
//     sibling field in another reported 900 to the ledger and 100 to the
//     client.
//   - B41-4: a url-sourced image was charged the allowance AND its own address
//     text, so the same body measured larger on this leg than on the other two.
//   - C41-2: tool_choice "none" was ignored — every tool was forwarded and the
//     model called tools the client had just ruled out.
//   - C41-3: thinking/output_config were dropped outright, so a client asking
//     for thinking OFF had it defaulted on.
//   - C41-5: a non-object element in a content array was skipped and the turn
//     answered, where the other leg refuses the same body.
//   - C41-6: a tool_use with no id or no name went on the wire as "".
//   - C41-8: top_k was dropped, max_tokens absent was answered with an invented
//     4096, and a stated null temperature was forwarded.
//   - C41-10: a bare element in a search_result's passage list was marshalled
//     into the prompt as a passage the search never returned.
//   - C41-11: system blocks were joined with one newline, gluing an appended
//     instruction to the one before it.
//   - C41-12: sibling text blocks were sent as separate parts, so each backend
//     chose its own separator for the same prompt.
//   - C41-14a: roles were forwarded with whatever spelling the client used.
//   - A41-5: a url source whose text is not url-shaped was forwarded to the
//     backend as an address to fetch.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// round41Body posts one /v1/messages body through a gateway whose upstream
// replies with one whole completion, and returns the converted request the
// upstream was sent (nil when the gateway refused the body) beside the client's
// status. It is how the body-conversion findings are pinned: what matters is
// the request that reached the backend, not the answer.
func round41Body(t *testing.T, body, reply string) (map[string]any, int) {
	t.Helper()
	sent := make(chan map[string]any, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req map[string]any
		if json.Unmarshal(raw, &req) == nil {
			select {
			case sent <- req:
			default:
			}
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, reply)
	}))
	defer upstream.Close()

	srv, _ := round39Gateway(t, upstream, nil)
	status, _ := round40Post(t, srv, body)
	select {
	case req := <-sent:
		return req, status
	default:
		return nil, status
	}
}

const round41Reply = `{"id":"c1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":9,"completion_tokens":2}}`

// TestAStatedCacheHitAboveTheStatedPromptReachesTheRow is B41-1. The hit the
// upstream stated is evidence about the prompt, so the prompt total is raised
// to it — clamped down inside cachedTokens() first, the ledger row's raise was
// unreachable and the two records of one request disagreed by 4000 tokens.
func TestAStatedCacheHitAboveTheStatedPromptReachesTheRow(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"an answer"}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":5000}}}`+"\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	srv, ledger := round39Gateway(t, upstream, nil)
	status, stream := round40Post(t, srv, round39Ask)
	if status != http.StatusOK {
		t.Fatalf("premise: status %d (%s)", status, stream)
	}
	in, cached, _ := round40Delta(t, stream)
	if cached != 5000 || in != 0 {
		t.Errorf("the client was told input_tokens=%d cache_read_input_tokens=%d for a stated hit of 5000 with a stated prompt of 1000: the hit is a measurement of the prompt and the prompt total is raised to it", in, cached)
	}
	rows := waitLedger(t, ledger, 1)
	if len(rows) == 0 {
		t.Fatal("no ledger row")
	}
	if rows[0].PromptTokens != in+cached {
		t.Errorf("the ledger row for this turn records prompt_tokens=%d while the client was told %d: the two records of one request must agree, and a row below its own cached_tokens records a fact that cannot hold", rows[0].PromptTokens, in+cached)
	}
	if rows[0].CachedTokens != cached {
		t.Errorf("the ledger row records cached_tokens=%d where the client read cache_read_input_tokens=%d", rows[0].CachedTokens, cached)
	}
}

// TestAnAdoptedAnswersEstimateMatchesWhatTheClientIsTold is B41-2, both shapes
// a whole completion can arrive in: a tool-only call, and an answer carrying
// reasoning beside its content. The estimate is the fallback the client is told
// when the upstream states no completion count, so the ledger row and the
// client's output_tokens have to be the same number — and both have to count
// what was actually RELAYED.
func TestAnAdoptedAnswersEstimateMatchesWhatTheClientIsTold(t *testing.T) {
	for _, tc := range []struct {
		name, reply string
	}{
		{
			"tool-only",
			`{"id":"c1","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"the meaning of life and everything else\"}"}}]}}],"usage":{"prompt_tokens":12}}`,
		},
		{
			"content-and-reasoning",
			`{"id":"c1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"a short answer","reasoning":"thinking hard about it for a while"}}],"usage":{"prompt_tokens":12}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := round40JSONReply(tc.reply)
			defer upstream.Close()

			srv, ledger := round39Gateway(t, upstream, nil)
			status, stream := round40Post(t, srv, round39Ask)
			if status != http.StatusOK {
				t.Fatalf("premise: status %d (%s)", status, stream)
			}
			_, _, out := round40Delta(t, stream)
			if out <= 0 {
				t.Errorf("the client was told output_tokens=%d for an answer %s: the relayed answer is the whole of the message that was adopted, not its content field alone", out, tc.name)
			}
			rows := waitLedger(t, ledger, 1)
			if len(rows) == 0 {
				t.Fatal("no ledger row")
			}
			if rows[0].CompletionTokens != out {
				t.Errorf("the client was told output_tokens=%d and the ledger books completion_tokens=%d for one adopted answer", out, rows[0].CompletionTokens)
			}
		})
	}
}

// TestOneStreamsTwoCacheSpellingsAgree is B41-3. The two spellings are resolved
// the way the recorder resolves them — details over the sibling, each keeping
// its own last positive statement — rather than last-writer-wins, which told
// the client 100 for a stream whose own last word was 100 beside an earlier
// stated 900.
func TestOneStreamsTwoCacheSpellingsAgree(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"an answer"}}],"usage":{"prompt_tokens":1000,"prompt_tokens_details":{"cached_tokens":900}}}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"choices":[],"usage":{"prompt_cache_hit_tokens":100}}`+"\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	srv, ledger := round39Gateway(t, upstream, nil)
	status, stream := round40Post(t, srv, round39Ask)
	if status != http.StatusOK {
		t.Fatalf("premise: status %d (%s)", status, stream)
	}
	_, cached, _ := round40Delta(t, stream)
	rows := waitLedger(t, ledger, 1)
	if len(rows) == 0 {
		t.Fatal("no ledger row")
	}
	if rows[0].CachedTokens != cached {
		t.Errorf("the client read cache_read_input_tokens=%d and the ledger recorded cached_tokens=%d for one upstream stream: the two spellings of one hit are resolved once, the same way on both records", cached, rows[0].CachedTokens)
	}
}

// TestAURLImageIsChargedTheAllowanceAlone is B41-4. All three prompt measures
// of this product replace an inline image by its allowance; charging the
// address text on top of it made one body measure differently by the leg that
// served it, and the estimate is what the client's context meter is told.
func TestAURLImageIsChargedTheAllowanceAlone(t *testing.T) {
	url := "https://cdn.example.test/very/long/signed/path/to/a/screenshot.png?X-Amz-Signature=deadbeefdeadbeefdeadbeefdeadbeef"
	body := func(img string) string {
		return `{"model":"kat-awq","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"` + img + `"}}]}]}`
	}
	measure := func(b string) int {
		var v map[string]any
		if err := json.Unmarshal([]byte(b), &v); err != nil {
			t.Fatal(err)
		}
		return messagesBytes(v)
	}
	base := measure(body(""))
	withURL := measure(body(url))
	if delta := withURL - base; delta != imagePartByteAllowance {
		t.Errorf("a url-sourced image is charged %d bytes here (url text %d bytes): the allowance is %d, and the other two measures of this product charge that alone", delta, len(url), imagePartByteAllowance)
	}
}

// TestToolChoiceNoneSendsNoTools is C41-2. "none" means the model is given no
// tool surface; forwarding all of them let the model call tools the client had
// ruled out, where the sibling converter drops every definition for the same
// body.
func TestToolChoiceNoneSendsNoTools(t *testing.T) {
	body := `{"model":"kat-awq","max_tokens":64,"tool_choice":{"type":"none"},"tools":[{"name":"Bash","description":"run","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"go"}]}`
	req, status := round41Body(t, body, round41Reply)
	if status != http.StatusOK {
		t.Fatalf("premise: the gateway refused the body (%d)", status)
	}
	if _, ok := req["tools"]; ok {
		t.Errorf("tool_choice \"none\" still sent the tool definitions: %v", req["tools"])
	}
	if _, ok := req["tool_choice"]; ok {
		t.Errorf("tool_choice \"none\" was rendered as %v on the OpenAI wire", req["tool_choice"])
	}
}

// TestTheThinkingControlReachesTheBackend is C41-3: a client that states a
// thinking switch, or an effort level, has it carried to the backend in the
// spelling this product's own server uses for the same control
// (chat_template_kwargs, llm.llamaServerChatTemplateKwargs). Dropped, a client
// asking for thinking OFF had it defaulted on and paid for reasoning it had
// ruled out.
func TestTheThinkingControlReachesTheBackend(t *testing.T) {
	ask := func(extra string) map[string]any {
		t.Helper()
		req, status := round41Body(t, `{"model":"kat-awq","max_tokens":64,`+extra+`"messages":[{"role":"user","content":"go"}]}`, round41Reply)
		if status != http.StatusOK {
			t.Fatalf("premise: refused (%d) for %s", status, extra)
		}
		kw, _ := req["chat_template_kwargs"].(map[string]any)
		return kw
	}

	if kw := ask(`"thinking":{"type":"disabled"},`); kw == nil || kw["enable_thinking"] != false {
		t.Errorf("thinking disabled reached the backend as %v: the model reasons when the client said not to", kw)
	}
	if kw := ask(`"thinking":{"type":"enabled"},`); kw == nil || kw["enable_thinking"] != true {
		t.Errorf("thinking enabled reached the backend as %v", kw)
	}
	if kw := ask(`"output_config":{"effort":"high"},`); kw == nil || kw["enable_thinking"] != true || kw["reasoning_effort"] != "high" {
		t.Errorf("an effort level reached the backend as %v", kw)
	}
	// The switch takes precedence over the effort, as it does in the sibling
	// converter: a body carrying both is a switch and no level.
	if kw := ask(`"thinking":{"type":"disabled"},"output_config":{"effort":"high"},`); kw == nil || kw["enable_thinking"] != false || kw["reasoning_effort"] != nil {
		t.Errorf("a body stating thinking disabled AND an effort reached the backend as %v", kw)
	}
	if kw := ask(``); kw != nil {
		t.Errorf("a body that stated no thinking control was sent %v: silence is not a statement", kw)
	}
}

// TestTheSamplingControlsReachTheBackend is C41-8 and C41-14b: top_k is carried
// by the sibling converter and was dropped here, and a stated null is not a
// measurement — forwarded, the backend read it as 0 and decoded greedily for a
// client that meant "unset".
func TestTheSamplingControlsReachTheBackend(t *testing.T) {
	req, status := round41Body(t, `{"model":"kat-awq","max_tokens":64,"top_k":40,"temperature":null,"top_p":null,"messages":[{"role":"user","content":"go"}]}`, round41Reply)
	if status != http.StatusOK {
		t.Fatalf("premise: refused (%d)", status)
	}
	if req["top_k"] != float64(40) {
		t.Errorf("top_k reached the backend as %v, want 40 (the sibling converter carries it as options[\"top_k\"])", req["top_k"])
	}
	for _, k := range []string{"temperature", "top_p"} {
		if v, ok := req[k]; ok {
			t.Errorf("%s was forwarded as %v: a JSON null is not a statement, and the backend reads it as 0", k, v)
		}
	}

	// max_tokens is required on this wire: refused as the sibling handler
	// refuses it, rather than answered with an invented 4096.
	_, status = round41Body(t, `{"model":"kat-awq","messages":[{"role":"user","content":"go"}]}`, round41Reply)
	if status == http.StatusOK {
		t.Errorf("a body with no max_tokens was answered: the wire requires it, and the cap this leg invented was not the client's")
	}
	_, status = round41Body(t, `{"model":"kat-awq","max_tokens":0,"messages":[{"role":"user","content":"go"}]}`, round41Reply)
	if status == http.StatusOK {
		t.Errorf("max_tokens:0 was answered: the sibling handler refuses a non-positive cap")
	}
}

// TestUnrepresentableElementsAreRefusedByName is C41-5, C41-6, C41-14a and
// C41-14d: the sibling converter refuses each of these in words and this leg
// answered the turn without them, so the model was asked about a conversation
// it had not been given.
func TestUnrepresentableElementsAreRefusedByName(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{
			"non-object content element",
			`{"model":"kat-awq","max_tokens":64,"messages":[{"role":"user","content":["stray-string","after"]}]}`,
		},
		{
			"non-object message",
			`{"model":"kat-awq","max_tokens":64,"messages":["stray-string"]}`,
		},
		{
			"tool_use with no id",
			`{"model":"kat-awq","max_tokens":64,"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{}}]}]}`,
		},
		{
			"tool_use with no name",
			`{"model":"kat-awq","max_tokens":64,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_1","input":{}}]}]}`,
		},
		{
			"url source that is not url-shaped",
			`{"model":"kat-awq","max_tokens":64,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"example.com/shot.png"}}]}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, status := round41Body(t, tc.body, round41Reply)
			if status == http.StatusOK {
				t.Errorf("the gateway answered this body (converted request: %v): the model was asked about content the client sent and it never received", req)
			}
		})
	}
}

// TestASchemeLessURLSourceIsRefusedAndARoleIsLowerCased is C41-14a beside the
// refusal: the role spelling is the sibling's, so two backends see one spelling
// of one body.
func TestARoleIsForwardedLowerCased(t *testing.T) {
	req, status := round41Body(t, `{"model":"kat-awq","max_tokens":64,"messages":[{"role":"User","content":"go"}]}`, round41Reply)
	if status != http.StatusOK {
		t.Fatalf("premise: refused (%d)", status)
	}
	msgs, _ := req["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatalf("no messages reached the backend: %v", req)
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "user" {
		t.Errorf("the role reached the backend as %v, where the sibling converter sends \"user\"", first["role"])
	}
}

// TestSystemBlocksAreJoinedWithABlankLine is C41-11. A system array is how an
// appended instruction arrives; joined with one newline it was glued to the
// instruction before it whenever that did not end in punctuation.
func TestSystemBlocksAreJoinedWithABlankLine(t *testing.T) {
	body := `{"model":"kat-awq","max_tokens":64,"system":[{"type":"text","text":"You are a helpful assistant"},{"type":"text","text":"Always answer in French"}],"messages":[{"role":"user","content":"go"}]}`
	req, status := round41Body(t, body, round41Reply)
	if status != http.StatusOK {
		t.Fatalf("premise: refused (%d)", status)
	}
	msgs, _ := req["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatalf("no messages reached the backend: %v", req)
	}
	first, _ := msgs[0].(map[string]any)
	got, _ := first["content"].(string)
	if got != "You are a helpful assistant\n\nAlways answer in French" {
		t.Errorf("the system blocks reached the backend as %q: the sibling converter separates them with a blank line, and one newline reads the two instructions as one sentence", got)
	}
}

// TestSiblingTextBlocksBecomeOneString is C41-12: two text blocks of one
// message are assembled into one string with the sibling's blank line, so every
// backend tokenizes the same prompt rather than choosing its own separator for
// two parts.
func TestSiblingTextBlocksBecomeOneString(t *testing.T) {
	body := `{"model":"kat-awq","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}]}`
	req, status := round41Body(t, body, round41Reply)
	if status != http.StatusOK {
		t.Fatalf("premise: refused (%d)", status)
	}
	msgs, _ := req["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatalf("no messages reached the backend: %v", req)
	}
	first, _ := msgs[0].(map[string]any)
	if got, ok := first["content"].(string); !ok || got != "a\n\nb" {
		t.Errorf("two sibling text blocks reached the backend as %v, want the single string \"a\\n\\nb\" (the sibling converter's assembly)", first["content"])
	}
}

// TestABarePassageElementIsNotAPassage is C41-10: a passage list holds blocks,
// and marshalling a bare element put the literal `"x"` — quotes and all — in
// the prompt as though the search had returned it.
func TestABarePassageElementIsNotAPassage(t *testing.T) {
	body := `{"model":"kat-awq","max_tokens":64,"messages":[{"role":"user","content":[{"type":"search_result","title":"t","source":"https://e.test","content":["x",{"type":"text","text":"real passage"}]}]}]}`
	req, status := round41Body(t, body, round41Reply)
	if status != http.StatusOK {
		t.Fatalf("premise: refused (%d)", status)
	}
	raw, _ := json.Marshal(req["messages"])
	if strings.Contains(string(raw), `\"x\"`) || strings.Contains(string(raw), `"x"`) {
		t.Errorf("the bare element was carried into the prompt:\n%s", raw)
	}
	if !strings.Contains(string(raw), "real passage") {
		t.Errorf("the real passage was lost:\n%s", raw)
	}
}
