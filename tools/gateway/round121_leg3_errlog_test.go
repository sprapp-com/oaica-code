package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A vLLM-shaped 400: FastAPI's RequestValidationError handler states str(exc) plus
// exc.errors(), whose "input" is the offending part of the request body verbatim.
func TestRound121ErrorLogPersistsRequestContent(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []map[string]any `json:"messages"`
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &req)
		last := req.Messages[len(req.Messages)-1]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		msg := fmt.Sprintf("1 validation error: {'type': 'literal_error', 'loc': ('body', 'messages', 0, 'role'), 'msg': \"Input should be 'user'\", 'input': %v}", last)
		json.NewEncoder(w).Encode(map[string]any{"object": "error", "message": msg, "type": "BadRequestError", "code": 400})
	}))
	defer up.Close()
	dir := t.TempDir()
	errLog := filepath.Join(dir, "errors.jsonl")
	g := &gateway{}
	if err := g.apply(gwConfig{UpstreamAddr: up.URL, ListenAddr: ":0", LedgerPath: filepath.Join(dir, "ledger.jsonl"), UpstreamErrorLogPath: errLog,
		APIKeys: []gwKey{{SHA256: keyHash("sk"), Label: "k"}},
		Models:  []gwModel{{ID: "kat-awq", OwnedBy: "oaica", ContextLength: 262144, MaxCompletionTokens: 32768, Pricing: gwPricing{Prompt: "0", Completion: "0"}}}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux(g))
	defer srv.Close()
	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		secret := "PATIENT-DIAGNOSIS-" + strings.Trim(path, "/v")
		body := `{"model":"kat-awq","max_tokens":5,"messages":[{"role":"user","content":"` + secret + `"}]}`
		st, _, rb := r107Post(t, srv, path, body, 0)
		time.Sleep(200 * time.Millisecond)
		logged, _ := os.ReadFile(errLog)
		t.Logf("%s: client status %d body %.120s", path, st, rb)
		if strings.Contains(string(logged), secret) {
			t.Errorf("%s: request content %q persisted to the on-disk upstream error log (0600, default-on): %s", path, secret, strings.TrimSpace(string(logged)))
		}
	}
}
