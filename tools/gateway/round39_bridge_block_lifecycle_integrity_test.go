package main

// round39_bridge_block_lifecycle_integrity_test.go — round 39's half of the
// bridge's contract, one test per finding:
//
//   - B-F1: a whole completion stated under `message` inside a frame was
//     discarded — a client got a 200 with empty content and the ledger booked
//     the discard.
//   - B-F2: indices are handed out in the order blocks OPEN, so a held call
//     whose name arrives late cannot be started after a higher-indexed block.
//   - B-F3: a stream request answered with ONE completion document and no data:
//     lines was adopted by finalize but the ledger row — written first, from
//     LedgerStatus — recorded 502 with zero tokens for a turn that was served
//     and billed.
//   - B-F6: an error frame stated as a bare STRING failed to parse, so it was
//     discarded and the turn ended as a success.
//   - B-F7: a fragment of a call's arguments arriving after its block closed
//     cannot be delivered, and must not be written into the stream anyway.
//   - B-F8: finish_reason tool_calls over a call the upstream never named told
//     Claude Code to wait for a call it would never receive.
//   - B-F9: two index-less, id-less calls to the same tool that both state no
//     arguments were folded into one block.
//   - C-F2: an image that yields no part is STATED in the text rather than
//     erased, so the message is not byte-identical to "the tool returned
//     nothing".
//   - C-F3: a turn whose upstream states no usage at all left the client's
//     context meter still (input_tokens:0) and the ledger row at zero.
//   - C-F7: a nil passage list and an empty passage are no text.
//   - C-F8: a search_result's origin is read from `ref` as well as `url`.
//   - C-F11: an inline image's data URL is written with the type its own magic
//     bytes name, not the label the client attached.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// round39Gateway is round38Gateway with the ledger path under the test's
// control, so a row can be read back beside the stream the client read.
func round39Gateway(t *testing.T, upstream *httptest.Server, modalities []string) (*httptest.Server, string) {
	t.Helper()
	ledger := filepath.Join(t.TempDir(), "ledger.jsonl")
	g := &gateway{}
	if err := g.apply(gwConfig{
		UpstreamAddr: upstream.URL, ListenAddr: ":0", LedgerPath: ledger,
		APIKeys: []gwKey{{SHA256: keyHash("sk"), Label: "k"}},
		Models: []gwModel{{
			ID: "kat-awq", OwnedBy: "oaica", ContextLength: 262144, MaxCompletionTokens: 32768,
			Pricing:         gwPricing{Prompt: "0.00000005", Completion: "0.00000012"},
			InputModalities: modalities,
		}},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(g.messagesHandler))
	t.Cleanup(srv.Close)
	return srv, ledger
}

const round39Ask = `{"model":"kat-awq","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"go"}]}`

// TestAWholeCompletionInAFrameIsRelayed is B-F1.
func TestAWholeCompletionInAFrameIsRelayed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		// An upstream emulating streaming around a non-streaming backend: the
		// whole completion arrives under `message`, not `delta`.
		io.WriteString(w, `data: {"choices":[{"index":0,"message":{"role":"assistant","content":"the answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":3}}`+"\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	srv, _ := round39Gateway(t, upstream, nil)
	stream := round36Stream(t, srv, round39Ask)

	if !strings.Contains(stream, "the answer") {
		t.Errorf("the answer the upstream stated under `message` reached the client in no block:\n%s\nthe fields of a whole completion are the fields of a delta, and the frame was discarded as an empty chunk — a 200 with no content", stream)
	}
}

// TestAWholeCompletionAnsweringAStreamIsNotABookedFailure is B-F3.
func TestAWholeCompletionAnsweringAStreamIsNotABookedFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		// A 200 with a whole completion and no SSE framing at all, for a
		// request that asked to stream.
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-1","model":"kat-awq","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"the answer"}}],"usage":{"prompt_tokens":12,"completion_tokens":3}}`)
	}))
	defer upstream.Close()

	srv, ledger := round39Gateway(t, upstream, nil)
	stream := round36Stream(t, srv, round39Ask)
	if !strings.Contains(stream, "the answer") {
		t.Fatalf("premise: the whole completion was not adopted as the turn:\n%s", stream)
	}

	rows := waitLedger(t, ledger, 1)
	if len(rows) == 0 {
		t.Fatal("no ledger row was written for a turn the client was served")
	}
	row := rows[0]
	if row.Status != http.StatusOK {
		t.Errorf("ledger status = %d for a turn the client read as a success: LedgerStatus is asked BEFORE finalize adopts the body, so the row recorded the upstream's silence as a failure of the bridge's own making", row.Status)
	}
	if row.PromptTokens != 12 || row.CompletionTokens != 3 {
		t.Errorf("ledger recorded prompt=%d completion=%d, want the document's 12/3: a served, billed turn booked as zero tokens", row.PromptTokens, row.CompletionTokens)
	}
}

// TestACacheHitOverAnUnstatedPromptIsStillReported is B-F4/C-F4. The upstream
// states the hit and never states the prompt size: clamping the hit against a
// prompt of zero told the client cache_read_input_tokens:0 for a turn the
// ledger row for the same request recorded the hit.
func TestACacheHitOverAnUnstatedPromptIsStillReported(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"hi"}}]}`+"\n\n")
		f.Flush()
		// The hit, and no prompt size anywhere in the stream.
		io.WriteString(w, `data: {"choices":[],"usage":{"prompt_cache_hit_tokens":900,"completion_tokens":3}}`+"\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	srv, _ := round39Gateway(t, upstream, nil)
	stream := round36Stream(t, srv, round39Ask)

	delta := ""
	for _, line := range strings.Split(stream, "\n") {
		if strings.Contains(line, `"type":"message_delta"`) {
			delta = line
		}
	}
	if delta == "" {
		t.Fatalf("no message_delta reached the client:\n%s", stream)
	}
	var event struct {
		Usage struct {
			CacheReadInputTokens int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	body := strings.TrimSpace(strings.TrimPrefix(delta, "data: "))
	if err := json.Unmarshal([]byte(body), &event); err != nil {
		t.Fatalf("message_delta is not JSON (%v): %s", err, delta)
	}
	if event.Usage.CacheReadInputTokens <= 0 {
		t.Errorf("the client was told cache_read_input_tokens:%d for a turn the cache served:\n%s\nthe hit was clamped against a prompt of zero, and the ledger row for the same request records it", event.Usage.CacheReadInputTokens, stream)
	}
}

// TestAnErrorFrameStatedAsABareStringFailsTheTurn is B-F6.
func TestAnErrorFrameStatedAsABareStringFailsTheTurn(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"partial"}}]}`+"\n\n")
		f.Flush()
		// The failure, stated as a bare string rather than an object.
		io.WriteString(w, `data: {"error":"upstream ran out of KV cache"}`+"\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	srv, ledger := round39Gateway(t, upstream, nil)
	stream := round36Stream(t, srv, round39Ask)

	if !strings.Contains(stream, "ran out of KV cache") {
		t.Errorf("the failure the upstream stated inside the stream reached the client as no error at all:\n%s\nmodelling only the object form made the whole frame fail to parse, so it was discarded and the turn ended normally", stream)
	}
	if strings.Contains(stream, `"stop_reason":"end_turn"`) && !strings.Contains(stream, `"type":"error"`) {
		t.Errorf("the client read a turn that ended normally after the upstream failed:\n%s", stream)
	}

	rows := waitLedger(t, ledger, 1)
	if len(rows) == 0 {
		t.Fatal("no ledger row for the failed turn")
	}
	if rows[0].Status == http.StatusOK {
		t.Errorf("ledger recorded 200 for a request the upstream failed with a stated error frame: %+v", rows[0])
	}
}

// TestAnUnnamedToolCallIsNotReportedAsToolUse is B-F8.
func TestAnUnnamedToolCallIsNotReportedAsToolUse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		// Arguments, and then a finish_reason of tool_calls — but no fragment
		// ever NAMES the call.
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"arguments":"{\"cmd\":\"ls\"}"}]}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	srv, _ := round39Gateway(t, upstream, nil)
	stream := round36Stream(t, srv, round39Ask)

	if strings.Contains(stream, `"type":"tool_use"`) {
		t.Errorf("the client was handed a tool_use for a call the upstream never named:\n%s\nClaude Code reports it as pending and can never run it", stream)
	}
	if !strings.Contains(stream, `"stop_reason":"end_turn"`) {
		t.Errorf("stop_reason is not end_turn over a turn with no usable call:\n%s\ntelling Claude Code to wait for a call it will never receive hangs the turn", stream)
	}
	if !strings.Contains(stream, "cmd") {
		t.Errorf("the arguments the upstream did state reached the client as nothing:\n%s\nunnamed, they are still the model's raw output and must be readable", stream)
	}
}

// TestTwoArgumentlessCallsToTheSameToolStayTwoCalls is B-F9.
func TestTwoArgumentlessCallsToTheSameToolStayTwoCalls(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		// Two index-less, id-less calls to the same tool, both taking no
		// arguments — the shape a model produces when it runs one command twice.
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"type":"function","function":{"name":"Bash"}}]}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"type":"function","function":{"name":"Bash"}}]}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	srv, _ := round39Gateway(t, upstream, nil)
	stream := round36Stream(t, srv, round39Ask)

	if got := strings.Count(stream, `"type":"tool_use"`); got != 2 {
		t.Errorf("two argument-less calls to the same tool produced %d tool_use blocks:\n%s\nan empty argument string is a COMPLETE argument list for a call that takes none, so the second is the next call — folded into one, the client is told the model asked for one", got, stream)
	}
}

// TestANameArrivingLateDoesNotStartAfterALaterBlock is B-F2: indices are handed
// out in the order blocks OPEN, so a call held for its name cannot be started
// after a block that opened first.
func TestANameArrivingLateDoesNotStartAfterALaterBlock(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		// An id, and nothing else: the block cannot open (the start is the only
		// event that carries a name) so it is held.
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function"}]}}]}`+"\n\n")
		f.Flush()
		// Narration in between: it must take the FIRST index.
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"Let me look."}}]}`+"\n\n")
		f.Flush()
		// Now the name, and the arguments.
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"Read","arguments":"{\"path\":\"a.txt\"}"}}]}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	srv, _ := round39Gateway(t, upstream, nil)
	stream := round36Stream(t, srv, round39Ask)

	// Every start must precede the deltas of its own block and no start may
	// arrive inside another block.
	lines := strings.Split(stream, "\n")
	open := -1
	starts := map[int]bool{}
	for i, line := range lines {
		if !strings.HasPrefix(line, "event: ") {
			continue
		}
		ev := strings.TrimSpace(strings.TrimPrefix(line, "event: "))
		payload := ""
		for j := i + 1; j < len(lines); j++ {
			if strings.HasPrefix(lines[j], "data: ") {
				payload = lines[j]
				break
			}
		}
		idx := indexOfBlock(payload)
		switch ev {
		case "content_block_start":
			if open != -1 {
				t.Fatalf("block %d opened while block %d was still open:\n%s", idx, open, stream)
			}
			open = idx
			starts[idx] = true
		case "content_block_delta", "content_block_stop":
			if open != idx {
				t.Fatalf("%s for block %d arrived with block %d open:\n%s", ev, idx, open, stream)
			}
			if ev == "content_block_stop" {
				open = -1
			}
		}
	}
	if !starts[0] || !starts[1] {
		t.Errorf("the stream opened blocks %v, want one text block and the tool block:\n%s\nthe held call took the first index and the text that arrived before its name took a later one", starts, stream)
	}
	if strings.Contains(stream, `"name":""`) {
		t.Errorf("the client was handed a tool call with an empty name:\n%s", stream)
	}
}

// TestArgumentsAfterABlockClosedAreNotWritten is B-F7's second half: a closed
// block cannot be reopened, so a fragment that arrives afterwards belongs to a
// call the client has already been told is finished.
func TestArgumentsAfterABlockClosedAreNotWritten(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}]}}]}`+"\n\n")
		f.Flush()
		// Narration: the call's arguments are complete JSON, so its block closes
		// and the text block follows it.
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"and now the rest"}}]}`+"\n\n")
		f.Flush()
		// An interleaving the OpenAI wire permits and Anthropic's does not: more
		// arguments for the call whose block is already closed.
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"more\":1}"}}]}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	srv, _ := round39Gateway(t, upstream, nil)
	stream := round36Stream(t, srv, round39Ask)

	stopIdx := strings.Index(stream, `"type":"content_block_stop"`)
	if stopIdx < 0 {
		t.Fatalf("premise: no block was closed:\n%s", stream)
	}
	if tail := stream[stopIdx:]; strings.Contains(tail, "more") {
		t.Errorf("an argument fragment was written after its block had been closed:\n%s\nthe client has been told that call is finished and the wire has no way to reopen it", stream)
	}
}

// TestAnImageThatYieldsNoPartIsStatedNotErased is C-F2.
func TestAnImageThatYieldsNoPartIsStatedNotErased(t *testing.T) {
	content := []any{map[string]any{
		"type": "tool_result", "tool_use_id": "t1",
		"content": []any{
			map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": ""}},
		},
	}}
	msgs, refusal := contentBlocksToOpenAI("user", content, true)
	if refusal != "" {
		t.Fatalf("convert: %s", refusal)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want the one tool message", len(msgs))
	}
	text, _ := msgs[0]["content"].(string)
	if text == "" {
		t.Fatalf("the image yielded no part and nothing was said about it: the message is byte-identical to \"the tool returned nothing\", and the model answers as if the tool had said nothing (%v)", msgs[0])
	}
	if !strings.Contains(text, "image") {
		t.Errorf("the notice does not name what was dropped: %q", text)
	}
}

// TestANilOrEmptyPassageIsNoText is C-F7.
func TestANilOrEmptyPassageIsNoText(t *testing.T) {
	if got := searchResultText(nil); got != "" {
		t.Errorf("searchResultText(nil) = %q, want the empty string: described, the literal `null` went into the prompt as though the search had returned it", got)
	}
	empty := []any{map[string]any{"type": "text", "text": ""}}
	if got := searchResultText(empty); got != "" {
		t.Errorf("searchResultText(one empty passage) = %q, want the empty string: `{\"text\":\"\",\"type\":\"text\"}` went into the prompt as though the search had returned it", got)
	}
	// An empty passage beside a real one must not leave a blank line in front.
	mixed := []any{map[string]any{"type": "text", "text": ""}, map[string]any{"type": "text", "text": "real"}}
	if got := searchResultText(mixed); got != "real" {
		t.Errorf("searchResultText(empty + real) = %q, want %q", got, "real")
	}
}

