package main

// round37_tool_keying_and_part_modality_integrity_test.go — a continuation
// fragment with no index after one that stated it, two calls with neither an
// index nor an id, the non-stream reader of the same wire, and a part whose
// type says nothing about the image it carries (2026-09-27 audit, round 37,
// B-F1/B-F2/B-F3/B-F7).

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAStatedIndexIsRememberedForAnIndexlessContinuation is B-F1: the round-36
// keying recorded the block a call opened only for an id-bearing delta, so the
// very next fragment that omitted the index — the ordinary way an upstream
// continues one call — found no remembered block and opened a PHANTOM tool_use
// with no name and no id, leaving the real call's input half-written.
func TestAStatedIndexIsRememberedForAnIndexlessContinuation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"Read","arguments":"{\"path\":"}}]}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"\"a.txt\"}"}}]}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	srv := round36Gateway(t, upstream)
	stream := round36Stream(t, srv, `{"model":"kat-awq","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"read a.txt"}]}`)

	if got := strings.Count(stream, `"type":"content_block_start"`); got != 1 {
		t.Errorf("one tool call became %d blocks: %s\nthe continuation fragment carried no index, which is how an upstream completes the call it is streaming — it must land in the block that call opened, not open a second one with no name and no id", got, stream)
	}
	var args strings.Builder
	for _, line := range strings.Split(stream, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			Delta struct {
				Type        string `json:"type"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) == nil && ev.Delta.Type == "input_json_delta" {
			args.WriteString(ev.Delta.PartialJSON)
		}
	}
	if args.String() != `{"path":"a.txt"}` {
		t.Errorf("the client would run the call with %q as its input: the fragments of one call's arguments must concatenate to the arguments, and a split into two blocks leaves the real call holding only the first half — invalid JSON the client cannot execute while the turn reports stop_reason tool_use", args.String())
	}
}

// TestTwoIdlessNamedToolCallsDoNotShareABlock is B-F2: a fragment that carries
// a NAME can only be introducing a call, so two such calls in a stream that
// states no index must not fold into one block.
func TestTwoIdlessNamedToolCallsDoNotShareABlock(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"function":{"name":"Read","arguments":"{}"}}]}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"function":{"name":"Bash","arguments":"{}"}}]}}]}`+"\n\n")
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
		t.Errorf("two tool calls were folded into one block: %s\nthe second call's name is discarded and its arguments are concatenated onto the first call's input, so the client executes one tool with the other's arguments — and stop_reason still reports tool_use", stream)
	}
	if got := strings.Count(stream, `"type":"content_block_start"`); got != 2 {
		t.Errorf("two tool calls produced %d content blocks: %s", got, stream)
	}
}

// TestTheNonStreamPathReadsTheSameToolCallSpellingAsTheStream is B-F3: the
// streaming reader gained a flat-spelling fallback for a call's name and
// arguments and the non-stream reader of the SAME wire did not, so identical
// upstream bytes produced a working call when the client streamed and
// {"name":"","input":{}} with stop_reason tool_use when it did not.
func TestTheNonStreamPathReadsTheSameToolCallSpellingAsTheStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"cmpl_1","choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","name":"Read","arguments":"{\"path\":\"a.txt\"}"}]}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	}))
	defer upstream.Close()

	srv := round36Gateway(t, upstream)
	body := round36Stream(t, srv, `{"model":"kat-awq","max_tokens":64,"messages":[{"role":"user","content":"read a.txt"}]}`)

	if !strings.Contains(body, `"name":"Read"`) || !strings.Contains(body, `a.txt`) {
		t.Errorf("the non-stream path answered %s\nthe streamed reader accepts this upstream's spelling and this one does not, so the same bytes yield a runnable call or an empty one depending only on whether the client asked to stream", body)
	}
}

// TestAnInlineImageIsDiscountedWhateverItsPartTypeSays is B-F7: the prompt walk
// charges a part carrying an image_url as an image, and the admission gate that
// decides whether a text-only model must refuse the body did not — so the two
// answered different questions about the same part.
func TestAnInlineImageIsDiscountedWhateverItsPartTypeSays(t *testing.T) {
	dataURL := "data:image/png;base64," + strings.Repeat("QUJD", 250000) // 1 MB of base64
	typeless := map[string]any{"messages": []any{map[string]any{
		"role": "user",
		"content": []any{map[string]any{
			"image_url": dataURL,
		}},
	}}}
	if !hasImageContent(typeless) {
		t.Errorf("a message part carrying a 1 MB data URI is not an image to the admission gate: the prompt walk charges it 4 KB (imagePartByteAllowance) as it does any other image spelling, so a text-only model is not asked to refuse the body and the two readers of the same part disagree — a model that cannot accept images is handed one, and any part the gate is blind to is free of both the charge and the check")
	}
	if got := messagesBytes(typeless); got > 1<<15 {
		t.Errorf("the part measured %d bytes: the gate says it is not an image and the meter charged it as one (or as a 1 MB blob)", got)
	}
}

// TestASearchResultBlockIsCarriedNotRefused is A-F1: round 36 gave the client
// leg a case for `search_result` and left this one refusing it, so the same
// Claude Code body became 200-with-content through `oaica launch` and a 400
// here — the cross-leg disagreement that case exists to close.
func TestASearchResultBlockIsCarriedNotRefused(t *testing.T) {
	var seen bytes.Buffer
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen.Write(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c1","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":1}}`)
	}))
	defer upstream.Close()

	srv := round36Gateway(t, upstream)
	out := round36Stream(t, srv, `{"model":"kat-awq","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"answer this"},{"type":"search_result","source":"https://example.com/s","title":"Study","content":[{"type":"text","text":"PASSAGE-A"}]}]}]}`)

	if strings.Contains(out, `"error"`) {
		t.Errorf("the gateway refused a body its own client leg serves: %s\nthe passages the client asked about are in the request, and the turn is answered 200 with the model asked about text it was never shown", out)
	}
	if !strings.Contains(seen.String(), "PASSAGE-A") {
		t.Errorf("the upstream prompt does not carry the search passage: %s", seen.String())
	}
	if !strings.Contains(seen.String(), "https://example.com/s") {
		t.Errorf("the passage's origin is dropped from the prompt: %s", seen.String())
	}
}

// TestAnInlineImageIsDiscountedNotMerelyDifferentially is B-F4: the round-36
// test compared the two spellings with each other only, so removing the image
// discount from the walk entirely left it green — both sides grew by the same
// megabyte and the difference stayed small. Both spellings need an absolute
// bound.
func TestAnInlineImageIsDiscountedNotMerelyDifferentially(t *testing.T) {
	dataURL := "data:image/png;base64," + strings.Repeat("QUJD", 250000)
	spellings := map[string]map[string]any{
		"string": {"messages": []any{map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "image_url", "image_url": dataURL}},
		}}},
		"map": {"messages": []any{map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": dataURL}}},
		}}},
	}
	for name, body := range spellings {
		if got := messagesBytes(body); got > 1<<15 {
			t.Errorf("the %s spelling measured %d bytes against a 1 MB image: a data URI is a transport encoding, not prompt, and an inline image is charged imagePartByteAllowance — charged as its base64 it reads as ~349563 prompt tokens on a model publishing 262144 and the fit clamp refuses the turn with the compaction-trigger wording, with no way for the session to recover", name, got)
		}
	}
}
