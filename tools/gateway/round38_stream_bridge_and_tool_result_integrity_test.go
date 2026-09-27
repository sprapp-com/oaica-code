package main

// round38_stream_bridge_and_tool_result_integrity_test.go — a cache hit the
// client was never told about, a tool name that arrived one fragment too late,
// two index-less calls to the same tool merged into one, a whole completion
// discarded as an empty stream, a tool's screenshot described away, a
// mid-stream error frame that ended the turn as a success, and the closing
// events emitted in map order (2026-09-27 audit, round 38,
// B-F1/B-F2/B-F3/B-F4/B-F5/B-F6/B-F7, and this leg's half of A-F4).
//
//   - B-F1: the stream bridge read each usage chunk's cache hit against THAT
//     chunk's own prompt_tokens — zero on the usage-only chunk several backends
//     send — so cachedTokens clamped a stated hit to 0 and the client was told
//     cache_read_input_tokens:0 while the ledger row for the same turn recorded
//     900.
//   - B-F2: content_block_start was emitted the moment a fragment named the
//     call by id, and the start is the only event that carries a name: an
//     upstream that states the id in one fragment and the name in the next
//     handed the client {"id":"call_1","name":""}.
//   - B-F3: the synthetic key minted for an index-less, id-less call was only
//     replaced when the NAME differed, so two calls to the same tool in one
//     stream shared a block whose partial_json held both argument objects.
//     The client leg's startsANewToolCall has always handled this.
//   - B-F4: a stream request answered with ONE completion document produced no
//     data: lines, so the bridge reported "upstream returned an empty stream"
//     with 502 and the ledger booked the discard of a turn the upstream had
//     produced.
//   - B-F5: a tool_result image is described where the client leg sends it, and
//     messagesBytes already charges the payload as an image.
//   - B-F6: an {"error": …} frame mid-stream parsed into a chunk with every
//     field zero, so it was discarded: the client read stop_reason end_turn and
//     the ledger recorded 200.
//   - B-F7: finishStream iterated a Go map, so the closing content_block_stop
//     events arrived in randomized order.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// truncateForLog shortens an upstream body for a failure message.
func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// round38Gateway is round36Gateway with the model's input modalities under the
// test's control: whether a nested tool_result image reaches the model is a
// property of the model (B-F5).
func round38Gateway(t *testing.T, upstream *httptest.Server, modalities []string) *httptest.Server {
	t.Helper()
	g := &gateway{}
	if err := g.apply(gwConfig{
		UpstreamAddr: upstream.URL, ListenAddr: ":0", LedgerPath: filepath.Join(t.TempDir(), "l.jsonl"),
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
	return srv
}

// TestACacheHitStatedInALaterChunkIsReported is B-F1.
func TestACacheHitStatedInALaterChunkIsReported(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"hi"}}]}`+"\n\n")
		f.Flush()
		// The prompt size, in one chunk...
		io.WriteString(w, `data: {"choices":[],"usage":{"prompt_tokens":1000}}`+"\n\n")
		f.Flush()
		// ...and the cache hit in another that states nothing else, which is
		// what several backends send (stream_options.include_usage).
		io.WriteString(w, `data: {"choices":[],"usage":{"prompt_cache_hit_tokens":900,"completion_tokens":3}}`+"\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	srv := round38Gateway(t, upstream, nil)
	stream := round36Stream(t, srv, `{"model":"kat-awq","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hello"}]}`)

	if !strings.Contains(stream, `"cache_read_input_tokens":900`) {
		t.Errorf("the client was told about no cache hit:\n%s\nthe upstream stated 900 cached of a 1000-token prompt, and the ledger row for the same turn records them — the session's context meter sums both fields, so it saw a 900-token miss that never happened", stream)
	}
	if !strings.Contains(stream, `"input_tokens":100`) {
		t.Errorf("input_tokens is not the uncached part of the prompt:\n%s\nAnthropic's contract is that input_tokens and cache_read_input_tokens partition prompt_tokens", stream)
	}
}

// TestAToolNameArrivingInALaterFragmentIsNotDropped is B-F2.
func TestAToolNameArrivingInALaterFragmentIsNotDropped(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function"}]}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"Read"}}]}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"a.txt\"}"}}]}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	srv := round38Gateway(t, upstream, nil)
	stream := round36Stream(t, srv, `{"model":"kat-awq","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"read a.txt"}]}`)

	if !strings.Contains(stream, `"name":"Read"`) {
		t.Errorf("the tool name reached the client in no block:\n%s\nthe name arrived in a fragment AFTER the one that stated the id, and content_block_start — the only event that carries a name — had already been emitted", stream)
	}
	if strings.Contains(stream, `"name":""`) {
		t.Errorf("the client was handed a tool call with an empty name:\n%s\nClaude Code can neither name nor execute it, and the turn still reports stop_reason tool_use", stream)
	}
}