// TestASearchResultsOriginIsReadFromRef is C-F8.
func TestASearchResultsOriginIsReadFromRef(t *testing.T) {
	if got := searchResultOrigin(map[string]any{"ref": "https://a.test/x"}); got != "https://a.test/x" {
		t.Errorf("searchResultOrigin(ref) = %q, want the ref: reading only url dropped the provenance of the same source on this leg only", got)
	}
	if got := searchResultOrigin(map[string]any{"url": "https://b.test/y"}); got != "https://b.test/y" {
		t.Errorf("searchResultOrigin(url) = %q, want the url", got)
	}
	if got := searchResultOrigin("https://c.test/z"); got != "https://c.test/z" {
		t.Errorf("searchResultOrigin(bare string) = %q, want the string", got)
	}
	block := map[string]any{
		"type": "search_result", "title": "T",
		"source":  map[string]any{"ref": "https://a.test/x"},
		"content": []any{map[string]any{"type": "text", "text": "passage"}},
	}
	if desc := describeBlock(block); !strings.Contains(desc, "https://a.test/x") {
		t.Errorf("describeBlock dropped the ref-sourced origin: %q", desc)
	}
}

// TestAnInlineImageIsLabelledByItsBytes is C-F11.
func TestAnInlineImageIsLabelledByItsBytes(t *testing.T) {
	jpeg := base64.StdEncoding.EncodeToString([]byte("\xff\xd8\xff\xe0rest of a jpeg"))
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nrest of a png"))
	gif := base64.StdEncoding.EncodeToString([]byte("GIF89a rest of a gif"))
	unknown := base64.StdEncoding.EncodeToString([]byte("not an image at all, honestly"))

	cases := []struct {
		name, media, data, want string
	}{
		{"the client's label is contradicted by the bytes", "image/png", jpeg, "image/jpeg"},
		{"png bytes", "image/jpeg", png, "image/png"},
		{"gif bytes", "", gif, "image/gif"},
		{"unknown bytes take the stated label", "image/avif", unknown, "image/avif"},
		{"unknown bytes with no label are a jpeg", "", unknown, "image/jpeg"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := inlineImageMediaType(tt.media, tt.data); got != tt.want {
				t.Errorf("inlineImageMediaType(%q, %s bytes) = %q, want %q", tt.media, tt.name, got, tt.want)
			}
		})
	}
}

