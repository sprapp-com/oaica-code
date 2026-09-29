package cmd

// Round 125 leg 1 (2026-09-29 audit): an escape sequence in router-authored text reaches the terminal on
// no CLI door, whatever the shape of the body it came in (F125-L1-1, F125-L1-2).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ollama/ollama/internal/sitebuilder"
)

func TestRound125NoRawEscapeFromAnyRouterErrorShape(t *testing.T) {
	for name, body := range map[string]string{
		"error object":  `{"error":{"message":"\u001b]0;pwned\u0007 quota"}}`,
		"plain text":    "\x1b]0;pwned\a upstream said no",
		"empty choices": `{"choices":[],"note":"\u001b[2J"}`,
	} {
		router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(body))
		}))
		t.Setenv("OAICA_HOST", router.URL)
		t.Setenv("OAICA_API_KEY", "sk-live-ROUTERKEY0123456789")
		t.Setenv("HOME", t.TempDir())
		_, _, errRun := oaicaChatComplete("m", []oaicaChatMessage{{Role: "user", Content: "hi"}})
		_, errSite := routerLLM{model: "m"}.Complete(context.Background(), sitebuilder.Request{User: "hi"})
		router.Close()
		for door, err := range map[string]error{"run": errRun, "site": errSite} {
			if err == nil {
				t.Fatalf("%s/%s: no error", name, door)
			}
			if strings.ContainsRune(err.Error(), 0x1b) {
				t.Errorf("%s/%s: a raw escape byte reached the terminal: %q", name, door, err.Error())
			}
		}
	}
}
