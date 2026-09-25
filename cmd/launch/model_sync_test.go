package launch

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// A hand-added entry is the user's, and the manifest's contract says automation
// never touches it. Syncing a catalog that happens to carry the same id must
// therefore FILL what the entry lacks and change nothing it already states —
// including its provenance. This was not true: sync replaced the entry
// wholesale (destroying model_path and launch_flags) and rewrote Source to
// "sync", which re-labelled a hand-added model as catalog-sourced and so made
// it prunable, deleting a model the help text promises is never touched.
func TestModelSyncNeverRewritesAHandAddedEntry(t *testing.T) {
	withTempHome(t)
	const id = "oaica-35b-a3b-vision"

	if _, err := ModelAdd(ModelAddOptions{
		ID: id, Engine: string(EngineVLLM), ContextWindow: 262144,
		LaunchFlags: []string{"--max-model-len", "262144"},
		ModelPath:   "/models/kat-awq",
		Notes:       "my own field notes",
	}); err != nil {
		t.Fatalf("ModelAdd: %v", err)
	}

	catalog := filepath.Join(t.TempDir(), "catalog.json")
	writeJSON(t, catalog, modelManifest{Version: 1, Models: map[string]ModelManifestEntry{
		id: {ID: id, Engine: EngineVLLM, ContextWindow: 1048576,
			LaunchFlags: []string{"--enable-prefix-caching"}, Quant: "awq"},
	}})
	if _, err := ModelSync("file://"+catalog, false); err != nil {
		t.Fatalf("ModelSync: %v", err)
	}

	after, ok := mustModelManifest(t).Models[id]
	if !ok {
		t.Fatalf("%s vanished from the manifest", id)
	}
	if after.ModelPath != "/models/kat-awq" {
		t.Errorf("sync repointed model_path to %q — a path comes from the filesystem or the user, never from a catalog", after.ModelPath)
	}
	if len(after.LaunchFlags) != 2 || after.LaunchFlags[0] != "--max-model-len" {
		t.Errorf("sync rewrote launch_flags: %v, want the user's two flags", after.LaunchFlags)
	}
	if after.Notes != "my own field notes" {
		t.Errorf("sync rewrote notes: %q", after.Notes)
	}
	if after.Source != "" {
		t.Errorf("sync rewrote Source to %q — that is what makes the entry prunable, and it is not the catalog's entry to claim", after.Source)
	}
	// A field the user left empty IS the catalog's to fill.
	if after.Quant != "awq" {
		t.Errorf("Quant = %q, want the catalog's value filled in", after.Quant)
	}

	// The consequence, end to end: the catalog drops the id, --prune runs, and
	// the user's entry is still there.
	writeJSON(t, catalog, modelManifest{Version: 1, Models: map[string]ModelManifestEntry{}})
	rep, err := ModelSync("file://"+catalog, true)
	if err != nil {
		t.Fatalf("ModelSync --prune: %v", err)
	}
	if len(rep.Pruned) != 0 {
		t.Errorf("pruned %v — a hand-added entry is documented as never touched by automation", rep.Pruned)
	}
	if _, ok := mustModelManifest(t).Models[id]; !ok {
		t.Fatal("--prune deleted the user's own entry")
	}
}

func mustModelManifest(t *testing.T) *modelManifest {
	t.Helper()
	m, err := loadModelManifest()
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// A catalog document need not carry a "version" field — parseModelCatalog
// tolerates its absence on purpose, so a hand-rolled catalog works. Its cached
// copy is still a perfectly good copy, and both fallbacks (a 304, and a host
// that cannot be reached) must use it. Gating them on Catalog.Version instead
// turned a working sync into a hard error the moment the network hiccuped,
// while a last-good copy sat on disk.
func TestModelSyncCacheServesAVersionlessCatalog(t *testing.T) {
	const versionless = `{"models":{"m-a":{"id":"m-a","engine":"vllm","context_window":1048576}}}`
	const versioned = `{"version":1,"models":{"m-a":{"id":"m-a","engine":"vllm","context_window":1048576}}}`

	for _, tc := range []struct{ name, body string }{
		{"catalog without a version field", versionless},
		{"control: catalog with version 1", versioned},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := withTempHome(t)
			notModified := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if notModified && r.Header.Get("If-None-Match") != "" {
					w.WriteHeader(http.StatusNotModified)
					return
				}
				w.Header().Set("ETag", `"etag-1"`)
				fmt.Fprint(w, tc.body)
			}))

			if _, err := ModelSync(srv.URL, false); err != nil {
				t.Fatalf("fresh sync: %v", err)
			}
			cachePath, err := modelSyncCachePath()
			if err != nil {
				t.Fatal(err)
			}
			if want := filepath.Join(home, ".oaica", "cache", "models", "sync.json"); cachePath != want {
				t.Fatalf("sync cache path = %q, want %q", cachePath, want)
			}
			if _, err := os.Stat(cachePath); err != nil {
				t.Fatalf("the first sync wrote no cache: %v", err)
			}

			notModified = true
			if rep, err := ModelSync(srv.URL, false); err != nil {
				t.Errorf("a 304 failed although the cache this sync just wrote is on disk: %v", err)
			} else if !rep.FromCache {
				t.Errorf("the 304 was not served from cache: %+v", rep)
			}

			srv.Close()
			if rep, err := ModelSync(srv.URL, false); err != nil {
				t.Errorf("offline sync failed although a last-good cache is on disk: %v", err)
			} else if !rep.FromCache {
				t.Errorf("the offline fallback did not use the cache: %+v", rep)
			}
		})
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
