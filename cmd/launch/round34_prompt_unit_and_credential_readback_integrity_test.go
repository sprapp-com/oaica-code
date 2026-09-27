package launch

// round34_prompt_unit_and_credential_readback_integrity_test.go — the client
// proxy's prompt unit charging bytes it never sends, in both directions, and a
// credential Qwen's reader does not read back (2026-09-27 audit, round 34,
// A-F1/F2/F3).
//
//   - A-F1: clientPromptBytes charged the raw Anthropic request body, so every
//     block the converter DROPS was still billed. A session with extended
//     thinking echoes its own reasoning back on the next turn, and the OpenAI
//     wire has no thinking block (api.Message.Thinking is never emitted by
//     chatRequestToOpenAI, and redacted_thinking is opaque), so ~800 KB of
//     reasoning was charged as ~200k prompt tokens of a 262144-token leg. The
//     clamp then refused a healthy turn LOCALLY with "prompt is too long" —
//     the wording Claude Code matches to its compaction recovery path — and
//     compaction cannot help, because the thinking blocks are in the newest
//     turns; a locally refused request reports no usage, so the session can
//     never calibrate out of it. Round 33 fixed the same unit's image
//     over-charge; this is the other half of "charge what the upstream will
//     tokenize".
//   - A-F2: the same walk discounted any map keyed "source" holding a "data"
//     string, so a document block with a TEXT source had its whole text
//     replaced by the 4096-byte image allowance although the converter writes
//     that text into the prompt. A 1.2 MB attachment was charged as 4 KB, the
//     clamp forwarded a request the upstream must reject, and the recovery then
//     depended on the upstream's 400 matching upstreamContextOverflowRE — an
//     OpenAI-worded remote yields a 502 api_error the client does not
//     recognise as an overflow.
//   - A-F3: Qwen's writer stores the remote's token in env.OLLAMA_API_KEY and
//     CurrentModel read only the model name and base URL, so after the remote
//     rotated its key the store still read as current, the managed launch
//     skipped the rewrite, and Qwen started against the remote with the
//     revoked bearer — the same class round 33 closed for OMP and Hermes.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ollama/ollama/anthropic"
)

// round34MessagesBody serializes one Anthropic turn whose single message
// carries the given content blocks.
func round34MessagesBody(t *testing.T, role string, content []map[string]any, maxTokens int) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"model": "kat-awq", "max_tokens": maxTokens, "stream": false,
		"messages": []map[string]any{{"role": role, "content": content}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestAPromptUnitDoesNotChargeReasoningTheUpstreamNeverSees is A-F1.
func TestAPromptUnitDoesNotChargeReasoningTheUpstreamNeverSees(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	const text = "real conversation text. "
	blob := strings.Repeat("QUJD", 300000) // 1.2 MB of redacted_thinking payload

	plain := round34MessagesBody(t, "assistant", []map[string]any{
		{"type": "text", "text": strings.Repeat(text, 2000)},
	}, 4096)
	reasoning := round34MessagesBody(t, "assistant", []map[string]any{
		{"type": "text", "text": strings.Repeat(text, 2000)},
		{"type": "thinking", "thinking": strings.Repeat("deliberation. ", 5000), "signature": "sig"},
		{"type": "redacted_thinking", "data": blob},
	}, 4096)

	// The unit: the two bodies differ only in blocks the OpenAI wire drops, so
	// they must measure as the same prompt. The remaining delta is the JSON
	// wrapper of the two dropped blocks.
	plainBytes, reasoningBytes := clientPromptBytes(plain), clientPromptBytes(reasoning)
	if delta := reasoningBytes - plainBytes; delta > 4096 {
		t.Errorf("clientPromptBytes = %d for a turn with thinking/redacted_thinking and %d for the same turn without them (raw bodies %d and %d): %d bytes of reasoning the converter never forwards are charged to the prompt, so a session that echoes its own thinking is billed for tokens it never sends",
			reasoningBytes, plainBytes, len(reasoning), len(plain), delta)
	}

	// Behaviourally: the proxy must serve the turn rather than refuse it
	// locally with the compaction-trigger wording.
	called := false
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		called = true
		mu.Unlock()
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "model": "kat-awq",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "ok"},
			}},
			"usage": map[string]any{"prompt_tokens": len(b) / 4, "completion_tokens": 1},
		})
	}))
	defer upstream.Close()

	proxyURL := startCalibProxy(t, upstream.URL, "sess-round34-reasoning")
	resp, err := http.Post(proxyURL+"/v1/messages", "application/json", bytes.NewReader(reasoning))
	if err != nil {
		t.Fatal(err)
	}
	rb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("a turn whose bytes are mostly reasoning the upstream never sees was refused: status=%d body=%s; want 200 — the real prompt is ~%d tokens of a 262144 window, and a local refusal here is the string Claude Code treats as \"compact the conversation\", which cannot help because the reasoning is in the newest turns",
			resp.StatusCode, strings.TrimSpace(string(rb)), len(plain)/4)
	}
	mu.Lock()
	reached := called
	mu.Unlock()
	if !reached {
		t.Errorf("the upstream was never called: the turn was answered without the model")
	}

	// And the check that this is not passing by the proxy dropping the whole
	// turn: the real text must still reach the upstream.
	seen := string(round34ConvertedBody(t, reasoning))
	if !strings.Contains(seen, strings.Repeat(text, 2000)) {
		t.Errorf("the converted prompt lost the conversation text: %d bytes measured, and the text the model must answer about is not in it", len(seen))
	}
}

