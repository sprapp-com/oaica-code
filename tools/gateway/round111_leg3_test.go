package main

// round111_leg3_test.go — leg 3, round 111 (2026-09-29 audit), F111-L3-1..4.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const r111Completion = `{"id":"c1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`

// --- F111-L3-1: request header hygiene --------------------------------------------

// TestMine111RequestHeadersThatNameTheCallerOrTheInsideAreNotForwarded: round 109
// stopped the gateway's own key and the caller's X-Forwarded-For reaching a foreign
// upstream and left every other header through: Cloudflare's Cf-Connecting-Ip and
// True-Client-Ip, Forwarded, X-Real-Ip, and a client's own X-Gatekeeper-*, X-Katlb-*
// and X-Oaica-Metered — control headers a client could set on the default upstream —
// plus credential-shaped headers of other vendors. A foreign upstream now gets an
// allowlist; the default upstream loses the control headers a client must not set.
func TestMine111RequestHeadersThatNameTheCallerOrTheInsideAreNotForwarded(t *testing.T) {
	t.Setenv("OAICA_GATEWAY_UPSTREAM_KEY", "GATEWAY-SECRET")
	var mu sync.Mutex
	seen := map[string]http.Header{}
	handler := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			mu.Lock()
			seen[name] = r.Header.Clone()
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, r111Completion)
		})
	}
	def := httptest.NewServer(handler("default"))
	ext := httptest.NewServer(handler("ext"))
	t.Cleanup(def.Close)
	t.Cleanup(ext.Close)
	g := &gateway{}
	price := gwPricing{Prompt: "0.00000005", Completion: "0.00000012"}
	if err := g.apply(gwConfig{UpstreamAddr: def.URL, ListenAddr: ":0", LedgerPath: filepath.Join(t.TempDir(), "ledger.jsonl"),
		APIKeys: []gwKey{{SHA256: keyHash("sk"), Label: "k"}},
		Models: []gwModel{
			{ID: "main", OwnedBy: "oaica", ContextLength: 262144, MaxCompletionTokens: 32768, Pricing: price},
			{ID: "ext", OwnedBy: "oaica", ContextLength: 262144, MaxCompletionTokens: 32768, Pricing: price, UpstreamAddr: ext.URL},
		}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux(g))
	t.Cleanup(srv.Close)

	for _, door := range []struct{ name, path, body string }{
		{"chat", "/v1/chat/completions", `{"model":"%s","messages":[{"role":"user","content":"go"}]}`},
		{"messages", "/v1/messages", `{"model":"%s","max_tokens":5,"messages":[{"role":"user","content":"go"}]}`},
	} {
		for _, m := range []string{"main", "ext"} {
			req, _ := http.NewRequest("POST", srv.URL+door.path, strings.NewReader(strings.Replace(door.body, "%s", m, 1)))
			req.Header.Set("Authorization", "Bearer sk")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json")
			req.Header.Set("User-Agent", "sdk/1")
			req.Header.Set("X-Session-Id", "sess-1")
			for k, v := range map[string]string{
				"Cf-Connecting-Ip": "203.0.113.9", "True-Client-Ip": "203.0.113.9", "X-Real-Ip": "203.0.113.9",
				"Forwarded": "for=203.0.113.9", "X-Forwarded-Host": "api.oaica.com",
				"X-Gatekeeper-Tier": "internal", "X-Katlb-Backend": "gpu-0", "X-Oaica-Metered": "0", "X-Oaica-Tier": "hijack",
				"X-Goog-Api-Key": "gk", "X-Auth-Token": "tok", "X-Amz-Security-Token": "amz", "Openai-Organization": "org",
			} {
				req.Header.Set(k, v)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("%s/%s: answered %d", door.name, m, resp.StatusCode)
			}
			mu.Lock()
			h := seen[map[string]string{"main": "default", "ext": "ext"}[m]]
			mu.Unlock()
			// Control headers a client must never set on either upstream.
			for _, bad := range []string{"X-Gatekeeper-Tier", "X-Katlb-Backend", "X-Oaica-Tier"} {
				if v := h.Get(bad); v != "" {
					t.Errorf("%s/%s: the upstream received the client's %s: %q (2026-09-29 audit, round 111, F111-L3-1)", door.name, m, bad, v)
				}
			}
			if m == "main" {
				if h.Get("X-Oaica-Metered") != "1" {
					t.Errorf("%s/main: X-Oaica-Metered is %q, want the gateway's own 1, not the client's (2026-09-29 audit, round 111, F111-L3-1)", door.name, h.Get("X-Oaica-Metered"))
				}
				continue
			}
			for _, bad := range []string{"Cf-Connecting-Ip", "True-Client-Ip", "X-Real-Ip", "Forwarded", "X-Forwarded-Host", "X-Forwarded-For",
				"X-Oaica-Metered", "X-Goog-Api-Key", "X-Auth-Token", "X-Amz-Security-Token", "Openai-Organization"} {
				if v := h.Get(bad); v != "" {
					t.Errorf("%s/ext: a third-party upstream received %s: %q — it is sent only what it needs (2026-09-29 audit, round 111, F111-L3-1)", door.name, bad, v)
				}
			}
			for _, need := range []string{"Content-Type", "Accept", "User-Agent", "X-Session-Id"} {
				if h.Get(need) == "" {
					t.Errorf("%s/ext: the third-party upstream lost %s (2026-09-29 audit, round 111, F111-L3-1)", door.name, need)
				}
			}
		}
	}
}

