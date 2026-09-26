package launch

// context_calibration_leg_isolation_integrity_test.go — one leg's prompt-size
// calibration decided another leg's context clamp (2026-09-26 audit, tenth
// round, auditor B, HIGH).
//
// The fit margin is |body − sample| scaled by the session's measured
// tokens-per-byte, and the sample is one successful response's REAL
// prompt_tokens for one body size. Keying that on the session alone
// (anthropic_openai_proxy.go's calibKey = table.SessionID) means a plan whose
// tiers sit on different base URLs share one sample: a 600 KB turn on a
// 262k-window leg taught the clamp that every later body of a different size
// is "unmeasured delta", and the delta for a 40 KB body came out ~42,000
// tokens against a 32k window — so EVERY turn on the small leg was refused
// locally, with a message that cannot be true ("10000 tokens > 32768
// maximum") and no in-session escape, because compaction only shrinks the body
// and the margin is anchored to the other leg's 600 KB.
//
// A real prompt's tokens-per-byte is a property of the leg's tokenizer, and
// the sample is one leg's measurement. The key carries the leg.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// calibrationUpstream answers with a chosen prompt_tokens, which is what the
// clamp calibrates against.
func calibrationUpstream(t *testing.T, hits *int, promptTokens int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"PONG"},"finish_reason":"stop"}],"usage":{"prompt_tokens":` +
			strconv.Itoa(promptTokens) + `,"completion_tokens":1,"total_tokens":2}}`))
	}))
}

func postSizedTurn(t *testing.T, proxy, model string, padBytes int) int {
	t.Helper()
	body := `{"model":"` + model + `","max_tokens":10,"messages":[{"role":"user","content":"` +
		strings.Repeat("x", padBytes) + `"}]}`
	req, err := http.NewRequest(http.MethodPost, proxy+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Logf("model %s body %d bytes -> %d: %.200s", model, padBytes, resp.StatusCode, b)
	}
	return resp.StatusCode
}

// A turn sized for one leg's window must not be judged by another leg's
// calibration.
func TestOneLegsCalibrationDoesNotClampAnotherLeg(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	bigHits, smallHits := 0, 0
	big := calibrationUpstream(t, &bigHits, 150000)
	defer big.Close()
	small := calibrationUpstream(t, &smallHits, 1)
	defer small.Close()

	bigRoute := proxyRoute{Label: "remote:big", BaseURL: big.URL + "/v1", Key: "k",
		UpstreamModel: "big-m", ContextWindow: 262144, Wire: "openai"}
	smallRoute := proxyRoute{Label: "remote:small", BaseURL: small.URL + "/v1", Key: "k",
		UpstreamModel: "small-m", ContextWindow: 32768, Wire: "openai"}

	proxy := startProxyWithOversize(t, proxyRouteTable{
		SessionID: "one-launch-shared-by-every-tier",
		Default:   bigRoute,
		ByModel:   map[string]proxyRoute{"big": bigRoute, "small": smallRoute},
	})

	// A large turn on the big leg: served, and it records the session's
	// calibrated tokens-per-byte.
	if code := postSizedTurn(t, proxy, "big", 600000); code != http.StatusOK {
		t.Fatalf("the big leg answered %d for a 600 KB turn, want 200 (premise)", code)
	}
	if bigHits == 0 {
		t.Fatalf("the big leg was never reached, so no calibration was recorded (premise)")
	}

	// The same session's smaller tier: 40 KB is ~10k tokens, comfortably
	// inside a 32k window, and this leg has no calibration of its own.
	if code := postSizedTurn(t, proxy, "small", 40000); code != http.StatusOK {
		t.Errorf("the small leg answered %d for a 40 KB turn (~10k tokens into a 32768-token window), want 200 — the local clamp computed its margin from the OTHER leg's 600 KB calibration sample, so the margin alone exceeded the whole window and refused a prompt that plainly fits", code)
	}
	if smallHits == 0 {
		t.Errorf("the small leg was never reached, so the turn was refused locally")
	}
}
