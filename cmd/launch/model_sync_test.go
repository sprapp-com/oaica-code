package launch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// both tests isolate the on-disk manifest/cache by pointing HOME at a
// temp dir (modelManifestPath and the sync cache both key off
// os.UserHomeDir, which on unix follows $HOME).

func withTempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func TestModelSyncFileUpsertsAndPrunes(t *testing.T) {
	withTempHome(t)
	catalog := filepath.Join(t.TempDir(), "catalog.json")
	writeJSON(t, catalog, modelManifest{Version: 1, Models: map[string]ModelManifestEntry{
		"m-a": {ID: "m-a", Engine: EngineVLLM, ContextWindow: 1048576},
		"m-b": {ID: "m-b", Engine: EnginePrism, ContextWindow: 32768},
	}})

	rep, err := ModelSync("file://"+catalog, false)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(rep.Added) != 2 || rep.FromCache {
		t.Fatalf("report = %+v", rep)
	}
	m, err := loadModelManifest()
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Models["m-a"].Source; got != "sync" {
		t.Fatalf("source = %q, want sync", got)
	}

	// hand-added entries must survive prune
	m.Put(ModelManifestEntry{ID: "mine", Engine: EngineLlamaCPP, Notes: "my notes"})
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	// local notes survive a note-less catalog update
	writeJSON(t, catalog, modelManifest{Version: 1, Models: map[string]ModelManifestEntry{
		"m-a": {ID: "m-a", Engine: EngineVLLM, ContextWindow: 2097152},
	}})

	rep, err = ModelSync("file://"+catalog, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Pruned) != 1 || rep.Pruned[0] != "m-b" {
		t.Fatalf("pruned = %v", rep.Pruned)
	}
	m, _ = loadModelManifest()
	if _, ok := m.Models["mine"]; !ok {
		t.Fatal("prune removed a hand-added entry")
	}
	if m.Models["m-a"].ContextWindow != 2097152 {
		t.Fatalf("context = %d, want remote value", m.Models["m-a"].ContextWindow)
	}

	// invalid catalog entries are skipped, not fatal
	writeJSON(t, catalog, modelManifest{Version: 1, Models: map[string]ModelManifestEntry{
		"ok":  {ID: "ok", Engine: EngineVLLM},
		"bad": {ID: "bad", Engine: "nope"},
	}})
	rep, err = ModelSync("file://"+catalog, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Skipped) != 1 || !strings.Contains(rep.Skipped[0], "bad") {
		t.Fatalf("skipped = %v", rep.Skipped)
	}
}

func TestModelScanDetectsPQMAndGGUF(t *testing.T) {
	home := withTempHome(t)
	dir := filepath.Join(home, ".oaica", "models")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"kat-35b.pqm", "kat-35b.pqm.bf16", "tiny.q4_k_m.gguf"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := ModelScan(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Added) != 2 {
		t.Fatalf("added = %v, want 2 (kat-35b deduped)", rep.Added)
	}
	m, _ := loadModelManifest()
	e := m.Models["kat-35b"]
	if e.Engine != EnginePrism || !strings.HasSuffix(e.ModelPath, "kat-35b.pqm") {
		t.Fatalf("kat-35b entry = %+v", e)
	}
	if m.Models["tiny"].Engine != EngineLlamaCPP || m.Models["tiny"].Quant != "q4_k_m" {
		t.Fatalf("tiny entry = %+v", m.Models["tiny"])
	}

	// idempotent, and never repoints an existing path
	rep, err = ModelScan(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Added) != 0 || len(rep.Updated) != 0 {
		t.Fatalf("rescan = %+v", rep)
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}
