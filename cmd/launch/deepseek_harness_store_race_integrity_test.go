package launch

// deepseek_harness_store_race_integrity_test.go — ConfigureWithModels reads
// ~/.ollama/launch/dsh/settings.yaml as a YAML document, edits the keys oaica
// owns in it and publishes the whole document back. The store is oaica's own
// (under ~/.ollama/launch, this integration's namespace), so its lock goes
// beside the file, as the other oaica-owned stores do — but it is still a
// read-modify-write with a real lost-update window: two `oaica launch deepseek`
// commands whose writes overlap each publish a snapshot taken before the other's
// settings landed, and the launch that renamed last decides the file. No lock
// was held (2026-09-26 audit, twelfth round).
//
// The patch file (~/.ollama/launch/dsh/ollama.cordis.yml) is a fresh document
// marshalled from a literal, with no read-back of anything, so it stays
// unlocked and is not covered here.
//
// The machinery lives in agent_config_store_race_test.go.

import (
	"path/filepath"
	"testing"
)

func TestDeepSeekHarnessConfigureTakesSettingsStoreLock(t *testing.T) {
	runAgentStoreExclusion(t, agentStoreCase{
		name:  "DeepSeekHarness.ConfigureWithModels",
		kind:  agentStoreDsh,
		id:    "dsh-race-model",
		store: filepath.Join(".ollama", "launch", "dsh", "settings.yaml"),
		seed:  "user-kept: value\nagent-default-model:\n  provider: openai\n",
		other: "user-kept: value\nagent-default-model:\n  provider: openai\nconcurrent_writer: kept\n",
		want:  []string{"user-kept: value", "concurrent_writer: kept", "model: dsh-race-model"},
		gone:  []string{"provider: openai"},
	})
}
