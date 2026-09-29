package main

// round107_leg3_output_cap_test.go — leg 3, round 107 (2026-09-29 audit), F107-L3-1.
//
// The output-budget clamp rewrote `max_tokens` / `max_completion_tokens` only when
// the key held a positive JSON number, and every other spelling was forwarded
// verbatim, uncapped: the field absent (the default shape of most OpenAI SDK
// calls), null, a numeric string an upstream coerces, a non-positive number.
// gwKey.MaxCompletionTokens is a per-key ceiling and the model's published limit
// is the operator's, and both were escaped by omitting the field — the Anthropic
// surface, where max_tokens is required, could only ask for a huge cap and got
// the clamp. The non-stream 8k clamp that keeps a reply under Cloudflare's edge
// timeout never ran for the same body, and the ledger row recorded a budget of 0.
// A field that is not a positive number is now dropped and, when nothing positive
// is stated, the ceiling is stated in its place.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMine107ABodyThatStatesNoCapIsHeldToTheCeiling(t *testing.T) {
	var got string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	}))
	defer up.Close()
	srv, _ := r107Gw(t, up, []gwKey{{SHA256: keyHash("sk"), Label: "k", MaxCompletionTokens: 100}})
	msg := `"messages":[{"role":"user","content":"go"}]`
	for name, body := range map[string]string{
		"absent":           `{"model":"kat-awq",` + msg + `}`,
		"null":             `{"model":"kat-awq","max_tokens":null,` + msg + `}`,
		"numeric string":   `{"model":"kat-awq","max_tokens":"999999",` + msg + `}`,
		"negative":         `{"model":"kat-awq","max_tokens":-1,` + msg + `}`,
		"zero":             `{"model":"kat-awq","max_tokens":0,` + msg + `}`,
		"other key only":   `{"model":"kat-awq","max_output_tokens":999999,` + msg + `}`,
		"string beside ok": `{"model":"kat-awq","max_tokens":"999999","max_completion_tokens":50,` + msg + `}`,
	} {
		got = ""
		if code, _, _ := r107Post(t, srv, "/v1/chat/completions", body, 0); code != 200 {
			t.Fatalf("%s: answered %d", name, code)
		}
		if !strings.Contains(got, `"max_tokens":100`) && !strings.Contains(got, `"max_completion_tokens":50`) {
			t.Errorf("%s: the upstream saw %s, want the key's ceiling of 100 stated — a body that states no positive cap is held to the operator's ceiling, as one that states a huge one is (2026-09-29 audit, round 107, F107-L3-1)", name, got)
		}
		for _, bad := range []string{`"max_tokens":null`, `"max_tokens":"999999"`, `"max_tokens":-1`, `"max_tokens":0`} {
			if strings.Contains(got, bad) {
				t.Errorf("%s: the upstream saw %s, want the malformed field dropped (2026-09-29 audit, round 107, F107-L3-1)", name, got)
			}
		}
	}
	// A stated cap under the ceiling is untouched.
	got = ""
	r107Post(t, srv, "/v1/chat/completions", `{"model":"kat-awq","max_tokens":40,`+msg+`}`, 0)
	if !strings.Contains(got, `"max_tokens":40`) {
		t.Errorf("a cap under the ceiling was rewritten: the upstream saw %s (2026-09-29 audit, round 107, F107-L3-1)", got)
	}
}
