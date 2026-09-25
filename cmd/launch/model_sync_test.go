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

// A 200 whose body is not a catalog must never be read as "this catalog
// declares zero models". Without that floor, any CDN or captive-portal error
// document — {"message":"Not Found"}, {}, or the literal null — decoded
// cleanly, and `model sync --prune` then deleted every synced entry while
// reporting a successful sync.
func TestModelSyncRefusesADocumentThatIsNotACatalog(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"an error document", `{"message":"Not Found"}`},
		{"an empty object", `{}`},
		{"the literal null", `null`},
		{"models is null", `{"version":1,"models":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withTempHome(t)
			good := filepath.Join(t.TempDir(), "good.json")
			writeJSON(t, good, modelManifest{Version: 1, Models: map[string]ModelManifestEntry{
				"m-a": {ID: "m-a", Engine: EngineVLLM, ContextWindow: 1048576},
			}})
			if _, err := ModelSync("file://"+good, false); err != nil {
				t.Fatalf("setup sync: %v", err)
			}
			// The same URL now serves a non-catalog body with a 200.
			writeJSON(t, good, json.RawMessage(tc.body))
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()

			rep, err := ModelSync(srv.URL, true)
			if err == nil {
				t.Errorf("a %d-byte non-catalog body was accepted as a catalog (report %+v)", len(tc.body), rep)
			}
			if len(rep.Pruned) > 0 {
				t.Errorf("pruned %v against a document that is not a catalog", rep.Pruned)
			}
			if _, ok := mustModelManifest(t).Models["m-a"]; !ok {
				t.Error("a non-catalog response deleted a synced entry")
			}
		})
	}
}

// A cache is a cache of the last GOOD copy. A cached catalog with nothing in it
// is not one, and serving it to an offline `--prune` deleted every synced
// entry — the mirror image of the bug above, one step later in time (the empty
// document is persisted, then the host goes away).
func TestModelSyncOfflineFallbackDeclinesAnEmptyCachedCatalog(t *testing.T) {
	withTempHome(t)
	good := filepath.Join(t.TempDir(), "good.json")
	writeJSON(t, good, modelManifest{Version: 1, Models: map[string]ModelManifestEntry{
		"m-a": {ID: "m-a", Engine: EngineVLLM, ContextWindow: 1048576},
	}})
	if _, err := ModelSync("file://"+good, false); err != nil {
		t.Fatalf("setup sync: %v", err)
	}

	// A catalog that is valid JSON with an empty model map: parseable, and
	// therefore cacheable — but not a copy anything can be pruned against.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"e1"`)
		fmt.Fprint(w, `{"version":1,"models":{}}`)
	}))
	url := srv.URL
	if _, err := ModelSync(url, false); err != nil {
		t.Fatalf("sync of an empty-but-valid catalog: %v", err)
	}
	srv.Close() // the host is now unreachable

	rep, err := ModelSync(url, true)
	if err == nil {
		t.Errorf("the offline fallback served an empty cached catalog (report %+v) — a last-good copy is not the same thing as a copy", rep)
	}
	if len(rep.Pruned) > 0 {
		t.Errorf("offline --prune against an empty cached catalog wiped %v", rep.Pruned)
	}
	if _, ok := mustModelManifest(t).Models["m-a"]; !ok {
		t.Error("an offline --prune served from an empty cache deleted a synced entry")
	}
}

// --prune only knows the catalog it just fetched. `--url` exists for an
// internal mirror, and entries that mirror supplied are not the public
// catalog's to withdraw: without recording which document an entry came from,
// one plain `model sync --prune` deleted everything the mirror had provided.
func TestModelSyncPruneSparesAnotherCatalogsEntries(t *testing.T) {
	withTempHome(t)
	internal := filepath.Join(t.TempDir(), "internal.json")
	writeJSON(t, internal, modelManifest{Version: 1, Models: map[string]ModelManifestEntry{
		"internal-a": {ID: "internal-a", Engine: EngineVLLM},
		"internal-b": {ID: "internal-b", Engine: EngineVLLM},
	}})
	if _, err := ModelSync("file://"+internal, false); err != nil {
		t.Fatalf("mirror sync: %v", err)
	}

	public := filepath.Join(t.TempDir(), "public.json")
	writeJSON(t, public, modelManifest{Version: 1, Models: map[string]ModelManifestEntry{
		"public-a": {ID: "public-a", Engine: EngineVLLM},
	}})
	rep, err := ModelSync("file://"+public, true)
	if err != nil {
		t.Fatalf("public sync: %v", err)
	}
	if len(rep.Pruned) != 0 {
		t.Errorf("pruned %v — those entries came from a different catalog, and nothing about this sync knows they were withdrawn", rep.Pruned)
	}
	m := mustModelManifest(t)
	for _, id := range []string{"internal-a", "internal-b", "public-a"} {
		if _, ok := m.Models[id]; !ok {
			t.Errorf("%s was deleted by a sync against another catalog", id)
		}
	}
	// And the entries now record which document they came from.
	if got := m.Models["internal-a"].SourceURL; got != "file://"+internal {
		t.Errorf("internal-a records source_url=%q, want the mirror it came from", got)
	}
}

