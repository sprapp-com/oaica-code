package launch

// opencode_daemon_id_family_integrity_test.go — the daemon's provider id can be
// renamed twice, and only two of its three spellings were recognised
// (2026-09-27 audit, round 21, F13).
//
// opencodeDaemonProviderID falls back to "ollama-local", then to
// "ollama-local-2" and up while a configured remote claims each candidate. Both
// the state-file rewriter and readModelJSONModels tested for the literal
// strings "ollama" and "ollama-local", so a launch with remotes named BOTH
// "ollama" and "ollama-local" — the configuration that produces
// "ollama-local-2" — wrote rows no later launch would ever replace: the model
// stayed in opencode's picker after it was dropped (and the picker resolved it
// to nothing), and readModelJSONModels returned none of the daemon's own rows,
// so Models() never matched the live config and every launch rewrote the state
// file again.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenCodeRecognisesEveryDaemonProviderIDItCanWrite(t *testing.T) {
	for _, pid := range []string{"ollama", "ollama-local", "ollama-local-2", "ollama-local-10"} {
		if !opencodeIsDaemonProviderID(pid) {
			t.Errorf("opencodeIsDaemonProviderID(%q) = false, want true: opencodeDaemonProviderID can write it", pid)
		}
	}
	for _, pid := range []string{"", "ollama2", "ollama-local-", "ollama-local-x", "ollama-locality", "their-provider"} {
		if opencodeIsDaemonProviderID(pid) {
			t.Errorf("opencodeIsDaemonProviderID(%q) = true, want false: it is not an id the daemon block uses", pid)
		}
	}
}

func TestOpenCodeReplacesStateRowsWrittenUnderARenamedDaemonID(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	// Both names a remote can take, so the daemon's block is "ollama-local-2".
	writeRemotes(t, `{"remotes":[
	  {"name":"ollama","base_url":"http://first.invalid/v1","api_key":"KEY_ONE","tool_format":"tool_calls"},
	  {"name":"ollama-local","base_url":"http://second.invalid/v1","api_key":"KEY_TWO","tool_format":"tool_calls"}
	]}`)

	statePath, err := openCodeStatePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatal(err)
	}
	// A previous launch's state file: the daemon's models under the renamed
	// block, one of them outside this launch's selection.
	stale := `{"recent":[
	  {"providerID":"ollama-local-2","modelID":"stale-model"},
	  {"providerID":"ollama-local-2","modelID":"llama3.2"}
	]}`
	if err := os.WriteFile(statePath, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}

	models := []LaunchModel{{Name: "llama3.2"}, {Name: "ollama/remote-m"}}
	if err := (&OpenCode{}).Edit(models); err != nil {
		t.Fatalf("Edit: %v", err)
	}

	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Recent []struct {
			ProviderID string `json:"providerID"`
			ModelID    string `json:"modelID"`
		} `json:"recent"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}

	counts := map[string]int{}
	for _, e := range state.Recent {
		counts[e.ModelID]++
	}
	if counts["llama3.2"] != 1 {
		t.Errorf("llama3.2 appears %d time(s) in the state file, want 1 — the row written under the renamed daemon id must be REPLACED, not kept beside the new one: %s", counts["llama3.2"], raw)
	}
	// The control: a daemon row for a model OUTSIDE this launch's selection is
	// history, not this launch's own row, and is left where it is.
	if counts["stale-model"] != 1 {
		t.Errorf("a daemon row for a model this launch does not carry was dropped (count %d, want 1): the cleanup owns the rows for the models being written, not opencode's whole history: %s", counts["stale-model"], raw)
	}

	// readModelJSONModels reads the daemon's rows back; it must see the renamed
	// block, or a re-launch loses every local model.
	names := readModelJSONModels()
	if len(names) == 0 {
		t.Fatalf("readModelJSONModels returned nothing for a state file whose daemon rows are under %q: %s", "ollama-local-2", raw)
	}
}
