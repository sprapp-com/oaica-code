package launch

// catalog_shape_guard_integrity_test.go — a 200 whose body is valid JSON but
// not a catalog replaced the good synced catalog and was reported as success
// (2026-09-26 audit, sixth round).
//
// The guard on `oaica remote sync` and `oaica model cloud-limits sync` was
// "does it parse as JSON" and nothing more:
//
//	{"message":"Not Found"}   ← a proxy or CDN answering 200 for a missing path
//	{}                        ← a placeholder someone committed by accident
//	null
//
// all decode cleanly into "this catalog has zero entries", are written over the
// previous cache, and the command prints `synced 0 provider(s) ... (fresh)` and
// exits 0. The cache wins over the embedded default, so with ACME_API_KEY set a
// provider that only the synced catalog knows is listed by `oaica remote list`
// before the clobber and gone after it — and the NEXT run, offline, re-reads
// the garbage and calls it `(cached/304)` with another exit 0. The same
// substitution silently resets every cloud alias's context window.
//
// `parseModelCatalog` (model_sync.go) already refuses exactly this shape, for
// exactly this reason: a document with no "models" MEMBER is not an empty
// catalog. Its two siblings never got the rule.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// catalogFileFor writes body as a file:// source and returns its URL.
func catalogFileFor(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return "file://" + p
}

// notACatalogBodies are documents that parse as JSON and say nothing about
// providers — each one used to be cached as "a catalog with zero entries".
var notACatalogBodies = []struct{ name, body string }{
	{"a proxy's 200 for a missing path", `{"message":"Not Found"}`},
	{"an empty object", `{}`},
	{"a literal null", `null`},
	{"a JSON array", `[]`},
	{"an unrelated document", `{"version":1,"error":"forbidden"}`},
	{"providers present but null", `{"version":1,"providers":null}`},
}

func TestProviderSyncRefusesABodyThatIsNotAProviderCatalog(t *testing.T) {
	for _, tc := range notACatalogBodies {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			setLaunchTestHome(t, home)

			// A good sync first, so there is a catalogue to lose.
			good := catalogFileFor(t, "good.json", `{"version":1,"providers":[{"name":"acme","base_url":"https://api.acme.example/v1","api_key_env":"ACME_API_KEY"}]}`)
			if _, err := ProviderSync(good); err != nil {
				t.Fatalf("premise: syncing a valid catalog failed: %v", err)
			}
			cache, err := providerCatalogCachePath()
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(cache)
			if err != nil {
				t.Fatalf("premise: no cache was written: %v", err)
			}

			bad := catalogFileFor(t, "bad.json", tc.body)
			rep, err := ProviderSync(bad)
			if err == nil {
				t.Errorf("%s was accepted as a provider catalog and synced as %d provider(s) — the cache now wins over the embedded default, so every provider it does not mention disappears from `oaica remote list` and from the picker, and the command reports success while doing it", tc.name, rep.Count)
			}

			after, rerr := os.ReadFile(cache)
			if rerr != nil {
				t.Fatalf("the cache file was removed: %v", rerr)
			}
			if string(after) != string(before) {
				t.Errorf("the cached catalog was replaced by %s — a body that is not a catalog must leave the last good copy alone:\nbefore: %s\nafter:  %s", tc.name, before, after)
			}
		})
	}
}

// The control: a genuine catalog — including one that honestly declares zero
// providers — still syncs. The rule is about the SHAPE of the document, not
// about how many entries it carries.
func TestProviderSyncStillAcceptsRealCatalogs(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"one provider", `{"version":1,"providers":[{"name":"acme","base_url":"https://api.acme.example/v1"}]}`, 1},
		{"an explicitly empty catalog", `{"version":1,"providers":[]}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setLaunchTestHome(t, t.TempDir())
			rep, err := ProviderSync(catalogFileFor(t, "c.json", tc.body))
			if err != nil {
				t.Fatalf("a real catalog was refused: %v — the shape rule must not reject a document that names its providers", err)
			}
			if rep.Count != tc.want {
				t.Errorf("Count = %d, want %d", rep.Count, tc.want)
			}
		})
	}
}

// The offline half of the same defect: once a bad body is in the cache, a later
// run with the server gone re-reads it and reports success again.
func TestProviderSyncRefusesAGarbageCachedCatalogOffline(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	cache, err := providerCatalogCachePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(cache), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, []byte(`{"message":"Not Found"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	rep, err := ProviderSync("http://127.0.0.1:1/catalog.json")
	if err == nil {
		t.Errorf("an unreachable source plus a cached body that is not a catalog reported `synced %d provider(s)` and exit 0 — the garbage cache keeps the whole catalogue empty for every later run, silently", rep.Count)
	}
}

func TestCloudLimitsSyncRefusesABodyThatIsNotALimitsCatalog(t *testing.T) {
	for _, tc := range notACatalogBodies {
		// The "providers" member is this catalogue's own key name spelled
		// differently; what matters is that a document about something else is
		// not read as "no limits".
		t.Run(tc.name, func(t *testing.T) {
			setLaunchTestHome(t, t.TempDir())
			cache, err := cloudLimitsCatalogCachePath()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(cache), 0o700); err != nil {
				t.Fatal(err)
			}
			good := `{"version":1,"limits":{"kat-awq":{"context_window":262144}}}`
			if err := os.WriteFile(cache, []byte(good), 0o600); err != nil {
				t.Fatal(err)
			}

			rep, err := CloudLimitsSync(catalogFileFor(t, "bad.json", tc.body))
			if err == nil {
				t.Errorf("%s was accepted as a cloud-limits catalog and synced as %d limit(s) — this cache feeds CLAUDE_CODE_MAX_CONTEXT_TOKENS and codex's context_window, so a silent zero resets every alias to the built-in size", tc.name, rep.Count)
			}
			after, _ := os.ReadFile(cache)
			if !strings.Contains(string(after), "262144") {
				t.Errorf("the cached limits were replaced by %s:\n%s", tc.name, after)
			}
		})
	}
}