// TestTheClientAndTheLedgerAreToldAnEstimateWhenNothingIsStated is C-F3.
func TestTheClientAndTheLedgerAreToldAnEstimateWhenNothingIsStated(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		// An answer, and no usage object anywhere in the stream.
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"an answer with no usage stated at all"}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	srv, ledger := round39Gateway(t, upstream, nil)
	stream := round36Stream(t, srv, round39Ask)

	// message_start states zeroes by the wire's own convention (the turn has
	// produced nothing yet); the closing message_delta is where the turn's usage
	// is reported, and that is the number a context meter reads.
	delta := ""
	for _, line := range strings.Split(stream, "\n") {
		if strings.Contains(line, `"type":"message_delta"`) {
			delta = line
		}
	}
	if delta == "" {
		t.Fatalf("no message_delta reached the client:\n%s", stream)
	}
	var event struct {
		Usage struct {
			InputTokens          int `json:"input_tokens"`
			CacheReadInputTokens int `json:"cache_read_input_tokens"`
			OutputTokens         int `json:"output_tokens"`
		} `json:"usage"`
	}
	body := strings.TrimSpace(strings.TrimPrefix(delta, "data: "))
	if err := json.Unmarshal([]byte(body), &event); err != nil {
		t.Fatalf("message_delta is not JSON (%v): %s", err, delta)
	}
	if event.Usage.InputTokens <= 0 {
		t.Errorf("the client was told input_tokens:%d for a real prompt:\n%s\na session's context meter never moves, so auto-compaction never fires and the session walks into the context wall", event.Usage.InputTokens, stream)
	}
	if event.Usage.OutputTokens <= 0 {
		t.Errorf("the client was told output_tokens:%d for a real answer:\n%s", event.Usage.OutputTokens, stream)
	}

	rows := waitLedger(t, ledger, 1)
	if len(rows) == 0 {
		t.Fatal("no ledger row for the turn")
	}
	if rows[0].PromptTokens <= 0 || rows[0].CompletionTokens <= 0 {
		t.Errorf("ledger recorded prompt=%d completion=%d: the recorder reads the upstream's own bytes, and this upstream said nothing — the row for a served, billed turn says it carried nothing", rows[0].PromptTokens, rows[0].CompletionTokens)
	}
	if rows[0].UsageSeen {
		t.Errorf("the ledger row claims the upstream stated usage: UsageSeen records the UPSTREAM's word, and this estimate is the gateway's own reading of the turn")
	}
}