// The catalog never carries a model_path, so replacing a sync-owned entry with
// the catalog entry cleared a path the user had recorded — and every later
// sync erased it again. A path comes from the filesystem or the user; it is the
// merge policy's standing exception in both directions.
func TestModelSyncKeepsARecordedModelPathOnASyncedEntry(t *testing.T) {
	withTempHome(t)
	const id = "kat-35b"
	catalog := filepath.Join(t.TempDir(), "catalog.json")
	writeJSON(t, catalog, modelManifest{Version: 1, Models: map[string]ModelManifestEntry{
		id: {ID: id, Engine: EngineVLLM, ContextWindow: 1048576},
	}})
	if _, err := ModelSync("file://"+catalog, false); err != nil {
		t.Fatalf("sync: %v", err)
	}

	// The user records where the weights are (the manifest is documented as a
	// file they also edit by hand), then the catalog advances.
	m, err := loadModelManifest()
	if err != nil {
		t.Fatal(err)
	}
	e := m.Models[id]
	e.ModelPath = "/models/kat-awq"
	m.Put(e)
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, catalog, modelManifest{Version: 1, Models: map[string]ModelManifestEntry{
		id: {ID: id, Engine: EngineVLLM, ContextWindow: 2097152},
	}})
	if _, err := ModelSync("file://"+catalog, false); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	after := mustModelManifest(t).Models[id]
	if after.ModelPath != "/models/kat-awq" {
		t.Errorf("model_path = %q, want /models/kat-awq: the catalog document never carries a path, so replacing the entry with it clears a field the merge policy says is not the catalog's to decide", after.ModelPath)
	}
	if after.ContextWindow != 2097152 {
		t.Errorf("context_window = %d, want the catalog's new value — only the path is exempt", after.ContextWindow)
	}
}

// A catalog-shipped model has no path and never will, so the local scan is the
// only thing that can record where its weights are — and it refused to touch a
// sync-owned entry, while its own header and `model list --help` both promise
// it fills a pathless entry. Refusing to REPOINT an entry that already names a
// different path is the discipline that matters.
func TestModelScanFillsAPathOnASyncedEntry(t *testing.T) {
	home := withTempHome(t)
	const id = "kat-35b"
	catalog := filepath.Join(t.TempDir(), "catalog.json")
	writeJSON(t, catalog, modelManifest{Version: 1, Models: map[string]ModelManifestEntry{
		id: {ID: id, Engine: EngineVLLM},
	}})
	if _, err := ModelSync("file://"+catalog, false); err != nil {
		t.Fatalf("sync: %v", err)
	}

	dir := filepath.Join(home, ".oaica", "models")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, id+".gguf")
	if err := os.WriteFile(want, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ModelScan(nil); err != nil {
		t.Fatalf("ModelScan: %v", err)
	}
	after := mustModelManifest(t).Models[id]
	if after.ModelPath != want {
		t.Errorf("model_path = %q, want %q — a pathless entry gains its path from the scan", after.ModelPath, want)
	}
	if after.Source != "sync" {
		t.Errorf("Source = %q: the scan must not claim an entry the catalog owns", after.Source)
	}

	// A second file claiming the same id must not repoint it.
	other := filepath.Join(dir, "sub")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, id+".gguf"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ModelScan([]string{other}); err != nil {
		t.Fatalf("ModelScan: %v", err)
	}
	if got := mustModelManifest(t).Models[id].ModelPath; got != want {
		t.Errorf("the scan repointed model_path to %q, want %q left alone", got, want)
	}
}

