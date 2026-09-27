package main

// round40_ledger_estimate_and_bridge_integrity_test.go — round 40's half of the
// gateway's contract, one test per finding:
//
//   - B40-1: the estimate was substituted into the ledger row for a turn the
//     client was told FAILED, booking tokens and cost for an answer nobody
//     received.
//   - B40-2: the substitution replaced a STATED count with the estimate, so a
//     served turn's row said counts the upstream never sent.
//   - B40-3: an answer adopted from a whole completion was relayed in full and
//     then closed with output_tokens:0, because the adopted bytes were never
//     counted.
//   - B40-4: narration held behind a tool call came out in the order it was
//     unblocked rather than the order it was written, so the client read the
//     model's prose reversed.
//   - B40-5: the sentence for an image a tool_result could not show was folded
//     in only when the result carried no text, so the mixed message — the one
//     that matters — lost the notice.
//   - A40-3: a stated cache hit over an unstated prompt reached the client but
//     not the ledger row, so the two records of one request disagreed.
//   - A40-4: a non-object element in a tool_result's content was marshalled
//     into the prompt, where the client leg refuses the same body in words.
//   - A40-5: a base64 payload that decodes to a URL was forwarded as an image,
//     where the client leg refuses it — one body, two answers.
//   - A40-9: a url-sourced image was charged nothing by this leg's prompt
//     measure and the allowance by the other two.
//   - C40-4: the prompt estimate measured the JSON ESCAPING of the body rather
//     than what the upstream decodes, so a markup-heavy turn was estimated up
//     to 6x its real size.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// round40Post posts one /v1/messages body and returns the client's status
// beside everything it was sent.
func round40Post(t *testing.T, srv *httptest.Server, body string) (int, string) {
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
		t.Fatalf("reading the body (status %d): %v", resp.StatusCode, err)
	}
	return resp.StatusCode, string(out)
}

// round40Delta returns the client's closing message_delta event for a stream,
// parsed, and the raw stream when it is absent.
func round40Delta(t *testing.T, stream string) (int, int, int) {
	t.Helper()
	line := ""
	for _, l := range strings.Split(stream, "\n") {
		if strings.Contains(l, `"type":"message_delta"`) {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no message_delta reached the client:\n%s", stream)
	}
	var event struct {
		Usage struct {
			InputTokens          int `json:"input_tokens"`
			CacheReadInputTokens int `json:"cache_read_input_tokens"`
			OutputTokens         int `json:"output_tokens"`
		} `json:"usage"`
	}
	body := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
	if err := json.Unmarshal([]byte(body), &event); err != nil {
		t.Fatalf("message_delta is not JSON (%v): %s", err, line)
	}
	return event.Usage.InputTokens, event.Usage.CacheReadInputTokens, event.Usage.OutputTokens
}

// round40JSONReply answers every request with one non-stream completion.
func round40JSONReply(body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}))
}