// TestADocumentTextSourceIsChargedTheTextItForwards is A-F2.
func TestADocumentTextSourceIsChargedTheTextItForwards(t *testing.T) {
	doc := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 20000) // ~880 KB

	asText := round34MessagesBody(t, "user", []map[string]any{
		{"type": "text", "text": "See the attached file."},
		{"type": "text", "text": doc},
	}, 64)
	asDocument := round34MessagesBody(t, "user", []map[string]any{
		{"type": "text", "text": "See the attached file."},
		{"type": "document", "source": map[string]any{
			"type": "text", "media_type": "text/plain", "data": doc,
		}},
	}, 64)

	textBytes, docBytes := clientPromptBytes(asText), clientPromptBytes(asDocument)
	if delta := textBytes - docBytes; delta > 4096 || delta < -4096 {
		t.Errorf("the same attachment measures %d bytes as a text block and %d as a document block: the walk discounts any map keyed \"source\", so a document with a text source has its whole content replaced by the image allowance while the converter writes that text into the prompt — a %d-byte attachment is then charged as 4 KB and the context-fit clamp forwards a request the upstream must reject",
			textBytes, docBytes, len(doc))
	}

	// The premise: the text really is forwarded, so charging it is correct
	// rather than a second over-charge to trade for this one.
	converted := string(round34ConvertedBody(t, asDocument))
	if !strings.Contains(converted, doc) {
		t.Fatalf("the fixture does not forward the document's text (%d converted bytes), so this test cannot show the charge is wrong", len(converted))
	}
}

// TestAQwenStoreHoldingAReplacedRemoteTokenIsDrift is A-F3.
func TestAQwenStoreHoldingAReplacedRemoteTokenIsDrift(t *testing.T) {
	const remoteBase = "http://10.9.9.9:8088/v1"

	home := t.TempDir()
	setLaunchTestHome(t, home)
	setQwenTestHome(t, home)

	configure := func(live string) {
		t.Helper()
		writeRemotes(t, `{"remotes":[{"name":"box","base_url":"`+remoteBase+`","api_key":"`+live+`"}]}`)
		if err := (&Qwen{}).Configure("box/big-model"); err != nil {
			t.Fatalf("Configure: %v", err)
		}
	}
	storedKey := func() string {
		t.Helper()
		cfg := qwenConfig(t, home)
		envCfg, _ := cfg["env"].(map[string]any)
		s, _ := envCfg[qwenOllamaEnvKey].(string)
		return s
	}
	rotateStored := func(revoked string) {
		t.Helper()
		cfg := qwenConfig(t, home)
		envCfg, _ := cfg["env"].(map[string]any)
		envCfg[qwenOllamaEnvKey] = revoked
		data, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".qwen", "settings.json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Control: the writer's own output reads as current.
	configure("live-token")
	if got := storedKey(); got != "live-token" {
		t.Fatalf("control: the writer stored %q, want the remote's live token", got)
	}
	if got := (&Qwen{}).CurrentModel(); got != "box/big-model" {
		t.Fatalf("control: CurrentModel = %q, want box/big-model (the fixture must be the shape the writer leaves, or this test cannot show the credential is unread)", got)
	}

	// The documented path: the user re-keys the remote, the store keeps the old
	// token, and the launcher must notice.
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"`+remoteBase+`","api_key":"rotated-token"}]}`)
	rotateStored("revoked-token")
	ep, _ := resolveRemoteEndpoint("box/big-model")
	if got := (&Qwen{}).CurrentModel(); got != "" {
		t.Errorf("Qwen's config holds a credential the remote has replaced (%s: %q, live: %q) and still reports %q as current: the managed launch skips applyQwenOllamaConfig and Qwen starts with the revoked bearer",
			qwenOllamaEnvKey, storedKey(), ep.Token, got)
	}
}

// round34ConvertedBody returns the OpenAI-wire body the proxy builds for an
// Anthropic request, so a test can inspect what the upstream would be asked.
func round34ConvertedBody(t *testing.T, body []byte) []byte {
	t.Helper()
	var req anthropic.MessagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	chatReq, err := anthropic.FromMessagesRequest(req)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	oai := chatRequestToOpenAI(chatReq, req, req.Model)
	b, err := json.Marshal(oai)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
