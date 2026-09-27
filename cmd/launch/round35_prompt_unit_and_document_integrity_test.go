package launch

// round35_prompt_unit_and_document_integrity_test.go — the prompt unit charging
// a second transport encoding, a converter that still drops a document without
// a word, and a keyless remote on another host (2026-09-27 audit, round 35,
// A-F1/A-F2/A-F3).
//
//   - A-F1: round 34 made the unit the JSON re-marshal of the converted
//     request, and encoding/json escapes `<`, `>` and `&` as `<`,
//     `>`, `&` — 6 bytes per 1-byte character. The client's own body
//     is written by Node, which does not escape them, so the round-34 unit
//     charged 5 extra bytes per angle bracket or ampersand: a markup-heavy
//     prompt (HTML, XML, JSX, SVG, `2>&1`, a Read-tool file listing) measured
//     up to 6x its real size and the clamp refused a healthy turn LOCALLY with
//     the compaction-trigger wording — the exact failure A-F1 set out to
//     remove. The upstream JSON-decodes the body before tokenizing, so the
//     escaping is transport, not prompt: it has to be discounted like the
//     image base64 is.
//   - A-F2: `anthropic.FromMessagesRequest` still drops a document block with a
//     binary source (and any unknown block type) and the turn is answered 200,
//     while the gateway leg refuses the same body in words. Round 34's commit
//     claimed the two converters now agree; they do not, and the client leg is
//     the one that answers a question about a document the model never got.
//   - A-F3: `qwenConfigHoldsLiveRemoteKey` answers "current" whenever the
//     remote resolves to no token, but `applyQwenOllamaKey` BLANKS the stored
//     value for a keyless remote reached on another host — so a stale bearer
//     survived the launch and was sent to that host.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ollama/ollama/anthropic"
)

// TestAPromptUnitDoesNotChargeJSONEscaping is A-F1: the unit has to describe
// the prompt, not the encoding of the body that carries it.
func TestAPromptUnitDoesNotChargeJSONEscaping(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	const promptChars = 200000
	plain := round34MessagesBody(t, "user", []map[string]any{
		{"type": "text", "text": strings.Repeat("a", promptChars)},
	}, 4096)
	markup := round34MessagesBody(t, "user", []map[string]any{
		{"type": "text", "text": strings.Repeat("<", promptChars)},
	}, 4096)

	plainBytes, markupBytes := clientPromptBytes(plain), clientPromptBytes(markup)
	if markupBytes > plainBytes+1024 {
		t.Errorf("two prompts of the same length measure %d and %d bytes (%d bytes of HTML escaping): encoding/json writes `<` as `\\u003c`, so the unit charges 6 bytes for every 1-byte character of markup while the upstream JSON-decodes the body and tokenizes the character it stands for",
			plainBytes, markupBytes, markupBytes-plainBytes)
	}

	// Behaviourally: a markup-heavy turn far under the window must be served.
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "model": "kat-awq",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "ok"},
			}},
			"usage": map[string]any{"prompt_tokens": 50000, "completion_tokens": 1},
		})
	}))
	defer upstream.Close()

	proxyURL := startCalibProxy(t, upstream.URL, "sess-round35-markup")
	resp, err := http.Post(proxyURL+"/v1/messages", "application/json", bytes.NewReader(markup))
	if err != nil {
		t.Fatal(err)
	}
	rb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("a %d-character markup prompt was refused: status=%d body=%s; want 200 — %d characters is ~%d tokens of a 262144 window, and the refusal is the string Claude Code matches to its compaction path, which cannot help because the markup is in the newest turn",
			promptChars, resp.StatusCode, strings.TrimSpace(string(rb)), promptChars, promptChars/4)
	}
	if !called {
		t.Error("the upstream was never called: the turn was answered without the model")
	}
}