// TestAMidStreamErrorIsRedactedLikeEveryOtherError is B-F5. The upstream's own
// text can name the URL it failed to reach WITH its credentials in it (an
// upstream that echoes back the request it rejected), and the non-stream path
// redacts that same text — the mid-stream path was the one place a credential
// reached the client unredacted.
func TestAMidStreamErrorIsRedactedLikeEveryOtherError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"partial"}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"error":{"message":"dial tcp http://svc:hunter2@backend.internal:8000: connection refused"}}`+"\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	srv, _ := round39Gateway(t, upstream, nil)
	stream := round36Stream(t, srv, round39Ask)

	if strings.Contains(stream, "hunter2") {
		t.Errorf("the upstream's credentials reached the client inside a mid-stream error:\n%s\nthe non-stream path redacts this same text; this was the one path that did not", stream)
	}
	if !strings.Contains(stream, "[redacted]") {
		t.Errorf("the error text was not passed through the redactor:\n%s", stream)
	}
}

// indexOfBlock reads a bridge event's content-block index out of its payload.
func indexOfBlock(payload string) int {
	k := strings.Index(payload, `"index":`)
	if k < 0 {
		return -1
	}
	var idx int
	fmt.Sscanf(payload[k:], `"index":%d`, &idx)
	return idx
}
