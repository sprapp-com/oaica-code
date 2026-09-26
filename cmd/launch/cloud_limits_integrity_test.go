package launch

// cloud_limits_integrity_test.go — the synced cloud-limits catalog
// (~/.oaica/cache/cloud_limits/cloud_limits.json) supplies context/output
// windows that reach CLAUDE_CODE_MAX_CONTEXT_TOKENS, AUTO_COMPACT_WINDOW,
// codex's context_window and kimi's max context size. Two defects in how it is
// merged with the embedded default (2026-09-26 audit, third round):
//
//   - the file carries a "version" that nothing read, so a synced document of
//     unknown age replaced a shipped one unconditionally — including with
//     numbers the embedded copy had already corrected, and including with
//     zeros (a row's Context is consumed without a check at tier_routing.go
//     and codex.go, so context 0 became MAX_CONTEXT_TOKENS=0);
//   - a row that states no window at all counted as a limit, so every
//     "0 means unknown" rule the sibling catalogs follow was inverted here.
//
// provider_catalog.go gates its synced override on the version for exactly the
// first reason; this catalog never did.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeCloudLimitsCache writes the synced catalog under the test HOME.
func writeCloudLimitsCache(t *testing.T, version int, limits map[string]map[string]int) {
	t.Helper()
	path, err := cloudLimitsCatalogCachePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{"version": version, "limits": limits})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// F1: a row with no context window is not a limit. Nothing downstream rejects
// one: tier_routing.go writes l.Context into the env pair verbatim.
func TestCloudLimitsCatalogIgnoresARowWithNoContext(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeCloudLimitsCache(t, 1, map[string]map[string]int{
		"no-window-alias": {"context": 0, "output": 4096},
		"output-only":     {"output": 4096},
	})

	for name, l := range cloudLimitsFromCatalog() {
		if l.Context <= 0 {
			t.Errorf("catalog accepted %q with context %d — a zero context flows into CLAUDE_CODE_MAX_CONTEXT_TOKENS=0 and turns auto-compact off instead of on", name, l.Context)
		}
		if l.Output <= 0 {
			t.Errorf("catalog accepted %q with output %d", name, l.Output)
		}
	}

	// And the lookup refuses one even when a caller hands the map in directly:
	// the limit table is the last thing between a bad number and the env vars.
	// keyed by base name, as the dynamic map is
	setDynamicCloudModelLimits(map[string]cloudModelLimit{"zeroed": {Context: 0, Output: 4096}})
	defer setDynamicCloudModelLimits(nil)
	if l, ok := lookupCloudModelLimit("zeroed:cloud"); ok {
		t.Errorf("lookupCloudModelLimit returned %+v, ok — every caller treats ok as 'this window is known'", l)
	}
}

// F1/F3: a synced document cannot blank a window the embedded default states,
// and cannot replace a stated one without a version bump of its own.
func TestCloudLimitsCacheCannotOverruleTheEmbeddedDefault(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	embedded := cloudLimitsFromCatalog()
	kimi, ok := embedded["kimi-k2.6"]
	if !ok {
		t.Fatal("setup: the embedded default no longer lists kimi-k2.6")
	}

	// Same version, different numbers: the shipped row stands. This is the
	// shape a stale cache takes — it was fetched when the number was wrong.
	writeCloudLimitsCache(t, 1, map[string]map[string]int{
		"kimi-k2.6": {"context": 0, "output": 0},
		"glm-5":     {"context": 4096, "output": 4096},
	})
	got := cloudLimitsFromCatalog()
	if got["kimi-k2.6"] != kimi {
		t.Errorf("a same-version synced row blanked the embedded window: got %+v, want %+v (a zero states nothing)", got["kimi-k2.6"], kimi)
	}
	if got["glm-5"] != embedded["glm-5"] {
		t.Errorf("a same-version synced row replaced the embedded window: got %+v, want %+v — a synced file is of unknown age; provider_catalog.go gates the same override on a version bump", got["glm-5"], embedded["glm-5"])
	}

	// A newer version, and it wins: that is the correction path.
	writeCloudLimitsCache(t, 2, map[string]map[string]int{
		"kimi-k2.6": {"context": 200000, "output": 200000},
	})
	got = cloudLimitsFromCatalog()
	if got["kimi-k2.6"].Context != 200000 {
		t.Errorf("a version-bumped synced row did not take effect: got %+v", got["kimi-k2.6"])
	}
	if got["glm-5"] != embedded["glm-5"] {
		t.Errorf("a version bump on one row changed another: got %+v", got["glm-5"])
	}
	// Additive keys still work at any version: sync exists to add aliases.
	writeCloudLimitsCache(t, 1, map[string]map[string]int{
		"brand-new-alias": {"context": 131072, "output": 8192},
	})
	if l, ok := cloudLimitsFromCatalog()["brand-new-alias"]; !ok || l.Context != 131072 {
		t.Errorf("a synced row for an alias the embedded default does not have did not land: %+v, ok=%v", l, ok)
	}
}

// An unparseable cache is not a catalog — it must not take the embedded
// default down with it.
func TestCloudLimitsUnparseableCacheIsIgnored(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	path, err := cloudLimitsCatalogCachePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if l, ok := cloudLimitsFromCatalog()["kimi-k2.6"]; !ok || l.Context <= 0 {
		t.Errorf("an unparseable synced cache removed the embedded limits: %+v, ok=%v", l, ok)
	}
}
