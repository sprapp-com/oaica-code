package cmd

// site_response_bound_integrity_test.go — routerLLM.Complete read the chat
// response with an unbounded io.ReadAll (2026-09-26 audit). The endpoint is
// whatever OAICA_HOST names — a loopback router by default, but any process
// listening on the port answers this request — and a response with no end
// would be read into memory until the machine gave out. Every other fetch in
// this repo bounds the body it reads (context_window_remote.go's 4 MiB for
// /v1/models, parseAPIError's 1 MiB); site.go did not.
//
// A bound is not enough on its own: a truncated body is a JSON parse error,
// which is a confusing way to report "the far side sent too much". The cap is
// checked explicitly so the error names the real cause.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ollama/ollama/internal/sitebuilder"
)

func TestAnOversizedChatResponseIsRefused(t *testing.T) {
	// More than the cap, and still inside one JSON document, so the only
	// thing that can stop the read is the bound itself.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"`))
		chunk := strings.Repeat("a", 1<<20)
		for i := 0; i < maxChatResponseBytes/(1<<20)+2; i++ {
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
		}
		_, _ = w.Write([]byte(`"}}]}`))
	}))
	defer srv.Close()
	t.Setenv("OAICA_HOST", srv.URL)

	_, err := routerLLM{model: "m"}.Complete(context.Background(), sitebuilder.Request{
		System: "s", User: "u", MaxTokens: 10,
	})
	if err == nil {
		t.Fatalf("Complete returned no error for a response larger than %d bytes — it read the whole body into memory", maxChatResponseBytes)
	}
	if !strings.Contains(err.Error(), "larger than") {
		t.Errorf("the error does not name the size limit: %v", err)
	}
}

// The control: an ordinary response still resolves.
func TestAnOrdinaryChatResponseIsRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hello from the router"}}]}`))
	}))
	defer srv.Close()
	t.Setenv("OAICA_HOST", srv.URL)

	got, err := routerLLM{model: "m"}.Complete(context.Background(), sitebuilder.Request{
		System: "s", User: "u", MaxTokens: 10,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got != "hello from the router" {
		t.Errorf("got %q", got)
	}
}
