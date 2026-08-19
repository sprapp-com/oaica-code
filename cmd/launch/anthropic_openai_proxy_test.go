package launch

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestProxyHonorsPerRequestModel verifies the opusplan tier-split mechanism:
// the proxy forwards WHICHEVER model each Anthropic request carries
// (anthReq.Model), not just the fixed upstreamModel it was started with. This
// is what lets claude.go point ANTHROPIC_DEFAULT_OPUS_MODEL and
// ANTHROPIC_DEFAULT_SONNET_MODEL at two different bare ids through one proxy.
func TestProxyHonorsPerRequestModel(t *testing.T) {
	var gotModels []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &req)
		gotModels = append(gotModels, req.Model)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":    "chatcmpl-1",
			"model": req.Model,
			"choices": []map[string]any{{
				"index":         0,
				"finish_reason": "stop",
				"message":       map[string]any{"role": "assistant", "content": "ok"},
			}},
		})
	}))
	defer upstream.Close()

	remote := userRemote{Name: "test", BaseURL: upstream.URL, APIKey: "test-key"}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go RunAnthropicOpenAIProxy(ln, remote, "glm-5.3")
	proxyURL := "http://" + ln.Addr().String()

	post := func(model string) {
		body, _ := json.Marshal(map[string]any{
			"model":      model,
			"max_tokens": 10,
			"messages":   []map[string]any{{"role": "user", "content": "hi"}},
		})
		resp, err := http.Post(proxyURL+"/v1/messages", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("proxy returned %d: %s", resp.StatusCode, b)
		}
	}

	// Opus-tier request (the model the proxy was started with).
	post("glm-5.3")
	// Sonnet-tier request — a DIFFERENT model, as claude.go's --sonnet-model
	// would set via ANTHROPIC_DEFAULT_SONNET_MODEL.
	post("muse-spark-1.2")

	if len(gotModels) != 2 {
		t.Fatalf("upstream got %d requests, want 2", len(gotModels))
	}
	if gotModels[0] != "glm-5.3" {
		t.Errorf("first upstream model = %q, want glm-5.3", gotModels[0])
	}
	if gotModels[1] != "muse-spark-1.2" {
		t.Errorf("second upstream model = %q, want muse-spark-1.2 (tier split not honored)", gotModels[1])
	}
}

// TestProxyFallsBackToFixedModelWhenRequestOmitsIt covers the
// byte-identical-for-non-split-launches guarantee: a request with no model
// field (or an empty one) still resolves to the proxy's fixed upstreamModel.
func TestProxyFallsBackToFixedModelWhenRequestOmitsIt(t *testing.T) {
	var gotModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &req)
		gotModel = req.Model
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-1",
			"model":   req.Model,
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "ok"}}},
		})
	}))
	defer upstream.Close()

	remote := userRemote{Name: "test", BaseURL: upstream.URL, APIKey: "test-key"}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go RunAnthropicOpenAIProxy(ln, remote, "deepseek-v4-flash")
	proxyURL := "http://" + ln.Addr().String()

	body, _ := json.Marshal(map[string]any{
		"max_tokens": 10,
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
	})
	resp, err := http.Post(proxyURL+"/v1/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("proxy returned %d: %s", resp.StatusCode, b)
	}
	if gotModel != "deepseek-v4-flash" {
		t.Errorf("upstream model = %q, want fixed upstreamModel deepseek-v4-flash (regression: non-split launch changed)", gotModel)
	}
}