// TestARefusedTurnIsNotBookedWithAnEstimate is B40-1. A turn the upstream
// refused was never served and never billed; booking it with the prompt
// estimate and a positive cost charged the overage accounting for an answer
// nobody received.
func TestARefusedTurnIsNotBookedWithAnEstimate(t *testing.T) {
	upstream := round40JSONReply(`{"error":{"message":"rate limited by the box","type":"rate_limit_error"}}`)
	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"message":"rate limited by the box","type":"rate_limit_error"}}`)
	})
	defer upstream.Close()

	srv, ledger := round39Gateway(t, upstream, nil)
	status, _ := round40Post(t, srv, `{"model":"kat-awq","max_tokens":64,"messages":[{"role":"user","content":"go"}]}`)
	if status != http.StatusTooManyRequests {
		t.Fatalf("premise: the client saw %d, want the upstream's 429", status)
	}

	rows := waitLedger(t, ledger, 1)
	if len(rows) == 0 {
		t.Fatal("no ledger row for the refused turn")
	}
	row := rows[0]
	if row.PromptTokens != 0 || row.CompletionTokens != 0 {
		t.Errorf("the row for a turn the upstream REFUSED records prompt=%d completion=%d: the recorder reads the upstream's own bytes, and this turn carried none — the estimate is this gateway's reading of a turn nobody was served, and booking it bills the overage accounting for an answer that never existed", row.PromptTokens, row.CompletionTokens)
	}
	if row.CostUSD != 0 {
		t.Errorf("the row for a refused turn carries cost_usd=%v, want 0", row.CostUSD)
	}
}

// TestAStatedPromptIsNotReplacedByTheEstimate is B40-2, first direction: an
// upstream that reports its prompt and not its answer must keep the prompt it
// stated, with the estimate filling only the field it was silent about.
func TestAStatedPromptIsNotReplacedByTheEstimate(t *testing.T) {
	upstream := round40JSONReply(`{"id":"chatcmpl-1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hello there friend"}}],"usage":{"prompt_tokens":12}}`)
	defer upstream.Close()

	srv, ledger := round39Gateway(t, upstream, nil)
	status, _ := round40Post(t, srv, `{"model":"kat-awq","max_tokens":64,"messages":[{"role":"user","content":"go"}]}`)
	if status != http.StatusOK {
		t.Fatalf("premise: status = %d", status)
	}

	rows := waitLedger(t, ledger, 1)
	if len(rows) == 0 {
		t.Fatal("no ledger row for the served turn")
	}
	row := rows[0]
	if row.PromptTokens != 12 {
		t.Errorf("the row records prompt_tokens=%d for a turn whose upstream stated 12: the gateway's own estimate replaced a count the upstream sent, so the one record kept of the request disagrees with the client's", row.PromptTokens)
	}
	if row.CompletionTokens <= 0 {
		t.Errorf("the row records completion_tokens=%d: the upstream said nothing about the answer, and the client was told the estimate for the same field — the two records of one request disagree", row.CompletionTokens)
	}
}

// TestASilentPromptIsFilledBesideAStatedCompletion is B40-2, the other
// direction: the answer is stated, the prompt is not.
func TestASilentPromptIsFilledBesideAStatedCompletion(t *testing.T) {
	upstream := round40JSONReply(`{"id":"chatcmpl-1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"completion_tokens":3}}`)
	defer upstream.Close()

	srv, ledger := round39Gateway(t, upstream, nil)
	status, stream := round40Post(t, srv, `{"model":"kat-awq","max_tokens":64,"messages":[{"role":"user","content":"go"}]}`)
	if status != http.StatusOK {
		t.Fatalf("premise: status = %d (%s)", status, stream)
	}

	rows := waitLedger(t, ledger, 1)
	if len(rows) == 0 {
		t.Fatal("no ledger row for the served turn")
	}
	row := rows[0]
	if row.CompletionTokens != 3 {
		t.Errorf("the row records completion_tokens=%d for a turn whose upstream stated 3", row.CompletionTokens)
	}
	if row.PromptTokens <= 0 {
		t.Errorf("the row records prompt_tokens=%d: the prompt was silent and nothing filled it, so a served turn is booked as carrying no prompt", row.PromptTokens)
	}
}

