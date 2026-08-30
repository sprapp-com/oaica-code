package main

// Tests for the `oaica pull` weights-distribution routes (pull.go). The
// wire contract here is with an already-shipped client
// (cmd/oaica_pull_serve.go), so these assert exact JSON field names and
// the exact error `type` strings that client string-matches on.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testLicenseKey = "lic-test-key"

// newPullGateway builds a gateway whose catalog covers every case: a
// public hf model, a licensed+encrypted hf model, a public local file, and
// a licensed local file.
func newPullGateway(t *testing.T) (*gateway, string) {
	t.Helper()
	blobPath := filepath.Join(t.TempDir(), "blob.gguf")
	blob := []byte(strings.Repeat("GGUF-BYTES-", 100)) // 1100 bytes
	if err := os.WriteFile(blobPath, blob, 0o600); err != nil {
		t.Fatalf("write blob: %v", err)
	}

	cfg := gwConfig{
		UpstreamAddr: "http://127.0.0.1:1",
		ListenAddr:   ":0",
		LedgerPath:   filepath.Join(t.TempDir(), "ledger.jsonl"),
		APIKeys:      []gwKey{{SHA256: keyHash("sk-new"), Label: "openrouter"}},
		Models: []gwModel{{
			ID: "kat-awq", OwnedBy: "oaica",
			Pricing: gwPricing{Prompt: "0", Completion: "0"},
		}},
		PullLicenseKeys: []gwKey{{SHA256: keyHash(testLicenseKey), Label: "acme-corp"}},
		PullCatalog: []gwPullEntry{
			{
				Model: "qwen2.5-0.5b", Source: "hf",
				HFURL:     "https://huggingface.co/Qwen/x/resolve/main/q.gguf",
				SizeBytes: 491400032, Description: "tiny smoke-test model",
			},
			{
				Model: "licensed-hf", Source: "hf",
				HFURL:           "https://huggingface.co/oaica/x/resolve/main/enc.gguf",
				SizeBytes:       123456,
				SHA256:          strings.Repeat("ab", 32),
				DecryptKeyHex:   strings.Repeat("11", 32),
				LicenseRequired: true,
			},
			{
				Model: "local-file", Source: "file", FilePath: blobPath,
				SizeBytes: int64(len(blob)), Description: "served off local disk",
			},
			{
				Model: "licensed-file", Source: "file", FilePath: blobPath,
				SizeBytes: int64(len(blob)), LicenseRequired: true,
			},
		},
	}
	g := &gateway{}
	if err := g.apply(cfg); err != nil {
		t.Fatalf("apply: %v", err)
	}
	return g, cfg.LedgerPath
}

func getWithKey(t *testing.T, url, key string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, body
}

// errType pulls error.type out of a failure body — the field the pull
// client branches on.
func errType(t *testing.T, body []byte) (typ, msg string) {
	t.Helper()
	var e struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("bad error body %q: %v", body, err)
	}
	return e.Error.Type, e.Error.Message
}

