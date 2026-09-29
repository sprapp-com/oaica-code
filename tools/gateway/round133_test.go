package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// A licence bought through Stripe (validated by oaica-saas) opens licensed weights; nothing else does.
func TestRound133StripeLicenceOpensLicensedWeights(t *testing.T) {
	var calls atomic.Int32
	saas := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.FormValue("license_key") {
		case "oaica-lic-good":
			w.Write([]byte(`{"valid":true,"meta":{"product":"oaica-code"}}`))
		case "oaica-lic-otherproduct":
			w.Write([]byte(`{"valid":true,"meta":{"product":"someone-else"}}`))
		case "not-a-licence-shape":
			w.Write([]byte(`{"valid":true,"meta":{"product":"oaica-code"}}`))
		case "oaica-lic-500":
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
	if got := status("oaica-lic-good"); got != 200 {
		t.Errorf("a valid Stripe licence: status %d, want 200", got)
	}
	before := calls.Load()
	status("oaica-lic-good")
	if calls.Load() != before {
		t.Error("a repeat within the TTL called the licence server again (no cache)")
	}
	for _, k := range []string{"oaica-lic-nope", "oaica-lic-otherproduct", "not-a-licence-shape", "oaica-lic-500"} {
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
	resp, _ := getWithKey(t, srv.URL+"/v1/manifest/licensed-hf", "oaica-lic-good")
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
