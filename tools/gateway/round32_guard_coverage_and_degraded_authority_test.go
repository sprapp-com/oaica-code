package main

// round32_guard_coverage_and_degraded_authority_test.go — what the prompt
// guards are actually measuring, which decisions count as authoritative, and
// which request shape reaches them at all (2026-09-27 audit, round 32, B-F1/F2/F3
// plus the sub-bar clamp/log note).
//
//   - B-F1: messagesBytes serialized req["messages"] with an inline image's
//     base64 left in it, so a screenshot was billed to the prompt as its
//     transport encoding. A 1 MB PNG (~1.4 MB of base64) read as 349563 tokens
//     against a model that publishes 262144 and accepts images -- a hard 400
//     the client could not recover from, because the calibrator then refused
//     the upstream's real measurement (ratio 0.00114 tok/byte, under
//     calibMinRatio) and every later turn in the session failed the same way.
//   - B-F2: checkWindowCap degraded exactly like the status probe (meterhub
//     unreachable / failing) but had no way to say so, so fetchAndDecide
//     stamped the result authoritatively and an active subscriber stayed 403'd
//     for the whole EntitlementCacheTTLSec after a blip -- or, fail-open,
//     spent past their cap for the same window.
//   - B-F3: /v1/completions is served by the same handler, but a legacy body
//     says "prompt", not "messages", and both guards read only the latter: a
//     1 MB prefill was admitted with estTokens 0 -- straight past the
//     admission pool and the fit clamp.
//   - The output-budget clamp compared `int(v) > limit` on a float64, so a
//     client asking for 1e19 wrapped to a negative, skipped the clamp
//     entirely and was forwarded verbatim -- and the error-log read that
//     exists to correlate "estTokens + maxTokens vs context_length" read only
//     one of the two keys the clamp writes.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// postJSON sends body to path through the gateway's real mux.
func postJSON(t *testing.T, g *gateway, path, apiKey, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	w := httptest.NewRecorder()
	mux(g).ServeHTTP(w, req)
	return w.Code, strings.TrimSpace(w.Body.String())
}

// TestAnInlineImageIsChargedAsAnImageNotItsBase64 is B-F1.
func TestAnInlineImageIsChargedAsAnImageNotItsBase64(t *testing.T) {
	var seen atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		seen.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","choices":[{"message":{"content":"seen"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1200,"completion_tokens":3}}`)
	}))
	defer upstream.Close()

	g := &gateway{}
	if err := g.apply(gwConfig{
		UpstreamAddr: upstream.URL, ListenAddr: ":0", LedgerPath: filepath.Join(t.TempDir(), "l.jsonl"),
		APIKeys: []gwKey{{SHA256: keyHash("sk"), Label: "k"}},
		Models: []gwModel{{
			ID: "kat-awq", OwnedBy: "oaica",
			ContextLength: 262144, MaxCompletionTokens: 32768,
			InputModalities: []string{"text", "image"},
			Pricing:         gwPricing{Prompt: "0.00000005", Completion: "0.00000012"},
		}},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	imageBody := func(imageBytes int) (string, map[string]any) {
		b64 := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x41}, imageBytes))
		m := map[string]any{
			"model": "kat-awq",
			"messages": []map[string]any{{
				"role": "user",
				"content": []map[string]any{
					{"type": "text", "text": "what is in this screenshot?"},
					{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + b64}},
				},
			}},
			"max_tokens": 2048,
		}
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		// Decode it back the way the handler does: the guards see the
		// canonical JSON types, not this file's typed literals.
		var decoded map[string]any
		if err := json.Unmarshal(b, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return string(b), decoded
	}

	// The estimate is a function of the image being present, not of how many
	// bytes its base64 happens to be: a transport encoding is not prompt.
	small, smallMap := imageBody(64 << 10)
	large, largeMap := imageBody(2 << 20)
	if got, want := messagesBytes(largeMap), messagesBytes(smallMap); got != want {
		t.Errorf("messagesBytes(2MB image) = %d, messagesBytes(64KB image) = %d: the prompt estimate is charging the base64 transport, so a screenshot is billed as its own encoded size (%d tokens for a 2MB image)", got, want, messagesBytes(largeMap)/4)
	}

	// The documented path: a Claude Code paste or a bridge-inlined image.
	if code, body := postJSON(t, g, "/v1/chat/completions", "sk", large); code != http.StatusOK {
		t.Errorf("1MB+ inline image: status = %d, body = %s; want 200 -- the model publishes context_length 262144 and input_modalities [text image], and the gateway refused the image for being too big as its base64 rather than as an image", code, body)
	}
	if code, body := postJSON(t, g, "/v1/chat/completions", "sk", small); code != http.StatusOK {
		t.Errorf("64KB inline image: status = %d, body = %s; want 200", code, body)
	}
	if seen.Load() == 0 {
		t.Fatal("upstream never saw an image request: this test cannot tell whether the clamp or something else refused it")
	}

	// The upstream answered with a real prompt_tokens count (1200). The
	// calibrator is the gateway's own way to learn that an image is not its
	// base64; a sample that measures a sane ratio must be kept, not thrown
	// away as bogus, or the session never recovers.
	c := g.calibrator()
	c.mu.Lock()
	samples := len(c.samples)
	c.mu.Unlock()
	if samples == 0 {
		t.Error("the calibrator stored no sample after a successful image request: the ground truth the upstream reported was measured against the base64-inflated size and discarded as below calibMinRatio, so the session stays on chars/4 and every later image turn fails the same way")
	}
}

