package launch

// pi_mixed_endpoint_selection_integrity_test.go — a selection spanning two
// endpoints was written to Pi's one provider slot (2026-09-26 audit, round 16).
//
// Pi's models.json gives oaica one provider slot ("ollama"), and every model
// this package registers in it is served from that slot's single baseUrl with
// its single apiKey — piProviderBaseURL/piProviderKey take the FIRST
// user-remote model's endpoint. A selection that mixes backends therefore
// declared the other backend's model under an endpoint that does not serve it:
// a daemon model registered in a provider pointed at a third-party API with
// that remote's credential, or a second remote's model declared under the
// first remote's URL and key. The launch then fails at the far end with a
// model-not-found the user cannot act on — the same shape the round-10 fix
// closed for a stale slot, reopened by a selection that spans backends.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPiDoesNotDeclareAModelUnderAnEndpointThatDoesNotServeIt(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[
	  {"name":"box","base_url":"http://third-party.invalid/v1","api_key":"KEY_BOX","tool_format":"tool_calls"},
	  {"name":"other","base_url":"http://other.invalid/v1","api_key":"KEY_OTHER","tool_format":"tool_calls"}
	]}`)

	for _, tc := range []struct {
		name    string
		primary string
		models  []LaunchModel
	}{
		{
			name:    "daemon model beside a remote model",
			primary: "llama3.2",
			models: []LaunchModel{
				{Name: "llama3.2"}, // the local daemon's own model
				{Name: "box/big-model"},
			},
		},
		{
			name:    "two remotes in one launch",
			primary: "box/big-model",
			models: []LaunchModel{
				{Name: "box/big-model"},
				{Name: "other/other-model"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkPiDoesNotCrossEndpoints(t, tc.primary, tc.models)
		})
	}
}

// The refusal is scoped to the selection that cannot be expressed: one
// endpoint's models still configure, whichever endpoint that is.
func TestPiStillConfiguresASingleEndpointSelection(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[
	  {"name":"box","base_url":"http://third-party.invalid/v1","api_key":"KEY_BOX","tool_format":"tool_calls"}
	]}`)

	for _, tc := range []struct {
		name   string
		models []LaunchModel
	}{
		{"all local", []LaunchModel{{Name: "llama3.2"}, {Name: "qwen3:8b"}}},
		{"all one remote", []LaunchModel{{Name: "box/big-model"}, {Name: "box/small-model"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := (&Pi{}).Edit(tc.models); err != nil {
				t.Fatalf("a selection that fits Pi's one provider slot was refused: %v", err)
			}
		})
	}
}

func checkPiDoesNotCrossEndpoints(t *testing.T, primary string, models []LaunchModel) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	configPath := filepath.Join(home, ".pi", "agent", "models.json")

	editErr := (&Pi{}).Edit(models)

	// A refusal is an acceptable answer — Pi has one provider slot and a
	// selection spanning endpoints cannot be expressed in it. What is not
	// acceptable is accepting the selection and writing it wrong.
	if editErr != nil {
		if !strings.Contains(editErr.Error(), "big-model") && !strings.Contains(editErr.Error(), "other-model") {
			t.Errorf("the refusal does not name a model the user selected, so it does not say what to change: %v", editErr)
		}
		if _, statErr := os.Stat(configPath); statErr == nil {
			t.Errorf("Edit refused the selection but still wrote %s", configPath)
		}
		return
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("Edit accepted the selection but wrote no config: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("the config Edit wrote is not JSON: %v", err)
	}
	providers, _ := cfg["providers"].(map[string]any)
	for pid, p := range providers {
		pm, _ := p.(map[string]any)
		base, _ := pm["baseUrl"].(string)
		entries, _ := pm["models"].([]any)
		for _, e := range entries {
			em, _ := e.(map[string]any)
			id, _ := em["id"].(string)
			remote := ""
			switch {
			case strings.Contains(id, "big-model"):
				remote = "box"
			case strings.Contains(id, "other-model"):
				remote = "other"
			case strings.HasPrefix(id, "llama3.2"):
				remote = "daemon"
			default:
				continue
			}
			wrong := (remote == "box" && !strings.Contains(base, "third-party.invalid")) ||
				(remote == "other" && !strings.Contains(base, "other.invalid")) ||
				(remote == "daemon" && (strings.Contains(base, "invalid") || pm["apiKey"] != "ollama"))
			if wrong {
				t.Errorf("provider %q declares %s's model %q but is pointed at %s with apiKey %v — the request goes to an endpoint that does not serve that model:\n%s",
					pid, remote, id, base, pm["apiKey"], data)
			}
		}
	}
}
