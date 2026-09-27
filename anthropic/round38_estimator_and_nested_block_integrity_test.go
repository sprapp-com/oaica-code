package anthropic

// round38_estimator_and_nested_block_integrity_test.go — the token estimator
// on a nested body, the blocks it never charged, a nested image that was pasted
// as base64, a typed tool_result content that converted to nothing, an image
// with no payload, and an empty passage (2026-09-27 audit, round 38,
// A-F1/A-F2/A-F4/A-F5/A-F6/A-F8).

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ollama/ollama/api"
)

func strPtrR38(s string) *string { return &s }

// TestANestedBodyIsEstimatedInOnePass is A-F1: the []any arm marshalled every
// item and unmarshalled it into a ContentBlock, and countContentBlock re-entered
// the same arm for a search_result's nested content — so every nesting level
// re-marshalled the whole remaining subtree. A 240 KB body nested a few
// thousand deep cost ~16 s of CPU on /v1/messages before any upstream call.
func TestANestedBodyIsEstimatedInOnePass(t *testing.T) {
	const depth = 3000
	var sb strings.Builder
	for i := 0; i < depth; i++ {
		sb.WriteString(`{"type":"search_result","title":"t","content":[`)
	}
	sb.WriteString(`{"type":"text","text":"x"}`)
	for i := 0; i < depth; i++ {
		sb.WriteString(`]}`)
	}
	body := `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":[` + sb.String() + `]}]}`

	var req MessagesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("premise: the nested body must parse (depth %d): %v", depth, err)
	}
	if len(body) < 100_000 || len(body) > 400_000 {
		t.Fatalf("premise: the body should be a realistic 100-400 KB, got %d", len(body))
	}

	start := time.Now()
	_ = EstimateInputTokens(req)
	elapsed := time.Since(start)
	if elapsed > time.Second {
		t.Errorf("estimating a %d-byte body nested %d deep took %v: the estimate runs on /v1/messages before any upstream call, so one request burns seconds of a CPU core and a handful of them stall the handler — the walk has to be a single pass over the decoded value, not one re-marshal per nesting level",
			len(body), depth, elapsed)
	}
}

// TestEveryBlockTheConverterCarriesIsCharged is A-F2: the switch had arms for
// tool_use/tool_result, document and search_result only, so an image, a
// server_tool_use and a web_search_tool_result each contributed 0 to an
// estimate that seeds the session's input_tokens — a 400 KB image read as 1
// token, the context meter never grew, and auto-compaction never fired.
func TestEveryBlockTheConverterCarriesIsCharged(t *testing.T) {
	payload := strings.Repeat("QUJD", 100_000) // 400 KB
	cases := map[string]ContentBlock{
		"server_tool_use": {Type: "server_tool_use", ID: "srv_1", Name: "web_search",
			Input: makeArgs("query", payload)},
		"web_search_tool_result": {Type: "web_search_tool_result", ToolUseID: "srv_1",
			Content: []WebSearchResult{{Type: "web_search_result", URL: "https://e.com",
				Title: payload, EncryptedContent: payload}}},
	}
	for name, block := range cases {
		req := MessagesRequest{Model: "m", MaxTokens: 64,
			Messages: []MessageParam{{Role: "user", Content: []ContentBlock{block}}}}
		got := EstimateInputTokens(req)
		if got < 10_000 {
			t.Errorf("a turn carrying %s of ~%d bytes of content is estimated at %d tokens: the estimate is what middleware/anthropic.go seeds the stream converter's input_tokens with when the upstream states no usage, so the session's context meter never grows and auto-compaction never fires while the prompt walks into the upstream's real limit",
				name, len(payload), got)
		}
	}

	// An image is the exception, and deliberately so (round 39, A-F3): it is
	// charged the inline allowance rather than its base64 length, which is what
	// the product's other two prompt-size measures do — cmd/launch's
	// prompt_payload_bytes and tools/gateway's imagePartByteAllowance, both
	// 4096 bytes an image. Charging the base64 here made one measure disagree
	// with the two it is meant to agree with: this is a 400 KB image, and the
	// estimate must not move with it.
	imageAt := func(data string) int {
		return EstimateInputTokens(MessagesRequest{Model: "m", MaxTokens: 64,
			Messages: []MessageParam{{Role: "user", Content: []ContentBlock{
				{Type: "image", Source: &ImageSource{Type: "base64", MediaType: "image/png", Data: data}}}}}})
	}
	big, small := imageAt(payload), imageAt("QUJD")
	if big != small {
		t.Errorf("a %d-byte image is estimated at %d tokens and a 4-byte one at %d: an image is charged the inline allowance, not its base64 length, as in the other two measures", len(payload), big, small)
	}
	if big < 1000 {
		t.Errorf("a turn carrying an image is estimated at %d tokens, want the 4096-byte allowance to be charged", big)
	}
}

