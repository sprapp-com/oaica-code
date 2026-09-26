package launch

// openclaw_store_race_integrity_test.go — four OpenClaw writers rewrite a
// document oaica only partly owns, and none of them held a lock: Openclaw.Edit
// merges the model provider into ~/.openclaw/openclaw.json and publishes the
// whole file, configureOllamaWebSearch rewrites the same file's plugin and tool
// sections, clearSessionModelOverride rewrites the session records under
// ~/.openclaw/agents/main/sessions/sessions.json, and patchDeviceScopes patches
// the gateway's pairing record ~/.openclaw/devices/paired.json. Each reads a
// snapshot, mutates it in memory and renames a whole document over the file, so
// two oaica commands whose writes overlap each lose the other's entry — and the
// pair over openclaw.json can be two different commands, which is the case a
// per-function lock would still miss (2026-09-26 audit, twelfth round).
//
// All four now take the store's lock (foreignStoreLockBase — these files are
// OpenClaw's, so the .lock is not dropped inside OpenClaw's own directories) and
// the read happens inside it.
//
// The writes that stay unlocked, deliberately: the identity and install
// bookkeeping this integration does not read back, and the daemon's own writes,
// which know nothing about this lock — only oaica-against-oaica is closed.
//
// The machinery lives in agent_config_store_race_test.go.

import (
	"path/filepath"
	"testing"
)

func TestOpenclawEditTakesOpenclawConfigLock(t *testing.T) {
	runAgentStoreExclusion(t, agentStoreCase{
		name:    "Openclaw.Edit",
		kind:    agentStoreOpenclawEdit,
		id:      "openclaw-race-model",
		store:   filepath.Join(".openclaw", "openclaw.json"),
		foreign: true,
		seed: `{
  "theme": "kept",
  "agents": {"defaults": {"model": {"primary": "openai/gpt"}}}
}
`,
		other: `{
  "theme": "kept",
  "agents": {"defaults": {"model": {"primary": "openai/gpt"}}},
  "concurrent_writer": {"entry": "kept"}
}
`,
		want: []string{`"concurrent_writer": {`, `"entry": "kept"`, `"theme": "kept"`, `"primary": "ollama/openclaw-race-model"`},
		gone: []string{`openai/gpt`},
	})
}

func TestOpenclawWebSearchTakesOpenclawConfigLock(t *testing.T) {
	runAgentStoreExclusion(t, agentStoreCase{
		name:    "configureOllamaWebSearch",
		kind:    agentStoreOpenclawWebSearch,
		id:      "openclaw-race-model",
		store:   filepath.Join(".openclaw", "openclaw.json"),
		foreign: true,
		seed: `{
  "theme": "kept",
  "plugins": {"entries": {"openclaw-web-search": {"enabled": true}}}
}
`,
		other: `{
  "theme": "kept",
  "plugins": {"entries": {"openclaw-web-search": {"enabled": true}}},
  "concurrent_writer": {"entry": "kept"}
}
`,
		want: []string{`"concurrent_writer": {`, `"entry": "kept"`, `"theme": "kept"`, `"provider": "ollama"`},
		gone: []string{`openclaw-web-search`},
	})
}

func TestOpenclawDeviceScopesTakesPairedStoreLock(t *testing.T) {
	runAgentStoreExclusion(t, agentStoreCase{
		name:    "patchDeviceScopes",
		kind:    agentStoreOpenclawDeviceScopes,
		id:      "openclaw-race-model",
		store:   filepath.Join(".openclaw", "devices", "paired.json"),
		foreign: true,
		extraFiles: map[string]string{
			filepath.Join(".openclaw", "identity", "device-auth.json"): `{"deviceId": "local-device"}`,
		},
		seed: `{
  "local-device": {"scopes": []}
}
`,
		other: `{
  "local-device": {"scopes": []},
  "concurrent_writer": {"entry": "kept"}
}
`,
		want: []string{`"concurrent_writer": {`, `"entry": "kept"`, `"operator.pairing"`},
		gone: []string{`"scopes": []`},
	})
}

func TestOpenclawSessionOverrideTakesSessionsStoreLock(t *testing.T) {
	runAgentStoreExclusion(t, agentStoreCase{
		name:    "clearSessionModelOverride",
		kind:    agentStoreOpenclawSessionOverrid,
		id:      "openclaw-race-model",
		store:   filepath.Join(".openclaw", "agents", "main", "sessions", "sessions.json"),
		foreign: true,
		seed: `{
  "session-1": {"modelOverride": "old-model", "providerOverride": "openai"}
}
`,
		other: `{
  "session-1": {"modelOverride": "old-model", "providerOverride": "openai"},
  "concurrent_writer": {"entry": "kept"}
}
`,
		want: []string{`"concurrent_writer": {`, `"entry": "kept"`},
		gone: []string{`"modelOverride": "old-model"`},
	})
}
