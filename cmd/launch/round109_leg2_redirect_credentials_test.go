package launch

// round109_leg2_redirect_credentials_test.go — leg 2, round 109 (2026-09-29 audit),
// F109-L2-1. See credentialSafeRedirect.

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// TestMine109ARedirectDoesNotCarryTheKeyToAnotherHost sends an Anthropic-wire
// turn to an upstream that answers 307 to a second host, and reads what the
// second host received.
func TestMine109ARedirectDoesNotCarryTheKeyToAnotherHost(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	var mu sync.Mutex
	var seen http.Header
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = r.Header.Clone()
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	t.Cleanup(evil.Close)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(up.Close)
	route := round54Route(up.URL, "m", "anthropic")
	url := r108Serve(t, proxyRouteTable{Default: route, ByModel: map[string]proxyRoute{"m": route}})
	if code, body := r108Message(t, url, 16); code != 200 {
		t.Fatalf("premise: the redirected turn answered %d %s", code, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen == nil {
		t.Fatal("premise: the redirect was not followed, so nothing was asked of the second host")
	}
	for _, h := range []string{"X-Api-Key", "Api-Key", "Authorization", "X-Session-Id", "Cookie"} {
		if v := seen.Get(h); v != "" {
			t.Errorf("the second host received %s: %q — a redirect to another host carries no credential (2026-09-29 audit, round 109, F109-L2-1)", h, v)
		}
	}
}

// TestMine109ASameHostRedirectKeepsTheKey is the control: a vendor that moves an
// endpoint on its own host keeps working.
func TestMine109ASameHostRedirectKeepsTheKey(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	var mu sync.Mutex
	var key string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/messages" {
			http.Redirect(w, r, "/moved", http.StatusTemporaryRedirect)
			return
		}
		mu.Lock()
		key = r.Header.Get("X-Api-Key")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	t.Cleanup(up.Close)
	route := round54Route(up.URL, "m", "anthropic")
	url := r108Serve(t, proxyRouteTable{Default: route, ByModel: map[string]proxyRoute{"m": route}})
	r108Message(t, url, 16)
	mu.Lock()
	defer mu.Unlock()
	if key == "" {
		t.Errorf("a same-host redirect arrived without the key — a vendor that moves an endpoint on its own host must keep working (2026-09-29 audit, round 109, F109-L2-1)")
	}
}
