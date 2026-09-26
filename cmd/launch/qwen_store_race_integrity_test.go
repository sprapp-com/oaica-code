package launch

// qwen_store_race_integrity_test.go — Qwen.Configure read Qwen's own
// ~/.qwen/settings.json, rewrote the keys that decide which provider and model
// qwen talks to, and published the whole document back with no lock held
// (2026-09-26 audit, fourteenth round).
//
// Every other integration writer in this package takes the store's lock over
// the read as well as the publish — cline, droid, muse, pi, hermes, omp,
// codex-app, claude-desktop, vscode, opencode. qwen was the one left off, and
// its rewritten values are model-dependent: two `oaica launch qwen` commands
// whose writes overlap each publish a snapshot taken before the other's entry
// landed, so the rename that lands last decides model.name, auth.baseUrl,
// modelProviders.openai and env.OLLAMA_API_KEY for BOTH launches while both
// report success — one of them runs the other's model.
//
// It is also a live document: qwen keeps its own keys in it, so a key qwen
// wrote between oaica's unlocked read and its rename is dropped too.
//
// The file is qwen's, so the lock is keyed under ~/.oaica/locks
// (foreignStoreLockBase) rather than dropped inside ~/.qwen, and the read
// happens inside it.
//
// The machinery lives in agent_config_store_race_test.go.

import (
	"path/filepath"
	"testing"
)

func TestQwenConfigureTakesConfigStoreLock(t *testing.T) {
	runAgentStoreExclusion(t, agentStoreCase{
		name:    "Qwen.Configure",
		kind:    agentStoreQwen,
		id:      "qwen-race-model",
		store:   filepath.Join(".qwen", "settings.json"),
		foreign: true,
		seed:    `{"userKey":"keep","model":{"name":"old-model"}}`,
		other:   `{"userKey":"keep","concurrent_writer":"kept","model":{"name":"old-model"}}`,
		want:    []string{"userKey", "concurrent_writer", "qwen-race-model"},
		gone:    []string{"old-model"},
	})
}
