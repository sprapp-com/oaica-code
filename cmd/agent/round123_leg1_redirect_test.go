package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/cmd/launch"
)

func TestRound123ShimDoesNotFollowTheBearerAcrossOrigins(t *testing.T) {
	key := "sk-oaica-0123456789abcdefghijklmnopqrstuv"
	var got string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(500)
	}))
	defer other.Close()
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/v1/messages", http.StatusTemporaryRedirect)
	}))
	defer router.Close()
	s := newShimClient(router.URL, key, "m", launch.AgentModelMeta{})
	_ = s.Chat(context.Background(), &api.ChatRequest{Model: "m", Messages: []api.Message{{Role: "user", Content: "hi"}}}, func(api.ChatResponse) error { return nil })
	shimGot := got
	got = ""
	// sibling arm: every credentialed client in cmd/oaica_client.go
	c := &http.Client{CheckRedirect: launch.CredentialSafeRedirect}
	req, _ := http.NewRequest("POST", router.URL+"/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	if resp, err := c.Do(req); err == nil {
		resp.Body.Close()
	}
	t.Logf("shim arm: bearer at the other origin = %q; CLI-client arm: %q", shimGot, got)
	if shimGot != "" {
		t.Fatalf("agent shim followed a cross-origin redirect with the bearer")
	}
}
