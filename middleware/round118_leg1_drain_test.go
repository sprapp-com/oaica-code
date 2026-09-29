// round118_leg1_followup_test.go — F118-L1-1/2 (2026-09-29 audit, round 118): the web_search loop's follow-up turn
// is served in-process (a drain that closed the listener does not fail it) and keeps the request's thinking and format.

package middleware

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ollama/ollama/anthropic"
	"github.com/ollama/ollama/api"
)

// round118Turn serves one /v1/messages request on a REAL http.Server (the one
// Serve() drains) and, when drain is set, calls srv.Shutdown -- exactly what
// server.drainServer does first -- while the request is in flight.
func round118Turn(t *testing.T, webSearch, drain bool) (int, string) {
	gin.SetMode(gin.TestMode)
	enableCloudForTest(t)

	release := make(chan struct{})
	inFlight := make(chan struct{}, 1)

	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inFlight <- struct{}{}
		<-release
		_ = json.NewEncoder(w).Encode(anthropic.OllamaWebSearchResponse{Results: []anthropic.OllamaWebSearchResult{{Title: "R", URL: "https://ok.example/x", Content: "c"}}})
	}))
	defer search.Close()
	orig := anthropic.WebSearchEndpoint
	anthropic.WebSearchEndpoint = search.URL
	defer func() { anthropic.WebSearchEndpoint = orig }()

	router := gin.New()
	router.POST("/v1/messages", AnthropicMessagesMiddleware(), func(c *gin.Context) {
		var resp api.ChatResponse
		if webSearch {
			resp = api.ChatResponse{Model: "test-model", Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{r70Search()}}, Done: true, DoneReason: "stop"}
		} else {
			inFlight <- struct{}{}
			<-release
			resp = api.ChatResponse{Model: "test-model", Message: api.Message{Role: "assistant", Content: "final answer"}, Done: true, DoneReason: "stop"}
		}
		data, _ := json.Marshal(resp)
		c.Writer.Write(data)
	})
	// The loop's follow-up: the server's own /api/chat.
	router.POST("/api/chat", func(c *gin.Context) {
		data, _ := json.Marshal(api.ChatResponse{Model: "test-model", Message: api.Message{Role: "assistant", Content: "final answer"}, Done: true, DoneReason: "stop"})
		c.Writer.Write(data)
	})

	// What server.Serve does: the loop's follow-up is served by the same handler in-process.
	SetFollowUpHandler(router)
	t.Cleanup(func() { followUpHandlerValue.Store((*http.Handler)(nil)) })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: router}
	go srv.Serve(ln)
	defer srv.Close()
	t.Setenv("OLLAMA_HOST", "http://"+ln.Addr().String())

	tools := ""
	if webSearch {
		tools = `,"tools":[{"type":"web_search_20250305","name":"web_search"}]`
	}
	body := `{"model":"test-model","max_tokens":100,"messages":[{"role":"user","content":"news"}]` + tools + `}`

	type result struct {
		code int
		body string
	}
	done := make(chan result, 1)
	go func() {
		resp, err := http.Post("http://"+ln.Addr().String()+"/v1/messages", "application/json", strings.NewReader(body))
		if err != nil {
			done <- result{-1, err.Error()}
			return
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		done <- result{resp.StatusCode, string(b)}
	}()

	<-inFlight
	if drain {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			srv.Shutdown(ctx)
		}()
		time.Sleep(300 * time.Millisecond) // listener closed
	}
	close(release)
	r := <-done
	return r.code, r.body
}

func TestRound118DrainPlainTurn(t *testing.T) {
	for _, drain := range []bool{false, true} {
		code, body := round118Turn(t, false, drain)
		t.Logf("plain turn drain=%v: %d %s", drain, code, body)
	}
}

func TestRound118DrainWebSearchTurn(t *testing.T) {
	for _, drain := range []bool{false, true} {
		code, body := round118Turn(t, true, drain)
		t.Logf("web_search turn drain=%v: %d %s", drain, code, body)
		if drain && !strings.Contains(body, "final answer") {
			t.Errorf("RED: the same turn that answers without a drain is not answered during the drain")
		}
	}
}
