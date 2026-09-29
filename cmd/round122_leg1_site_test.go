package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ollama/ollama/internal/sitebuilder"
)

const round122Key = "sk-live-PROBESECRET0123456789"

func TestRound122SiteRedirectKeepsBearer(t *testing.T) {
	var mu sync.Mutex
	var got []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer other.Close()
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer router.Close()
	t.Setenv("OAICA_HOST", router.URL)
	t.Setenv("OAICA_API_KEY", round122Key)
	t.Setenv("HOME", t.TempDir())

	_, _, errChat := oaicaChatComplete("m", []oaicaChatMessage{{Role: "user", Content: "hi"}})
	mu.Lock()
	chatSaw := strings.Join(got, "|")
	got = nil
	mu.Unlock()
	_, errSite := routerLLM{model: "m"}.Complete(context.Background(), sitebuilder.Request{User: "hi"})
	mu.Lock()
	siteSaw := strings.Join(got, "|")
	mu.Unlock()
	t.Logf("other port (%s) saw Authorization: chat=%q (err=%v) site=%q (err=%v)", other.URL, chatSaw, errChat, siteSaw, errSite)
	if chatSaw != siteSaw {
		t.Errorf("DIVERGE: run/chat door and site door hand the redirect target different credentials")
	}
}

func TestRound122SiteErrorBodyUnredacted(t *testing.T) {
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("<html>upstream rejected key " + round122Key + "</html>"))
	}))
	defer router.Close()
	t.Setenv("OAICA_HOST", router.URL)
	t.Setenv("OAICA_API_KEY", round122Key)
	t.Setenv("HOME", t.TempDir())
	_, _, errChat := oaicaChatComplete("m", []oaicaChatMessage{{Role: "user", Content: "hi"}})
	_, errSite := routerLLM{model: "m"}.Complete(context.Background(), sitebuilder.Request{User: "hi"})
	t.Logf("chat: %v", errChat)
	t.Logf("site: %v", errSite)
	if strings.Contains(errSite.Error(), round122Key) != strings.Contains(errChat.Error(), round122Key) {
		t.Errorf("DIVERGE: site door prints the key the chat door redacts")
	}
	// empty-choices arm
	router2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[],"note":"key ` + round122Key + `"}`))
	}))
	defer router2.Close()
	t.Setenv("OAICA_HOST", router2.URL)
	_, _, errChat = oaicaChatComplete("m", []oaicaChatMessage{{Role: "user", Content: "hi"}})
	_, errSite = routerLLM{model: "m"}.Complete(context.Background(), sitebuilder.Request{User: "hi"})
	t.Logf("empty chat: %v", errChat)
	t.Logf("empty site: %v", errSite)
	if strings.Contains(errSite.Error(), round122Key) != strings.Contains(errChat.Error(), round122Key) {
		t.Errorf("DIVERGE (empty arm): site door prints the key the chat door redacts")
	}
}
