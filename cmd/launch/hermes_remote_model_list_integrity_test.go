package launch

// hermes_remote_model_list_integrity_test.go — Hermes' provider entry was given
// the DAEMON's model list whatever endpoint it pointed at (2026-09-27 audit,
// round 21, F6).
//
// Configure writes the launch's models into the provider it manages, next to an
// `api` that is either the daemon's /v1 or a user remote's base
// (hermesBaseURLFor). The list came from hermesOllamaClient() unconditionally,
// so a remote-backed launch advertised the local daemon's models — and every
// remote model the bare-id sweep finds — as available on that remote. The user
// picked one from Hermes' own list and the remote answered model-not-found.
//
// The list now comes from the endpoint the entry names, and a remote that
// cannot be listed yields the launched model alone rather than another
// endpoint's inventory.

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

// hermesListModelsEnv points HOME at a temp dir, records the launch's remote in
// remotes.json, and stands up two endpoints: an OpenAI-shaped remote answering
// /v1/models and an Ollama-shaped daemon answering /api/tags. Returns the picker
// name of a model served by the remote.
func hermesListModelsEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	setTestHome(t, home)
	withHermesPlatform(t, "darwin")

	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"models":[{"name":"llama3.2-local"}]}`))
	}))
	t.Cleanup(daemon.Close)
	t.Setenv("OLLAMA_HOST", daemon.URL)

	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-box" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"remote-a"},{"id":"remote-b"}]}`))
	}))
	t.Cleanup(remote.Close)

	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"`+remote.URL+`/v1","api_key":"sk-box"}]}`)

	const name = "box/remote-a"
	if _, ok := resolveRemoteEndpoint(name); !ok {
		t.Fatalf("premise: %q does not resolve to the fixture remote", name)
	}
	if got := hermesBaseURLFor(name); got != remote.URL+"/v1" {
		t.Fatalf("premise: hermesBaseURLFor(%q) = %q, want the remote's base", name, got)
	}
	return name
}

func TestHermesListsModelsFromTheRemoteItsProviderPointsAt(t *testing.T) {
	name := hermesListModelsEnv(t)

	got := (&Hermes{}).listModels(name)

	if slices.Contains(got, "llama3.2-local") {
		t.Errorf("listModels(%q) = %v: the provider entry's api is the remote's base, and the daemon's models are not served there", name, got)
	}
	for _, want := range []string{"remote-a", "remote-b"} {
		if !slices.Contains(got, want) {
			t.Errorf("listModels(%q) = %v, want the remote's own %q among them", name, got, want)
		}
	}
	if got[0] != "remote-a" {
		t.Errorf("listModels(%q)[0] = %q, want the configured model first", name, got[0])
	}
}

// TestHermesStillListsTheDaemonsOwnModels is the control: a daemon-backed
// launch keeps the inventory it always had, so the fix above did not simply
// stop listing.
func TestHermesStillListsTheDaemonsOwnModels(t *testing.T) {
	hermesListModelsEnv(t)
	// A daemon-backed launch: the name resolves to no remote. The daemon's
	// inventory is stubbed, so the sweep for the bare id must not go out.
	stubBareIndex(t, map[string][]string{})

	got := (&Hermes{}).listModels("llama3.2")

	if !slices.Contains(got, "llama3.2-local") {
		t.Errorf("listModels(llama3.2) = %v, want the daemon's inventory: a daemon-backed launch still advertises the daemon's models", got)
	}
	if got[0] != "llama3.2" {
		t.Errorf("listModels(llama3.2)[0] = %q, want the configured model first", got[0])
	}
}

// TestHermesRemoteModelListUsesTheBareIDSpelling pins the spelling the config
// selects by: the entry stores the bare upstream id (hermesModelIDFor), so the
// ids fetched from the remote are written in the same form the picker used.
func TestHermesRemoteModelListUsesTheBareIDSpelling(t *testing.T) {
	name := hermesListModelsEnv(t)

	got := (&Hermes{}).listModels(name)
	if !slices.Contains(got, "remote-b") {
		t.Errorf("listModels(%q) = %v, want the bare remote id remote-b", name, got)
	}
	ep, _ := resolveRemoteEndpoint(name)
	if got[0] != ep.UpstreamModel {
		t.Errorf("listModels(%q)[0] = %q, want the upstream id %q the provider selects by", name, got[0], ep.UpstreamModel)
	}
}