func TestManifest_PublicHFModelNeedsNoAuth(t *testing.T) {
	g, _ := newPullGateway(t)
	srv := httptest.NewServer(mux(g))
	defer srv.Close()

	resp, body := getWithKey(t, srv.URL+"/v1/manifest/qwen2.5-0.5b", "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, body)
	}
	// Decode into the client's exact shape to prove compatibility.
	var m struct {
		Model         string  `json:"model"`
		SizeBytes     int64   `json:"size_bytes"`
		SHA256        *string `json:"sha256"`
		PullURL       string  `json:"pull_url"`
		Source        string  `json:"source"`
		HFURL         *string `json:"hf_url"`
		DecryptKeyHex *string `json:"decrypt_key"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal manifest: %v (%s)", err, body)
	}
	if m.Model != "qwen2.5-0.5b" || m.Source != "hf" || m.SizeBytes != 491400032 {
		t.Fatalf("manifest = %+v", m)
	}
	if m.HFURL == nil || !strings.HasPrefix(*m.HFURL, "https://huggingface.co/") {
		t.Fatalf("hf_url = %v, want the HF resolve URL", m.HFURL)
	}
	if m.PullURL != "" {
		t.Fatalf("pull_url = %q, want empty for source=hf", m.PullURL)
	}
	if m.SHA256 != nil {
		t.Fatalf("sha256 = %v, want null when not configured", *m.SHA256)
	}
	// No license required, so no key must leak even though none was asked for.
	if m.DecryptKeyHex != nil {
		t.Fatalf("decrypt_key leaked on an unencrypted model: %v", *m.DecryptKeyHex)
	}
}

func TestManifest_LicensedModelWithoutKeyIsLicenseRequired(t *testing.T) {
	g, _ := newPullGateway(t)
	srv := httptest.NewServer(mux(g))
	defer srv.Close()

	resp, body := getWithKey(t, srv.URL+"/v1/manifest/licensed-hf", "")
	if resp.StatusCode != 401 {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	typ, msg := errType(t, body)
	if typ != "license_required" {
		t.Fatalf("type = %q, want license_required", typ)
	}
	if msg == "" {
		t.Fatal("empty message: the client prints it verbatim")
	}
	if strings.Contains(string(body), "1111") {
		t.Fatalf("decrypt key leaked in the 401 body: %s", body)
	}
}

func TestManifest_LicensedModelWithBadKeyIsLicenseInvalid(t *testing.T) {
	g, _ := newPullGateway(t)
	srv := httptest.NewServer(mux(g))
	defer srv.Close()

	resp, body := getWithKey(t, srv.URL+"/v1/manifest/licensed-hf", "lic-WRONG")
	if resp.StatusCode != 401 {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if typ, _ := errType(t, body); typ != "license_invalid" {
		t.Fatalf("type = %q, want license_invalid", typ)
	}
}

func TestManifest_ChatAPIKeyIsNotALicense(t *testing.T) {
	// A leaked chat key must not become a weights-download credential.
	g, _ := newPullGateway(t)
	srv := httptest.NewServer(mux(g))
	defer srv.Close()

	resp, body := getWithKey(t, srv.URL+"/v1/manifest/licensed-hf", "sk-new")
	if resp.StatusCode != 401 {
		t.Fatalf("status = %d, want 401 for a chat api key", resp.StatusCode)
	}
	if typ, _ := errType(t, body); typ != "license_invalid" {
		t.Fatalf("type = %q, want license_invalid", typ)
	}
}

func TestManifest_LicensedModelWithGoodKeyReturnsDecryptKey(t *testing.T) {
	g, _ := newPullGateway(t)
	srv := httptest.NewServer(mux(g))
	defer srv.Close()

	resp, body := getWithKey(t, srv.URL+"/v1/manifest/licensed-hf", testLicenseKey)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 (%s)", resp.StatusCode, body)
	}
	var m struct {
		SHA256        *string `json:"sha256"`
		DecryptKeyHex *string `json:"decrypt_key"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.DecryptKeyHex == nil || len(*m.DecryptKeyHex) != 64 {
		t.Fatalf("decrypt_key = %v, want the 64-hex AES-256 key", m.DecryptKeyHex)
	}
	if m.SHA256 == nil || len(*m.SHA256) != 64 {
		t.Fatalf("sha256 = %v, want the configured digest", m.SHA256)
	}
}

func TestManifest_FileSourceGivesPullURL(t *testing.T) {
	g, _ := newPullGateway(t)
	srv := httptest.NewServer(mux(g))
	defer srv.Close()

	_, body := getWithKey(t, srv.URL+"/v1/manifest/local-file", "")
	var m struct {
		PullURL string  `json:"pull_url"`
		Source  string  `json:"source"`
		HFURL   *string `json:"hf_url"`
	}
	json.Unmarshal(body, &m)
	if m.Source != "file" || m.PullURL != "/v1/pull/local-file" {
		t.Fatalf("manifest = %+v", m)
	}
	if m.HFURL != nil {
		t.Fatalf("hf_url = %v, want null for a file model", *m.HFURL)
	}
}

func TestManifest_UnknownModelIs404(t *testing.T) {
	g, _ := newPullGateway(t)
	srv := httptest.NewServer(mux(g))
	defer srv.Close()

	resp, body := getWithKey(t, srv.URL+"/v1/manifest/nope", "")
	if resp.StatusCode != 404 {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	typ, msg := errType(t, body)
	if typ != "model_not_found" {
		t.Fatalf("type = %q, want model_not_found", typ)
	}
	if !strings.Contains(msg, `unknown model "nope"`) || !strings.Contains(msg, "https://oaica.com") {
		t.Fatalf("message = %q, want the catalog pointer", msg)
	}
}

// The generic catch-all 404 ("unknown route") is exactly the regression
// this file fixes: /v1/manifest must never fall through to it again.
func TestManifest_DoesNotFallThroughToUnknownRoute(t *testing.T) {
	g, _ := newPullGateway(t)
	srv := httptest.NewServer(mux(g))
	defer srv.Close()

	_, body := getWithKey(t, srv.URL+"/v1/manifest/nope", "")
	if strings.Contains(string(body), "unknown route") {
		t.Fatalf("manifest hit the catch-all: %s", body)
	}
}

func TestManifest_IsNotLedgered(t *testing.T) {
	// Weights downloads consume no upstream tokens; a zero-token ledger row
	// per pull would corrupt usage reporting and billing.
	g, ledger := newPullGateway(t)
	srv := httptest.NewServer(mux(g))
	defer srv.Close()

	getWithKey(t, srv.URL+"/v1/manifest/qwen2.5-0.5b", "")
	getWithKey(t, srv.URL+"/v1/catalog", "")
	getWithKey(t, srv.URL+"/v1/pull/local-file", "")

	b, err := os.ReadFile(ledger)
	if err == nil && len(strings.TrimSpace(string(b))) != 0 {
		t.Fatalf("pull routes wrote ledger entries: %s", b)
	}
}

func TestPull_StreamsFileWithContentLength(t *testing.T) {
	g, _ := newPullGateway(t)
	srv := httptest.NewServer(mux(g))
	defer srv.Close()

	resp, body := getWithKey(t, srv.URL+"/v1/pull/local-file", "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if resp.ContentLength != 1100 {
		t.Fatalf("Content-Length = %d, want 1100", resp.ContentLength)
	}
	if len(body) != 1100 || !strings.HasPrefix(string(body), "GGUF-BYTES-") {
		t.Fatalf("body len = %d, prefix %.20q", len(body), body)
	}
}

func TestPull_SupportsRangeResume(t *testing.T) {
	// An interrupted multi-gigabyte pull must be resumable from an offset.
	g, _ := newPullGateway(t)
	srv := httptest.NewServer(mux(g))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/pull/local-file", nil)
	req.Header.Set("Range", "bytes=1000-")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("range get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	if len(body) != 100 {
		t.Fatalf("range body len = %d, want 100", len(body))
	}
	if got := resp.Header.Get("Content-Range"); got != "bytes 1000-1099/1100" {
		t.Fatalf("Content-Range = %q", got)
	}
	if resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("missing Accept-Ranges: %v", resp.Header)
	}
}

func TestPull_LicensedFileGating(t *testing.T) {
	g, _ := newPullGateway(t)
	srv := httptest.NewServer(mux(g))
	defer srv.Close()

	resp, body := getWithKey(t, srv.URL+"/v1/pull/licensed-file", "")
	if resp.StatusCode != 401 {
		t.Fatalf("no key: status = %d, want 401", resp.StatusCode)
	}
	if typ, _ := errType(t, body); typ != "license_required" {
		t.Fatalf("type = %q", typ)
	}

	resp, body = getWithKey(t, srv.URL+"/v1/pull/licensed-file", "nope")
	if resp.StatusCode != 401 {
		t.Fatalf("bad key: status = %d, want 401", resp.StatusCode)
	}
	if typ, _ := errType(t, body); typ != "license_invalid" {
		t.Fatalf("type = %q", typ)
	}

	resp, body = getWithKey(t, srv.URL+"/v1/pull/licensed-file", testLicenseKey)
	if resp.StatusCode != 200 || len(body) != 1100 {
		t.Fatalf("good key: status = %d, len = %d", resp.StatusCode, len(body))
	}
}

func TestPull_HFModelIs404(t *testing.T) {
	// hf-source bytes come straight from HuggingFace; a client asking us
	// for them is mis-wired and should fail loudly.
	g, _ := newPullGateway(t)
	srv := httptest.NewServer(mux(g))
	defer srv.Close()

	resp, body := getWithKey(t, srv.URL+"/v1/pull/qwen2.5-0.5b", "")
	if resp.StatusCode != 404 {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if typ, _ := errType(t, body); typ != "model_not_found" {
		t.Fatalf("type = %q", typ)
	}
}

func TestPull_UnknownModelIs404(t *testing.T) {
	g, _ := newPullGateway(t)
	srv := httptest.NewServer(mux(g))
	defer srv.Close()

	resp, _ := getWithKey(t, srv.URL+"/v1/pull/nope", "")
	if resp.StatusCode != 404 {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestPull_MissingFileIsNotAServerPanic(t *testing.T) {
	g, _ := newPullGateway(t)
	g.cfg.PullCatalog = append(g.cfg.PullCatalog, gwPullEntry{
		Model: "ghost", Source: "file", FilePath: "/nonexistent/ghost.gguf", SizeBytes: 1,
	})
	srv := httptest.NewServer(mux(g))
	defer srv.Close()

	resp, body := getWithKey(t, srv.URL+"/v1/pull/ghost", "")
	if resp.StatusCode != 500 {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if strings.Contains(string(body), "/nonexistent") {
		t.Fatalf("host path leaked to the client: %s", body)
	}
}

func TestCatalog_IsPublicAndHidesLocations(t *testing.T) {
	g, _ := newPullGateway(t)
	srv := httptest.NewServer(mux(g))
	defer srv.Close()

	resp, body := getWithKey(t, srv.URL+"/v1/catalog", "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out []gwCatalogEntry
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal catalog: %v (%s)", err, body)
	}
	if len(out) != 4 {
		t.Fatalf("catalog has %d entries, want 4", len(out))
	}
	byModel := map[string]gwCatalogEntry{}
	for _, e := range out {
		byModel[e.Model] = e
	}
	q, ok := byModel["qwen2.5-0.5b"]
	if !ok || q.SizeBytes != 491400032 || q.Description != "tiny smoke-test model" || q.LicenseRequired {
		t.Fatalf("qwen entry = %+v", q)
	}
	if !byModel["licensed-hf"].LicenseRequired {
		t.Fatal("licensed-hf should report license_required")
	}
	// Never expose the bytes' location or key from the public listing.
	for _, bad := range []string{"huggingface.co", "file_path", "decrypt_key", "hf_url"} {
		if strings.Contains(string(body), bad) {
			t.Fatalf("catalog leaked %q: %s", bad, body)
		}
	}
}

// --- config validation ---

func TestValidatePullConfig(t *testing.T) {
	base := func(e gwPullEntry) gwConfig {
		return gwConfig{PullCatalog: []gwPullEntry{e}}
	}
	good := gwPullEntry{Model: "m", Source: "hf", HFURL: "https://h/x.gguf", SizeBytes: 10}

	cases := []struct {
		name string
		cfg  gwConfig
		want string
	}{
		{"ok hf", base(good), ""},
		{"ok file", base(gwPullEntry{Model: "m", Source: "file", FilePath: "/x.gguf", SizeBytes: 10}), ""},
		{"no model", base(gwPullEntry{Source: "hf", HFURL: "u", SizeBytes: 1}), "model is required"},
		{"bad source", base(gwPullEntry{Model: "m", Source: "s3", SizeBytes: 1}), `source must be "hf" or "file"`},
		{"hf without url", base(gwPullEntry{Model: "m", Source: "hf", SizeBytes: 1}), "source=hf requires hf_url"},
		{"file without path", base(gwPullEntry{Model: "m", Source: "file", SizeBytes: 1}), "source=file requires file_path"},
		{"zero size", base(gwPullEntry{Model: "m", Source: "hf", HFURL: "u"}), "size_bytes must be > 0"},
		{"negative size", base(gwPullEntry{Model: "m", Source: "hf", HFURL: "u", SizeBytes: -1}), "size_bytes must be > 0"},
		{"short sha", base(gwPullEntry{Model: "m", Source: "hf", HFURL: "u", SizeBytes: 1, SHA256: "abcd"}), "sha256 must be 64 hex chars"},
		{"nonhex sha", base(gwPullEntry{Model: "m", Source: "hf", HFURL: "u", SizeBytes: 1, SHA256: strings.Repeat("z", 64)}), "sha256 is not hex"},
		{"short key", base(gwPullEntry{Model: "m", Source: "hf", HFURL: "u", SizeBytes: 1, DecryptKeyHex: "aa"}), "decrypt_key must be 64 hex chars"},
		{"nonhex key", base(gwPullEntry{Model: "m", Source: "hf", HFURL: "u", SizeBytes: 1, DecryptKeyHex: strings.Repeat("z", 64)}), "decrypt_key is not hex"},
		{"dupe", gwConfig{PullCatalog: []gwPullEntry{good, good}}, "duplicate model"},
		{"bad license key", gwConfig{PullLicenseKeys: []gwKey{{SHA256: "short"}}}, "sha256 must be 64 hex chars"},
		{"nonhex license key", gwConfig{PullLicenseKeys: []gwKey{{SHA256: strings.Repeat("z", 64)}}}, "sha256 is not hex"},
		{"empty catalog is fine", gwConfig{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePullConfig(tc.cfg)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// loadConfig must refuse a bad catalog outright so a reload keeps serving
// the previous config instead of half-adopting a broken one.
func TestLoadConfig_RejectsBadPullCatalog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg.json")
	cfg := `{
	  "api_keys": [{"sha256": "` + strings.Repeat("a", 64) + `", "label": "k"}],
	  "models": [{"id": "m", "owned_by": "oaica"}],
	  "pull_catalog": [{"model": "x", "source": "hf", "size_bytes": 5}]
	}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil || !strings.Contains(err.Error(), "requires hf_url") {
		t.Fatalf("err = %v, want an hf_url validation failure", err)
	}
}

func TestLoadConfig_AcceptsGoodPullCatalog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg.json")
	cfg := `{
	  "api_keys": [{"sha256": "` + strings.Repeat("a", 64) + `", "label": "k"}],
	  "models": [{"id": "m", "owned_by": "oaica"}],
	  "pull_license_keys": [{"sha256": "` + strings.Repeat("b", 64) + `", "label": "lic"}],
	  "pull_catalog": [
	    {"model": "x", "source": "hf", "hf_url": "https://h/x.gguf", "size_bytes": 5, "license_required": false},
	    {"model": "y", "source": "file", "file_path": "/tmp/y.gguf", "size_bytes": 7, "license_required": true}
	  ]
	}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(got.PullCatalog) != 2 || got.PullCatalog[1].FilePath != "/tmp/y.gguf" {
		t.Fatalf("catalog = %+v", got.PullCatalog)
	}
	if len(got.PullLicenseKeys) != 1 || got.PullLicenseKeys[0].Label != "lic" {
		t.Fatalf("license keys = %+v", got.PullLicenseKeys)
	}
}

// The shipped a100b config must stay loadable — it is the file the
// production gateway reads, and a typo there is an outage.
func TestSampleA100BConfigIsValid(t *testing.T) {
	b, err := os.ReadFile("../a100b/gateway.json")
	if err != nil {
		t.Skipf("sample config not present: %v", err)
	}
	var cfg gwConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("parse a100b/gateway.json: %v", err)
	}
	if err := validatePullConfig(cfg); err != nil {
		t.Fatalf("a100b/gateway.json pull_catalog invalid: %v", err)
	}
	want := map[string]bool{"qwen2.5-0.5b": false, "oaica-nemotron-30b-a3b": false}
	for _, e := range cfg.PullCatalog {
		if _, ok := want[e.Model]; ok {
			want[e.Model] = true
		}
	}
	for m, found := range want {
		if !found {
			t.Errorf("a100b/gateway.json is missing pull_catalog entry %q", m)
		}
	}
}
