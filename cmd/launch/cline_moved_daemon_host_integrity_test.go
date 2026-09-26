package launch

// cline_moved_daemon_host_integrity_test.go — the value oaica itself wrote under
// a since-moved OLLAMA_HOST was read back as the user's (2026-09-27 audit,
// round 22).
//
// clineEndpointWasOurs recognised only the CURRENT daemon (envconfig.
// ConnectableHost) and configured remotes. A launch with
// OLLAMA_HOST=192.168.1.50:11434 writes http://192.168.1.50:11434/v1 and records
// it in Cline's ollama provider; the next launch from a shell without that
// export — .bashrc exports are invisible to non-interactive invocations — saw a
// value matching neither and refused with "an endpoint oaica did not write …
// so it is yours", which was false, and blocked the launch. The daemon's
// documented default address is the missing case, exactly as it is in hermes'
// predicate (hermesEndpointWasOurs).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func seedClineOllamaProvider(t *testing.T, home, baseURL string) string {
	t.Helper()
	providersPath := clineProvidersPath(home)
	if err := os.MkdirAll(filepath.Dir(providersPath), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `{"version":1,"lastUsedProvider":"ollama","providers":{"ollama":{"settings":{"provider":"ollama","model":"llama3","baseUrl":"` + baseURL + `","apiKey":"from-an-earlier-oaica"},"tokenSource":"manual"}}}`
	if err := os.WriteFile(providersPath, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	return providersPath
}

func TestClineRecognisesItsOwnValueUnderAMovedDaemonHost(t *testing.T) {
	for _, recorded := range []string{"http://127.0.0.1:11434/v1", "http://localhost:11434/v1"} {
		t.Run(recorded, func(t *testing.T) {
			home := t.TempDir()
			setLaunchTestHome(t, home)
			t.Setenv("OLLAMA_HOST", "192.168.1.50:11434")

			providersPath := seedClineOllamaProvider(t, home, recorded)

			if err := (&Cline{}).Edit([]LaunchModel{fallbackLaunchModel("llama3.2")}); err != nil {
				t.Fatalf("Cline.Edit with the daemon's default address recorded and OLLAMA_HOST moved = %v: that value is one an earlier oaica wrote, not the user's", err)
			}
			data, readErr := os.ReadFile(providersPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			var doc map[string]any
			if err := json.Unmarshal(data, &doc); err != nil {
				t.Fatal(err)
			}
			providers, _ := doc["providers"].(map[string]any)
			provider, _ := providers["ollama"].(map[string]any)
			settings, _ := provider["settings"].(map[string]any)
			got, _ := settings["baseUrl"].(string)
			if got != "http://192.168.1.50:11434/v1" {
				t.Errorf("base URL = %q, want the daemon OLLAMA_HOST names now (http://192.168.1.50:11434/v1)", got)
			}
		})
	}
}

// Control: a base URL that is neither the daemon (now or at its default) nor a
// configured remote is still refused and left alone.
func TestClineStillRefusesAForeignEndpointUnderAMovedDaemonHost(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	t.Setenv("OLLAMA_HOST", "192.168.1.50:11434")

	providersPath := seedClineOllamaProvider(t, home, "https://lan.example:11434/v1")

	err := (&Cline{}).Edit([]LaunchModel{fallbackLaunchModel("llama3.2")})
	if err == nil {
		t.Fatal("Cline.Edit took over an endpoint oaica never wrote")
	}
	if !strings.Contains(err.Error(), "lan.example") {
		t.Errorf("the refusal does not name the endpoint it found: %v", err)
	}
	data, readErr := os.ReadFile(providersPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(data), "https://lan.example:11434/v1") {
		t.Error("the user's endpoint was rewritten despite the refusal")
	}
}