// TestANestedImageIsDescribedNotInlined is A-F4: an image reached through a
// path with no carrier for it — a passage inside a search_result, which is
// walked by searchResultText and not by the tool_result image reader — fell
// through to the JSON fallback, so a 600 KB screenshot was pasted into the
// prompt as base64 prose. The gateway leg sends a one-line notice, and the
// document arm beside it exists precisely to keep binary out of the prompt.
func TestANestedImageIsDescribedNotInlined(t *testing.T) {
	const payload = "QUJDRA" // repeated to 600 KB
	big := strings.Repeat(payload, 100_000)

	cases := map[string]ContentBlock{
		"inside a search_result passage list": {Type: "search_result", Title: "T",
			Content: []any{
				map[string]any{"type": "text", "text": "read the file"},
				map[string]any{"type": "image", "source": map[string]any{
					"type": "base64", "media_type": "image/png", "data": big}},
			}},
		"inside a search_result nested in a tool_result": {Type: "tool_result",
			ToolUseID: "t1", Content: []any{
				map[string]any{"type": "search_result", "title": "T", "content": []any{
					map[string]any{"type": "image", "source": map[string]any{
						"type": "base64", "media_type": "image/png", "data": big}},
				}},
			}},
	}
	for name, block := range cases {
		req := MessagesRequest{Model: "m", MaxTokens: 4096,
			Messages: []MessageParam{{Role: "user", Content: []ContentBlock{block}}}}

		converted, err := FromMessagesRequest(req)
		if err != nil {
			t.Fatalf("an image %s failed to convert: %v", name, err)
		}
		wire, err := json.Marshal(converted)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(wire), "QUJDRAQUJDRA") {
			t.Errorf("a %d-byte image %s was pasted into the prompt as base64: the gateway leg answers the same body with a one-line description, the document arm next door describes a binary attachment for the same reason, and the prompt unit charges the whole payload to the session's context budget", len(big), name)
		}
	}
}

// TestAURLImageSourceIsCarriedNotRefused is A-F3: an image whose source is a
// URL is forwarded by the gateway leg as an image_url and was refused here with
// a 400 naming an unsupported source type, so the same body the model could see
// through one leg was answered with an error through the other.
func TestAURLImageSourceIsCarriedNotRefused(t *testing.T) {
	req := MessagesRequest{Model: "m", MaxTokens: 64,
		Messages: []MessageParam{{Role: "user", Content: []ContentBlock{
			{Type: "text", Text: strPtrR38("what is in this picture?")},
			{Type: "image", Source: &ImageSource{
				Type: "url", URL: "https://example.com/cat.png"}},
		}}}}

	converted, err := FromMessagesRequest(req)
	if err != nil {
		t.Fatalf("a url image source was refused: %v — the gateway leg forwards the same block as an image_url", err)
	}
	var carried []api.ImageData
	for _, m := range converted.Messages {
		carried = append(carried, m.Images...)
	}
	if len(carried) != 1 || string(carried[0]) != "https://example.com/cat.png" {
		t.Errorf("the url source came through as %q: want the URL itself, so the wire sends the location the model can fetch rather than a picture of the address", carried)
	}
}

// TestATypedToolResultContentIsConverted is A-F5: convertToolResultContent
// matched only the JSON-decoded []any shape, so the package's own typed
// []ContentBlock fell to the trailing default and converted to an EMPTY tool
// message with no error — a tool the model is told returned nothing.
func TestATypedToolResultContentIsConverted(t *testing.T) {
	req := MessagesRequest{Model: "m", MaxTokens: 4096,
		Messages: []MessageParam{{Role: "user", Content: []ContentBlock{
			{Type: "tool_result", ToolUseID: "t1", Content: []ContentBlock{
				{Type: "text", Text: strPtrR38("TYPED-A")},
				{Type: "text", Text: strPtrR38("TYPED-B")},
			}},
		}}}}

	converted, err := FromMessagesRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var got string
	for _, m := range converted.Messages {
		if m.Role == "tool" {
			got = m.Content
		}
	}
	if got != "TYPED-A\nTYPED-B" {
		t.Errorf("a typed tool_result content converted to %q: the same logical content JSON-decoded converts to \"TYPED-A\\nTYPED-B\", so the shape of the field decides whether the tool's answer reaches the model — silently, with a 200", got)
	}
}

// TestAnImageWithNoPayloadIsRefused is A-F6: an image source with no data
// decodes to zero bytes and was carried into the request as a blank image —
// where the gateway leg refuses the same body. The client is told its request
// succeeded and the model is handed an undecodable part.
func TestAnImageWithNoPayloadIsRefused(t *testing.T) {
	for name, ct := range map[string]any{
		"no data key":       map[string]any{"type": "base64", "media_type": "image/png"},
		"empty data":        map[string]any{"type": "base64", "media_type": "image/png", "data": ""},
		"non-string data":   map[string]any{"type": "base64", "media_type": "image/png", "data": 42},
		"url source no url": map[string]any{"type": "url"},
	} {
		req := MessagesRequest{Model: "m", MaxTokens: 64,
			Messages: []MessageParam{{Role: "user", Content: []ContentBlock{
				{Type: "tool_result", ToolUseID: "t1", Content: []any{
					map[string]any{"type": "image", "source": ct},
				}},
			}}}}
		converted, err := FromMessagesRequest(req)
		if err != nil {
			continue // refusing is the wanted outcome
		}
		wire, _ := json.Marshal(converted)
		t.Errorf("an image source (%s) with no payload converted without error: %s\nthe gateway leg refuses the same body, and a blank image is a part the upstream cannot decode — answered as a success", name, wire)
	}
}

// TestAnEmptyTextPassageIsNotDescribed is A-F8: the typed arm of
// searchResultText described a text block whose text was empty, injecting a
// JSON blob into the prompt where the JSON-decoded arm skips it.
func TestAnEmptyTextPassageIsNotDescribed(t *testing.T) {
	got := searchResultText([]ContentBlock{{Type: "text", Text: strPtrR38("")}})
	if strings.Contains(got, `{"`) || strings.Contains(got, "text") {
		t.Errorf("an empty passage rendered as %q: an empty text block is no passage and carries nothing to describe, and the same block JSON-decoded is skipped", got)
	}
}
