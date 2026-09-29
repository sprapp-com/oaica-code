package launch

// round99_leg2_relay_surface_test.go — leg 2, F99-L2-1 (2026-09-29 audit,
// round 99).
//
// A peer that is this tree's own server decides a turn by the surface the client
// arrived on: the marks that lift upstream ollama's tool-call gate for a client
// that declared no tools, that keep a merged turn's run order, and that let an
// Anthropic client ask for thinking against a model which cannot think, live in
// the gin context of the process that SAW the client. A relayed request arrives
// with none of them, so round 96 gave the relay a header that states it
// (api.RelayedSurfaceHeader) and round 97 taught the peer to apply the pair of
// marks the Anthropic surface sets locally (F97-L1-1).
//
// This leg is a relay of the same kind — its only client wire is Anthropic — and
// it stated nothing, so the peer answered it as the native wire:
//
//	one Anthropic body carrying thinking, one model with no thinking capability
//	  runner lane (round 97's pin)  200
//	  through this relay            400 "\"glm-5.3\" does not support thinking"
//
// The upstream request now states the surface its client arrived on. A vendor
// upstream ignores the header; a peer of this tree applies the marks, which is
// the whole point of stating it.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ollama/ollama/api"
)

func TestARelayedTurnStatesTheSurfaceItsClientArrivedOn(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	var mu sync.Mutex
	seen := make([]http.Header, 0, 2)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`))
	}))
	defer up.Close()

	route := round54Route(up.URL, "glm-5.3", "openai")
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"glm-5.3": route})

	for _, stream := range []bool{false, true} {
		body, _ := json.Marshal(map[string]any{
			"model": "glm-5.3", "max_tokens": 16, "stream": stream,
			"thinking": map[string]any{"type": "enabled", "budget_tokens": 1024},
			"messages": []map[string]any{{"role": "user", "content": "ping"}},
		})
		req, err := http.NewRequest(http.MethodPost, proxy+"/v1/messages", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("content-type", "application/json")
		req.Header.Set("x-api-key", "proxy-client-token")
		req.Header.Set("anthropic-version", "2023-06-01")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream=%v: the client saw %d %s — premise: this route answers the turn", stream, resp.StatusCode, strings.TrimSpace(string(out)))
		}

		mu.Lock()
		hdr := seen[len(seen)-1]
		mu.Unlock()
		if got := hdr.Get(api.RelayedSurfaceHeader); got != "anthropic" {
			t.Errorf("stream=%v: the relay stated %s=%q, want \"anthropic\" — a peer of this tree decides the turn by the surface the client arrived on, and one Anthropic body reached this client as 400 through the relay where the runner lane answers it 200 (2026-09-29 audit, round 99, F99-L2-1)",
				stream, api.RelayedSurfaceHeader, got)
		}
	}
}
