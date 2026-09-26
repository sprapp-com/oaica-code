package launch

// omp_store_race_integrity_test.go — OMP.ConfigureWithModels writes two of
// OMP's own documents, both read-modify-write and both unlocked: models.yml
// (the ollama provider's model list, merged into whatever the file already
// holds) and config.yml (one key set, then the whole document published). A
// concurrent `oaica launch omp` therefore publishes a snapshot taken before the
// other's edit, and the publish that lands last deletes it while both commands
// report success (2026-09-26 audit, twelfth round).
//
// Both files live under OMP's agent directory (~/.omp/agent, or wherever
// PI_CONFIG_DIR / PI_CODING_AGENT_DIR point), so the locks are keyed under
// ~/.oaica/locks (foreignStoreLockBase) rather than beside the stores, and each
// read happens inside its lock.
//
// The machinery lives in agent_config_store_race_test.go.

import (
	"path/filepath"
	"testing"
)

func TestOMPModelsConfigTakesModelsStoreLock(t *testing.T) {
	runAgentStoreExclusion(t, agentStoreCase{
		name:    "writeOMPModelsConfig",
		kind:    agentStoreOmpModels,
		id:      "omp-race-model",
		store:   filepath.Join(".omp", "agent", "models.yml"),
		foreign: true,
		seed:    "user-kept: value\nproviders:\n  ollama:\n    auth: apiKey\n",
		other:   "user-kept: value\nproviders:\n  ollama:\n    auth: apiKey\nconcurrent_writer: kept\n",
		want:    []string{"user-kept: value", "concurrent_writer: kept", "id: omp-race-model"},
		gone:    []string{"auth: apiKey"},
	})
}

func TestOMPAgentConfigTakesAgentConfigStoreLock(t *testing.T) {
	runAgentStoreExclusion(t, agentStoreCase{
		name:    "writeOMPAgentConfig",
		kind:    agentStoreOmpAgent,
		id:      "omp-race-model",
		store:   filepath.Join(".omp", "agent", "config.yml"),
		foreign: true,
		seed:    "user-kept: value\nsetupVersion: old-version\n",
		other:   "user-kept: value\nsetupVersion: old-version\nconcurrent_writer: kept\n",
		want:    []string{"user-kept: value", "concurrent_writer: kept"},
		gone:    []string{"setupVersion: old-version"},
	})
}