// TestAStatedCacheHitReachesTheLedgerAndTheClient is A40-3: the hit the client
// is told must be the hit the ledger books. Reported against a prompt this
// gateway had to work out for itself, the client read a cache read the row for
// the same turn did not have.
func TestAStatedCacheHitReachesTheLedgerAndTheClient(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"an answer"}}]}`+"\n\n")
		f.Flush()
		// The closing chunk states the hit and not the prompt size — the shape
		// mergeUsage preserves and promptSplit reports against the estimate.
		io.WriteString(w, `data: {"choices":[],"usage":{"prompt_cache_hit_tokens":900}}`+"\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	srv, ledger := round39Gateway(t, upstream, nil)
	stream := round36Stream(t, srv, round39Ask)
	input, cached, _ := round40Delta(t, stream)
	if cached != 900 {
		t.Fatalf("the client was told cache_read_input_tokens=%d for a turn whose upstream stated a 900-token hit", cached)
	}
	// input_tokens excludes what the cache served, so the two halves partition
	// the prompt: 900 of cache read over a prompt the upstream left unstated
	// means the whole 900 was cached, and the halves must sum to exactly the
	// hit — not to the hit plus an estimate of a prompt already accounted for.
	if input < 0 || input+cached != 900 {
		t.Errorf("the client was told input_tokens=%d beside cache_read_input_tokens=%d: the two partition the prompt and must sum to 900, the hit the upstream stated — summing to %d tells a context meter the turn used %d prompt tokens", input, cached, input+cached, input+cached)
	}

	rows := waitLedger(t, ledger, 1)
	if len(rows) == 0 {
		t.Fatal("no ledger row for the turn")
	}
	row := rows[0]
	if row.PromptTokens != 900 {
		t.Errorf("the ledger row records prompt_tokens=%d while the client was told %d + %d: the two records of one request disagree, and the row's is the one the cost math reads", row.PromptTokens, input, cached)
	}
	if row.CachedTokens != 900 {
		t.Errorf("the ledger row records cached_tokens=%d for a turn whose upstream stated a 900-token hit — the row for the same request the client was told 900 about: reconciliation between the two reads a mismatch that is a bug, not a measurement", row.CachedTokens)
	}
}

// TestAStatedCacheHitOverAnUnstatedPromptOnTheNonStreamLeg is A40-3 in the
// other shape: the same usage object, answered with stream:false. The client
// leg's own copy of the hit already reported it here (round 39's A-F4); this
// leg read the size it never got, clamped the hit into it, and reported both
// the client and the row as if the cache had served nothing.
func TestAStatedCacheHitOverAnUnstatedPromptOnTheNonStreamLeg(t *testing.T) {
	upstream := round40JSONReply(`{"id":"x","choices":[{"finish_reason":"stop","message":{"content":"hello"}}],"usage":{"prompt_cache_hit_tokens":900,"completion_tokens":3}}`)
	defer upstream.Close()

	srv, ledger := round39Gateway(t, upstream, nil)
	status, body := round40Post(t, srv, `{"model":"kat-awq","max_tokens":64,"messages":[{"role":"user","content":"go"}]}`)
	if status != http.StatusOK {
		t.Fatalf("premise: status = %d (%s)", status, body)
	}
	var got struct {
		Usage struct {
			InputTokens          int `json:"input_tokens"`
			CacheReadInputTokens int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("response is not JSON (%v): %s", err, body)
	}
	if got.Usage.CacheReadInputTokens != 900 {
		t.Errorf("the client was told cache_read_input_tokens=%d for a turn whose upstream stated a 900-token hit: the hit was clamped into a prompt size the upstream never gave, so the client reads a cache-served turn as a miss (%s)", got.Usage.CacheReadInputTokens, body)
	}
	if sum := got.Usage.InputTokens + got.Usage.CacheReadInputTokens; sum != 900 || got.Usage.InputTokens < 0 {
		t.Errorf("the two usage fields are input=%d cache_read=%d, want them to partition a 900-token prompt: the stated hit is carved out of the total, not added to an estimate of a prompt it already accounts for", got.Usage.InputTokens, got.Usage.CacheReadInputTokens)
	}

	rows := waitLedger(t, ledger, 1)
	if len(rows) == 0 {
		t.Fatal("no ledger row for the turn")
	}
	row := rows[0]
	if row.CachedTokens != 900 || row.PromptTokens != 900 {
		t.Errorf("the ledger row records prompt=%d cached=%d, want 900/900: the row and the client keep the same two records of one request, and a hit larger than the prompt it is recorded against is a fact that cannot hold", row.PromptTokens, row.CachedTokens)
	}
}

// TestThePromptEstimateChargesWhatTheUpstreamDecodes is C40-4. encoding/json
// writes `<`, `>`, `&` and `"` as multi-byte escapes that are not prompt; the
// estimate is the number the client is told for a turn whose upstream states
// nothing, and it is sized on the DECODED body — the same unit messagesBytes
// returns for every other measure in this gateway.
func TestThePromptEstimateChargesWhatTheUpstreamDecodes(t *testing.T) {
	text := strings.Repeat("<", 100) + strings.Repeat("&", 100)
	// The decoded body, spelled out: what the upstream reads after JSON-decoding
	// the request. Its length is the measure the estimate must report.
	decoded := `[{"role":"user","content":"` + text + `"}]`
	want := len(decoded) / 4

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		// No usage anywhere: the estimate is the only number this turn has.
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"ok"}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	srv, ledger := round39Gateway(t, upstream, nil)
	body, _ := json.Marshal(map[string]any{
		"model": "kat-awq", "max_tokens": 64, "stream": true,
		"messages": []map[string]any{{"role": "user", "content": text}},
	})
	stream := round36Stream(t, srv, string(body))
	input, _, _ := round40Delta(t, stream)

	if input != want {
		t.Errorf("the client was told input_tokens=%d for a %d-byte prompt: the estimate measured the bytes Go spent ENCODING the body (%d escapes of six bytes each) rather than what the upstream decodes, so a markup-heavy turn — every code file a client sends — reports a prompt up to 6x its real size and auto-compaction fires early", input, len(decoded), 200)
	}

	rows := waitLedger(t, ledger, 1)
	if len(rows) == 0 {
		t.Fatal("no ledger row for the turn")
	}
	if rows[0].PromptTokens != want {
		t.Errorf("the ledger row records prompt_tokens=%d, want %d — the row and the client must be the same measure", rows[0].PromptTokens, want)
	}
}

