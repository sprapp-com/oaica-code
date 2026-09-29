package main

import (
	"context"
	"fmt"
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

// A licence bought through Stripe (validated by oaica-saas) opens licensed weights; nothing else does.
func TestRound133StripeLicenceOpensLicensedWeights(t *testing.T) {
	var calls atomic.Int32
	saas := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.FormValue("license_key") {
		case "oaica-lic-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":
			w.Write([]byte(`{"valid":true,"meta":{"product":"oaica-code"}}`))
		case "oaica-lic-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb":
			w.Write([]byte(`{"valid":true,"meta":{"product":"someone-else"}}`))
		case "not-a-licence-shape":
			w.Write([]byte(`{"valid":true,"meta":{"product":"oaica-code"}}`))
		case "oaica-lic-dddddddddddddddddddddddddddddddd":
			w.WriteHeader(500)
			w.Write([]byte(`{"valid":true,"meta":{"product":"oaica-code"}}`))
		default:
			w.Write([]byte(`{"valid":false,"error":"license key not found","meta":{"product":"oaica-code"}}`))
		}
	}))
	defer saas.Close()

	g, _ := newPullGateway(t)
	cfg := g.cfg
	cfg.PullLicenseValidateURL = saas.URL + "/license/validate"
	if err := g.apply(cfg); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux(g))
	defer srv.Close()

	status := func(key string) int {
		resp, _ := getWithKey(t, srv.URL+"/v1/manifest/licensed-hf", key)
		return resp.StatusCode
	}
	if got := status("oaica-lic-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); got != 200 {
		t.Errorf("a valid Stripe licence: status %d, want 200", got)
	}
	before := calls.Load()
	status("oaica-lic-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if calls.Load() != before {
		t.Error("a repeat within the TTL called the licence server again (no cache)")
	}
	for _, k := range []string{"oaica-lic-cccccccccccccccccccccccccccccccc", "oaica-lic-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "not-a-licence-shape", "oaica-lic-dddddddddddddddddddddddddddddddd"} {
		if got := status(k); got != 401 {
			t.Errorf("key %q: status %d, want 401", k, got)
		}
	}
	// A static key still works, and a chat API key is still not a licence.
	if got := status(testLicenseKey); got != 200 {
		t.Errorf("static key: status %d", got)
	}
	if got := status("sk-new"); got != 401 {
		t.Errorf("chat key opened weights: status %d", got)
	}
}

func TestRound133UnreachableLicenceServerRefuses(t *testing.T) {
	g, _ := newPullGateway(t)
	cfg := g.cfg
	cfg.PullLicenseValidateURL = "http://127.0.0.1:1/license/validate"
	if err := g.apply(cfg); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux(g))
	defer srv.Close()
	resp, _ := getWithKey(t, srv.URL+"/v1/manifest/licensed-hf", "oaica-lic-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if resp.StatusCode != 401 {
		t.Errorf("status %d with the licence server down, want 401 (fail closed)", resp.StatusCode)
	}
}

func TestRound133LicenceValidateURLMustBeHTTPSOrLoopback(t *testing.T) {
	for raw, ok := range map[string]bool{"": true, "https://saas.example/license/validate": true, "http://127.0.0.1:4965/license/validate": true,
		"http://localhost/x": true, "http://saas.example/license/validate": false, "ftp://x": false, "not a url": false} {
		if err := validatePullLicenseURL(raw); (err == nil) != ok {
			t.Errorf("%q: err=%v, want ok=%v", raw, err, ok)
		}
	}
	_ = strings.TrimSpace
}

func TestRound133LoadConfigRefusesAnInsecureValidateURL(t *testing.T) {
	p := filepath.Join(t.TempDir(), "gw.json")
	body := func(u string) string {
		return `{"api_keys":[{"sha256":"` + keyHash("sk-x") + `","label":"x"}],"models":[{"id":"m","owned_by":"oaica","pricing":{"prompt":"0","completion":"0"}}],"pull_license_validate_url":"` + u + `"}`
	}
	os.WriteFile(p, []byte(body("https://saas.example/license/validate")), 0o600)
	if _, err := loadConfig(p); err != nil {
		t.Fatalf("a valid config failed to load: %v", err)
	}
	os.WriteFile(p, []byte(body("http://saas.example/license/validate")), 0o600)
	if _, err := loadConfig(p); err == nil {
		t.Error("an http:// validate URL to a remote host loaded")
	}
}

const goodSK = "oaica-sk-0123456789abcdef0123456789abcdef0123456789abcdef"

func bridgeGateway(t *testing.T, saasURL string) (*gateway, *httptest.Server) {
	g, _ := newPullGateway(t)
	cfg := g.cfg
	cfg.PullLicenseValidateURL = saasURL + "/license/validate"
	cfg.APIKeyValidateURL = saasURL + "/keys/validate"
	cfg.APIKeyValidateToken = "ent-token"
	if err := g.apply(cfg); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux(g))
	t.Cleanup(srv.Close)
	return g, srv
}

