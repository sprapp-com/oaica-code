package main

// round36_streamed_tools_and_prompt_unit_integrity_test.go — a streamed tool
// call that arrives with no name, an index-less tool-call delta that merges two
// calls into one, an image charged as its base64, and the escape overhead the
// unit still bills (2026-09-27 audit, round 36, B-F1/B-F2/B-F3, A-F4).
//
//   - B-F1: the streaming parser reads the tool call's identity from the top
//     level of the delta (`name`, `arguments`), but the OpenAI streaming wire
//     nests both under `function` — the same file's NON-stream struct nests
//     them correctly. Every streamed tool call therefore reached the client as
//     `{"name":"","input":{}}` with `stop_reason:"tool_use"`: the model's chosen
//     tool and its arguments dropped, no input_json_delta emitted, and the turn
//     answered as a success. Claude Code always streams.
//   - B-F2: the delta's index is an `int`, so an upstream that omits it —
//     several do, which is why the sibling client leg made it a pointer in
//     round 15 — files every call into block 0 and discards the later ids.
//   - B-F3: the admission gate counts an image written as a bare string
//     (`"image_url":"data:..."`) as an image, while the payload walk discounts
//     only the map spelling, so the base64 is charged as prompt: for the same
//     1 MB image the gate admits the part and the meter then refuses the turn
//     with Anthropic's compaction-trigger wording.
//   - A-F4: the unit charged Go's HTML escaping (five extra bytes per `<`, `>`
//     or `&`) that the round-35 client leg stopped charging, so the two legs no
//     longer measured the same quantity and this one still locally refused
//     markup-heavy turns.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// round36Gateway stands a gateway in front of an SSE upstream and returns a
// real server for its /v1/messages handler (a recorder would hide the
// Content-Length enforcement the round-34 finding was about).
func round36Gateway(t *testing.T, upstream *httptest.Server) *httptest.Server {
	t.Helper()
	g := &gateway{}
	if err := g.apply(gwConfig{
		UpstreamAddr: upstream.URL, ListenAddr: ":0", LedgerPath: filepath.Join(t.TempDir(), "l.jsonl"),
		APIKeys: []gwKey{{SHA256: keyHash("sk"), Label: "k"}},
		Models: []gwModel{{
			ID: "kat-awq", OwnedBy: "oaica", ContextLength: 262144, MaxCompletionTokens: 32768,
			Pricing: gwPricing{Prompt: "0.00000005", Completion: "0.00000012"},
		}},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(g.messagesHandler))
	t.Cleanup(srv.Close)
	return srv
}

// round36Stream drives one streaming turn through the gateway and returns the
// SSE body the client read.
func round36Stream(t *testing.T, srv *httptest.Server, body string) string {
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
		t.Fatalf("reading the stream (status %d): %v", resp.StatusCode, err)
	}
	return string(out)
}

// TestAStreamedToolCallKeepsItsNameAndArguments is B-F1.
func TestAStreamedToolCallKeepsItsNameAndArguments(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		// The canonical OpenAI streaming shape: `function` nests name and
		// arguments inside the tool call.
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"path\":\"a.txt\"}"}}]}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	srv := round36Gateway(t, upstream)
	stream := round36Stream(t, srv, `{"model":"kat-awq","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"read a.txt"}]}`)

	if !strings.Contains(stream, `"name":"Read"`) {
		t.Errorf("the streamed tool call reached the client with no name: %s\nthe model chose a tool and the client cannot see which one; the turn is answered with stop_reason tool_use and no error", stream)
	}
	if !strings.Contains(stream, "input_json_delta") || !strings.Contains(stream, `a.txt`) {
		t.Errorf("the streamed tool call's arguments never reached the client: %s\nthe upstream emitted them on the OpenAI wire's nested function.arguments, and the client cannot execute a call it cannot see", stream)
	}
}

// TestAnIndexLessToolCallDeltaIsNotMergedIntoTheFirst is B-F2.
func TestAnIndexLessToolCallDeltaIsNotMergedIntoTheFirst(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		// No "index" on either delta: the sibling client leg made its own index
		// a pointer for exactly this upstream (round 15).
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"id":"call_a","type":"function","function":{"name":"Read","arguments":"{}"}}]}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"id":"call_b","type":"function","function":{"name":"Bash","arguments":"{}"}}]}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	srv := round36Gateway(t, upstream)
	stream := round36Stream(t, srv, `{"model":"kat-awq","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"go"}]}`)

	if !strings.Contains(stream, `"name":"Read"`) || !strings.Contains(stream, `"name":"Bash"`) {
		t.Errorf("two index-less tool calls were merged into one block: %s\nthe second call's id and name are discarded and its arguments would be concatenated onto the first call's input", stream)
	}
	if !strings.Contains(stream, "call_a") || !strings.Contains(stream, "call_b") {
		t.Errorf("an index-less tool call lost its id: %s", stream)
	}
}

