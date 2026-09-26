package launch

// pi_provider_repoint_integrity_test.go — Pi's single provider kept the first
// remote's base URL and bearer forever (2026-09-26 audit, tenth round).
//
// Pi has ONE "ollama" provider slot, and every model this package registers is
// served from that slot's baseUrl with that slot's apiKey. The base URL and key
// were only ever written when the provider did not exist yet — afterwards the
// provider was reused as-is. So after `oaica launch pi acme/foo`, a later
// `oaica launch pi llama3.2` registered a LOCAL model id in a provider still
// pointing at acme's host with acme's key: the daemon's model was posted to
// someone else's API, with a credential, and the run failed at the far end.
//
// The slot has to match the models being registered in this launch — that is
// the only reading under which a single provider is coherent — and a user whose
// own provider config is being repointed is told, not silently rewritten.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// piProvider reads the "ollama" provider the last Configure wrote.
func piProvider(t *testing.T, home string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".pi", "agent", "models.json"))
	if err != nil {
		t.Fatalf("read models.json: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse models.json: %v", err)
	}
	providers, _ := doc["providers"].(map[string]any)
	provider, _ := providers["ollama"].(map[string]any)
	if provider == nil {
		t.Fatalf("no ollama provider in %v", providers)
	}
	return provider
}

func TestALocalPiLaunchRepointsTheProviderOffTheRemote(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[{"name":"acme","base_url":"https://acme.example/v1","api_key":"sk-acme-SECRET-1234567890","tool_format":"tool_calls"}]}`)
	stubDaemon(t)
	stubBareIndex(t, map[string][]string{})

	pi := &Pi{}
	if err := pi.Edit([]LaunchModel{{Name: "acme/foo"}}); err != nil {
		t.Fatalf("configure remote model: %v", err)
	}
	remote := piProvider(t, home)
	if got, _ := remote["baseUrl"].(string); got != "https://acme.example/v1" {
		t.Fatalf("premise: the provider points at %q, want the remote", got)
	}

	// Now a LOCAL model on the same single-provider slot.
	if err := pi.Edit([]LaunchModel{{Name: "llama3.2"}}); err != nil {
		t.Fatalf("configure local model: %v", err)
	}
	local := piProvider(t, home)
	base, _ := local["baseUrl"].(string)
	if base == "https://acme.example/v1" {
		t.Errorf("the provider still points at the remote (%q) after a local-model launch — Pi has one provider slot, so the local model id is posted to the remote with the remote's key", base)
	}
	if key, _ := local["apiKey"].(string); key == "sk-acme-SECRET-1234567890" {
		t.Errorf("the provider still carries the remote's credential after a local-model launch")
	}
	// And the local model really is registered in that slot.
	assertPiHasModel(t, local, "llama3.2")
}

// The reverse direction: a remote launch after a local one must carry the
// remote's URL and key, or the remote model 401s.
func TestARemotePiLaunchRepointsTheProviderOntoTheRemote(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[{"name":"acme","base_url":"https://acme.example/v1","api_key":"sk-acme-SECRET-1234567890","tool_format":"tool_calls"}]}`)
	stubDaemon(t)
	stubBareIndex(t, map[string][]string{})

	pi := &Pi{}
	if err := pi.Edit([]LaunchModel{{Name: "llama3.2"}}); err != nil {
		t.Fatalf("configure local model: %v", err)
	}
	if err := pi.Edit([]LaunchModel{{Name: "acme/foo"}}); err != nil {
		t.Fatalf("configure remote model: %v", err)
	}

	provider := piProvider(t, home)
	if base, _ := provider["baseUrl"].(string); base != "https://acme.example/v1" {
		t.Errorf("the provider points at %q, want the remote the model was resolved on", base)
	}
	if key, _ := provider["apiKey"].(string); key != "sk-acme-SECRET-1234567890" {
		t.Errorf("the provider carries %q, want the remote's token", key)
	}
	assertPiHasModel(t, provider, "foo")
}

// A user's own provider value is not repointed in silence.
func TestRepointingTheProviderIsAnnounced(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[{"name":"acme","base_url":"https://acme.example/v1","api_key":"sk-acme-SECRET-1234567890","tool_format":"tool_calls"}]}`)
	stubDaemon(t)
	stubBareIndex(t, map[string][]string{})

	dir := filepath.Join(home, ".pi", "agent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The user configured this slot by hand, for their own endpoint.
	body := `{"providers":{"ollama":{"baseUrl":"http://my-own-box:11434/v1","api":"openai-completions","apiKey":"mine"}}}`
	if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStderr(t, func() {
		if err := (&Pi{}).Edit([]LaunchModel{{Name: "acme/foo"}}); err != nil {
			t.Errorf("configure: %v", err)
		}
	})
	if !strings.Contains(out, "my-own-box") {
		t.Errorf("the provider was repointed from the user's endpoint with nothing said:\n%s", out)
	}
}

// assertPiHasModel fails when the provider's model list lacks the id.
func assertPiHasModel(t *testing.T, provider map[string]any, id string) {
	t.Helper()
	models, _ := provider["models"].([]any)
	for _, m := range models {
		obj, _ := m.(map[string]any)
		if toStr(obj["id"]) == id {
			return
		}
	}
	t.Errorf("model %q is not registered in the provider: %v", id, models)
}