// --- F111-L3-2: trailers -----------------------------------------------------------

// TestMine111UpstreamTrailersAreScrubbedLikeHeaders: round 109's denylist filtered
// resp.Header and never resp.Trailer, so a cookie or an internal name an upstream
// sent as a trailer reached the public client — on /v1/messages as an ordinary
// header, because the bridge writes its own headers after the proxy has stored the
// trailers under a Trailer: prefix.
func TestMine111UpstreamTrailersAreScrubbedLikeHeaders(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Trailer", "Set-Cookie, X-Katlb-Backend, X-Upstream-Host, X-Ok-Trailer")
		io.WriteString(w, r111Completion)
		w.Header().Set("Set-Cookie", "sess=internal")
		w.Header().Set("X-Katlb-Backend", "gpu-7.internal")
		w.Header().Set("X-Upstream-Host", "10.9.8.7")
		w.Header().Set("X-Ok-Trailer", "fine")
	}))
	t.Cleanup(up.Close)
	srv, _ := r107Gw(t, up, nil)
	for _, c := range []struct{ path, body string }{
		{"/v1/chat/completions", `{"model":"kat-awq","messages":[{"role":"user","content":"go"}]}`},
		{"/v1/messages", `{"model":"kat-awq","max_tokens":5,"messages":[{"role":"user","content":"go"}]}`},
	} {
		req, _ := http.NewRequest("POST", srv.URL+c.path, strings.NewReader(c.body))
		req.Header.Set("Authorization", "Bearer sk")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		// The announcement: Go's client removes the Trailer header and lists the
		// declared names as KEYS of resp.Trailer, so a denied name that is still
		// declared shows up here even when its value is gone.
		for _, h := range []string{"Set-Cookie", "X-Katlb-Backend", "X-Upstream-Host"} {
			if _, declared := resp.Trailer[h]; declared {
				t.Errorf("%s: the caller was told to expect the trailer %s — the announcement names the inside as surely as the value does (2026-09-29 audit, round 111, F111-L3-2)", c.path, h)
			}
		}
		for _, h := range []string{"Set-Cookie", "X-Katlb-Backend", "X-Upstream-Host"} {
			if v := resp.Header.Get(h); v != "" {
				t.Errorf("%s: the caller received %s as a header: %q (2026-09-29 audit, round 111, F111-L3-2)", c.path, h, v)
			}
			if v := resp.Trailer.Get(h); v != "" {
				t.Errorf("%s: the caller received %s as a trailer: %q (2026-09-29 audit, round 111, F111-L3-2)", c.path, h, v)
			}
		}
		if c.path == "/v1/chat/completions" && resp.Trailer.Get("X-Ok-Trailer") != "fine" {
			t.Errorf("%s: an ordinary trailer was dropped: %v (2026-09-29 audit, round 111, F111-L3-2)", c.path, resp.Trailer)
		}
	}
}

