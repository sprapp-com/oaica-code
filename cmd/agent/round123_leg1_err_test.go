package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/cmd/launch"
)

func TestRound123ShimErrorRedactedAndBounded(t *testing.T) {
	key := "sk-oaica-0123456789abcdefghijklmnopqrstuv"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid api key ` + r.Header.Get("Authorization")[7:] + ` ` + strings.Repeat("y", 5000) + `"}}`))
	}))
	defer srv.Close()
	s := newShimClient(srv.URL, key, "m", launch.AgentModelMeta{})
	err := s.Chat(context.Background(), &api.ChatRequest{Model: "m", Messages: []api.Message{{Role: "user", Content: "hi"}}}, func(api.ChatResponse) error { return nil })
	msg := err.Error()
	t.Logf("len=%d head=%q", len(msg), msg[:90])
	if strings.Contains(msg, key) {
		t.Fatalf("agent shim error carries the bearer it sent")
	}
	if len(msg) > 400 {
		t.Fatalf("agent shim error is %d bytes: an upstream body is relayed unbounded", len(msg))
	}
}

func TestRound123ShimStreamErrorEventRedacted(t *testing.T) {
	key := "sk-oaica-0123456789abcdefghijklmnopqrstuv"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"authentication_error\",\"message\":\"invalid api key " + r.Header.Get("Authorization")[7:] + "\"}}\n\n"))
	}))
	defer srv.Close()
	s := newShimClient(srv.URL, key, "m", launch.AgentModelMeta{})
	err := s.Chat(context.Background(), &api.ChatRequest{Model: "m", Messages: []api.Message{{Role: "user", Content: "hi"}}}, func(api.ChatResponse) error { return nil })
	if err == nil || strings.Contains(err.Error(), key) {
		t.Fatalf("a stream error event carried the bearer (or no error): %v", err)
	}
}

func TestRound124ShimErrorIsCutOnARuneBoundary(t *testing.T) {
	s := newShimClient("http://127.0.0.1:1", "tok", "m", launch.AgentModelMeta{})
	err := s.safeErr(errors.New("x" + strings.Repeat("模", 400)))
	if !utf8.ValidString(err.Error()) {
		t.Fatalf("cut inside a rune")
	}
}