// TestADegradedUsageCheckIsNotCachedForTheWholeTTL is B-F2, both policies.
func TestADegradedUsageCheckIsNotCachedForTheWholeTTL(t *testing.T) {
	const degradedTTL = 5 * time.Second
	// Comfortably past the degraded TTL, well inside the 60s entitlement TTL
	// the bug cached the degraded answer for.
	const settle = degradedTTL + 300*time.Millisecond

	newMeterhub := func(usageBroken, over *atomic.Bool, hits *atomic.Int64) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/subscribers/get":
				json.NewEncoder(w).Encode(map[string]string{"key_label": "alice", "status": "active"})
			case "/subscribers/usage":
				hits.Add(1)
				if usageBroken.Load() {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				json.NewEncoder(w).Encode(map[string]any{
					"key_label": "alice", "plan": "starter",
					"window_5h": map[string]any{"tokens": 0, "over": over.Load()},
					"window_7d": map[string]any{"tokens": 0, "over": false},
				})
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
	}

	t.Run("fail_closed", func(t *testing.T) {
		var usageBroken, over atomic.Bool
		var hits atomic.Int64
		usageBroken.Store(true)
		meterhub := newMeterhub(&usageBroken, &over, &hits)
		defer meterhub.Close()

		var got map[string]any
		g := newTestGatewayWithEntitlement(t, fakeUpstream(t, &got).URL, meterhub.URL, false)

		if w := postCompletion(t, g, "sk-active"); w.Code != http.StatusForbidden {
			t.Fatalf("usage endpoint broken: status = %d, body = %s, want 403 (fail-closed)", w.Code, w.Body.String())
		}
		usageBroken.Store(false)
		if w := postCompletion(t, g, "sk-active"); w.Code != http.StatusForbidden {
			t.Fatalf("immediately after repair: status = %d, want 403 (the degraded answer is still within its 5s TTL)", w.Code)
		}
		time.Sleep(settle)
		w := postCompletion(t, g, "sk-active")
		if w.Code != http.StatusOK {
			t.Errorf("6s after the usage endpoint recovered: status = %d, body = %s; want 200 -- a degraded usage answer was cached for the whole EntitlementCacheTTLSec (60s), so one meterhub blip took a paying subscriber's key out of service for a minute", w.Code, w.Body.String())
		}
		if n := hits.Load(); n < 2 {
			t.Errorf("/subscribers/usage probed %d time(s) across three requests spanning a recovery; the gateway never re-probed, so the degraded answer stuck for the full entitlement TTL", n)
		}
	})

	t.Run("fail_open", func(t *testing.T) {
		var usageBroken, over atomic.Bool
		var hits atomic.Int64
		usageBroken.Store(true)
		meterhub := newMeterhub(&usageBroken, &over, &hits)
		defer meterhub.Close()

		var got map[string]any
		g := newTestGatewayWithEntitlement(t, fakeUpstream(t, &got).URL, meterhub.URL, true)

		if w := postCompletion(t, g, "sk-active"); w.Code != http.StatusOK {
			t.Fatalf("usage endpoint broken, fail-open: status = %d, want 200 (a meterhub hiccup must not block traffic)", w.Code)
		}
		// Meterhub recovers and reports this key has gone over its plan cap.
		usageBroken.Store(false)
		over.Store(true)
		time.Sleep(settle)
		w := postCompletion(t, g, "sk-active")
		if w.Code != http.StatusTooManyRequests {
			t.Errorf("cap exceeded, 6s after a degraded usage check was cached: status = %d, body = %s; want 429 rate_limited -- the degraded admission was stamped authoritative, so an over-cap key kept spending for the full entitlement TTL after recovery", w.Code, w.Body.String())
		}
		if n := hits.Load(); n < 2 {
			t.Errorf("/subscribers/usage probed %d time(s); the gateway never re-probed the cap after the degraded answer", n)
		}
	})
}

