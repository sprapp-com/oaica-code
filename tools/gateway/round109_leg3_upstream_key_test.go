package main

// round109_leg3_upstream_key_test.go — leg 3, round 109 (2026-09-29 audit), F109-L3-3.
//
// OAICA_GATEWAY_UPSTREAM_KEY is the gateway's credential for its DEFAULT upstream
// (gatekeeper). A model may declare its own upstream_addr, and the field's own doc
// names "a third-party OpenAI endpoint" as a use, but the key was set on every
// request and every health probe whichever upstream the model routed to: the
// gatekeeper credential, and the caller's address in X-Forwarded-For, went to
// someone else. The comment on the caller-credential strip states the concern for
// the caller's key ("relaying the caller's key there would leak it into someone
// else's logs"); the gateway's own key is the same secret one hop over. The key now
// goes only to the default upstream, and a model on another upstream names its own
// key with upstream_key_env.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestMine109TheGatewaysKeyGoesOnlyToItsOwnUpstream(t *testing.T) {
	t.Setenv("OAICA_GATEWAY_UPSTREAM_KEY", "GATEWAY-SECRET")
	t.Setenv("EXT_UPSTREAM_KEY", "EXT-KEY")
	var mu sync.Mutex
	seen := map[string]http.Header{}
	handler := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			mu.Lock()
			seen[name] = r.Header.Clone()
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"c1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
		})
	}
	def := httptest.NewServer(handler("default"))
	third := httptest.NewServer(handler("third"))
	own := httptest.NewServer(handler("own"))
	t.Cleanup(def.Close)
	t.Cleanup(third.Close)
	t.Cleanup(own.Close)

	g := &gateway{}
	price := gwPricing{Prompt: "0.00000005", Completion: "0.00000012"}
	if err := g.apply(gwConfig{UpstreamAddr: def.URL, ListenAddr: ":0", LedgerPath: filepath.Join(t.TempDir(), "ledger.jsonl"),
		APIKeys: []gwKey{{SHA256: keyHash("sk"), Label: "k"}},
		Models: []gwModel{
			{ID: "main", OwnedBy: "oaica", ContextLength: 262144, MaxCompletionTokens: 32768, Pricing: price},
			{ID: "third", OwnedBy: "oaica", ContextLength: 262144, MaxCompletionTokens: 32768, Pricing: price, UpstreamAddr: third.URL},
			{ID: "own", OwnedBy: "oaica", ContextLength: 262144, MaxCompletionTokens: 32768, Pricing: price, UpstreamAddr: own.URL, UpstreamKeyEnv: "EXT_UPSTREAM_KEY"},
		}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux(g))
	t.Cleanup(srv.Close)
	msg := `"messages":[{"role":"user","content":"go"}]`
	for _, m := range []string{"main", "third", "own"} {
		if code, _, body := r107Post(t, srv, "/v1/chat/completions", `{"model":"`+m+`",`+msg+`}`, 0); code != 200 {
			t.Fatalf("%s: answered %d %s", m, code, body)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if got := seen["default"].Get("Authorization"); got != "Bearer GATEWAY-SECRET" {
		t.Errorf("the default upstream received Authorization %q, want the gateway's key (2026-09-29 audit, round 109, F109-L3-3)", got)
	}
	if got := seen["third"].Get("Authorization"); got != "" {
		t.Errorf("a third-party upstream received Authorization %q — the gateway's own key is for its own upstream (2026-09-29 audit, round 109, F109-L3-3)", got)
	}
	if _, has := seen["third"]["X-Forwarded-For"]; has {
		t.Errorf("a third-party upstream received the caller's address in X-Forwarded-For %v (2026-09-29 audit, round 109, F109-L3-3)", seen["third"]["X-Forwarded-For"])
	}
	if seen["default"].Get("X-Oaica-Metered") != "1" || seen["third"].Get("X-Oaica-Metered") != "" {
		t.Errorf("X-Oaica-Metered is %q on the default upstream and %q on a third-party one, want 1 and none: only the gateway's own upstream is downstream of oaicalb (2026-09-29 audit, round 109, F109-L3-3)",
			seen["default"].Get("X-Oaica-Metered"), seen["third"].Get("X-Oaica-Metered"))
	}
	if got := seen["own"].Get("Authorization"); got != "Bearer EXT-KEY" {
		t.Errorf("an upstream that names its key received Authorization %q, want Bearer EXT-KEY (2026-09-29 audit, round 109, F109-L3-3)", got)
	}
}

// TestMine109UpstreamTopologyHeadersDoNotReachTheCaller is F109-L3-1's pin. The
// gateway deleted five named headers (its own audit L16: internal topology must not
// leak to the public) and relayed every other, so a cookie set by a model's own
// upstream landed on the public origin and the internal names a new hop added
// (X-Powered-By naming a host, an X-Upstream-Host, an Alt-Svc, a redirect to an
// internal address) reached callers. Both doors relayed the same headers.
func TestMine109UpstreamTopologyHeadersDoNotReachTheCaller(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		h := w.Header()
		h.Set("Content-Type", "application/json")
		h.Set("Retry-After", "3")
		h.Set("Cache-Control", "no-store")
		h.Set("X-Ratelimit-Remaining-Requests", "9")
		h.Set("Set-Cookie", "sess=internal; Domain=10.0.0.5")
		h.Set("X-Powered-By", "vllm-internal-10.0.0.5")
		h.Set("Alt-Svc", `h3=":443"`)
		h.Set("X-Gatekeeper-Key-Label", "tenant-x")
		h.Set("X-Upstream-Host", "gpu-a100b.internal")
		h.Set("X-Katlb-Pool", "a")
		io.WriteString(w, `{"id":"c1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	}))
	t.Cleanup(up.Close)
	srv, _ := r107Gw(t, up, nil)
	for _, c := range []struct{ path, body string }{
		{"/v1/chat/completions", `{"model":"kat-awq","messages":[{"role":"user","content":"go"}]}`},
		{"/v1/messages", `{"model":"kat-awq","max_tokens":5,"messages":[{"role":"user","content":"go"}]}`},
	} {
		code, hdr, body := r107Post(t, srv, c.path, c.body, 0)
		if code != 200 {
			t.Fatalf("%s: answered %d %s", c.path, code, body)
		}
		for _, h := range []string{"Set-Cookie", "X-Powered-By", "Alt-Svc", "X-Gatekeeper-Key-Label", "X-Upstream-Host", "X-Katlb-Pool"} {
			if v := hdr.Get(h); v != "" {
				t.Errorf("%s: the caller received %s: %q — internal topology and upstream cookies stay inside (2026-09-29 audit, round 109, F109-L3-1)", c.path, h, v)
			}
		}
		if hdr.Get("Retry-After") != "3" && c.path == "/v1/chat/completions" || hdr.Get("Cache-Control") == "" && c.path == "/v1/chat/completions" || hdr.Get("X-Ratelimit-Remaining-Requests") == "" && c.path == "/v1/chat/completions" {
			t.Errorf("%s: a header the caller needs was dropped: %v (2026-09-29 audit, round 109, F109-L3-1)", c.path, hdr)
		}
	}

	// An upstream redirect to an internal address is not relayed with its Location.
	red := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://10.0.0.5:8000/elsewhere")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(red.Close)
	rsrv, _ := r107Gw(t, red, nil)
	req, _ := http.NewRequest("POST", rsrv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"kat-awq","messages":[{"role":"user","content":"go"}]}`))
	req.Header.Set("Authorization", "Bearer sk")
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Errorf("the caller received Location %q from an upstream redirect (2026-09-29 audit, round 109, F109-L3-1)", loc)
	}
}

