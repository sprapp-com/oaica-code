package server

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/ollama/ollama/anthropic"
	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/middleware"
)

func TestRound119CloudLegacyWebSearchAcceptEncoding(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setTestHome(t, t.TempDir())
	t.Setenv("OLLAMA_NO_CLOUD", "")

	var n atomic.Int32
	var sawAE atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k := n.Add(1)
		sawAE.Store(r.Header.Get("Accept-Encoding"))
		var resp api.ChatResponse
		if k == 1 {
			args := api.NewToolCallFunctionArguments()
			args.Set("query", "latest news")
			resp = api.ChatResponse{Model: "m", Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{{ID: "c1", Function: api.ToolCallFunction{Name: "web_search", Arguments: args}}}}, Done: true, DoneReason: "stop"}
		} else {
			resp = api.ChatResponse{Model: "m", Message: api.Message{Role: "assistant", Content: "final answer"}, Done: true, DoneReason: "stop"}
		}
		b, _ := json.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		// A standard compressing front (what a CDN does when asked).
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			gz.Write(b)
			gz.Close()
			return
		}
		w.Write(b)
	}))
	defer upstream.Close()
	ob, os_ := cloudProxyBaseURL, cloudProxySignRequest
	cloudProxyBaseURL = upstream.URL
	cloudProxySignRequest = func(context.Context, *http.Request) error { return nil }
	t.Cleanup(func() { cloudProxyBaseURL, cloudProxySignRequest = ob, os_ })

	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(anthropic.OllamaWebSearchResponse{Results: []anthropic.OllamaWebSearchResult{{Title: "R", URL: "https://ok.example/x", Content: "c"}}})
	}))
	defer search.Close()
	orig := anthropic.WebSearchEndpoint
	anthropic.WebSearchEndpoint = search.URL
	defer func() { anthropic.WebSearchEndpoint = orig }()

	s := &Server{}
	router, err := s.GenerateRoutes()
	if err != nil {
		t.Fatal(err)
	}
	middleware.SetFollowUpHandler(router)
	local := httptest.NewServer(router)
	defer local.Close()
	t.Setenv("OLLAMA_HOST", local.URL)

	for _, ae := range []string{"", "gzip, deflate, br"} {
		for _, stream := range []string{"false", "true"} {
			n.Store(0)
			body := `{"model":"kimi-k2.5:cloud","max_tokens":64,"stream":` + stream + `,"messages":[{"role":"user","content":"news"}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`
			req, _ := http.NewRequest(http.MethodPost, local.URL+"/v1/messages", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			if ae != "" {
				req.Header.Set("Accept-Encoding", ae)
			}
			resp, err := (&http.Transport{DisableCompression: true}).RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			decoded := string(raw)
			if resp.Header.Get("Content-Encoding") == "gzip" {
				zr, zerr := gzip.NewReader(strings.NewReader(string(raw)))
				if zerr != nil {
					decoded = "CLIENT GZIP DECODE FAILED: " + zerr.Error() + " | raw=" + string(raw)
				} else {
					d, rerr := io.ReadAll(zr)
					decoded = string(d)
					if rerr != nil {
						decoded += " | read err " + rerr.Error()
					}
				}
			}
			full := decoded
			if len(decoded) > 600 {
				decoded = decoded[:600]
			}
			t.Logf("client Accept-Encoding=%q stream=%s -> %d Content-Encoding=%q upstreamCalls=%d\n  %s", ae, stream, resp.StatusCode, resp.Header.Get("Content-Encoding"), n.Load(), decoded)
			if !strings.Contains(full, "final answer") || n.Load() != 2 {
				t.Errorf("client Accept-Encoding=%q stream=%s: the turn was not answered (cloud calls %d)", ae, stream, n.Load())
			}
		}
	}
}
