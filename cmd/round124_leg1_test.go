package cmd

// Round 124 leg 1 (2026-09-29 audit): a router-authored error message is bounded, quoted if it
// carries control characters, and cut on a rune boundary (F124-L1-2, F124-L1-4).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ollama/ollama/internal/sitebuilder"
)

func TestRound124RouterErrorMessageIsBoundedAndQuoted(t *testing.T) {
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"\u001b]0;pwned\u0007` + strings.Repeat("模", 400000) + `"}}`))
	}))
	defer router.Close()
	t.Setenv("OAICA_HOST", router.URL)
	t.Setenv("OAICA_API_KEY", "sk-live-ROUTERKEY0123456789")
	t.Setenv("HOME", t.TempDir())
	_, _, err := oaicaChatComplete("m", []oaicaChatMessage{{Role: "user", Content: "hi"}})
	if err == nil {
		t.Fatal("no error")
	}
	msg := err.Error()
	if len(msg) > 1200 {
		t.Errorf("oaica run printed a %d-byte router message", len(msg))
	}
	if strings.ContainsRune(msg, 0x1b) {
		t.Errorf("a raw escape byte reached the terminal: %q", msg[:40])
	}
	if !utf8.ValidString(msg) {
		t.Errorf("the message was cut inside a rune")
	}
	// the sibling door agrees
	_, serr := routerLLM{model: "m"}.Complete(context.Background(), sitebuilder.Request{User: "hi"})
	if serr != nil && !utf8.ValidString(serr.Error()) {
		t.Errorf("oaica site cut its message inside a rune")
	}
}

func TestRound124TruncateForErrorCutsOnARuneBoundary(t *testing.T) {
	if got := truncateForError([]byte("x" + strings.Repeat("模", 200))); !utf8.ValidString(got) {
		t.Errorf("cut inside a rune: %q", got[len(got)-8:])
	}
}
