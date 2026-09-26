package launch

// droid_store_race_integrity_test.go — Droid.Edit loads ~/.factory/settings.json
// into a map, rebuilds the entries it owns, keeps the user's own customModels
// and publishes the whole document back. The load-mutate-save was unlocked, so
// two `oaica launch droid` commands whose writes overlapped each published a
// snapshot taken before the other's entries landed, and the launch that renamed
// last decided the file while both reported success (2026-09-26 audit,
// thirteenth round).
//
// ~/.factory belongs to Factory, so the lock is keyed under ~/.oaica/locks
// (foreignStoreLockBase) rather than dropped inside that directory, and the read
// happens inside it.
//
// The machinery lives in agent_config_store_race_test.go.

import (
	"path/filepath"
	"testing"
)

func TestDroidEditTakesSettingsStoreLock(t *testing.T) {
	runAgentStoreExclusion(t, agentStoreCase{
		name:    "Droid.Edit",
		kind:    agentStoreDroidEdit,
		id:      "droid-race-model",
		store:   filepath.Join(".factory", "settings.json"),
		foreign: true,
		seed: `{"customModels": [{"model": "user-model", "id": "custom:user-model-7", ` +
			`"apiKey": "user-key", "baseUrl": "https://example.invalid/v1", "note": "kept"}], ` +
			`"sessionDefaultSettings": {"model": "custom:user-model-7", "reasoningEffort": "high"}}`,
		other: `{"customModels": [{"model": "user-model", "id": "custom:user-model-7", ` +
			`"apiKey": "user-key", "baseUrl": "https://example.invalid/v1", "note": "kept"}], ` +
			`"sessionDefaultSettings": {"model": "custom:user-model-7", "reasoningEffort": "high"}, ` +
			`"concurrentWriter": "kept"}`,
		want: []string{
			`"concurrentWriter": "kept"`,
			// The user's own entry, extra fields and all, is not this writer's to drop.
			`"user-model"`,
			`"note": "kept"`,
			// The entry only this writer builds.
			`"custom:droid-race-model-0"`,
			`"reasoningEffort": "high"`,
		},
	})
}
