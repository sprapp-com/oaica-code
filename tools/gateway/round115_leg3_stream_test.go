package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// vllmLaxBool mirrors pydantic v2 lax-mode bool coercion (vLLM's
// ChatCompletionRequest.stream: Optional[bool]).
func vllmLaxBool(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case float64:
		return x == 1
	case string:
		switch strings.ToLower(x) {
		case "true", "1", "yes", "on", "t", "y":
			return true
		}
	}
	return false
}

func TestRound115StreamSpelledAsString(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req map[string]any
		json.Unmarshal(b, &req)
		if !vllmLaxBool(req["stream"]) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"c1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1000,"completion_tokens":500}}`)
			return
		}
		so, _ := req["stream_options"].(map[string]any)
		incl, _ := so["include_usage"].(bool)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n")
		io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		if incl {
			io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[],\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":500}}\n\n")
		}
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer up.Close()
	srv, ledger := r107Gw(t, up, nil)
	msg := `"messages":[{"role":"user","content":"go"}]`
	for _, spell := range []string{`true`, `"true"`, `1`, `"yes"`} {
		code, hdr, body := r107Post(t, srv, "/v1/chat/completions", `{"model":"kat-awq","stream":`+spell+`,`+msg+`}`, 0)
		fmt.Printf("stream=%-7s -> %d ct=%s served-frames=%d\n", spell, code, hdr.Get("Content-Type"), strings.Count(body, "data:"))
	}
	b, _ := os.ReadFile(ledger)
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var e ledgerEntry
		json.Unmarshal([]byte(line), &e)
		fmt.Printf("row: stream=%v status=%d prompt=%d completion=%d usage_seen=%v cost=%g\n", e.Stream, e.Status, e.PromptTokens, e.CompletionTokens, e.UsageSeen, e.CostUSD)
		if e.Status == 200 && !e.UsageSeen {
			t.Errorf("served turn metered as zero: %s", line)
		}
	}
}
