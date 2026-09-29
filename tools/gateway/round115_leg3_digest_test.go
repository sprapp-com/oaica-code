package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRound115UppercaseDigest(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	}))
	defer up.Close()
	dir := t.TempDir()
	upper := strings.ToUpper(keyHash("sk"))
	cfgPath := filepath.Join(dir, "gw.json")
	os.WriteFile(cfgPath, []byte(fmt.Sprintf(`{"upstream_addr":%q,"ledger_path":%q,"upstream_error_log_path":%q,
"api_keys":[{"sha256":%q,"label":"cust"}],
"pull_catalog":[{"model":"m","source":"hf","hf_url":"https://x","size_bytes":1,"license_required":true}],
"pull_license_keys":[{"sha256":%q,"label":"lic"}],
"models":[{"id":"kat-awq"}]}`, up.URL, filepath.Join(dir, "l.jsonl"), filepath.Join(dir, "e.jsonl"), upper, upper)), 0o600)
	var out bytes.Buffer
	rc := runCheck(cfgPath, &out)
	fmt.Printf("--check exit=%d\n%s", rc, out.String())
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	g := &gateway{}
	if err := g.apply(cfg); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux(g))
	defer srv.Close()
	code, _, body := r107Post(t, srv, "/v1/chat/completions", `{"model":"kat-awq","messages":[{"role":"user","content":"go"}]}`, 0)
	fmt.Printf("completion with the configured key -> %d %s", code, body)
	req, _ := http.NewRequest("GET", srv.URL+"/v1/manifest/m", nil)
	req.Header.Set("Authorization", "Bearer sk")
	resp, _ := http.DefaultClient.Do(req)
	b, _ := io.ReadAll(resp.Body)
	fmt.Printf("manifest with the configured license -> %d %s", resp.StatusCode, b)
	if rc == 0 && code == 401 {
		t.Errorf("--check says config OK, the key it lists answers 401")
	}
	if rc == 0 && resp.StatusCode == 401 {
		t.Errorf("--check says config OK, the license it lists answers 401 on the manifest door")
	}
}