// F133-L3-2: a subscriber's oaica-sk key (never in api_keys) authenticates through the saas bridge, under the
// label the saas states; a cancelled one, an unknown one and one of the wrong shape do not.
func TestRound133SubscriberKeyAuthenticatesThroughTheSaas(t *testing.T) {
	var calls atomic.Int32
	var gotAuth atomic.Value
	saas := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		gotAuth.Store(r.Header.Get("Authorization"))
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		if r.FormValue("api_key") == goodSK {
			w.Write([]byte(`{"valid":true,"label":"sub_ABC","status":"active","plan":"pro"}`))
			return
		}
		w.Write([]byte(`{"valid":false}`))
	}))
	defer saas.Close()
	g, _ := bridgeGateway(t, saas.URL)
	req := func(key string) (gwKey, bool) {
		r := httptest.NewRequest("GET", "/v1/models", nil)
		r.Header.Set("Authorization", "Bearer "+key)
		return g.lookupKey(r)
	}
	k, ok := req(goodSK)
	if !ok || k.Label != "sub_ABC" {
		t.Fatalf("subscriber key: ok=%v label=%q", ok, k.Label)
	}
	if gotAuth.Load() != "Bearer ent-token" {
		t.Errorf("the saas was not sent the entitlement token: %v", gotAuth.Load())
	}
	n := calls.Load()
	req(goodSK)
	if calls.Load() != n {
		t.Error("no cache for a valid subscriber key")
	}
	for _, bad := range []string{"oaica-sk-" + strings.Repeat("f", 48), "sk-new-but-not-ours", "oaica-sk-short"} {
		if _, ok := req(bad); ok && bad != "sk-new-but-not-ours" {
			t.Errorf("%q authenticated", bad)
		}
	}
	before := calls.Load()
	req("oaica-sk-short")
	req("random-bearer-token")
	if calls.Load() != before {
		t.Error("a key of the wrong shape was sent to the saas")
	}
	if _, ok := req("sk-new"); !ok {
		t.Error("a static key stopped working")
	}
}

// F133-L3-1: wrong keys during a reload must not deadlock the gateway.
func TestRound133WrongKeysDuringReloadDoNotDeadlock(t *testing.T) {
	g, _ := newPullGateway(t)
	srv := httptest.NewServer(mux(g))
	defer srv.Close()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					getWithKey(t, srv.URL+"/v1/manifest/licensed-hf", "wrong-key")
				}
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			cfg := g.cfg
			g.apply(cfg)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		close(stop)
		t.Fatal("apply (SIGHUP reload) is stuck behind wrong-key pulls: deadlock")
	}
	close(stop)
	wg.Wait()
}

// F133-L3-3: concurrent lookups of one key make one call; a 429/5xx is refused but never remembered; junk keys
// cannot evict a paying customer's cached answer; the call is bounded by the request.
func TestRound133RemoteCacheProperties(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	saas := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-release
		w.Write([]byte(`{"valid":true,"meta":{"product":"oaica-code"}}`))
	}))
	defer saas.Close()
	var c remoteCache
	fn := func(ctx context.Context) (remoteResult, bool) {
		return callLicenseValidate(ctx, saas.URL, "oaica-lic-"+strings.Repeat("a", 32))
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c.check(context.Background(), "one", fn) }()
	}
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Errorf("50 concurrent lookups made %d calls, want 1", calls.Load())
	}

	// 429 is refused and not remembered
	var n429 atomic.Int32
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { n429.Add(1); w.WriteHeader(429) }))
	defer limited.Close()
	var c2 remoteCache
	f2 := func(ctx context.Context) (remoteResult, bool) { return callLicenseValidate(ctx, limited.URL, "k") }
	if c2.check(context.Background(), "k", f2).ok {
		t.Error("a 429 authenticated")
	}
	c2.check(context.Background(), "k", f2)
	if n429.Load() != 2 {
		t.Errorf("a 429 was cached (%d calls for 2 lookups)", n429.Load())
	}

	// junk cannot evict a valid answer
	var c3 remoteCache
	c3.init()
	c3.m["vip"] = remoteEntry{res: remoteResult{ok: true, label: "x"}, expires: time.Now().Add(time.Hour)}
	for i := 0; i < remoteMaxEntries+10; i++ {
		c3.m[fmt.Sprint("junk", i)] = remoteEntry{res: remoteResult{}, expires: time.Now().Add(time.Hour)}
		if len(c3.m) >= remoteMaxEntries {
			c3.mu.Lock()
			c3.evictLocked(time.Now())
			c3.mu.Unlock()
		}
	}
	if _, ok := c3.m["vip"]; !ok {
		t.Error("a flood of refused keys evicted a valid cached answer")
	}
}

// The outbound call is bound to the request: a client that gives up does not hold a slot for the full timeout.
func TestRound133RemoteCallFollowsTheRequestContext(t *testing.T) {
	hang := make(chan struct{})
	saas := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-hang }))
	defer saas.Close()
	defer close(hang)
	var c remoteCache
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	c.check(ctx, "k", func(cx context.Context) (remoteResult, bool) { return callLicenseValidate(cx, saas.URL, "k") })
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("the call outlived its request by %v", d)
	}
}
