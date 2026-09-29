package main

// round108_leg3_policy_before_body_test.go — leg 3, round 108 (2026-09-29 audit),
// F108-L3-1 and F108-L3-2.
//
// F108-L3-1: /v1/messages read, parsed and translated the body before it looked at
// the credential (the key check lives in completionHandler, behind the bridge), so
// an anonymous caller got body-content feedback (400 for bad JSON, 413 for size)
// and could make the gateway buffer and parse 16 MiB per connection outside every
// per-key limit, where /v1/chat/completions and /v1/completions answer 401 before
// reading a byte. The credential is now checked first, and the 401 keeps the bytes
// the bridge always gave it.
//
// F108-L3-2: the output ceiling capped max_tokens and nothing that multiplies it.
// `n` and `best_of` fan out inside one request, so a key configured for 100 output
// tokens got ~6,400 on the OpenAI doors (n=64) while /v1/messages, which builds its
// upstream body from scratch, could never carry them. A request that asks for more
// than one choice under a configured ceiling is refused, not silently narrowed:
// the client asked for n choices and would be answered with one.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

const r108Anon401 = `{"error":{"code":"invalid_api_key","message":"missing or invalid API key","type":"invalid_api_key"},"type":"error"}`

func TestMine108AnAnonymousMessagesCallIsRefusedBeforeItsBodyIsRead(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.Copy(io.Discard, r.Body) }))
	defer up.Close()
	srv, _ := r107Gw(t, up, nil)
	for name, body := range map[string]string{
		"valid":      `{"model":"kat-awq","max_tokens":5,"messages":[{"role":"user","content":"go"}]}`,
		"bad json":   `{not json`,
		"no model":   `{"max_tokens":5,"messages":[]}`,
		"huge":       `{"model":"kat-awq","pad":"` + strings.Repeat("x", 17<<20) + `"}`,
		"bad number": `{"model":"kat-awq","max_tokens":5.0,"messages":[{"role":"user","content":"go"}]}`,
	} {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/messages", strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 401 || strings.TrimSpace(string(b)) != r108Anon401 {
			t.Errorf("%s: an anonymous /v1/messages call answered %d %q, want the 401 the OpenAI doors give before reading the body (2026-09-29 audit, round 108, F108-L3-1)", name, resp.StatusCode, strings.TrimSpace(string(b)))
		}
	}
}

func TestMine108AKeyCeilingHoldsAgainstChoiceFanOut(t *testing.T) {
	var upstream string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		upstream = string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	}))
	defer up.Close()
	capped, _ := r107Gw(t, up, []gwKey{{SHA256: keyHash("sk"), Label: "k", MaxCompletionTokens: 100}})
	// A model that publishes no limit and a key that states none: no ceiling.
	ledger := filepath.Join(t.TempDir(), "ledger.jsonl")
	fg := &gateway{}
	if err := fg.apply(gwConfig{UpstreamAddr: up.URL, ListenAddr: ":0", LedgerPath: ledger,
		APIKeys: []gwKey{{SHA256: keyHash("sk"), Label: "k"}},
		Models:  []gwModel{{ID: "kat-awq", OwnedBy: "oaica", ContextLength: 262144, Pricing: gwPricing{Prompt: "0.00000005", Completion: "0.00000012"}}}}); err != nil {
		t.Fatal(err)
	}
	free := httptest.NewServer(mux(fg))
	t.Cleanup(free.Close)
	msg := `"messages":[{"role":"user","content":"go"}]`
	for name, c := range map[string]struct{ path, body string }{
		"chat n":        {"/v1/chat/completions", `{"model":"kat-awq","n":64,` + msg + `}`},
		"chat best_of":  {"/v1/chat/completions", `{"model":"kat-awq","best_of":8,` + msg + `}`},
		"completions n": {"/v1/completions", `{"model":"kat-awq","n":64,"prompt":"hi"}`},
		"n as a string": {"/v1/chat/completions", `{"model":"kat-awq","n":"64",` + msg + `}`},
	} {
		upstream = ""
		code, _, body := r107Post(t, capped, c.path, c.body, 0)
		if code != 400 || upstream != "" {
			t.Errorf("%s: answered %d (upstream saw %q), want a 400 before the upstream — a ceiling of 100 tokens per request is not 6,400 by fan-out (2026-09-29 audit, round 108, F108-L3-2) %s", name, code, upstream, body)
		}
	}
	// n=1 is ordinary and unaffected, and a request with no ceiling anywhere is not held.
	upstream = ""
	if code, _, _ := r107Post(t, capped, "/v1/chat/completions", `{"model":"kat-awq","n":1,`+msg+`}`, 0); code != 200 {
		t.Errorf("n=1 under a ceiling answered %d, want 200 (2026-09-29 audit, round 108, F108-L3-2)", code)
	}
	if code, _, _ := r107Post(t, free, "/v1/chat/completions", `{"model":"kat-awq","n":2,`+msg+`}`, 0); code != 200 {
		t.Errorf("n=2 with no ceiling answered %d, want 200 (2026-09-29 audit, round 108, F108-L3-2)", code)
	}
}
