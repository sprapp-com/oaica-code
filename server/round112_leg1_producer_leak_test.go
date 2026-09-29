package server

// round112_leg1_producer_leak_test.go — leg 1, round 112 (2026-09-29 audit), F112-L1-1.
//
// Each door's producer goroutine hands chunks to its consumer over an unbuffered
// channel with a plain `ch <- x`, and the consumer (streamResponse, writeChatResponse,
// waitForStream) returns as soon as the client is gone or a write fails. Nothing
// drained the channel after that, so a producer that had a send in flight blocked
// forever: inside the runner callback, which meant llm.Completion never returned, its
// deferred sem.Release never ran, and one of the model's parallel slots was gone for the
// life of the process; or on the final error frame, which leaked the goroutine and
// everything it captured. A client that stalls and then disconnects (a slow reader, a
// dropped mobile link) triggers it on every door that streams. With NumParallel N, N such
// clients wedge the model: new requests block in sem.Acquire. The consumers now drain the
// channel when they return, so the producer's sends complete, the runner sees its context
// cancelled, and the slot is released.

import (
	"context"
	"net"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/fs/ggml"
	"github.com/ollama/ollama/llm"
	"github.com/ollama/ollama/middleware"
)

func r112Settle(base int) int {
	n := 0
	for i := 0; i < 60; i++ {
		time.Sleep(50 * time.Millisecond)
		if n = runtime.NumGoroutine(); n <= base {
			return n
		}
	}
	return n
}

// r112StalledClients runs 15 clients that each read the first bytes of a stream whose
// chunks are large enough to fill the socket, then reset the connection, against the
// given door, and returns the runner calls, how many returned, and the goroutines
// before and after.
func r112StalledClients(t *testing.T, surface string) (calls, returned int32, before, after int) {
	t.Helper()
	t.Setenv("OLLAMA_CONTEXT_LENGTH", "4096")
	t.Setenv("OLLAMA_GO_TEMPLATE", "")
	gin.SetMode(gin.TestMode)
	setTestHome(t, t.TempDir())
	var inFn, done atomic.Int32
	runner := &mockRunner{CompletionFn: func(ctx context.Context, _ llm.CompletionRequest, fn func(llm.CompletionResponse)) error {
		inFn.Add(1)
		defer done.Add(1)
		for i := 0; i < 200000; i++ {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			fn(llm.CompletionResponse{Content: strings.Repeat("x", 256*1024)})
			time.Sleep(time.Millisecond)
		}
		fn(llm.CompletionResponse{Done: true})
		return nil
	}, ChatFn: func(ctx context.Context, _ llm.ChatRequest, fn func(llm.ChatResponse)) error {
		inFn.Add(1)
		defer done.Add(1)
		for i := 0; i < 200000; i++ {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			fn(llm.ChatResponse{Message: api.Message{Role: "assistant", Content: strings.Repeat("x", 256*1024)}})
			time.Sleep(time.Millisecond)
		}
		fn(llm.ChatResponse{Done: true})
		return nil
	}}
	s := newServerWithMockRunner(t, runner)
	createMinimalGGUFModel(t, s, "m", ggml.KV{"tokenizer.chat_template": zzTmpl}, "",
		map[string]any{"capabilities": []string{"completion", "tools"}})
	r := gin.New()
	path := "/api/chat"
	switch surface {
	case "openai":
		r.Use(middleware.ChatMiddleware())
		path = "/v1/chat/completions"
	case "anthropic":
		r.Use(middleware.AnthropicMessagesMiddleware())
		path = "/v1/messages"
	}
	handler, body0 := s.ChatHandler, ""
	if surface == "generate" {
		path, handler, body0 = "/api/generate", s.GenerateHandler, `{"model":"m","prompt":"hi","stream":true}`
	}
	r.POST(path, handler)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	time.Sleep(200 * time.Millisecond)
	before = runtime.NumGoroutine()
	for i := 0; i < 15; i++ {
		conn, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		body := body0
		if body == "" {
			body = zzBody(surface, true, "m")
		}
		conn.Write([]byte("POST " + path + " HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body))
		time.Sleep(300 * time.Millisecond) // the producer outruns a client that is not reading
		buf := make([]byte, 4096)
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		conn.Read(buf)
		if tc, ok := conn.(*net.TCPConn); ok {
			tc.SetLinger(0)
		}
		conn.Close()
	}
	after = r112Settle(before)
	return inFn.Load(), done.Load(), before, after
}

func TestMine112AStalledClientsDoNotWedgeTheRunner(t *testing.T) {
	for _, surface := range []string{"native", "openai", "anthropic", "generate"} {
		t.Run(surface, func(t *testing.T) {
			calls, returned, before, after := r112StalledClients(t, surface)
			if calls == 0 {
				t.Fatalf("premise: the runner was never called")
			}
			if returned != calls || after > before+2 {
				t.Errorf("%s: runner calls=%d returned=%d, goroutines before=%d after=%d — a client that stalls and disconnects must not leave a runner call, and its parallel slot, blocked on a send nobody reads (2026-09-29 audit, round 112, F112-L1-1)", surface, calls, returned, before, after)
			}
		})
	}
}
