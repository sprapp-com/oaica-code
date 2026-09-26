package launch

// context_calibration_same_host_leg_isolation_integrity_test.go — two legs on
// ONE host shared a calibration slot, so they clamped each other
// (2026-09-26 audit, twelfth round).
//
// The tenth round fixed exactly this defect for two legs on DIFFERENT base
// URLs, keying the calibration slot on session + leg. The leg was taken to be
// the base URL alone — but a base URL is not a leg's identity, and the code
// says so itself: sameRoute (context_window_remote.go) is "same base URL AND
// same upstream model", and the documented same-remote tier split
// (`--model box/small --sonnet-model box/big`) puts two models with two
// tokenizers and two context windows behind one URL. Those two legs then shared
// a slot, which is the same defect one level down: a 600 KB turn on the big leg
// taught the clamp that every later body of a different size is unmeasured
// delta, and the delta for a 40 KB turn (~10k tokens) came out ~42,000 tokens
// against the small leg's 32,768-token window — refused locally, with a message
// that cannot be true and no in-session escape, because compaction only shrinks
// the body while the margin stays anchored to the other model's 600 KB.
//
// Tokens-per-byte is a property of the tokenizer, so the slot has to be one per
// model, not one per host.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// perModelCalibrationUpstream answers each upstream model with its OWN real
// prompt_tokens, which is what happens on a host serving two models.
func perModelCalibrationUpstream(t *testing.T, tokens map[string]int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		asked := ""
		for model := range tokens {
			if strings.Contains(string(raw), `"`+model+`"`) {
				asked = model
				break
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"PONG"},"finish_reason":"stop"}],"usage":{"prompt_tokens":` +
			strconv.Itoa(tokens[asked]) + `,"completion_tokens":1,"total_tokens":2}}`))
	}))
}

func TestTwoModelsOnOneHostDoNotShareACalibrationSlot(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	host := perModelCalibrationUpstream(t, map[string]int{"big-m": 150000, "small-m": 1})
	defer host.Close()

	// One host, one key, two models with two windows: the shape
	// `--model box/small --sonnet-model box/big` produces.
	bigRoute := proxyRoute{Label: "remote:box/big", BaseURL: host.URL + "/v1", Key: "k",
		UpstreamModel: "big-m", ContextWindow: 262144, Wire: "openai"}
	smallRoute := proxyRoute{Label: "remote:box/small", BaseURL: host.URL + "/v1", Key: "k",
		UpstreamModel: "small-m", ContextWindow: 32768, Wire: "openai"}

	proxy := startProxyWithOversize(t, proxyRouteTable{
		SessionID: "one-launch-one-host-two-models",
		Default:   bigRoute,
		ByModel:   map[string]proxyRoute{"big": bigRoute, "small": smallRoute},
	})

	// A large turn on the big leg: served, and it records a sample of
	// 150,000 tokens for a 600 KB body.
	if code := postSizedTurn(t, proxy, "big", 600000); code != http.StatusOK {
		t.Fatalf("the big leg answered %d for a 600 KB turn, want 200 (premise)", code)
	}

	// The same host's smaller model: 40 KB is ~10k tokens, comfortably inside
	// its 32,768-token window, and that model has no calibration of its own.
	if code := postSizedTurn(t, proxy, "small", 40000); code != http.StatusOK {
		t.Errorf("the small model answered %d for a 40 KB turn (~10k tokens into a 32768-token window), want 200 — the local clamp computed its margin from the OTHER model's 600 KB sample, because the calibration slot was keyed on the base URL alone; the margin alone then exceeded the whole window and refused a prompt that plainly fits", code)
	}
}
