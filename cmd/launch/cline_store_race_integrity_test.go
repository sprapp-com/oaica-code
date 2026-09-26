package launch

// cline_store_race_integrity_test.go — Cline.Edit rewrites two of Cline's own
// documents whole from a snapshot of each: ~/.cline/data/settings/providers.json
// (the ollama provider entry) and ~/.cline/data/globalState.json (the active
// provider and model ids). Neither read was under a lock, so two
// `oaica launch cline` commands whose writers overlapped each published a
// snapshot taken before the other's entry landed, and the launch that renamed
// last decided the file while both reported success (2026-09-26 audit,
// thirteenth round).
//
// Both stores belong to Cline, so each lock is keyed under ~/.oaica/locks
// (foreignStoreLockBase) rather than dropped inside ~/.cline, and each store's
// read happens inside its own lock. The two documents are separate files, so
// they take separate locks; the ordering between them is fixed by Edit (the only
// caller), so two commands cannot deadlock against each other.
//
// The machinery lives in agent_config_store_race_test.go.

import (
	"path/filepath"
	"testing"
)

func TestClineEditTakesProvidersStoreLock(t *testing.T) {
	runAgentStoreExclusion(t, agentStoreCase{
		name:    "Cline.Edit over providers.json",
		kind:    agentStoreClineEdit,
		id:      "cline-race-model",
		store:   filepath.Join(".cline", "data", "settings", "providers.json"),
		foreign: true,
		seed: `{"version": 1, "lastUsedProvider": "anthropic", ` +
			`"telemetry": {"ratio": 1.5}, ` +
			`"providers": {"anthropic": {"settings": {"model": "claude-old"}}}}`,
		other: `{"version": 1, "lastUsedProvider": "anthropic", ` +
			`"telemetry": {"ratio": 1.5}, ` +
			`"providers": {"anthropic": {"settings": {"model": "claude-old"}}}, ` +
			`"concurrentWriter": "kept"}`,
		want: []string{
			`"concurrentWriter": "kept"`,
			`"lastUsedProvider": "ollama"`,
			`"model": "cline-race-model"`,
			// Every number in the document survives the round-trip (UseNumber).
			`"ratio": 1.5`,
		},
		gone: []string{`"lastUsedProvider": "anthropic"`},
	})
}

func TestClineEditTakesLegacyGlobalStateStoreLock(t *testing.T) {
	runAgentStoreExclusion(t, agentStoreCase{
		name:    "Cline.Edit over globalState.json",
		kind:    agentStoreClineEdit,
		id:      "cline-race-model",
		store:   filepath.Join(".cline", "data", "globalState.json"),
		foreign: true,
		seed: `{"welcomeViewCompleted": false, "actModeApiProvider": "anthropic", ` +
			`"actModeOllamaModelId": "", "planModeApiProvider": "anthropic", "keptKey": "kept"}`,
		other: `{"welcomeViewCompleted": false, "actModeApiProvider": "anthropic", ` +
			`"actModeOllamaModelId": "", "planModeApiProvider": "anthropic", "keptKey": "kept", ` +
			`"concurrentWriter": "kept"}`,
		want: []string{
			`"concurrentWriter": "kept"`,
			`"keptKey": "kept"`,
			`"actModeOllamaModelId": "cline-race-model"`,
			`"welcomeViewCompleted": true`,
		},
		gone: []string{`"actModeApiProvider": "anthropic"`},
	})
}