// TestTwoIndexlessCallsToTheSameToolStayTwoCalls is B-F3.
func TestTwoIndexlessCallsToTheSameToolStayTwoCalls(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		// Neither fragment states an index or an id; both name the SAME tool,
		// which is what a model that runs two shell commands in one turn
		// produces from an upstream that omits both fields.
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}]}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"pwd\"}"}}]}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	srv := round38Gateway(t, upstream, nil)
	stream := round36Stream(t, srv, `{"model":"kat-awq","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"go"}]}`)

	if got := strings.Count(stream, `"type":"tool_use"`); got != 2 {
		t.Errorf("two index-less calls to the same tool produced %d tool_use blocks:\n%s\nthe client leg's startsANewToolCall treats a repeated name over complete arguments as the NEXT call; folded into one block, the partial_json holds two concatenated objects and neither call can be executed", got, stream)
	}
	if !strings.Contains(stream, `\"cmd\":\"ls\"`) || !strings.Contains(stream, `\"cmd\":\"pwd\"`) {
		t.Errorf("the two calls' arguments are not both on the wire as their own deltas:\n%s\nthe client leg's startsANewToolCall treats a repeated name over complete arguments as the NEXT call; folded into one block, the partial_json holds two concatenated objects and neither call can be executed", stream)
	}
}

// TestAWholeCompletionAnsweringAStreamIsAdopted is B-F4.
func TestAWholeCompletionAnsweringAStreamIsAdopted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		// A 200 with a whole completion and no SSE framing at all.
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-1","model":"kat-awq","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"the answer"}}],"usage":{"prompt_tokens":12,"completion_tokens":3}}`)
	}))
	defer upstream.Close()

	srv := round38Gateway(t, upstream, nil)
	stream := round36Stream(t, srv, `{"model":"kat-awq","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hello"}]}`)

	if !strings.Contains(stream, "the answer") {
		t.Errorf("a whole completion sent in answer to a stream request was discarded:\n%s\nthe client is told the stream was empty, the ledger books the discard, and the answer the upstream produced and billed is thrown away", stream)
	}
	if !strings.Contains(stream, "message_stop") {
		t.Errorf("the adopted turn has no well-formed end:\n%s", stream)
	}
}

// TestAnErrorFrameMidStreamFailsTheTurn is B-F6.
func TestAnErrorFrameMidStreamFailsTheTurn(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"partial"}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"error":{"message":"upstream ran out of KV cache","type":"server_error"}}`+"\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	srv := round38Gateway(t, upstream, nil)
	stream := round36Stream(t, srv, `{"model":"kat-awq","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hello"}]}`)

	if !strings.Contains(stream, `event: error`) || !strings.Contains(stream, "KV cache") {
		t.Errorf("a mid-stream error frame was discarded:\n%s\nthe client reads a turn that ended normally with stop_reason end_turn, and the ledger records 200 for a request the upstream failed", stream)
	}
	if strings.Contains(stream, `"stop_reason":"end_turn"`) {
		t.Errorf("the failed turn was closed with a fabricated stop_reason:\n%s", stream)
	}
}

