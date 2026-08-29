package launch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// startCalibProxy boots RunAnthropicOpenAIProxyRoutes against upstreamURL
// with the given per-launch session id and returns its base URL.
func startCalibProxy(t *testing.T, upstreamURL, sessionID string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	table := proxyRouteTable{
		SessionID: sessionID,
		Default:   proxyRoute{BaseURL: upstreamURL, UpstreamModel: "kat-awq", ContextWindow: 262144, Label: "primary"},
	}
	go RunAnthropicOpenAIProxyRoutes(ln, table)
	url := "http://" + ln.Addr().String()
	time.Sleep(50 * time.Millisecond)
	return url
}

func calibMessagesBody(t *testing.T, contentLen, maxTokens int, stream bool) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"model":      "kat-awq",
		"max_tokens": maxTokens,
		"stream":     stream,
		"messages":   []map[string]any{{"role": "user", "content": strings.Repeat("x", contentLen)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestPromptCalibrator_RecordAndEstimate covers the store itself: session
// isolation, the sanity bounds on an absurd ratio, and bounded eviction.
func TestPromptCalibrator_RecordAndEstimate(t *testing.T) {
	c := newPromptCalibrator(4)

	if _, ok := c.estimate("s1", 1000); ok {
		t.Error("expected no calibration before any record")
	}
	c.record("s1", 10_000, 3_000) // 0.30 tokens/byte
	est, ok := c.estimate("s1", 20_000)
	if !ok || est != 6_000 {
		t.Errorf("expected a calibrated 6000, got %d (ok=%v)", est, ok)
	}
	// (c) A different session must NOT inherit another session's ratio.
	if _, ok := c.estimate("s2", 20_000); ok {
		t.Error("expected session s2 to have no calibration of its own")
	}

	// Absurd ratios are ignored rather than trusted: a bogus pairing that
	// clamped every future request to nothing would be worse than chars/4.
	c.record("bad-high", 100, 100_000) // 1000 tokens/byte
	if _, ok := c.estimate("bad-high", 100); ok {
		t.Error("expected an absurdly high ratio to be rejected")
	}
	c.record("bad-low", 100_000, 10) // 0.0001 tokens/byte
	if _, ok := c.estimate("bad-low", 100_000); ok {
		t.Error("expected an absurdly low ratio to be rejected")
	}
	// Never calibrate off a zero/absent usage count.
	c.record("zero", 10_000, 0)
	if _, ok := c.estimate("zero", 10_000); ok {
		t.Error("expected a zero prompt_tokens to be ignored")
	}
	c.record("", 10_000, 3_000)
	if _, ok := c.estimate("", 10_000); ok {
		t.Error("expected an empty session key to be ignored")
	}
}

func TestPromptCalibrator_BoundedEviction(t *testing.T) {
	c := newPromptCalibrator(3)
	for i := 0; i < 10; i++ {
		c.record(fmt.Sprintf("s%d", i), 10_000, 3_000)
	}
	c.mu.Lock()
	n := len(c.samples)
	c.mu.Unlock()
	if n != 3 {
		t.Fatalf("expected the map bounded to 3 sessions, got %d", n)
	}
	// Oldest-last-seen-first: s0..s6 evicted, the newest three survive.
	for _, k := range []string{"s7", "s8", "s9"} {
		if _, ok := c.estimate(k, 10_000); !ok {
			t.Errorf("expected the recent session %q to survive eviction", k)
		}
	}
	if _, ok := c.estimate("s0", 10_000); ok {
		t.Error("expected the oldest session to have been evicted")
	}
	// Re-recording an existing key must refresh it, not grow the map.
	c.record("s9", 10_000, 3_100)
	c.mu.Lock()
	n = len(c.samples)
	c.mu.Unlock()
	if n != 3 {
		t.Fatalf("expected an update of an existing key not to grow the map, got %d", n)
	}
}

func TestContextFitPlan_UncalibratedMatchesLegacyBehaviour(t *testing.T) {
	// (a) With no calibration the plan must be byte-for-byte the old
	// chars/4 + max(30%, 4096) rule.
	c := newPromptCalibrator(8)
	for _, bodyBytes := range []int{1000, 400_000, 806_000} {
		est, margin, calibrated := contextFitPlan(c, "unknown", bodyBytes)
		if calibrated {
			t.Fatalf("expected the uncalibrated path for %d bytes", bodyBytes)
		}
		wantEst := bodyBytes / 4
		wantMargin := int(float64(wantEst) * 0.30)
		if wantMargin < 4096 {
			wantMargin = 4096
		}
		if est != wantEst || margin != wantMargin {
			t.Errorf("bodyBytes=%d: got est=%d margin=%d, want est=%d margin=%d",
				bodyBytes, est, margin, wantEst, wantMargin)
		}
	}
}

func TestParseUpstreamContextOverflow(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantPrompt int
		wantMax    int
		wantOK     bool
	}{
		{
			name:       "vllm full message with in-the-messages breakdown",
			body:       `{"error":{"message":"This model's maximum context length is 262144 tokens. However, you requested 262145 tokens (230145 in the messages, 32000 in the completion). Please reduce the length of the messages or completion.","type":"BadRequestError"}}`,
			wantPrompt: 230145, wantMax: 262144, wantOK: true,
		},
		{
			name:       "no breakdown falls back to the requested total",
			body:       "This model's maximum context length is 8192 tokens. However, you requested 9000 tokens. Please reduce.",
			wantPrompt: 9000, wantMax: 8192, wantOK: true,
		},
		{name: "unrelated 400", body: `{"error":{"message":"model not found"}}`},
		{name: "empty", body: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prompt, max, ok := parseUpstreamContextOverflow(tc.body)
			if ok != tc.wantOK || prompt != tc.wantPrompt || max != tc.wantMax {
				t.Errorf("got (%d,%d,%v), want (%d,%d,%v)", prompt, max, ok, tc.wantPrompt, tc.wantMax, tc.wantOK)
			}
		})
	}
}

