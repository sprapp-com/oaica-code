package launch

// round28_store_endpoint_declaration_integrity_test.go — a declaration that
// reads the ids but not the ADDRESS (2026-09-27 audit, round 28, F1/F2).
//
// Round 27 moved the drift question into each store's own vocabulary so it
// could compare the things a bare model name cannot tell apart. Two stores
// answered with only part of that vocabulary.
//
// OpenClaw's provider block carries the endpoint the app dials
// (models.providers.ollama.baseUrl, written from ConnectableHost()), and
// DeclaresSelection read the ids, the primary and the session file — never the
// address. So a config left pointing at a since-moved daemon read as current
// and the launch skipped the rewrite, leaving the app talking to the old
// endpoint (F1). Same shape as the round-22 cline finding, in the one writer
// that fix did not reach.
//
// OpenCode keyed its entries by (provider block, model id) but filtered the
// list down to oaica's own blocks BEFORE the prefix check, so a foreign block
// sitting at the head of `recent` — the entry opencode actually resolves —
// read as "already declared" while Edit would have moved our pair in front of
// it (F2). Its own doc comment says the pairs are compared "at the head of the
// recent list"; the filter is what made that false.

import (
	"testing"
)

// TestAnOpenclawConfigDiallingAnotherEndpointIsDrift is F1: the provider's
// address is part of what the store says, so a moved daemon is drift.
func TestAnOpenclawConfigDiallingAnotherEndpointIsDrift(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	t.Setenv("OLLAMA_HOST", "127.0.0.1:11434")

	row := []LaunchModel{fallbackLaunchModel("llama3.2")}
	if err := (&Openclaw{}).Edit(row); err != nil {
		t.Fatalf("Openclaw.Edit: %v", err)
	}
	// Control: what Edit just wrote must read as declared, or every launch
	// rewrites the config and this test would pass for the wrong reason.
	if !(&Openclaw{}).DeclaresSelection(row) {
		t.Fatal("control: the config Openclaw.Edit just wrote does not read as declared")
	}

	t.Setenv("OLLAMA_HOST", "127.0.0.1:11500")
	if (&Openclaw{}).DeclaresSelection(row) {
		t.Error("a config whose ollama provider still points at 127.0.0.1:11434 reads as declared while this launch would write 127.0.0.1:11500: the launch is skipped and OpenClaw keeps dialling the old endpoint")
	}
}

// TestAnOpenclawConfigOnAForeignAddressIsDrift is the same omission at its
// widest: an address oaica never wrote at all.
func TestAnOpenclawConfigOnAForeignAddressIsDrift(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	t.Setenv("OLLAMA_HOST", "127.0.0.1:11434")

	row := []LaunchModel{fallbackLaunchModel("llama3.2")}
	if err := (&Openclaw{}).Edit(row); err != nil {
		t.Fatalf("Openclaw.Edit: %v", err)
	}
	writeOpenclawBaseURLForTest(t, home, "http://192.0.2.9:11434")

	if (&Openclaw{}).DeclaresSelection(row) {
		t.Error("a config whose ollama provider points at 192.0.2.9 reads as declared: the launch is skipped and OpenClaw is left dialling a host oaica never wrote")
	}
}

// writeOpenclawBaseURLForTest repoints the ollama provider's baseUrl in
// OpenClaw's config, the way a hand edit or an older build would leave it.
func writeOpenclawBaseURLForTest(t *testing.T, home, baseURL string) {
	t.Helper()
	path := openclawConfigPath(t, home)
	doc := readJSONMapForTest(t, path)
	modelsSection, _ := doc["models"].(map[string]any)
	providers, _ := modelsSection["providers"].(map[string]any)
	ollama, _ := providers["ollama"].(map[string]any)
	if ollama == nil {
		t.Fatalf("no models.providers.ollama block in %s", path)
	}
	ollama["baseUrl"] = baseURL
	writeJSONMapForTest(t, path, doc)
}

// TestAForeignBlockAheadOfOursIsNotDeclared is F2: opencode resolves the FIRST
// recent entry, so a foreign block at the head is a store that does not hold
// this selection.
func TestAForeignBlockAheadOfOursIsNotDeclared(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:11434")

	statePath, err := openCodeStatePath()
	if err != nil {
		t.Fatal(err)
	}
	writeJSONMapForTest(t, statePath, map[string]any{
		"recent": []any{
			map[string]any{"providerID": "anthropic", "modelID": "claude-sonnet-5"},
			map[string]any{"providerID": "ollama", "modelID": "llama3.2"},
		},
	})

	row := []LaunchModel{fallbackLaunchModel("llama3.2")}
	if (&OpenCode{}).DeclaresSelection(row) {
		t.Error("opencode's state has a foreign block at the head of recent while this selection's pair sits behind it: the store the user launches into resolves to that block, yet it reads as declared and the launch is skipped")
	}

	// Control: once Edit has moved our pair in front, the same state IS current.
	if err := (&OpenCode{}).Edit(row); err != nil {
		t.Fatalf("OpenCode.Edit: %v", err)
	}
	if !(&OpenCode{}).DeclaresSelection(row) {
		t.Error("control: the state OpenCode.Edit just wrote does not read as declared: every launch would rewrite the file")
	}
}
