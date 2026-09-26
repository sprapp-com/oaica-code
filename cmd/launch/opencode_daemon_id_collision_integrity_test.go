package launch

// opencode_daemon_id_collision_integrity_test.go — the daemon's provider id was
// renamed to the one name it could still collide with (2026-09-26 audit, round
// 16).
//
// buildInlineConfig merges provider groups by id, so two groups under one id
// become one block owned by whichever was added first. opencodeDaemonProviderID
// exists to keep the daemon's block out of a user remote's way: it renames
// "ollama" to "ollama-local" when a remote claims "ollama". But "ollama-local"
// is itself a legal remote name, and the rename did not check for it — so a
// user with remotes named both "ollama" and "ollama-local" got the daemon
// block's local models declared under the "ollama-local" remote's base URL and
// apiKey: local traffic exported to a third-party endpoint, carrying that
// endpoint's credential, which is the exact failure the rename was added to
// prevent.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOpenCodeDaemonBlockDoesNotTakeARemotesID(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[
	  {"name":"ollama","base_url":"http://first.invalid/v1","api_key":"KEY_ONE","tool_format":"tool_calls"},
	  {"name":"ollama-local","base_url":"http://third-party.invalid/v1","api_key":"KEY_THIRD_PARTY","tool_format":"tool_calls"}
	]}`)

	// Both orders: whichever group is added first owns the merged block, so the
	// daemon-first order hides the remote's models behind the daemon's base URL
	// and the remote-first order ships the daemon's local models to the remote.
	// The collision is the defect either way.
	for _, tc := range []struct {
		name   string
		models []LaunchModel
	}{
		{"daemon first", []LaunchModel{
			{Name: "llama3.2"}, // the local daemon's own model
			{Name: "ollama/remote-m"},
			{Name: "ollama-local/remote-n"},
		}},
		{"remote first", []LaunchModel{
			{Name: "ollama-local/remote-n"},
			{Name: "llama3.2"},
			{Name: "ollama/remote-m"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) { checkOpenCodeDaemonBlockDoesNotCollide(t, tc.models) })
	}
}

func checkOpenCodeDaemonBlockDoesNotCollide(t *testing.T, models []LaunchModel) {
	t.Helper()
	content, err := buildInlineConfig(models[0], models)
	if err != nil {
		t.Fatalf("buildInlineConfig: %v", err)
	}

	var cfg struct {
		Provider map[string]struct {
			Options struct {
				BaseURL string `json:"baseURL"`
				APIKey  string `json:"apiKey"`
			} `json:"options"`
			Models map[string]any `json:"models"`
		} `json:"provider"`
	}
	if err := json.Unmarshal([]byte(content), &cfg); err != nil {
		t.Fatalf("the generated config is not JSON: %v\n%s", err, content)
	}

	var daemonBlock string
	for id, p := range cfg.Provider {
		if _, ok := p.Models["llama3.2"]; ok {
			daemonBlock = id
			if strings.Contains(p.Options.BaseURL, "third-party.invalid") {
				t.Errorf("the daemon's own model llama3.2 is declared under provider %q, whose base URL is %s — local traffic is sent to a third-party endpoint:\n%s",
					id, p.Options.BaseURL, content)
			}
			if p.Options.APIKey != "" {
				t.Errorf("the daemon's block %q carries an apiKey (%q) — the local models are shipped to a remote's endpoint with that remote's credential:\n%s",
					id, p.Options.APIKey, content)
			}
		}
	}
	if daemonBlock == "" {
		t.Fatalf("no provider declares the daemon's model llama3.2 at all:\n%s", content)
	}
	if _, ok := cfg.Provider["ollama-local"]; !ok {
		t.Errorf("the remote named ollama-local has no provider block of its own — it was merged away:\n%s", content)
	}
	if got := len(cfg.Provider); got != 3 {
		t.Errorf("the config declares %d provider block(s) for three distinct endpoints (daemon, ollama, ollama-local), so two backends were merged under one id:\n%s", got, content)
	}
}