func TestPromptTooLongMessage_UsesAnthropicWording(t *testing.T) {
	// (d) Claude Code matches on this exact sentence to take its
	// context-recovery path -- see promptTooLongMessage.
	got := promptTooLongMessage(243_000, 262_128)
	if !strings.HasPrefix(got, "prompt is too long: ") {
		t.Errorf("message must START with Anthropic's wording, got %q", got)
	}
	if !strings.Contains(got, " tokens > ") || !strings.Contains(got, " maximum") {
		t.Errorf("message must contain ' tokens > ' and ' maximum', got %q", got)
	}
	if !strings.Contains(got, "243000") || !strings.Contains(got, "262128") {
		t.Errorf("message must carry both counts, got %q", got)
	}
}

// TestContextFitClamp_ClientProxy_CalibratedEstimateSavesTheIncident is the
// 2026-08-29 incident itself, end to end: an ~806 KB Claude Code body whose
// chars/4 estimate (201,670) x 1.30 overflows a 262,144 window, but whose
// REAL prompt is ~243,000 tokens with ~19,000 to spare. Uncalibrated the
// proxy rejects it (and that rejection killed a compaction call, so the
// session could never shrink). After one successful response has reported
// the session's real prompt_tokens, the same body must be forwarded.
func TestContextFitClamp_ClientProxy_CalibratedEstimateSavesTheIncident(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	const realRatio = 0.30 // tokens per request byte, the incident's own ratio
	var promptTokensToReport int
	var lastMaxTokens int
	var upstreamCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		var req struct {
			MaxTokens int `json:"max_tokens"`
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &req)
		lastMaxTokens = req.MaxTokens
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "model": "kat-awq",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "ok"},
			}},
			"usage": map[string]any{
				"prompt_tokens": promptTokensToReport, "completion_tokens": 2,
				"total_tokens": promptTokensToReport + 2,
			},
		})
	}))
	defer upstream.Close()

	// The incident's body: ~806 KB of conversation.
	bigBody := calibMessagesBody(t, 808_000, 32000, false)
	estUncal := len(bigBody) / 4
	if margin := int(float64(estUncal) * 0.30); 262144-estUncal-margin >= 16 {
		t.Fatalf("fixture no longer reproduces the incident: uncalibrated budget is %d",
			262144-estUncal-margin)
	}

	// (a) No calibration yet -> the old behaviour: reject, never call upstream.
	proxyURL := startCalibProxy(t, upstream.URL, "sess-incident")
	resp, err := http.Post(proxyURL+"/v1/messages", "application/json", bytes.NewReader(bigBody))
	if err != nil {
		t.Fatal(err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("uncalibrated: expected 400, got %d: %s", resp.StatusCode, respBody)
	}
	if upstreamCalls != 0 {
		t.Fatalf("uncalibrated: expected no upstream call, got %d", upstreamCalls)
	}
	// (d) and in Anthropic's wording. Decode first: encoding/json escapes
	// '>' as >, so a raw substring match on the body would never hit.
	var errResp struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respBody, &errResp); err != nil {
		t.Fatalf("bad error JSON: %v (%s)", err, respBody)
	}
	if !strings.HasPrefix(errResp.Error.Message, "prompt is too long: ") ||
		!strings.Contains(errResp.Error.Message, " tokens > ") ||
		!strings.Contains(errResp.Error.Message, " maximum") {
		t.Errorf("expected Anthropic's 'prompt is too long: N tokens > M maximum', got %q", errResp.Error.Message)
	}
	if errResp.Error.Type != "invalid_request_error" {
		t.Errorf("expected invalid_request_error, got %q", errResp.Error.Type)
	}

	// One successful small turn of the SAME session reports the real count.
	smallBody := calibMessagesBody(t, 20_000, 1024, false)
	promptTokensToReport = int(float64(len(smallBody)) * realRatio)
	resp, err = http.Post(proxyURL+"/v1/messages", "application/json", bytes.NewReader(smallBody))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("calibration turn: expected 200, got %d", resp.StatusCode)
	}

	// (b) Now the identical big body is estimated at ~243,000 tokens, leaving
	// real room -- forward it, with max_tokens clamped into that room.
	upstreamCalls = 0
	resp, err = http.Post(proxyURL+"/v1/messages", "application/json", bytes.NewReader(bigBody))
	if err != nil {
		t.Fatal(err)
	}
	respBody, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("calibrated: expected the incident's request to be FORWARDED, got %d: %s",
			resp.StatusCode, respBody)
	}
	if upstreamCalls != 1 {
		t.Fatalf("calibrated: expected exactly one upstream call, got %d", upstreamCalls)
	}
	// Same integer math the calibrator uses, from the pair it actually saw.
	estCal := int(int64(len(bigBody)) * int64(promptTokensToReport) / int64(len(smallBody)))
	margin := int(float64(estCal) * calibratedMarginRatio)
	if margin < calibratedMarginFloor {
		margin = calibratedMarginFloor
	}
	wantMax := 262144 - estCal - margin
	if lastMaxTokens != wantMax {
		t.Errorf("expected max_tokens clamped to the calibrated budget %d, got %d", wantMax, lastMaxTokens)
	}
	if lastMaxTokens < 10_000 {
		t.Errorf("expected a genuinely useful budget (~19k of real headroom), got %d", lastMaxTokens)
	}

	// (c) A different session must not inherit that calibration: a fresh
	// launch (its own session id) falls back to chars/4 and rejects again.
	upstreamCalls = 0
	otherURL := startCalibProxy(t, upstream.URL, "sess-other")
	resp, err = http.Post(otherURL+"/v1/messages", "application/json", bytes.NewReader(bigBody))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a different session must not inherit the calibration, got %d", resp.StatusCode)
	}
	if upstreamCalls != 0 {
		t.Fatalf("a different session must not reach upstream, got %d calls", upstreamCalls)
	}
}

