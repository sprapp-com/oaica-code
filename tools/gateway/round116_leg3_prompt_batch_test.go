// round116_leg3_prompt_batch_test.go — F116-L3-1 (2026-09-29 audit, round 116): a batched prompt is a fan-out
// the per-request output ceiling must refuse, as it refuses n and best_of.

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRound116PromptBatchFanOut(t *testing.T) {
	var upstream string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		upstream = string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c1","object":"text_completion","choices":[{"index":0,"finish_reason":"length","text":"x"}],"usage":{"prompt_tokens":10,"completion_tokens":6400}}`)
	}))
	defer up.Close()
	capped, _ := r107Gw(t, up, []gwKey{{SHA256: keyHash("sk"), Label: "k", MaxCompletionTokens: 100}})
	ps := make([]string, 64)
	for i := range ps {
		ps[i] = `"hi"`
	}
	for name, body := range map[string]string{
		"n=64 (refused since r108)": `{"model":"kat-awq","n":64,"prompt":"hi"}`,
		"prompt batch of 64":        `{"model":"kat-awq","prompt":[` + strings.Join(ps, ",") + `]}`,
		"token-id batch of 64":      `{"model":"kat-awq","prompt":[` + strings.Repeat(`[1,2],`, 63) + `[1,2]]}`,
	} {
		upstream = ""
		code, _, rb := r107Post(t, capped, "/v1/completions", body, 0)
		t.Logf("%s: status=%d upstream-max_tokens=100? %v upstream=%.160s resp=%.100s", name, code, strings.Contains(upstream, `"max_tokens":100`), upstream, rb)
		if code != 400 || upstream != "" {
			t.Errorf("%s: forwarded (status %d); a 100-token key ceiling fans out 64x", name, code)
		}
	}
}

// The control: one prompt in any spelling is served under the same ceiling.
func TestRound116SinglePromptSpellingsAreServed(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c1","object":"text_completion","choices":[{"index":0,"finish_reason":"stop","text":"x"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	}))
	defer up.Close()
	capped, _ := r107Gw(t, up, []gwKey{{SHA256: keyHash("sk"), Label: "k", MaxCompletionTokens: 100}})
	for name, body := range map[string]string{
		"string":            `{"model":"kat-awq","prompt":"hi"}`,
		"one-element list":  `{"model":"kat-awq","prompt":["hi"]}`,
		"flat token ids":    `{"model":"kat-awq","prompt":[1,2,3]}`,
		"one token-id list": `{"model":"kat-awq","prompt":[[1,2,3]]}`,
		"empty list":        `{"model":"kat-awq","prompt":[]}`,
	} {
		if code, _, rb := r107Post(t, capped, "/v1/completions", body, 0); code != 200 {
			t.Errorf("%s: a single prompt answered %d %.100s", name, code, rb)
		}
	}
}
