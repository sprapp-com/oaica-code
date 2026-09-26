package launch

// muse_store_race_integrity_test.go — writeMuseSettingsFile (reached by
// Muse.Edit and by Muse.Run) layers the launch-owned keys onto the whole
// settings document it reads back — museBaseSettings — and publishes it whole.
// The load-mutate-save was unlocked, so two overlapping launches each published
// a snapshot taken before the other's settings landed, and the launch that
// renamed last decided the file while both reported success (2026-09-26 audit,
// thirteenth round). The document is a live one: muse persists its own settings
// into the config root launch hands it, so what a lost publish deletes is not
// only oaica's keys.
//
// This store is oaica's OWN — ~/.ollama/launch/muse-config is the
// XDG_CONFIG_HOME launch passes to muse, not muse's ~/.config/muse — so the lock
// goes beside the file (the dsh arrangement) rather than under ~/.oaica/locks,
// and the read inside museBaseSettings happens inside it.
//
// The machinery lives in agent_config_store_race_test.go.

import (
	"path/filepath"
	"testing"
)

func TestMuseEditTakesSettingsStoreLock(t *testing.T) {
	runAgentStoreExclusion(t, agentStoreCase{
		name: "Muse.Edit",
		kind: agentStoreMuseEdit,
		id:   "muse-race-model",
		// oaica's own launch store, inside the launcher's directory tree.
		store:   filepath.Join(".ollama", "launch", "muse-config", "muse", "settings.json"),
		foreign: false,
		seed: `{"schema_version": 1, "provider": "openai", "model": "old-model", ` +
			`"userKept": "kept"}`,
		other: `{"schema_version": 1, "provider": "openai", "model": "old-model", ` +
			`"userKept": "kept", "concurrentWriter": "kept"}`,
		want: []string{
			`"concurrentWriter": "kept"`,
			`"userKept": "kept"`,
			`"provider": "meta"`,
			`"model": "muse-race-model"`,
			`"model_catalog"`,
		},
		gone: []string{`"old-model"`, `"provider": "openai"`},
	})
}