// TestMine109CredentialURLRedactionCoversOnlyStatusErrors RECORDS F109-L3-2
// (rank b, not changed). redactCredentialURLs strips the userinfo from a URL in an
// upstream error text, and its one call site is ModifyResponse for statuses of 400
// and above. An error the upstream states after the headers have gone out — an SSE
// error frame under a 200, or a 200 document that is an error object — reaches an
// OpenAI-surface caller as written, where the Anthropic door refuses the same body
// and states a generic cause. Recorded rather than fixed: redacting there means
// rewriting a live SSE body line by line (and every 200 document), which gives up
// the byte-for-byte relay the streaming path depends on and costs a scan of every
// response, for a leak that needs an upstream whose own error text quotes a URL with
// credentials in it; no upstream in this tree states one. What a later round should
// know: a fix is a line-buffered filter that only inspects `data:` lines containing
// an error object, and the pin below states today's reading and goes red if it moves.
func TestMine109CredentialURLRedactionCoversOnlyStatusErrors(t *testing.T) {
	const leaky = `backend http://svc:hunter2@10.0.0.5:8000/x failed`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"error":{"message":"`+leaky+`"}}`)
	}))
	t.Cleanup(up.Close)
	srv, _ := r107Gw(t, up, nil)
	_, _, body := r107Post(t, srv, "/v1/chat/completions", `{"model":"kat-awq","messages":[{"role":"user","content":"go"}]}`, 0)
	if !strings.Contains(body, "hunter2") {
		t.Errorf("a 200 error document now reaches the OpenAI door as %q — if the credential is redacted the fix is made and this record is spent (2026-09-29 audit, round 109, F109-L3-2)", body)
	}
}
