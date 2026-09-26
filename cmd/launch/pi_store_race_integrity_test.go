package launch

// pi_store_race_integrity_test.go — Pi.Edit rewrites two of Pi's own documents
// (~/.pi/agent/models.json, ~/.pi/agent/settings.json) with a load →
// mutate-in-memory → publish-whole-document that took no lock, so it could
// publish a snapshot taken before another writer's entry landed — the lost
// update the ~/.oaica stores and opencode/VS Code's stores were fixed for
// (2026-09-26 audit, twelfth round).
//
// Two oaica commands overlapping is one path: `oaica launch pi` twice, or a
// launch while another command repoints Pi's provider. The other is Pi itself —
// the documents are Pi's, and oaica models only part of each one, so anything
// Pi (or the user, hand-editing) wrote between oaica's read and its rename is
// published away. Either way the loser's change is gone and both writers look
// fine.
//
// models.json and settings.json are one lock each: the pair is read together
// and written together, and a second Edit that only holds one of the two locks
// is still free to interleave with the other file's read-modify-write.
//
// The machinery (child writer, rendezvous, the parent's own publish under the
// lock) lives in agent_config_store_race_test.go.

import (
	"path/filepath"
	"testing"
)

func TestPiEditTakesPiModelsStoreLock(t *testing.T) {
	runAgentStoreExclusion(t, agentStoreCase{
		name:    "Pi.Edit (models.json)",
		kind:    agentStorePi,
		id:      "pi-race-model",
		store:   filepath.Join(".pi", "agent", "models.json"),
		foreign: true,
		seed: `{
  "theme": "kept",
  "providers": {"user": {"baseUrl": "https://example.invalid"}}
}`,
		other: `{
  "theme": "kept",
  "concurrentWriter": "kept",
  "providers": {"user": {"baseUrl": "https://example.invalid"}}
}`,
		want: []string{`"concurrentWriter": "kept"`, `"theme": "kept"`, `"pi-race-model"`},
	})
}

func TestPiEditTakesPiSettingsStoreLock(t *testing.T) {
	runAgentStoreExclusion(t, agentStoreCase{
		name:    "Pi.Edit (settings.json)",
		kind:    agentStorePi,
		id:      "pi-race-model",
		store:   filepath.Join(".pi", "agent", "settings.json"),
		foreign: true,
		seed:    `{"theme": "kept"}`,
		other:   `{"theme": "kept", "concurrentWriter": "kept"}`,
		want:    []string{`"concurrentWriter": "kept"`, `"theme": "kept"`, `"defaultProvider": "ollama"`},
	})
}
