package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// --- store-level tests (mirror cmd/launch's, same file, same semantics) ---

func TestPromptCalibrator_SessionIsolationAndSanityBounds(t *testing.T) {
	c := newPromptCalibrator(4)
	if _, ok := c.estimate("s1", 1000); ok {
		t.Error("expected no calibration before any record")
	}
	c.record("s1", 10_000, 3_000)
	if est, ok := c.estimate("s1", 20_000); !ok || est != 6_000 {
		t.Errorf("expected 6000, got %d (ok=%v)", est, ok)
	}
	// (c) session keys must not bleed into one another.
	if _, ok := c.estimate("s2", 20_000); ok {
		t.Error("expected s2 to have no calibration")
	}
	for _, bad := range []struct {
		key                 string
		bytes, promptTokens int
	}{
		{"high", 100, 100_000}, // 1000 tok/byte
		{"low", 100_000, 10},   // 0.0001 tok/byte
		{"zero", 10_000, 0},    // no usage reported
		{"negbytes", 0, 3_000}, // no body
		{"", 10_000, 3_000},    // no session key
	} {
		c.record(bad.key, bad.bytes, bad.promptTokens)
		if _, ok := c.estimate(bad.key, 10_000); ok {
			t.Errorf("expected %q to be rejected as a calibration source", bad.key)
		}
	}
}

// TestPromptCalibrator_BoundedMapEviction: the gateway sees every client's
// sessions, so the map must stay bounded rather than grow forever.
func TestPromptCalibrator_BoundedMapEviction(t *testing.T) {
	const max = 8
	c := newPromptCalibrator(max)
	for i := 0; i < 200; i++ {
		c.record(fmt.Sprintf("sess-%d", i), 10_000, 3_000)
	}
	c.mu.Lock()
	n := len(c.samples)
	c.mu.Unlock()
	if n != max {
		t.Fatalf("expected the map bounded to %d entries, got %d", max, n)
	}
	// Eviction is oldest-last-seen-first: the most recent max survive.
	for i := 200 - max; i < 200; i++ {
		if _, ok := c.estimate(fmt.Sprintf("sess-%d", i), 10_000); !ok {
			t.Errorf("expected recent session sess-%d to survive", i)
		}
	}
	if _, ok := c.estimate("sess-0", 10_000); ok {
		t.Error("expected the oldest session to have been evicted")
	}
}

func TestContextFitPlan_GatewayUncalibratedIsLegacyBehaviour(t *testing.T) {
	// (a) No calibration -> the pre-2026-08-30 chars/4 + max(30%, 4096).
	c := newPromptCalibrator(4)
	for _, n := range []int{1000, 400_000, 920_580} {
		est, margin, calibrated := contextFitPlan(c, "nobody", n)
		if calibrated {
			t.Fatalf("expected the uncalibrated path for %d bytes", n)
		}
		wantEst := n / 4
		wantMargin := int(float64(wantEst) * 0.30)
		if wantMargin < 4096 {
			wantMargin = 4096
		}
		if est != wantEst || margin != wantMargin {
			t.Errorf("%d bytes: got (%d,%d), want (%d,%d)", n, est, margin, wantEst, wantMargin)
		}
	}
}

func TestGatewayPromptTooLongMessage_AnthropicWording(t *testing.T) {
	// (d) Claude Code matches this sentence to take its recovery path.
	got := promptTooLongMessage(243_000, 262_128)
	if !strings.HasPrefix(got, "prompt is too long: ") ||
		!strings.Contains(got, " tokens > ") || !strings.Contains(got, " maximum") {
		t.Errorf("expected 'prompt is too long: N tokens > M maximum', got %q", got)
	}
}

// --- end-to-end through the gateway's completion handler ---

func calibGatewayFor(t *testing.T, upstreamURL string) *gateway {
	t.Helper()
	cfg := gwConfig{
		UpstreamAddr: upstreamURL, ListenAddr: ":0",
		LedgerPath: filepath.Join(t.TempDir(), "ledger.jsonl"),
		APIKeys:    []gwKey{{SHA256: keyHash("sk-new"), Label: "openrouter"}},
		Models: []gwModel{{
			ID: "kat-awq", UpstreamID: "kat-awq-served", OwnedBy: "oaica",
			ContextLength: 262144, MaxCompletionTokens: 32768,
			Pricing: gwPricing{Prompt: "0.00000005", Completion: "0.00000012"},
		}},
	}
	g := &gateway{}
	if err := g.apply(cfg); err != nil {
		t.Fatalf("apply: %v", err)
	}
	return g
}

