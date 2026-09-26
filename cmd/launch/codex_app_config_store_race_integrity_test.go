package launch

// codex_app_config_store_race_integrity_test.go — the ChatGPT/Codex desktop
// integration writes Codex's own ~/.codex/config.toml twice: it configures the
// root keys (model, model_provider, model_catalog_json) and its profile table,
// and Restore puts the previous values back. Both are load → mutate-in-text →
// publish-whole-document, and neither held the store's lock — the same file
// codex.go's legacy cleanup rewrites, so the two commands could publish over
// each other as well (2026-09-26 audit, twelfth round).
//
// The restore-state file (~/.ollama/launch/codex-app-restore.json) is
// read-modify-written by the same two paths and is covered by the same lock:
// its only writers are these call chains, and the read of config.toml it merges
// into is part of the same critical section — saving a state derived from a
// snapshot another writer has already replaced is the same lost update one file
// over.
//
// The model catalog is a fresh document oaica owns (no read-back), so it takes
// no lock.
//
// The machinery lives in agent_config_store_race_test.go.

import (
	"path/filepath"
	"testing"
)

func TestCodexAppConfigureTakesCodexConfigLock(t *testing.T) {
	runAgentStoreExclusion(t, agentStoreCase{
		name:    "CodexApp.ConfigureWithModels",
		kind:    agentStoreCodexApp,
		id:      "codex-app-race-model",
		store:   filepath.Join(".codex", "config.toml"),
		foreign: true,
		seed:    "model = \"gpt-5\"\ntheme = \"kept\"\n",
		other:   "model = \"gpt-5\"\ntheme = \"kept\"\nconcurrent_writer = \"kept\"\n",
		want: []string{
			`concurrent_writer = "kept"`,
			`theme = "kept"`,
			`model_provider = "ollama-launch-codex-app"`,
			`model = "codex-app-race-model"`,
		},
		gone: []string{`model = "gpt-5"`},
	})
}

func TestCodexAppRestoreTakesCodexConfigLock(t *testing.T) {
	runAgentStoreExclusion(t, agentStoreCase{
		name:    "CodexApp.Restore",
		kind:    agentStoreCodexAppRestore,
		id:      "codex-app-race-model",
		store:   filepath.Join(".codex", "config.toml"),
		foreign: true,
		seed:    "model = \"gpt-5\"\nmodel_provider = \"ollama-launch-codex-app\"\ntheme = \"kept\"\n",
		other:   "model = \"gpt-5\"\nmodel_provider = \"ollama-launch-codex-app\"\ntheme = \"kept\"\nconcurrent_writer = \"kept\"\n",
		want:    []string{`concurrent_writer = "kept"`, `theme = "kept"`},
		gone:    []string{`model_provider = "ollama-launch-codex-app"`},
	})
}
