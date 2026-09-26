package launch

// pi_user_slot_integrity_test.go — Pi's one provider slot was repointed when
// the user's own slot happened to carry oaica's api value (2026-09-27 audit,
// round 21, F10).
//
// The ownership test was `ollama["api"] == "openai-completions"`, but that
// string is Pi's OWN value for every OpenAI-compatible provider, not a mark
// this package leaves: a user who configured the slot for their own server
// with that api value had their baseUrl and apiKey replaced by the daemon's on
// the next launch — the credential destroyed, silently.
//
// The slot is now repointed only when the api value AND the base URL are both
// this package's (piEndpointWasOurs), and a slot written in the package's own
// shape but pointed elsewhere says so instead of being rewritten.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// piSlotEnv sets up a test home with Pi's two documents, the models document
// holding the given providers object, and returns the paths.
func piSlotEnv(t *testing.T, providersJSON string) (configPath, settingsPath string) {
	t.Helper()
	home := t.TempDir()
	setTestHome(t, home)

	dir := filepath.Join(home, ".pi", "agent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath = filepath.Join(dir, "models.json")
	settingsPath = filepath.Join(dir, "settings.json")
	if err := os.WriteFile(configPath, []byte(`{"providers":`+providersJSON+`}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsPath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return configPath, settingsPath
}

func piSlotRead(t *testing.T, configPath string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	config, err := decodeJSONObject(data)
	if err != nil {
		t.Fatalf("rewritten models.json does not parse: %v\n%s", err, data)
	}
	providers, _ := config["providers"].(map[string]any)
	slot, _ := providers["ollama"].(map[string]any)
	if slot == nil {
		t.Fatalf("the ollama provider slot disappeared: %s", data)
	}
	return slot
}

// TestPiLeavesTheUsersOwnProviderEndpointAlone is the defect: the user's slot
// carries Pi's own api value with THEIR endpoint and key.
func TestPiLeavesTheUsersOwnProviderEndpointAlone(t *testing.T) {
	configPath, _ := piSlotEnv(t, `{
	  "ollama": {
	    "baseUrl": "http://their-box.invalid/v1",
	    "api": "openai-completions",
	    "apiKey": "THEIR_KEY",
	    "models": [{"id": "their-model"}]
	  }
	}`)

	if err := (&Pi{}).Edit(launchModelsFromNames([]string{"llama3.2"})); err != nil {
		t.Fatalf("Edit: %v", err)
	}

	slot := piSlotRead(t, configPath)
	if got := slot["baseUrl"]; got != "http://their-box.invalid/v1" {
		t.Errorf("baseUrl = %v, want the user's endpoint kept: an api value Pi itself uses is not proof this package wrote the slot", got)
	}
	if got := slot["apiKey"]; got != "THEIR_KEY" {
		t.Errorf("apiKey = %v, want the user's credential kept — repointing the slot overwrote it", got)
	}
	// The model list contract is unchanged: this launch's models are added to
	// the slot, beside the user's own untagged entry.
	models, _ := slot["models"].([]any)
	ids := map[string]bool{}
	for _, raw := range models {
		if entry, ok := raw.(map[string]any); ok {
			id, _ := entry["id"].(string)
			ids[id] = true
		}
	}
	if !ids["llama3.2"] {
		t.Errorf("the launch's model is missing from the slot: %v", models)
	}
	if !ids["their-model"] {
		t.Errorf("the user's own model entry was dropped: %v", models)
	}
}

// TestPiStillRepointsTheSlotItWrote is the control: a slot this package wrote
// (its api value AND an endpoint oaica writes) still follows this launch's
// models, which is the round-10 fix.
func TestPiStillRepointsTheSlotItWrote(t *testing.T) {
	configPath, _ := piSlotEnv(t, `{
	  "ollama": {
	    "baseUrl": "http://localhost:11434/v1",
	    "api": "openai-completions",
	    "apiKey": "ollama",
	    "models": [{"id": "old-model", "_launch": true}]
	  }
	}`)
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[
	  {"name":"box","base_url":"http://box.invalid/v1","api_key":"BOX_KEY","tool_format":"tool_calls"}
	]}`)

	if err := (&Pi{}).Edit(launchModelsFromNames([]string{"box/remote-m"})); err != nil {
		t.Fatalf("Edit: %v", err)
	}

	slot := piSlotRead(t, configPath)
	if got, _ := slot["baseUrl"].(string); strings.TrimRight(got, "/") != "http://box.invalid/v1" {
		t.Errorf("baseUrl = %v, want the remote's base: a slot this package wrote is repointed at this launch's endpoint", got)
	}
	if got := slot["apiKey"]; got != "BOX_KEY" {
		t.Errorf("apiKey = %v, want the remote's token: the model is served from the remote's base", got)
	}
}

// TestPiOwnershipOfASlotEndpoint pins the endpoint half of the rule on its own,
// including the shapes that must NOT count: an empty base URL is a slot whose
// endpoint the user removed, and a lookalike host is somebody else's server.
func TestPiOwnershipOfASlotEndpoint(t *testing.T) {
	setTestHome(t, t.TempDir())

	for _, base := range []string{
		piDaemonProviderBaseURL(),
		"http://localhost:11434/v1",
		"http://127.0.0.1:11434",
	} {
		if !piEndpointWasOurs(base) {
			t.Errorf("piEndpointWasOurs(%q) = false, want true: this package writes that base URL", base)
		}
	}
	for _, base := range []string{
		"",
		"http://their-box.invalid/v1",
		"http://localhost:11435/v1",
		"http://localhost:11434.evil.invalid/v1",
	} {
		if piEndpointWasOurs(base) {
			t.Errorf("piEndpointWasOurs(%q) = true, want false: it is not an endpoint this package writes", base)
		}
	}
}