// TestANonObjectToolResultElementIsRefused is A40-4: the client leg 400s a
// tool_result whose content holds an element that is not a JSON object, and
// this leg used to marshal it into the prompt — one body, two prompts.
func TestANonObjectToolResultElementIsRefused(t *testing.T) {
	upstream := round40JSONReply(`{"id":"x","choices":[{"finish_reason":"stop","message":{"content":"ok"}}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`)
	defer upstream.Close()

	srv, _ := round39Gateway(t, upstream, nil)
	status, body := round40Post(t, srv, `{"model":"kat-awq","max_tokens":64,"messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"Read","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":"1"},"x",{"type":"text","text":"after"}]}]}]}`)

	if status != http.StatusBadRequest {
		t.Fatalf("status = %d for a tool_result holding a non-object element (%s): the client leg 400s this body, and this one marshalled the element into the prompt — one body, two answers", status, body)
	}
	if !strings.Contains(body, "not a JSON object") {
		t.Errorf("the refusal does not name what it refused: %s", body)
	}
}

// TestABase64URLImageIsRefused is A40-5. A source that says "base64" and
// decodes to a URL is not an image: sent on as a data URI it reaches the model
// as a picture of the address text, and the client leg refuses the same
// payload in words.
func TestABase64URLImageIsRefused(t *testing.T) {
	payload := base64.StdEncoding.EncodeToString([]byte("https://example.test/a.png"))

	t.Run("top level", func(t *testing.T) {
		upstream := round40JSONReply(`{"id":"x","choices":[{"finish_reason":"stop","message":{"content":"ok"}}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`)
		defer upstream.Close()
		srv, _ := round39Gateway(t, upstream, []string{"text", "image"})

		status, body := round40Post(t, srv, `{"model":"kat-awq","max_tokens":64,"messages":[{"role":"user","content":[
			{"type":"text","text":"what is this"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"`+payload+`"}}]}]}`)
		if status != http.StatusBadRequest {
			t.Fatalf("status = %d for a base64 payload that decodes to a URL (%s): the client leg refuses it, and this leg forwarded it as an image the model was never sent", status, body)
		}
	})

	t.Run("nested in a tool_result", func(t *testing.T) {
		upstream := round40JSONReply(`{"id":"x","choices":[{"finish_reason":"stop","message":{"content":"ok"}}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`)
		defer upstream.Close()
		srv, _ := round39Gateway(t, upstream, []string{"text", "image"})

		status, body := round40Post(t, srv, `{"model":"kat-awq","max_tokens":64,"messages":[
			{"role":"user","content":"go"},
			{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"Shot","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":[
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"`+payload+`"}}]}]}]}`)
		if status != http.StatusBadRequest {
			t.Fatalf("status = %d for a NESTED base64 payload that decodes to a URL (%s): the same rule applies at both depths on both legs", status, body)
		}
	})
}

// TestAUrlSourcedImageIsChargedTheAllowance is A40-9: a remote URL is an image
// the upstream will fetch — the other two measures in this product charge it
// the same allowance, and counting it zero here made one turn measure
// differently depending on which leg served it.
func TestAUrlSourcedImageIsChargedTheAllowance(t *testing.T) {
	base := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "go"}}}
	// Both spellings of a remote image reach this walk: the Anthropic block,
	// and the OpenAI content part the converter writes for it.
	spellings := map[string]any{
		"anthropic block": map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://e.test/a.png"}},
		"openai part (map)": map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://e.test/a.png"}},
		"openai part (string)": map[string]any{"type": "image_url", "image_url": "https://e.test/a.png"},
	}
	for name, part := range spellings {
		withURL := map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "go"},
			part,
		}}}}

		got, want := messagesBytes(withURL), messagesBytes(base)+imagePartByteAllowance
		if got < want {
			t.Errorf("%s: a url-sourced image measured %d bytes against a %d-byte prompt without it: the difference is %d, want at least the %d-byte allowance — a remote image is fetched by the upstream and is not free, and the other two measures charge it", name, got, messagesBytes(base), got-messagesBytes(base), imagePartByteAllowance)
		}
	}
}

