package main

// round35_server_tool_and_tool_result_integrity_test.go — two block types the
// round-34 default case turned into a dead session, and a payload the tool
// bridge pasted in full (2026-09-27 audit, round 35, B-F1/A-F4 and B-F2).
//
//   - The default case refuses any block type the switch does not name, and the
//     switch does not name the two the server-side web search tool produces:
//     `server_tool_use` (the search the client asked for) and
//     `web_search_tool_result` (what came back). Both live in the client's own
//     history once the turn is answered, so the refusal is permanent: the next
//     turn echoes them and is refused too, and every turn after it — a session
//     this gateway served before the default case existed is bricked by it. The
//     sibling client-side converter translates both
//     (anthropic/anthropic.go, cases `server_tool_use` and
//     `web_search_tool_result`), so refusing them also answers a turn this
//     product serves on its other leg.
//   - A block nested in a `tool_result` is described by toolResultText /
//     describeBlock instead, and that description inlined ANY unrecognised
//     block as its JSON — including a document's base64. A tool that returns a
//     PDF put 393 KB of base64 into the prompt (charged as ~98k estimated
//     tokens) and handed a model that cannot read base64 a payload it cannot
//     use, while the same block at message level is refused in words and the
//     same-sized image is summarised to 112 bytes.

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// TestAWebSearchTurnIsNotRefused is B-F1/A-F4.
func TestAWebSearchTurnIsNotRefused(t *testing.T) {
	var seen atomic.Pointer[map[string]any]
	upstream := round34BlockUpstream(t, &seen)
	defer upstream.Close()

	g := &gateway{}
	round34Gateway(t, g, upstream.URL, round34Model(), -1, 0)

	body := `{"model":"kat-awq","max_tokens":64,"messages":[` +
		`{"role":"user","content":"what happened today?"},` +
		`{"role":"assistant","content":[` +
		`{"type":"text","text":"Let me search."},` +
		`{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query":"today"}}]},` +
		`{"role":"user","content":[` +
		`{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[` +
		`{"type":"web_search_result","title":"News","url":"https://example.com/news"}]}]}]}`

	code, forwarded := round34BlockPost(t, g, &seen, body)
	if code != http.StatusOK {
		t.Fatalf("a turn carrying the web search tool's own blocks was answered %d: %s\nserver_tool_use and web_search_tool_result are in the client's history as soon as the turn is answered, so the client resends them and the refusal repeats for every following turn of the session", code, forwarded)
	}
	if !strings.Contains(forwarded, "https://example.com/news") {
		t.Errorf("the search result never reached the upstream: the model was asked to continue a conversation whose search it cannot see (forwarded=%s)", forwarded)
	}
	if !strings.Contains(forwarded, "web_search") {
		t.Errorf("the search the client requested is missing from the conversation the model continues: %s", forwarded)
	}
}

// TestADocumentInsideAToolResultIsNotInlinedAsBase64 is B-F2.
func TestADocumentInsideAToolResultIsNotInlinedAsBase64(t *testing.T) {
	var seen atomic.Pointer[map[string]any]
	upstream := round34BlockUpstream(t, &seen)
	defer upstream.Close()

	g := &gateway{}
	round34Gateway(t, g, upstream.URL, round34Model(), -1, 0)

	pdf := strings.Repeat("JVBERi0xLjQK", 4000) // ~48 KB of base64
	body := `{"model":"kat-awq","max_tokens":64,"messages":[` +
		`{"role":"user","content":[{"type":"text","text":"read the report"}]},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"read_pdf","input":{"path":"report.pdf"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[` +
		`{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"` + pdf + `"}}]}]}]}`

	code, forwarded := round34BlockPost(t, g, &seen, body)
	if strings.Contains(forwarded, "JVBERi0xLjQKJVBERi0xLjQK") {
		t.Errorf("the tool result's document base64 was pasted into the prompt (answered %d, %d bytes forwarded): the model cannot read base64, every backend tokenizes it as noise, and the same block at message level is refused rather than forwarded — a description of what the tool returned is what the image case above already does", code, len(forwarded))
	}
	if code != http.StatusOK {
		t.Fatalf("a tool result carrying a document was answered %d: %s", code, forwarded)
	}
	if !strings.Contains(forwarded, "application/pdf") {
		t.Errorf("the model was told nothing about what the tool returned: a document-shaped tool result reached it as content:\"\", which is indistinguishable from the tool returning nothing (forwarded=%s)", forwarded)
	}
}

// TestATextSourceDocumentIsNotDiscountedAsAnImage: the payload walk charged any
// map keyed "source" holding a "data" string as an image transport encoding.
// A text-source document is prompt content — the client leg carried the same
// over-discount until round 34 fixed it there — and discounting it to the 4096
// byte image allowance under-measures the prompt the upstream must accept.
func TestATextSourceDocumentIsNotDiscountedAsAnImage(t *testing.T) {
	fileText := strings.Repeat("the attached style guide says: use tabs. ", 20000) // ~800 KB
	doc := map[string]any{
		"type":   "document",
		"source": map[string]any{"type": "text", "media_type": "text/plain", "data": fileText},
	}
	if payload, images := inlineImageBytes(doc); payload != 0 || images != 0 {
		t.Errorf("a text-source document was charged as %d bytes of image payload across %d images: its content is written into the prompt by the very case this file fixed, so discounting it to the 4096-byte image allowance hides ~200k tokens of prompt from the clamp", payload, images)
	}

	// Control: an image's source is still an encoding, and is still discounted.
	img := map[string]any{
		"type":   "image",
		"source": map[string]any{"type": "base64", "media_type": "image/png", "data": strings.Repeat("QUJD", 1000)},
	}
	if payload, images := inlineImageBytes(img); payload != 4000 || images != 1 {
		t.Errorf("control: an inline image measured %d bytes across %d images, want 4000 across 1 — the discount must still apply to the blocks it is for", payload, images)
	}
}
