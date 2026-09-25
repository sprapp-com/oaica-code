package launch

// opencode_edit_state_pairs_test.go — OpenCode.Edit writes opencode's model
// state file (model.json), the list its picker reads. It used to write a
// hardcoded "ollama"/<picker name> for every model, blind to the partition
// buildInlineConfig performs: a user remote's models are declared under the
// REMOTE's provider block and under the remote's BARE upstream id, so the
// entry named a block that declares no such model (the picker entry then
// resolved to nothing), and a remote named "ollama" made the daemon's own
// block "ollama-local" while the state file kept saying "ollama"
// (2026-09-26 audit).
//
// The state file, the config partition and readModelJSONModels all answer to
// the same spelling now; this test pins all three against each other.

import (
	"encoding/json"
	"os"
	"testing"
)

func TestOpenCodeEditStateNamesTheBlockThatDeclaresEachModel(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[
	  {"name":"kat","base_url":"http://kat.invalid/v1","api_key":"KEY_KAT","tool_format":"tool_calls"},
	  {"name":"ollama","base_url":"http://other-box.invalid/v1","api_key":"KEY_OTHER_BOX","tool_format":"tool_calls"}
	]}`)

	models := []LaunchModel{
		{Name: "kat/kat-coder"},   // a user remote: declared as "kat" / "kat-coder"
		{Name: "llama3.2"},        // the local daemon: declared as "ollama-local" / "llama3.2"
		{Name: "ollama/remote-m"}, // the remote that took the "ollama" name
	}
	o := &OpenCode{}
	if err := o.Edit(models); err != nil {
		t.Fatalf("Edit: %v", err)
	}

	statePath, err := openCodeStatePath()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("Edit did not write the model state file: %v", err)
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

	pairs := map[string]string{} // modelID → providerID
	for _, e := range state.Recent {
		pairs[e.ModelID] = e.ProviderID
	}

	// What buildInlineConfig declares, per model, is the contract the state
	// file has to match: the block that declares the id.
	want := map[string]string{
		"kat-coder": "kat",          // the remote's own block, its bare upstream id
		"llama3.2":  "ollama-local", // the daemon, renamed because a remote took "ollama"
		"remote-m":  "ollama",       // the remote that owns the name
	}
	for modelID, providerID := range want {
		got, ok := pairs[modelID]
		if !ok {
			t.Errorf("no state entry for model id %q; entries: %v", modelID, state.Recent)
			continue
		}
		if got != providerID {
			t.Errorf("state entry for %q names provider %q, want %q — the entry must name the block that DECLARES the model, or opencode's picker resolves it to nothing", modelID, got, providerID)
		}
	}

	// The picker name is not a model id for a remote: an entry keyed by the
	// namespaced picker string is the shape this fix removed.
	if pid, ok := pairs["kat/kat-coder"]; ok {
		t.Errorf("the state file contains the picker name \"kat/kat-coder\" (under provider %q); opencode's provider for that remote declares the bare id, not the picker string", pid)
	}

	// readModelJSONModels reads this file back for a re-launch; the daemon
	// spelling it accepts has to include the renamed id, or a re-launch loses
	// every local model.
	names := readModelJSONModels()
	found := false
	for _, n := range names {
		if n == "llama3.2" {
			found = true
		}
	}
	if !found {
		t.Errorf("readModelJSONModels did not return the local model from the state file it just wrote (got %v) — the renamed daemon block \"ollama-local\" is not accepted as the daemon spelling", names)
	}
}