// TestAnAdoptedAnswerIsNotClosedWithZeroOutput is B40-3: a whole completion
// adopted out of a frame is written by the bridge itself, and the bytes it
// relays are the answer's size — uncounted, the turn was closed with
// output_tokens:0 for an answer the client had just read.
func TestAnAdoptedAnswerIsNotClosedWithZeroOutput(t *testing.T) {
	const answer = "the adopted answer"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		// A STREAM request answered with one whole completion document: no
		// frames, no usage — an upstream (or a shim in front of one) that
		// ignores stream:true. The bridge adopts the document as the turn.
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"`+answer+`"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()

	srv, ledger := round39Gateway(t, upstream, nil)
	stream := round36Stream(t, srv, round39Ask)
	if !strings.Contains(stream, answer) {
		t.Fatalf("premise: the adopted answer did not reach the client:\n%s", stream)
	}
	_, _, out := round40Delta(t, stream)
	want := len(answer)/4 + 1
	if out != want {
		t.Errorf("the client was told output_tokens=%d for an answer of %d bytes (%d tokens): the adopted bytes were relayed and never counted, so a turn the client just read was closed as an empty one", out, len(answer), want)
	}

	rows := waitLedger(t, ledger, 1)
	if len(rows) == 0 {
		t.Fatal("no ledger row for the adopted turn")
	}
	if rows[0].CompletionTokens != want {
		t.Errorf("the ledger row records completion_tokens=%d, want %d — the same answer, the same measure", rows[0].CompletionTokens, want)
	}
}

// TestHeldNarrationIsFlushedInWrittenOrder is B40-4: narration that arrives
// while a tool call's arguments are mid-JSON is held behind that call, and the
// text that unblocks it arrives in its own chunk — the held text was written
// FIRST and must be read first.
func TestHeldNarrationIsFlushedInWrittenOrder(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		// A call opens with an INCOMPLETE argument object.
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"path\":"}}]}}]}`+"\n\n")
		f.Flush()
		// Narration while the call is mid-JSON: held, not delivered.
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"FIRST"}}]}`+"\n\n")
		f.Flush()
		// The arguments complete.
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.txt\"}"}}]}}]}`+"\n\n")
		f.Flush()
		// And now more narration, which unblocks the held text.
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"SECOND"}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	srv, _ := round39Gateway(t, upstream, nil)
	stream := round36Stream(t, srv, round39Ask)

	first := strings.Index(stream, `"text":"FIRST"`)
	second := strings.Index(stream, `"text":"SECOND"`)
	if first < 0 || second < 0 {
		t.Fatalf("the run delivered FIRST=%d SECOND=%d:\n%s", first, second, stream)
	}
	if first > second {
		t.Errorf("the client read the model's prose reversed — SECOND at %d, FIRST at %d:\n%s\nthe held narration was written first and must be emitted first", second, first, stream)
	}
}

// TestAMixedToolResultStatesItsDroppedImage is B40-5: folding the notice in
// only when the result carried no text lost it for exactly the mixed message —
// text beside one showable image and one the model cannot be shown.
func TestAMixedToolResultStatesItsDroppedImage(t *testing.T) {
	var got map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","choices":[{"finish_reason":"stop","message":{"content":"ok"}}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`)
	}))
	defer upstream.Close()

	srv, _ := round39Gateway(t, upstream, []string{"text", "image"})
	status, body := round40Post(t, srv, `{"model":"kat-awq","max_tokens":64,"messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"Shot","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":[
			{"type":"text","text":"here is the screenshot"},
			{"type":"image","source":{"type":"url","url":"https://e.test/a.png"}},
			{"type":"image","source":{"type":"url","url":""}}]}]}]}`)
	if status != http.StatusOK {
		t.Fatalf("premise: status = %d (%s)", status, body)
	}

	toolText := ""
	if msgs, ok := got["messages"].([]any); ok {
		for _, m := range msgs {
			mm, _ := m.(map[string]any)
			if mm["role"] != "tool" {
				continue
			}
			switch c := mm["content"].(type) {
			case string:
				toolText += c
			case []any:
				// The mixed result rides as blocks: the text this gateway
				// folded the notices into, then the images it can attach.
				for _, b := range c {
					bm, _ := b.(map[string]any)
					if s, _ := bm["text"].(string); s != "" {
						toolText += s + "\n"
					}
				}
			}
		}
	}
	if !strings.Contains(toolText, "here is the screenshot") {
		t.Fatalf("premise: the tool result's own text did not reach the model: %q", toolText)
	}
	if !strings.Contains(toolText, "omitted") && !strings.Contains(toolText, "no url") && !strings.Contains(toolText, "url") {
		t.Errorf("the tool message the model was sent is %q: the image this model cannot be shown was neither attached nor stated, so the message reads as one screenshot the model can see — the omission is silent, which is what C-F2 forbids", toolText)
	}
	if !strings.Contains(toolText, "\n") {
		t.Errorf("the notice was not folded beside the result's own text: %q", toolText)
	}
}

// round40Base64 builds a data URI the way a client spells one.
func round40Base64(media string, data []byte) string {
	return fmt.Sprintf("data:%s;base64,%s", media, base64.StdEncoding.EncodeToString(data))
}