// TestALegacyPromptBodyIsGuardedLikeAMessagesBody is B-F3.
func TestALegacyPromptBodyIsGuardedLikeAMessagesBody(t *testing.T) {
	var mu atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		mu.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","choices":[{"text":"ok"}],"usage":{"prompt_tokens":9,"completion_tokens":1}}`)
	}))
	defer upstream.Close()

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

	huge := strings.Repeat("A", 1<<20)
	chat := `{"model":"kat-awq","messages":[{"role":"user","content":"` + huge + `"}],"max_tokens":2048}`
	legacy := `{"model":"kat-awq","prompt":"` + huge + `","max_tokens":2048}`

	// Control: the message-shaped body is refused by the fit clamp, never
	// forwarded.
	if code, _ := postJSON(t, g, "/v1/chat/completions", "sk", chat); code != http.StatusBadRequest {
		t.Fatalf("control: 1MB of prompt via /v1/chat/completions: status = %d, want 400 from the context-fit clamp", code)
	}
	if code, body := postJSON(t, g, "/v1/completions", "sk", legacy); code != http.StatusBadRequest {
		t.Errorf("the same 1MB prefill via /v1/completions: status = %d, body = %s; want the same 400 -- this route is served by the same handler, but a legacy body carries \"prompt\" instead of \"messages\", so both prompt guards measured it as zero tokens and let it through to the upstream as a raw multi-hundred-thousand-token prefill", code, body)
	}
	if n := mu.Load(); n != 0 {
		t.Errorf("upstream received %d of the 2 oversized requests: the legacy route admitted a prompt both guards exist to refuse", n)
	}
}

// TestAnAbsurdOutputBudgetIsClampedAndLoggedAsItself is the sub-bar note from
// the same round: `int(v) > limit` on a float64 wraps to a negative for absurd
// asks, and the error-log correlation read one key of the two the clamp writes.
func TestAnAbsurdOutputBudgetIsClampedAndLoggedAsItself(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		if err := json.Unmarshal(b, &m); err == nil {
			mu.Lock()
			bodies = append(bodies, m)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"bad request"}}`)
	}))
	defer upstream.Close()

	g, errLogPath := newTestGatewayWithErrorLog(t, upstream.URL)

	post := func(body string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer sk-new")
		w := httptest.NewRecorder()
		mux(g).ServeHTTP(w, req)
	}

	// A client asking for a 1e19-token completion: the clamp exists so this
	// is rewritten to what the gateway publishes, non-streaming included.
	post(`{"model":"kat-awq","messages":[{"role":"user","content":"hi"}],"max_tokens":1e19}`)
	// The same budget under the other spelling, which the clamp also writes.
	post(`{"model":"kat-awq","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":200000}`)

	if len(bodies) != 2 {
		t.Fatalf("upstream saw %d requests, want 2", len(bodies))
	}
	if v := bodies[0]["max_tokens"]; v != float64(nonStreamMaxTokens) {
		t.Errorf("upstream received max_tokens = %v for a max_tokens:1e19 ask; want the clamp's %d -- int(1e19) wraps to a negative, so the comparison skipped the clamp and the absurd ask was forwarded verbatim", v, nonStreamMaxTokens)
	}
	if v := bodies[1]["max_completion_tokens"]; v != float64(nonStreamMaxTokens) {
		t.Errorf("upstream received max_completion_tokens = %v for a max_completion_tokens:200000 ask; want the clamp's %d", v, nonStreamMaxTokens)
	}

	deadline := time.Now().Add(2 * time.Second)
	var lines []string
	for {
		b, _ := os.ReadFile(errLogPath)
		if s := strings.TrimSpace(string(b)); s != "" {
			lines = strings.Split(s, "\n")
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no upstream error log line was written")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(lines) < 2 {
		t.Fatalf("upstream error log has %d line(s), want 2", len(lines))
	}
	// The field exists to correlate "estimated prompt + output budget vs the
	// model's context_length" (errCaptureInfo's doc), so it must be what was
	// actually sent for either spelling, and never a wrapped negative.
	for i, line := range []string{lines[0], lines[1]} {
		var got upstreamErrorLogLine
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("bad JSON line %d: %v", i, err)
		}
		if got.MaxTokens != nonStreamMaxTokens {
			t.Errorf("error-log row %d: max_tokens = %d, want the clamped %d (the row reads one key of the two the clamp writes, and int(1e19) wraps to a negative)", i, got.MaxTokens, nonStreamMaxTokens)
		}
	}
}