// TestABinaryDocumentIsRefusedOnBothLegs is A-F2.
func TestABinaryDocumentIsRefusedOnBothLegs(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	pdf := strings.Repeat("JVBERi0xLjQK", 30000) // ~330 KB of base64
	body := round34MessagesBody(t, "user", []map[string]any{
		{"type": "text", "text": "summarise the attached PDF"},
		{"type": "document", "source": map[string]any{
			"type": "base64", "media_type": "application/pdf", "data": pdf,
		}},
	}, 4096)

	// The converter is where the drop happens: the turn is converted happily
	// and the document is gone from the prompt the model is asked about.
	var anthReq anthropic.MessagesRequest
	if err := json.Unmarshal(body, &anthReq); err != nil {
		t.Fatal(err)
	}
	if _, err := anthropic.FromMessagesRequest(anthReq); err == nil {
		t.Errorf("a document block with a %d-byte base64 source converts with no error: the converter counts it and drops it, so the prompt the model receives is the question with no attachment and the client is answered 200 — a confident reply about a document the model never received, which is what the gateway leg now refuses in words",
			len(pdf))
	}

	// And the leg must not answer it.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "model": "kat-awq",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "The PDF is about tabs."},
			}},
			"usage": map[string]any{"prompt_tokens": 20, "completion_tokens": 6},
		})
	}))
	defer upstream.Close()

	proxyURL := startCalibProxy(t, upstream.URL, "sess-round35-document")
	resp, err := http.Post(proxyURL+"/v1/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	rb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Errorf("a PDF attachment was answered 200 (body=%s): the document was dropped from the prompt, so the answer cannot be about it", strings.TrimSpace(string(rb)))
	} else if !strings.Contains(string(rb), "document") {
		t.Errorf("the refusal does not name the block it could not represent: %s", strings.TrimSpace(string(rb)))
	}
}

// TestAnEmptyConversationIsNotSentAsNull: a turn whose every block the
// conversion does not forward converts to no messages, and json.Marshal writes
// that as `"messages":null` — a malformed body the upstream answers with a 400
// worded for an OpenAI client, matched by no recovery path this proxy knows.
func TestAnEmptyConversationIsNotSentAsNull(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	body := round34MessagesBody(t, "user", []map[string]any{
		{"type": "some_future_block", "text": "unrepresentable"},
		// A second block the converter does not forward. This used to be a
		// search_result, which the round-36 converter gained a case for: its
		// title now reaches the prompt, so the turn was no longer the
		// all-unrepresentable one this test is named for and both of its
		// assertions passed for a reason unrelated to the guard (2026-09-27
		// audit, round 37, A-F5).
		{"type": "another_future_block", "text": "also unrepresentable"},
	}, 4096)

	var anthReq anthropic.MessagesRequest
	if err := json.Unmarshal(body, &anthReq); err != nil {
		t.Fatal(err)
	}
	chatReq, err := anthropic.FromMessagesRequest(anthReq)
	if err != nil {
		t.Fatalf("converting an all-unrepresentable turn failed: %v", err)
	}
	wire, err := json.Marshal(chatRequestToOpenAI(chatReq, anthReq, anthReq.Model))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wire, []byte(`"messages":null`)) || bytes.Contains(wire, []byte(`"messages":[null]`)) {
		t.Errorf("the converted body carries no messages array: %s", wire)
	}
	if !bytes.Contains(wire, []byte(`"messages":[{`)) {
		t.Errorf("the converted body has no user turn in it (the conversation really is empty of representable content, so an empty turn is the honest request): %s", wire)
	}
}

// TestAQwenKeylessRemoteOnAnotherHostDoesNotKeepAStaleKey is A-F3.
func TestAQwenKeylessRemoteOnAnotherHostDoesNotKeepAStaleKey(t *testing.T) {
	const remoteBase = "http://10.9.9.9:8088/v1"

	home := t.TempDir()
	setLaunchTestHome(t, home)
	setQwenTestHome(t, home)

	// A launch against an AUTHENTICATED remote stores its token.
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"`+remoteBase+`","api_key":"sk-owned-by-another-host"}]}`)
	if err := (&Qwen{}).Configure("box/big-model"); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	storedKey := func() string {
		t.Helper()
		cfg := qwenConfig(t, home)
		envCfg, _ := cfg["env"].(map[string]any)
		s, _ := envCfg[qwenOllamaEnvKey].(string)
		return s
	}
	if got := storedKey(); got != "sk-owned-by-another-host" {
		t.Fatalf("control: the writer stored %q, want the remote's token", got)
	}

	// The remote loses its credential and is reached on ANOTHER host, where the
	// writer blanks the stored value.
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"`+remoteBase+`"}]}`)
	if got := (&Qwen{}).CurrentModel(); got != "" {
		t.Errorf("CurrentModel = %q with OLLAMA_API_KEY still holding %q against a remote that now issues no credential: a launch would be skipped, and the stale bearer stays in settings.json and in the provider entry's envKey — sent to a host the user has stopped authenticating to",
			got, storedKey())
	}
}