// calibGatewayBody returns the request body and the exact messages-byte
// count the clamp and the calibrator both key on (see messagesBytes).
func calibGatewayBody(t *testing.T, contentLen, maxTokens int) ([]byte, int) {
	t.Helper()
	msgs := []map[string]any{{"role": "user", "content": strings.Repeat("x", contentLen)}}
	body, err := json.Marshal(map[string]any{
		"model": "kat-awq", "messages": msgs, "max_tokens": maxTokens, "stream": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	mb, _ := json.Marshal(msgs)
	return body, len(mb)
}

func calibGatewayPost(t *testing.T, srvURL, sessionID string, body []byte) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", srvURL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-new")
	if sessionID != "" {
		req.Header.Set("X-Session-Id", sessionID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(b)
}

// TestContextFitClamp_Gateway_CalibratedEstimateSavesTheIncident mirrors the
// client-proxy test: the 2026-08-29 shape (an ~806 KB Claude Code body,
// chars/4 estimate 201,670 x 1.30 over a 262,144 window, REAL prompt
// ~243,000 with ~19,000 to spare) must be rejected before calibration and
// forwarded after it, and the rejection must use Anthropic's wording.
func TestContextFitClamp_Gateway_CalibratedEstimateSavesTheIncident(t *testing.T) {
	const realRatio = 0.30
	var promptTokensToReport int
	var gotMaxTokens float64
	var upstreamCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		var req map[string]any
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &req)
		gotMaxTokens, _ = req["max_tokens"].(float64)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"index":0,"delta":{"content":"ok"}}]}`)
		fmt.Fprintf(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":2,\"total_tokens\":%d}}\n\n",
			promptTokensToReport, promptTokensToReport+2)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	g := calibGatewayFor(t, upstream.URL)
	srv := httptest.NewServer(mux(g))
	defer srv.Close()

	bigBody, bigMsgBytes := calibGatewayBody(t, 808_000, 32000)
	estUncal := bigMsgBytes / 4
	if budget := 262144 - estUncal - int(float64(estUncal)*0.30); budget >= 16 {
		t.Fatalf("fixture no longer reproduces the incident: uncalibrated budget is %d", budget)
	}

	// (a) Uncalibrated: the old behaviour -- reject, never reach upstream.
	status, respBody := calibGatewayPost(t, srv.URL, "sess-gw-incident", bigBody)
	if status != http.StatusBadRequest {
		t.Fatalf("uncalibrated: expected 400, got %d: %s", status, respBody)
	}
	if upstreamCalls != 0 {
		t.Fatalf("uncalibrated: expected no upstream call, got %d", upstreamCalls)
	}
	// (d) Anthropic's wording. Decode: encoding/json escapes '>' as >.
	var errResp struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(respBody), &errResp); err != nil {
		t.Fatalf("bad error JSON: %v (%s)", err, respBody)
	}
	if !strings.HasPrefix(errResp.Error.Message, "prompt is too long: ") ||
		!strings.Contains(errResp.Error.Message, " tokens > ") ||
		!strings.Contains(errResp.Error.Message, " maximum") {
		t.Errorf("expected 'prompt is too long: N tokens > M maximum', got %q", errResp.Error.Message)
	}
	if errResp.Error.Type != "invalid_request_error" {
		t.Errorf("expected invalid_request_error, got %q", errResp.Error.Type)
	}

	// One successful turn of the SAME session reports its real prompt size
	// (in the stream's final usage-only chunk).
	smallBody, smallMsgBytes := calibGatewayBody(t, 20_000, 1024)
	promptTokensToReport = int(float64(smallMsgBytes) * realRatio)
	if status, respBody = calibGatewayPost(t, srv.URL, "sess-gw-incident", smallBody); status != 200 {
		t.Fatalf("calibration turn: expected 200, got %d: %s", status, respBody)
	}

	// (b) The identical big body now fits, with max_tokens clamped into the
	// real headroom instead of being rejected outright.
	upstreamCalls, gotMaxTokens = 0, 0
	if status, respBody = calibGatewayPost(t, srv.URL, "sess-gw-incident", bigBody); status != 200 {
		t.Fatalf("calibrated: expected the incident's request FORWARDED, got %d: %s", status, respBody)
	}
	if upstreamCalls != 1 {
		t.Fatalf("calibrated: expected one upstream call, got %d", upstreamCalls)
	}
	estCal := int(int64(bigMsgBytes) * int64(promptTokensToReport) / int64(smallMsgBytes))
	margin := int(float64(estCal) * calibratedMarginRatio)
	if margin < calibratedMarginFloor {
		margin = calibratedMarginFloor
	}
	if want := float64(262144 - estCal - margin); gotMaxTokens != want {
		t.Errorf("expected max_tokens clamped to the calibrated budget %v, got %v", want, gotMaxTokens)
	}
	if gotMaxTokens < 10_000 {
		t.Errorf("expected a genuinely useful budget (~19k of real headroom), got %v", gotMaxTokens)
	}

	// (c) A different session must not inherit the calibration.
	upstreamCalls = 0
	if status, _ = calibGatewayPost(t, srv.URL, "sess-gw-other", bigBody); status != http.StatusBadRequest {
		t.Fatalf("a different session must not inherit the calibration, got %d", status)
	}
	if upstreamCalls != 0 {
		t.Fatalf("a different session must not reach upstream, got %d calls", upstreamCalls)
	}
}

// TestContextFitClamp_Gateway_TranslatesUpstreamOverflow: an upstream
// context-overflow 400 is rewritten for the CLIENT into Anthropic's
// "prompt is too long" wording (so an Anthropic-shaped caller recovers
// instead of retrying forever), while the upstream-error LOG keeps the
// verbatim upstream text for diagnosis.
func TestContextFitClamp_Gateway_TranslatesUpstreamOverflow(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"object":"error","message":"This model's maximum context length is 262144 tokens. However, you requested 392144 tokens (260000 in the messages, 132144 in the completion). Please reduce the length of the messages or completion.","type":"BadRequestError"}`)
	}))
	defer upstream.Close()

	g := calibGatewayFor(t, upstream.URL)
	srv := httptest.NewServer(mux(g))
	defer srv.Close()

	body, _ := calibGatewayBody(t, 400_000, 32000)
	status, respBody := calibGatewayPost(t, srv.URL, "sess-gw-overflow", body)
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400 passed through, got %d: %s", status, respBody)
	}
	var errResp struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(respBody), &errResp); err != nil {
		t.Fatalf("bad error JSON: %v (%s)", err, respBody)
	}
	if !strings.HasPrefix(errResp.Error.Message, "prompt is too long: 260000 tokens > 262144 maximum") {
		t.Errorf("expected the translated Anthropic wording with the upstream's real numbers, got %q",
			errResp.Error.Message)
	}
}
