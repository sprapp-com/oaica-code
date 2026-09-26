package launch

// codex_config_store_race_integrity_test.go — cleanupCodexLegacyProfileConfig
// rewrites Codex's own ~/.codex/config.toml: it reads the file, removes the
// legacy profile key and the [profiles.ollama-launch] table from the text, and
// publishes the whole document back. No lock was held, so the publish is a
// snapshot taken before anything another writer — the user, Codex itself, or
// `oaica launch chatgpt`, which writes the same file's root keys — put in
// between (2026-09-26 audit, twelfth round).
//
// The removal being deterministic does not make the rewrite safe: the document
// is Codex's, oaica edits two keys of it and publishes all of it, and a
// concurrent edit to any other part of it is silently gone.
//
// The write to the profile file (~/.codex/ollama-launch.config.toml) and the
// model catalog (~/.codex/model.json) is untouched by this: those are fresh
// documents oaica owns end to end, with no read-back of anyone else's data, so
// an atomic write is the whole of what they need.
//
// The machinery lives in agent_config_store_race_test.go.

import (
	"path/filepath"
	"testing"
)

func TestCodexLegacyCleanupTakesCodexConfigLock(t *testing.T) {
	runAgentStoreExclusion(t, agentStoreCase{
		name:    "cleanupCodexLegacyProfileConfig",
		kind:    agentStoreCodexCleanup,
		id:      "codex-race-model",
		store:   filepath.Join(".codex", "config.toml"),
		foreign: true,
		seed:    "profile = \"ollama-launch\"\ntheme = \"kept\"\n",
		other:   "profile = \"ollama-launch\"\ntheme = \"kept\"\nconcurrent_writer = \"kept\"\n",
		want:    []string{`concurrent_writer = "kept"`, `theme = "kept"`},
		gone:    []string{`profile = "ollama-launch"`},
	})
}
