package launch

// wildcard_host_not_a_client_url_integrity_test.go — a wildcard OLLAMA_HOST was
// written into integrations as the URL they should CONNECT to (2026-09-27
// audit, round 19).
//
// OLLAMA_HOST doubles as the daemon's BIND address and as the address clients
// use. "0.0.0.0:11434" is the usual way to serve every interface, and it is
// valid for binding but not for connecting: a client that dials it fails on
// Windows (envconfig.ConnectableHost exists for exactly this, and says so).
// Every place this package writes a daemon URL into an agent's configuration
// is a client, so a box serving on the wildcard produced configs that could
// not reach the daemon — while the same package already used ConnectableHost
// for six other integrations, so the two halves of the same job disagreed.
//
// The rule: a URL written for someone else to dial goes through
// ConnectableHost; Host itself stays for bind addresses and for messages about
// where the daemon listens.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wildcardHostEnv points OLLAMA_HOST at the wildcard, the way a box that
// serves every interface does.
func wildcardHostEnv(t *testing.T) {
	t.Helper()
	setLaunchTestHome(t, t.TempDir())
	t.Setenv("OLLAMA_HOST", "0.0.0.0:11434")
	stubDaemon(t)
}

func assertNoWildcardURL(t *testing.T, what, got string) {
	t.Helper()
	if strings.Contains(got, "0.0.0.0") {
		t.Errorf("%s = %q: the daemon's bind address is not something a client can dial — this config is read by another program, which fails to connect on Windows", what, got)
	}
	if !strings.Contains(got, "127.0.0.1") {
		t.Errorf("%s = %q, want the loopback address the daemon is reachable on", what, got)
	}
}

func TestDaemonURLsWrittenForClientsAreConnectable(t *testing.T) {
	wildcardHostEnv(t)
	primary, ok := findLaunchModel(testLaunchModels("gemma4"), "gemma4")
	if !ok {
		t.Fatal("premise: the fixture model is not a LaunchModel")
	}

	for _, c := range []struct {
		what string
		got  string
	}{
		{"copilotBaseURLFor", copilotBaseURLFor("gemma4")},
		{"poolsideBaseURLFor", poolsideBaseURLFor("gemma4")},
		{"qwenBaseURL", qwenBaseURL()},
		{"qwenBaseURLFor", qwenBaseURLFor("gemma4")},
		{"piDaemonProviderBaseURL", piDaemonProviderBaseURL()},
		{"piProviderBaseURL", piProviderBaseURL(testLaunchModels("gemma4"))},
		{"daemonEndpoint", daemonEndpoint("gemma4").BaseURL},
	} {
		assertNoWildcardURL(t, c.what, c.got)
	}
	baseURL, _, _ := openAIBaseURLAndKey(primary)
	assertNoWildcardURL(t, "openAIBaseURLAndKey", baseURL)
}

func TestAnIntegrationConfigWrittenUnderAWildcardHostIsConnectable(t *testing.T) {
	home := t.TempDir()
	wildcardHostEnv(t)
	setLaunchTestHome(t, home)
	stubDaemon(t)

	d := &Droid{}
	if err := d.Edit(testLaunchModels("gemma4")); err != nil {
		t.Fatalf("droid Edit: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".factory", "settings.json"))
	if err != nil {
		t.Fatalf("reading droid settings: %v", err)
	}
	var settings struct {
		CustomModels []struct {
			BaseURL string `json:"baseUrl"`
		} `json:"customModels"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("droid settings is not valid JSON: %v", err)
	}
	if len(settings.CustomModels) == 0 {
		t.Fatalf("no model entry was written:\n%s", data)
	}
	assertNoWildcardURL(t, "droid customModels baseUrl", settings.CustomModels[0].BaseURL)

	p := &Pi{}
	if err := p.Edit(testLaunchModels("gemma4")); err != nil {
		t.Fatalf("pi Edit: %v", err)
	}
	data, err = os.ReadFile(filepath.Join(home, ".pi", "agent", "models.json"))
	if err != nil {
		t.Fatalf("reading pi models.json: %v", err)
	}
	if strings.Contains(string(data), "0.0.0.0") {
		t.Errorf("pi's models.json tells pi to connect to the daemon's bind address:\n%s", data)
	}
	if !strings.Contains(string(data), "127.0.0.1") {
		t.Errorf("pi's models.json holds no loopback address, so the check above proved nothing:\n%s", data)
	}
}