// The manifest keys on the exact id, and every other writer trims (model add)
// or sanitizes (the scan) what it stores. Sync took the catalog document's key
// verbatim, so one stray space produced two entries for one model — the padded
// one addressable only by retyping the padding.
func TestModelSyncNormalisesCatalogIDs(t *testing.T) {
	withTempHome(t)
	catalog := filepath.Join(t.TempDir(), "catalog.json")
	writeJSON(t, catalog, modelManifest{Version: 1, Models: map[string]ModelManifestEntry{
		"m-a":  {ID: "m-a", Engine: EngineVLLM, ContextWindow: 1048576},
		"m-a ": {ID: "m-a ", Engine: EngineVLLM, ContextWindow: 1048576},
		"  ":   {ID: "  ", Engine: EngineVLLM},
	}})
	if _, err := ModelSync("file://"+catalog, false); err != nil {
		t.Fatalf("ModelSync: %v", err)
	}
	if got := len(mustModelManifest(t).Models); got != 1 {
		t.Errorf("manifest holds %d entries after syncing a catalog with %q and a whitespace-only id, want 1", got, "m-a/m-a ")
	}
	if _, ok := mustModelManifest(t).Models["m-a"]; !ok {
		t.Error("the trimmed id is missing")
	}
}

// TestModelSyncDoesNotPersistOrEchoTheURLCredential: `oaica model sync --url
// https://KEY@mirror/models.json` is the documented way to sync from an
// internal mirror, and docs/ENTERPRISE.md states the client "never writes an
// API key into requests.log, the picker cache, the catalog caches, or doctor
// output". The three sync fetchers were the only request builders that never
// went through redactBaseURL/NewRedactedRequest, so the credential landed in
// the sync cache, in every entry's source_url, and on the success line the CLI
// prints (2026-09-26 audit).
func TestModelSyncDoesNotPersistOrEchoTheURLCredential(t *testing.T) {
	withTempHome(t)
	const token = "sk-live-mirror-do-not-print-me"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":1,"models":{"m-a":{"id":"m-a","engine":"vllm","context_window":1048576}}}`))
	}))
	defer srv.Close()

	url := strings.Replace(srv.URL, "http://", "http://"+token+"@", 1) + "/models.json"
	rep, err := ModelSync(url, false)
	if err != nil {
		t.Fatalf("ModelSync against a live mirror: %v", err)
	}
	if len(rep.Added) != 1 {
		t.Fatalf("report = %+v, want one added model", rep)
	}
	if strings.Contains(rep.URL, token) {
		t.Errorf("ModelSyncReport.URL carries the credential, and cmd prints it on the success line: %q", rep.URL)
	}

	// Both files the credential could land in: the sync cache and the manifest.
	for _, get := range []func() (string, error){modelSyncCachePath, modelManifestPath} {
		path, perr := get()
		if perr != nil {
			t.Fatalf("resolve path: %v", perr)
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatalf("read %s: %v", path, rerr)
		}
		if strings.Contains(string(b), token) {
			t.Errorf("the credential from --url was written into %s:\n%s", path, b)
		}
	}
}

// TestModelSyncNeverServesAnotherCatalogsCache: the sync cache is keyed by
// URL, so a 304 from a catalog that has never been fetched must not be
// answered with the body cached for a different one — that would silently
// install one mirror's models as another's (2026-09-26 audit).
func TestModelSyncNeverServesAnotherCatalogsCache(t *testing.T) {
	withTempHome(t)
	body := `{"version":1,"models":{"from-a":{"id":"from-a","engine":"vllm","context_window":1048576}}}`
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"a-1"`)
		fmt.Fprint(w, body)
	}))
	defer srvA.Close()
	if _, err := ModelSync(srvA.URL, false); err != nil {
		t.Fatalf("sync A: %v", err)
	}

	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srvB.Close()

	_, err := ModelSync(srvB.URL, false)
	if err == nil {
		t.Error("a 304 from a catalog that has never been fetched succeeded — the other URL's cached body was served as this catalog's")
	} else if !strings.Contains(err.Error(), "no cached copy") {
		t.Errorf("error = %v, want the no-cached-copy-for-this-URL error", err)
	}
}
