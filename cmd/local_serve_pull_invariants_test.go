package cmd

// local_serve_pull_invariants_test.go — pins two halves of the model
// acquisition path (round-4 audit, 2026-09-26):
//
//   - `oaica pull` builds its byte-stream URL as oaicaHost()+pull_url and
//     attaches the distribution license. pull_url is DATA from the router's
//     manifest; a value that is not a path can carry its own authority, so the
//     manifest used to choose the host that received the license and the bytes
//     installed as the model file.
//   - two `oaica serve` instances of ONE model collide in the registry: it is
//     keyed by model name, so the older instance's teardown deleted the entry
//     the still-running newer one had written, and local_servers.json is the
//     only source for a "<model>:local" row.
//
// Both use a temp HOME and loopback httptest servers only.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func invPullSetup(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".oaica"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("OAICA_MODELS_DIR", filepath.Join(home, "models"))
	// A licensed install: this is the credential a pull carries.
	t.Setenv("OAICA_LICENSE_KEY", "sk-license-9f3c")
	t.Setenv("HF_TOKEN", "")
}

// invPullRouter serves the manifest for one model and streams the body from
// /v1/pull/<model>, recording what the client asked for.
func invPullRouter(t *testing.T, pullURL string, body []byte) (baseURL string, pulls *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/manifest/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"model":      "steal-test",
				"size_bytes": len(body),
				"pull_url":   pullURL,
				"source":     "file",
			})
		case strings.HasPrefix(r.URL.Path, "/v1/pull/"):
			seen = append(seen, fmt.Sprintf("%s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization")))
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &seen
}

// invPullOtherHost is any host that is NOT the router: it records what it was
// handed and serves its own bytes.
func invPullOtherHost(t *testing.T, body []byte) (baseURL string, hits *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, fmt.Sprintf("%s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization")))
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &seen
}

// A pull_url that is a path is what the router's own gateway emits
// ("/v1/pull/<model>"), and it is the only shape that may be accepted: the
// license has to stay on the router and the bytes have to come from it.
func TestPullURLMustAddressTheRouter(t *testing.T) {
	invPullSetup(t)
	body := []byte("GGUF\x00attacker-chosen-weights")

	// Control: the documented shape works, with the license attached.
	routerURL, pulls := invPullRouter(t, "/v1/pull/steal-test", body)
	t.Setenv("OAICA_HOST", routerURL)
	dest, err := oaicaPullModel("steal-test")
	if err != nil {
		t.Fatalf("control: pull with a path pull_url: %v", err)
	}
	if len(*pulls) != 1 || !strings.Contains((*pulls)[0], `auth="Bearer sk-license-9f3c"`) {
		t.Fatalf("control: the router did not receive the licensed pull: %v", *pulls)
	}
	if err := os.Remove(dest); err != nil {
		t.Fatal(err)
	}

	// Every shape that is not a path. "user@host/path" is a valid URL whose
	// authority is everything after the LAST "@", and "//host/path" is
	// protocol-relative — both move the request off the router.
	otherURL, hits := invPullOtherHost(t, body)
	other := strings.TrimPrefix(otherURL, "http://")
	for _, pullURL := range []string{
		"@" + other + "/v1/pull/steal-test",
		"//" + other + "/v1/pull/steal-test",
		"http://" + other + "/v1/pull/steal-test",
		other + "/v1/pull/steal-test",
		"",
	} {
		s2, pulls2 := invPullRouter(t, pullURL, body)
		t.Setenv("OAICA_HOST", s2)
		if _, err := oaicaPullModel("steal-test"); err == nil {
			t.Errorf("manifest pull_url %q was accepted: the router's response data chose where the license key "+
				"and the downloaded weights went (the sibling field hf_url is refused unless it is https on "+
				"huggingface.co for exactly this reason)", pullURL)
		}
		if len(*pulls2) != 0 {
			t.Errorf("manifest pull_url %q: the router was asked for the bytes anyway (%v) — a pull whose URL is "+
				"not the router's must not proceed", pullURL, *pulls2)
		}
		if _, err := oaicaModelPath("steal-test"); err == nil {
			if _, serr := os.Stat(mustModelPath(t, "steal-test")); serr == nil {
				t.Errorf("manifest pull_url %q: a model file was installed at %s", pullURL, mustModelPath(t, "steal-test"))
			}
		}
	}
	if len(*hits) != 0 {
		t.Errorf("a host that is not the router was contacted %v: the manifest's pull_url moved the license key "+
			"off the router", *hits)
	}
}

func mustModelPath(t *testing.T, model string) string {
	t.Helper()
	p, err := oaicaModelPath(model)
	if err != nil {
		t.Fatalf("oaicaModelPath(%q): %v", model, err)
	}
	return p
}

// A serve tears down its OWN entry. Keyed on the model name alone, the older
// instance's exit removed the entry the newer one had written — and
// local_servers.json is the only source for a "<model>:local" row, so a
// running, healthy server disappeared from the picker.
func TestServeTeardownRemovesOnlyItsOwnRegistryEntry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("OAICA_MODELS_DIR", filepath.Join(home, "models"))

	path := filepath.Join(home, ".oaica", "local_servers.json")
	read := func() []oaicaLocalServerEntry { return oaicaReadLocalServers(path) }

	// Two live instances of one model on two ports. Written directly: one
	// process cannot be two serves, but the registry does not care who wrote
	// it, and each entry records the pid of the serve that owns it.
	seed := []oaicaLocalServerEntry{
		{Model: "bonsai", Origin: "http://127.0.0.1:30001", PID: os.Getpid(), StartedAt: "2026-09-26T00:00:00Z"},
		{Model: "bonsai", Origin: "http://127.0.0.1:30002", PID: os.Getpid(), StartedAt: "2026-09-26T00:01:00Z"},
	}
	b, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}

	// The older instance exits.
	oaicaUnregisterLocalServerAt("bonsai", "http://127.0.0.1:30001")
	got := read()
	if len(got) != 1 || got[0].Origin != "http://127.0.0.1:30002" {
		t.Errorf("after the older `oaica serve bonsai` (port 30001) exited, the registry holds %+v — the entry "+
			"for the newer instance (port 30002), still running, must survive. A teardown keyed on the model name "+
			"alone cannot tell its own entry from another live instance's.", got)
	}

	// A teardown whose entry is gone (its entry was replaced by a newer
	// instance, or it never registered) must not disturb the live servers.
	oaicaUnregisterLocalServerAt("bonsai", "http://127.0.0.1:30099")
	if got := read(); len(got) != 1 {
		t.Errorf("an unmatched teardown rewrote the registry to %+v", got)
	}

	// The model-only form, for a caller that knows nothing but the name,
	// removes the OLDEST entry for that model.
	oaicaUnregisterLocalServer("bonsai")
	if got := read(); len(got) != 0 {
		t.Errorf("the model-only unregister left %+v", got)
	}
}
