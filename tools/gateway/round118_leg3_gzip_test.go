// round118_leg3_gzip_test.go — F118-L3-1 (2026-09-29 audit, round 118): a client Accept-Encoding must not
// reach the upstream, or a compressing upstream is served unmetered and breaks /v1/messages.

package main

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// A compressing upstream (any CDN-fronted OpenAI-compatible host, nginx gzip on):
// gzip when, and only when, the request asks for it.
func round118GzipUpstream() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req map[string]any
		json.Unmarshal(b, &req)
		stream, _ := req["stream"].(bool)
		var out io.Writer = w
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			defer gz.Close()
			out = gz
		}
		if !stream {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(out, `{"id":"c1","object":"chat.completion","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1000,"completion_tokens":500}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(out, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n")
		io.WriteString(out, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		io.WriteString(out, "data: {\"id\":\"c1\",\"choices\":[],\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":500}}\n\n")
		io.WriteString(out, "data: [DONE]\n\n")
	}))
}

func TestRound118GzipUnmetered(t *testing.T) {
	up := round118GzipUpstream()
	defer up.Close()
	srv, ledger := r107Gw(t, up, nil)
	msg := `"messages":[{"role":"user","content":"go"}]`
	for _, ae := range []string{"", "gzip, deflate"} {
		for _, c := range []struct{ path, body string }{
			{"/v1/chat/completions", `{"model":"kat-awq","stream":false,` + msg + `}`},
			{"/v1/chat/completions", `{"model":"kat-awq","stream":true,` + msg + `}`},
			{"/v1/messages", `{"model":"kat-awq","max_tokens":64,"stream":false,` + msg + `}`},
		} {
			req, _ := http.NewRequest("POST", srv.URL+c.path, strings.NewReader(c.body))
			req.Header.Set("Authorization", "Bearer sk")
			req.Header.Set("Content-Type", "application/json")
			if ae != "" {
				req.Header.Set("Accept-Encoding", ae)
			}
			resp, err := http.DefaultTransport.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			served := string(raw)
			if resp.Header.Get("Content-Encoding") == "gzip" {
				zr, err := gzip.NewReader(strings.NewReader(served))
				if err == nil {
					d, _ := io.ReadAll(zr)
					served = string(d)
				}
			}
			if len(served) > 90 {
				served = served[:90]
			}
			fmt.Printf("AE=%-14q %-22s stream=%-5v -> %d CE=%q served(decoded)=%q\n", ae, c.path, strings.Contains(c.body, `"stream":true`), resp.StatusCode, resp.Header.Get("Content-Encoding"), served)
		}
	}
	b, _ := os.ReadFile(ledger)
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var e ledgerEntry
		json.Unmarshal([]byte(line), &e)
		fmt.Printf("row: path=%s stream=%v status=%d prompt=%d completion=%d usage_seen=%v cost=%g\n", e.Path, e.Stream, e.Status, e.PromptTokens, e.CompletionTokens, e.UsageSeen, e.CostUSD)
		if e.Status == 200 && e.CompletionTokens == 0 {
			t.Errorf("served 200 booked with zero output: %s", line)
		}
	}
}
