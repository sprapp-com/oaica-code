package launch

// hermes_store_race_integrity_test.go — Hermes.Configure reads Hermes' own
// ~/.hermes/config.yaml, unmarshals it into a map, sets the launch-owned keys
// and publishes the whole document back, carrying unknown keys through but not
// merging into the file. No lock was held, so two `oaica launch hermes`
// commands whose writes overlapped each published a snapshot taken before the
// other's settings, and the launch that renamed last decided the file while both
// reported success (2026-09-26 audit, twelfth round).
//
// The file is Hermes', so the lock is keyed under ~/.oaica/locks
// (foreignStoreLockBase) rather than dropped inside ~/.hermes, and the read
// happens inside it.
//
// The machinery lives in agent_config_store_race_test.go.

import (
	"path/filepath"
	"testing"
)

func TestHermesConfigureTakesConfigStoreLock(t *testing.T) {
	runAgentStoreExclusion(t, agentStoreCase{
		name:    "Hermes.Configure",
		kind:    agentStoreHermes,
		id:      "hermes-race-model",
		store:   filepath.Join(".hermes", "config.yaml"),
		foreign: true,
		seed:    "user-kept: value\nmodel:\n  provider: openai\n  default: old-model\n",
		other:   "user-kept: value\nmodel:\n  provider: openai\n  default: old-model\nconcurrent_writer: kept\n",
		want:    []string{"user-kept: value", "concurrent_writer: kept", "base_url:"},
		gone:    []string{"provider: openai", "default: old-model"},
	})
}
