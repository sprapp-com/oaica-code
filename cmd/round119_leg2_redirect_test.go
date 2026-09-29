// round119_leg2_redirect_test.go — F119-L2-1 (2026-09-29 audit, round 119): the router bearer does not follow a redirect to another origin on the cmd doors.

package cmd

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestRound119RouterBearerFollowsPortRedirect(t *testing.T) {
	var mu sync.Mutex
	var got []string
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.URL.Path+" auth="+r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[]}`))
	}))
	defer b.Close()
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, b.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer a.Close()
	os.Setenv("OAICA_HOST", a.URL)
	os.Setenv("OAICA_API_KEY", "sk-secret-router-key")
	defer os.Setenv("OAICA_HOST", "")
	defer os.Setenv("OAICA_API_KEY", "")
	_, err := oaicaListModelsDetailedLive()
	t.Logf("A=%s B=%s err=%v", a.URL, b.URL, err)
	_, err = oaicaChatLive("m", []oaicaChatMessage{{Role: "user", Content: "hi"}})
	t.Logf("chat err=%v", err)
	mu.Lock()
	defer mu.Unlock()
	for _, g := range got {
		t.Logf("other origin received: %s", g)
		if g != "" && len(g) > 0 && contains(g, "sk-secret") {
			t.Errorf("RED: bearer followed a redirect to another origin: %s", g)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Every client in oaica_client.go carries the redirect guard: the probe above reaches two of the
// eight, and a client added or copied without it is the same leak.
func TestRound119EveryRouterClientHasTheRedirectGuard(t *testing.T) {
	b, err := os.ReadFile("oaica_client.go")
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, "&http.Client{") && !strings.Contains(line, "CheckRedirect") {
			t.Errorf("oaica_client.go:%d builds an http.Client without CheckRedirect: %s", i+1, strings.TrimSpace(line))
		}
	}
}
