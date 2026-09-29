// round118_leg2_redirect_test.go — F118-L2-4: the router's key does not follow a same-host redirect to another port (2026-09-29 audit, round 118).

package launch

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestRound118RouterKeyFollowsRedirect(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	var mu sync.Mutex
	seen := map[string]string{}
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path+"#"+r.Header.Get("X-P")+"#"+string(rune(len(seen)+65))] = r.Header.Get("Authorization")
		mu.Unlock()
		w.Write([]byte(`{"object":"list","data":[{"id":"m"}]}`))
	}))
	defer b.Close()
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, b.URL+"/moved"+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer a.Close()
	t.Logf("A=%s B=%s (same host, other port)", a.URL, b.URL)

	t.Setenv("OAICA_API_KEY", "ROUTER-KEY-p118")
	_, _, err := oaicaFetchCloudModelEntriesLiveUncached(a.URL, "")
	t.Logf("router fetch err=%v", err)

	t.Setenv("P118_REMOTE_KEY", "REMOTE-KEY-p118")
	_, err = fetchRemoteModels(userRemote{Name: "p118", BaseURL: a.URL + "/v1", APIKeyEnv: "P118_REMOTE_KEY"})
	t.Logf("remote fetch err=%v", err)

	mu.Lock()
	defer mu.Unlock()
	t.Logf("B saw: %q", seen)
	if seen["/moved/v1/models"] == "" {
		t.Logf("note: remote path key empty at B")
	}
	var router, remote string
	for p, v := range seen {
		if v == "Bearer ROUTER-KEY-p118" {
			router = p
		}
		if v == "Bearer REMOTE-KEY-p118" {
			remote = p
		}
	}
	if router != "" && remote == "" {
		t.Errorf("RED: the router's key reached the redirect target %s; the remote's key did not", router)
	}
}

// The same rule on the LoRA listing, which sends the router key to OAICA_HOST.
func TestRound118LoraListingDoesNotFollowTheKeyToAnotherPort(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	var got sync.Map
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.URL.Path, r.Header.Get("Authorization"))
		w.Write([]byte(`{"data":[]}`))
	}))
	defer b.Close()
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, b.URL+"/moved"+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer a.Close()
	t.Setenv("OAICA_HOST", a.URL)
	t.Setenv("OAICA_API_KEY", "ROUTER-KEY-p118")
	oaicaLiveLoraEntries()
	if v, _ := got.Load("/moved/v1/lora"); v == "Bearer ROUTER-KEY-p118" {
		t.Errorf("the router key followed a redirect to another port on the same host")
	}
}