// TestContextFitClamp_ClientProxy_StreamCalibratesFromFinalUsageChunk covers
// (f): a streaming response reports prompt_tokens only in its final
// usage-only SSE chunk, which must feed the calibration just like a
// non-streaming usage object.
func TestContextFitClamp_ClientProxy_StreamCalibratesFromFinalUsageChunk(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	const realRatio = 0.30
	var promptTokensToReport int
	var upstreamCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl, _ := w.(http.Flusher)
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"index":0,"delta":{"role":"assistant","content":"ok"}}]}`)
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		// The final usage-only chunk (stream_options.include_usage).
		fmt.Fprintf(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":2,\"total_tokens\":%d}}\n\n",
			promptTokensToReport, promptTokensToReport+2)
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	defer upstream.Close()

	proxyURL := startCalibProxy(t, upstream.URL, "sess-stream")

	smallBody := calibMessagesBody(t, 20_000, 1024, true)
	promptTokensToReport = int(float64(len(smallBody)) * realRatio)
	resp, err := http.Post(proxyURL+"/v1/messages", "application/json", bytes.NewReader(smallBody))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("stream calibration turn: expected 200, got %d", resp.StatusCode)
	}

	// The incident-shaped body would be rejected on chars/4 alone; with the
	// stream-derived calibration it must be forwarded.
	bigBody := calibMessagesBody(t, 808_000, 32000, true)
	upstreamCalls = 0
	resp, err = http.Post(proxyURL+"/v1/messages", "application/json", bytes.NewReader(bigBody))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || upstreamCalls != 1 {
		t.Fatalf("expected the stream-calibrated request to be forwarded, got status %d after %d upstream calls",
			resp.StatusCode, upstreamCalls)
	}
}

// TestContextFitClamp_ClientProxy_TranslatesUpstreamOverflow covers (e): a
// vLLM context-overflow 400 must come back to the client as Anthropic's
// "prompt is too long" 400 (not the generic "upstream HTTP 400: {...}"
// wrapper, which Claude Code cannot recognise), AND its ground-truth
// message-token count must seed the session calibration.
func TestContextFitClamp_ClientProxy_TranslatesUpstreamOverflow(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	var upstreamCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"object":"error","message":"This model's maximum context length is 262144 tokens. However, you requested 392144 tokens (260000 in the messages, 132144 in the completion). Please reduce the length of the messages or completion.","type":"BadRequestError","code":400}`)
	}))
	defer upstream.Close()

	proxyURL := startCalibProxy(t, upstream.URL, "sess-overflow")

	// 400,000 chars: passes the uncalibrated clamp (est 100k + 30k margin),
	// so it actually reaches upstream and gets the overflow back.
	body := calibMessagesBody(t, 400_000, 32000, false)
	resp, err := http.Post(proxyURL+"/v1/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if upstreamCalls != 1 {
		t.Fatalf("expected the request to reach upstream, got %d calls", upstreamCalls)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected the overflow re-emitted as 400 (not 502), got %d: %s", resp.StatusCode, respBody)
	}
	var errResp struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respBody, &errResp); err != nil {
		t.Fatalf("bad error JSON: %v (%s)", err, respBody)
	}
	if errResp.Error.Type != "invalid_request_error" {
		t.Errorf("expected invalid_request_error, got %q", errResp.Error.Type)
	}
	if !strings.HasPrefix(errResp.Error.Message, "prompt is too long: 260000 tokens > 262144 maximum") {
		t.Errorf("expected the translated Anthropic wording with the upstream's real numbers, got %q",
			errResp.Error.Message)
	}
	if strings.Contains(errResp.Error.Message, "upstream HTTP") {
		t.Errorf("expected the generic upstream wrapper to be replaced, got %q", errResp.Error.Message)
	}

	// The upstream's 260,000-in-the-messages is ground truth for this
	// session: the same body must now be rejected CLIENT-side (est 260,000
	// + 3% margin leaves no room), without touching upstream again.
	upstreamCalls = 0
	resp, err = http.Post(proxyURL+"/v1/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	respBody, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected the seeded calibration to reject client-side, got %d", resp.StatusCode)
	}
	if upstreamCalls != 0 {
		t.Fatalf("expected no second upstream call once calibrated, got %d", upstreamCalls)
	}
	// Decode rather than substring-match: encoding/json escapes '>' as >.
	if err := json.Unmarshal(respBody, &errResp); err != nil {
		t.Fatalf("bad error JSON: %v (%s)", err, respBody)
	}
	if !strings.HasPrefix(errResp.Error.Message, "prompt is too long: 260000 tokens > ") {
		t.Errorf("expected the calibrated estimate in the rejection, got %q", errResp.Error.Message)
	}
}