// TestAnImagePartInTheStringSpellingIsChargedAsAnImage is B-F3.
func TestAnImagePartInTheStringSpellingIsChargedAsAnImage(t *testing.T) {
	dataURL := "data:image/png;base64," + strings.Repeat("QUJD", 250000) // 1 MB of base64
	mapForm := map[string]any{"messages": []any{map[string]any{
		"role": "user",
		"content": []any{map[string]any{
			"type": "image_url", "image_url": map[string]any{"url": dataURL},
		}},
	}}}
	strForm := map[string]any{"messages": []any{map[string]any{
		"role": "user",
		"content": []any{map[string]any{
			"type": "image_url", "image_url": dataURL,
		}},
	}}}
	gotMap, gotStr := messagesBytes(mapForm), messagesBytes(strForm)
	if gotStr > gotMap+4096 {
		t.Errorf("the same image measures %d bytes written as {\"image_url\":{\"url\":...}} and %d written as {\"image_url\":\"data:...\"}: the admission gate counts both spellings as an image (main.go's modality check) and the payload walk discounts only the map, so the gate admits the part and the meter that follows refuses the turn with \"prompt is too long: N tokens > M maximum; reduce the prompt or compact the conversation\" — the wording Claude Code matches into a compaction path that cannot help", gotMap, gotStr)
	}
}

// TestThePromptUnitDoesNotChargeJSONEscaping is A-F4 and B-F5: the round-35
// client leg stopped charging Go's HTML escaping; this leg still charged it, so
// the same prompt measured differently depending on which leg served it. The
// table also covers the two-character escapes (`"`, `\`, newline, tab — a
// quote, a path or a line break appears in nearly every real prompt) and the
// `\u` forms, each measured against the size of the character it decodes to
// rather than against another spelling: the unit's contract is the DECODED
// prompt, so a six-byte escape standing for a three-byte character is worth
// three bytes, not one and not six (2026-09-27 audit, round 37, B-F5/B-F6).
func TestThePromptUnitDoesNotChargeJSONEscaping(t *testing.T) {
	const n = 100000
	plain := map[string]any{"messages": []any{map[string]any{
		"role": "user", "content": strings.Repeat("a", n),
	}}}
	baseline := messagesBytes(plain)
	for name, ch := range map[string]string{
		"quote":        `"`,
		"backslash":    `\`,
		"newline":      "\n",
		"tab":          "\t",
		"less-than":    "<",
		"ampersand":    "&",
		"control-0x01": "\x01",
		"line-sep":     "\u2028",
		"para-sep":     "\u2029",
	} {
		body := map[string]any{"messages": []any{map[string]any{
			"role": "user", "content": strings.Repeat(ch, n),
		}}}
		// What the upstream decodes the prompt to: the same n characters, at
		// this character's own width.
		want := baseline + (len(ch)-1)*n
		if got := messagesBytes(body); got > want+64 || got < want-64 {
			t.Errorf("%d %s characters measure %d bytes where the decoded prompt is %d: the upstream JSON-decodes the body before tokenizing anything, so the bytes an escape spends on the wire are the transport's cost and not the prompt's — and the same unit feeds the fit clamp, the calibration ratio and the estInputTokens fallback",
				n, name, got, want)
		}
	}
}

// TestEveryStreamedToolBlockIsClosedInOnePass guards the block bookkeeping the
// two findings above sit on: two calls in one stream must produce two starts,
// two stops, and no stray text block.
func TestEveryStreamedToolBlockIsClosedInOnePass(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c0","function":{"name":"Read","arguments":"{}"}},{"index":1,"id":"c1","function":{"name":"Bash","arguments":"{}"}}]}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	srv := round36Gateway(t, upstream)
	stream := round36Stream(t, srv, `{"model":"kat-awq","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"go"}]}`)

	// The SSE writes the name twice per block event (the `event:` line and the
	// `type` field of its data), so count the data lines only.
	if got := strings.Count(stream, `"type":"content_block_start"`); got != 2 {
		t.Errorf("two tool calls produced %d content_block_start events: %s", got, stream)
	}
	if got := strings.Count(stream, `"type":"content_block_stop"`); got != 2 {
		t.Errorf("two tool calls produced %d content_block_stop events: %s", got, stream)
	}
}
