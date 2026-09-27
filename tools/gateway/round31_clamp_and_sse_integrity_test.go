package main

// round31_clamp_and_sse_integrity_test.go — three ways a guard stopped
// guarding (2026-09-27 audit, round 31).
//
// The output-budget clamp rewrites max_tokens with a Go int, and the
// context-fit clamp sixty lines below reads the same field as a float64 only:
// the request that asks for more output than the model can possibly fit keeps
// its budget and goes upstream as the 400 the fit clamp exists to prevent,
// while the modest request beside it is clamped and served.
//
// The SSE partial-line cap was a one-way door: scanSSE drains u.tail, and the
// cap check skipped the call that does the draining, so one oversized line
// stopped usage extraction for the rest of the stream — a served 200 metered
// as zero.
//
// loadConfig refuses an api_keys entry with an empty label ("empty labels
// authenticate by digest but then 401 and unmeter") and accepted the identical
// shape in pull_license_keys, where the empty label turns the licensee's
// correct key into "license_invalid".

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestARequestTheOutputBudgetClampedStillGetsTheContextFitClamp is finding 1.
func TestARequestTheOutputBudgetClampedStillGetsTheContextFitClamp(t *testing.T) {
	var seen []float64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req map[string]any
		json.Unmarshal(b, &req)
		v, _ := req["max_tokens"].(float64)
		seen = append(seen, v)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		// No usage object: the calibrator must not move the estimate between
		// the two requests, so the two budgets are comparable.
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()

	cfg := gwConfig{
		UpstreamAddr: upstream.URL, ListenAddr: ":0", LedgerPath: filepath.Join(t.TempDir(), "l.jsonl"),
		APIKeys: []gwKey{{SHA256: keyHash("sk-new"), Label: "openrouter"}},
		Models: []gwModel{{
			ID: "kat-awq", UpstreamID: "kat-awq", OwnedBy: "oaica",
			// A window the prompt below cannot fit inside at 32768 output:
			// ~240,000 bytes is ~60,000 tokens, and the margin is 30% of that.
			ContextLength: 100000, MaxCompletionTokens: 32768,
			Pricing: gwPricing{Prompt: "0.00000005", Completion: "0.00000012"},
		}},
	}
	g := &gateway{}
	if err := g.apply(cfg); err != nil {
		t.Fatalf("apply: %v", err)
	}
	srv := httptest.NewServer(mux(g))
	defer srv.Close()

	prompt := strings.Repeat("x", 240000)
	ask := func(maxTokens int) {
		body, _ := json.Marshal(map[string]any{
			"model":      "kat-awq",
			"messages":   []map[string]any{{"role": "user", "content": prompt}},
			"max_tokens": maxTokens,
			"stream":     true,
		})
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer sk-new")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("max_tokens=%d: status %d, want the gateway to clamp and forward", maxTokens, resp.StatusCode)
		}
	}

	ask(30000)  // under the model's 32768: stays float64, so the fit clamp sees it
	ask(100000) // over it: the output-budget clamp rewrites the field to a Go int
	if len(seen) != 2 {
		t.Fatalf("upstream saw %d requests, want 2", len(seen))
	}
	modest, greedy := seen[0], seen[1]
	if modest >= 30000 {
		t.Fatalf("control: the fit clamp did not apply to the modest ask (upstream saw %v), so this test cannot tell whether the greedy ask was clamped", modest)
	}
	if greedy >= 32768 {
		t.Errorf("upstream saw max_tokens=%v for a request asking 100000 with a 100000-token window: the output-budget clamp rewrote the field to an int, the context-fit clamp only reads float64, so the clamp was skipped and the gateway forwarded the request its own comment says is already guaranteed to 400", greedy)
	}
}

// sseOverlongLine is one data: line longer than the 1 MB partial-line cap,
// with no newline, delivered in 32 KB pieces the way a wedged upstream would.
func sseOverlongLine() [][]byte {
	line := append([]byte(`data: {"choices":[{"delta":{"content":"`), bytes.Repeat([]byte("x"), 1<<20)...)
	var pieces [][]byte
	for i := 0; i < len(line); i += 32 << 10 {
		end := i + 32<<10
		if end > len(line) {
			end = len(line)
		}
		pieces = append(pieces, line[i:end])
	}
	return pieces
}

const sseTailAfter = "\"}}]}\n\ndata: {\"choices\":[{\"delta\":{}}],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":5}}\n\ndata: [DONE]\n\n"

// TestAnOverlongSSELineDoesNotStopUsageExtraction is finding 2.
func TestAnOverlongSSELineDoesNotStopUsageExtraction(t *testing.T) {
	feed := func(withOverlong bool) *usageRecorder {
		u := &usageRecorder{ResponseWriter: httptest.NewRecorder(), stream: true}
		if withOverlong {
			for _, p := range sseOverlongLine() {
				u.Write(p)
			}
		} else {
			u.Write([]byte(`data: {"choices":[{"delta":{"content":"`))
		}
		u.Write([]byte(sseTailAfter))
		return u
	}

	control := feed(false)
	if !control.seen || control.usage.PromptTokens != 11 || control.usage.CompletionTokens != 5 {
		t.Fatalf("control: seen=%v usage=%+v; the usage chunk on its own line must be parsed — this test cannot tell whether the overlong line broke anything", control.seen, control.usage)
	}

	u := feed(true)
	if !u.seen || u.usage.PromptTokens != 11 || u.usage.CompletionTokens != 5 {
		t.Errorf("seen=%v usage=%+v after an overlong line: the tail cap skipped the very call that drains the tail, so the buffer stays over the cap and every later line is dropped too — a served 200 metered as zero", u.seen, u.usage)
	}
}

// TestAnOverlongSSELineDoesNotStallTranslation is finding 2 on the Anthropic
// bridge, where the same gate also stops translation rather than only usage.
func TestAnOverlongSSELineDoesNotStallTranslation(t *testing.T) {
	rec := httptest.NewRecorder()
	b := newAnthropicBridge(rec, true, "m")
	for _, p := range sseOverlongLine() {
		b.writeStream(p)
	}
	b.writeStream([]byte("\"}}]}\n\n"))
	b.writeStream([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}\n\n"))

	if !strings.Contains(rec.Body.String(), "hello") {
		t.Errorf("the chunk after the overlong line never reached the client (body %q): the tail cap skipped the write that feeds the translator, and the buffer never falls back under the cap", rec.Body.String())
	}
	if b.sse.stopMsg != "stop" {
		t.Errorf("stopMsg=%q, want \"stop\": the translator is still stuck on the abandoned line", b.sse.stopMsg)
	}
}

// TestLoadConfig_RejectsPullLicenseKeyWithEmptyLabel is finding 3.
func TestLoadConfig_RejectsPullLicenseKeyWithEmptyLabel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg.json")
	cfg := `{
	  "api_keys": [{"sha256": "` + strings.Repeat("a", 64) + `", "label": "k"}],
	  "models": [{"id": "m", "owned_by": "oaica"}],
	  "pull_license_keys": [{"sha256": "` + strings.Repeat("b", 64) + `", "label": ""}],
	  "pull_catalog": [{"model": "y", "source": "file", "file_path": "/tmp/y.gguf", "size_bytes": 7, "license_required": true}]
	}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loadConfig(path)
	if err == nil {
		t.Fatalf("loadConfig accepted a pull_license_keys entry with an empty label: the gateway starts, then tells the licensee their correct key is \"license_invalid\" — the same shape api_keys refuses at load")
	}
	if !strings.Contains(err.Error(), "label") {
		t.Fatalf("err = %v, want a label-validation failure naming the field", err)
	}
}