// TestANestedToolResultImageReachesAModelThatSees is B-F5.
func TestANestedToolResultImageReachesAModelThatSees(t *testing.T) {
	for _, tc := range []struct {
		name       string
		modalities []string
		want       bool
	}{
		{"a vision model", []string{"text", "image"}, true},
		{"a text-only model", []string{"text"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const payload = "QUJDRA" // 6 bytes of base64, repeated
			big := strings.Repeat(payload, 1000)
			var got string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				got = string(b)
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"id":"chatcmpl-1","model":"kat-awq","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`)
			}))
			defer upstream.Close()

			srv := round38Gateway(t, upstream, tc.modalities)
			body := `{"model":"kat-awq","max_tokens":64,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"the screenshot"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + big + `"}}]}]}]}`
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
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()

			carried := strings.Contains(got, "data:image/png;base64,"+payload)
			if tc.want && !carried {
				t.Errorf("a tool result's screenshot never reached a model that accepts images: %s\nthe client leg attaches it, the prompt meter already charges the payload as an image, and the model is asked about a picture it was never given", truncateForLog(got, 400))
			}
			if !tc.want && carried {
				t.Errorf("a tool result's screenshot was sent to a text-only model: %s", truncateForLog(got, 400))
			}
			if !tc.want && strings.Contains(got, payload) {
				t.Errorf("the base64 of a tool result's screenshot was pasted into a text-only model's prompt: %s", truncateForLog(got, 400))
			}
		})
	}
}

// TestClosingToolBlocksFollowIndexOrder is B-F7: the closing events must come
// out in index order.
func TestClosingToolBlocksFollowIndexOrder(t *testing.T) {
	b := newAnthropicBridge(httptest.NewRecorder(), true, "m")
	for i := 9; i >= 0; i-- {
		b.toolBlocks["?"+string(rune('a'+i))] = &toolBlock{index: i, started: true}
	}
	for run := 0; run < 20; run++ {
		prev := -1
		for _, tb := range b.toolBlocksByIndex() {
			if tb.index <= prev {
				t.Fatalf("the closing content_block_stop events are not in index order: %d after %d (run %d)\nfinishStream iterated a Go map, whose order changes run to run", tb.index, prev, run)
			}
			prev = tb.index
		}
	}
}

// TestANestedSearchResultImageIsDescribedNotInlined is this leg's half of A-F4:
// the same block the client leg describes rather than pastes.
func TestANestedSearchResultImageIsDescribedNotInlined(t *testing.T) {
	big := strings.Repeat("QUJDRA", 100_000) // 600 KB of base64
	req := map[string]any{
		"model": "kat-awq", "max_tokens": 64,
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": []any{
				map[string]any{"type": "search_result", "title": "T", "content": []any{
					map[string]any{"type": "image", "source": map[string]any{
						"type": "base64", "media_type": "image/png", "data": big}},
				}},
			}},
		}}},
	}
	out, errStr := anthropicToOpenAI(req, true)
	if errStr != "" {
		t.Fatalf("conversion refused the body: %s", errStr)
	}
	wire, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "QUJDRAQUJDRA") {
		t.Errorf("a %d-byte screenshot inside a nested search_result was pasted into the prompt as base64: the image arm beside it describes the identical block one level up", len(big))
	}
}

// TestAFailedStreamIsBookedAsFailed is B-F6's ledger half: the row must not
// record a 200 for a turn the client read as an error.
func TestAFailedStreamIsBookedAsFailed(t *testing.T) {
	dir := t.TempDir()
	ledgerPath := filepath.Join(dir, "l.jsonl")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"partial"}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"error":{"message":"upstream fell over"}}`+"\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	g := &gateway{}
	if err := g.apply(gwConfig{
		UpstreamAddr: upstream.URL, ListenAddr: ":0", LedgerPath: ledgerPath,
		APIKeys: []gwKey{{SHA256: keyHash("sk"), Label: "k"}},
		Models: []gwModel{{
			ID: "kat-awq", OwnedBy: "oaica", ContextLength: 262144, MaxCompletionTokens: 32768,
			Pricing: gwPricing{Prompt: "0.00000005", Completion: "0.00000012"},
		}},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(g.messagesHandler))
	defer srv.Close()

	round36Stream(t, srv, `{"model":"kat-awq","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hello"}]}`)

	raw, err := os.ReadFile(ledgerPath)
	if err != nil || len(raw) == 0 {
		t.Fatalf("no ledger row was written: %v", err)
	}
	var row ledgerEntry
	if err := json.Unmarshal([]byte(strings.TrimSpace(strings.Split(string(raw), "\n")[0])), &row); err != nil {
		t.Fatalf("ledger row: %v", err)
	}
	if row.Status == http.StatusOK {
		t.Errorf("the ledger row records %d for a stream the client read as an error: %s\nthe one record kept of the request claims a success that never happened", row.Status, strings.TrimSpace(string(raw)))
	}
}