// --- F111-L3-3: flat pricing --------------------------------------------------------

// TestMine111FlatPricingIsAFiniteNonNegativeDecimal: pricing_tiers were validated on
// load and the flat prompt/completion/cached_prompt prices were not, so NaN made a
// served turn's ledger row fail to encode and vanish, and a negative price booked a
// negative cost.
func TestMine111FlatPricingIsAFiniteNonNegativeDecimal(t *testing.T) {
	write := func(prompt, completion, cached string) string {
		p := filepath.Join(t.TempDir(), "gw.json")
		cfg := `{"upstream_addr":"http://127.0.0.1:1","api_keys":[{"label":"k","sha256":"` + keyHash("sk") + `"}],` +
			`"models":[{"id":"m","pricing":{"prompt":` + prompt + `,"completion":` + completion + `,"cached_prompt":` + cached + `}}]}`
		if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, bad := range [][3]string{
		{`"NaN"`, `"0.1"`, `""`}, {`"Inf"`, `"0.1"`, `""`}, {`"-1"`, `"0.1"`, `""`}, {`"1e999"`, `"0.1"`, `""`},
		{`"abc"`, `"0.1"`, `""`}, {`"0.1"`, `"NaN"`, `""`}, {`"0.1"`, `"-0.5"`, `""`}, {`"0.1"`, `"0.1"`, `"-2"`}, {`"0.1"`, `"0.1"`, `"NaN"`},
	} {
		if _, err := loadConfig(write(bad[0], bad[1], bad[2])); err == nil {
			t.Errorf("prices %v were accepted — a price the ledger cannot encode or that books a negative cost must be refused on load (2026-09-29 audit, round 111, F111-L3-3)", bad)
		}
	}
	for _, good := range [][3]string{{`"0.0000001"`, `"0.0000002"`, `""`}, {`"0"`, `"0"`, `""`}, {`"0.1"`, `"0.2"`, `"0.05"`}, {`""`, `""`, `""`}} {
		if _, err := loadConfig(write(good[0], good[1], good[2])); err != nil {
			t.Errorf("prices %v were refused: %v (2026-09-29 audit, round 111, F111-L3-3)", good, err)
		}
	}
}

// --- F111-L3-4: max_concurrent past int32 ---------------------------------------------

// TestMine111AMaxConcurrentPastInt32IsNotALockout: the cap was compared through
// int32(MaxConcurrent), which wraps negative at 2^31, so cur >= wrapped was always
// true and a config typo locked a paying key out of every door.
func TestMine111AMaxConcurrentPastInt32IsNotALockout(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, r111Completion)
	}))
	t.Cleanup(up.Close)
	for _, limit := range []int{2147483648, 4294967296} {
		srv, _ := r107Gw(t, up, []gwKey{{SHA256: keyHash("sk"), Label: "k", MaxConcurrent: limit}})
		for _, c := range []struct{ path, body string }{
			{"/v1/chat/completions", `{"model":"kat-awq","messages":[{"role":"user","content":"go"}]}`},
			{"/v1/messages", `{"model":"kat-awq","max_tokens":5,"messages":[{"role":"user","content":"go"}]}`},
		} {
			for i := 0; i < 2; i++ { // the second request sees the counter the first one created
				if code, _, body := r107Post(t, srv, c.path, c.body, 0); code != 200 {
					t.Errorf("max_concurrent %d, %s request %d answered %d %s — a huge limit is no limit, not a lockout (2026-09-29 audit, round 111, F111-L3-4)", limit, c.path, i+1, code, strings.TrimSpace(body))
				}
			}
		}
	}
}
