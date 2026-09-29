package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestRound115InfiniteTierPrice(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	}))
	defer up.Close()
	for _, price := range []string{"Inf", "1e308"} {
		dir := t.TempDir()
		ledger := filepath.Join(dir, "l.jsonl")
		cfgPath := filepath.Join(dir, "gw.json")
		os.WriteFile(cfgPath, []byte(fmt.Sprintf(`{"upstream_addr":%q,"ledger_path":%q,"upstream_error_log_path":%q,
"api_keys":[{"sha256":%q,"label":"k"}],
"models":[{"id":"kat-awq","pricing":{"prompt":"0.00000005","completion":"0.0000001"},
  "pricing_tiers":[{"up_to_prompt_tokens":32000,"prompt":%q},{"prompt":"0.0000001"}]}]}`, up.URL, ledger, filepath.Join(dir, "e"), keyHash("sk"), price)), 0o600)
		var out bytes.Buffer
		rc := runCheck(cfgPath, &out)
		cfg, err := loadConfig(cfgPath)
		if err != nil {
			fmt.Printf("price %s: refused on load: %v\n", price, err)
			continue
		}
		g := &gateway{}
		if err := g.apply(cfg); err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(mux(g))
		code, _, _ := r107Post(t, srv, "/v1/chat/completions", `{"model":"kat-awq","messages":[{"role":"user","content":"go"}]}`, 0)
		srv.Close()
		b, _ := os.ReadFile(ledger)
		fmt.Printf("tier price %q: --check exit=%d, served=%d, ledger bytes=%d\n", price, rc, code, len(b))
		if code == 200 && len(b) == 0 {
			t.Errorf("tier price %q: --check OK, the turn was served, the ledger has no row", price)
		}
	}
}
