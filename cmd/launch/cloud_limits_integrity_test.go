package launch

// cloud_limits_integrity_test.go — the cloud-alias context/output windows that
// reach CLAUDE_CODE_MAX_CONTEXT_TOKENS, AUTO_COMPACT_WINDOW, codex's
// context_window and kimi's max context size, and how the synced overlay is
// allowed to move them. Two defects in the file these came from (2026-09-26
// audit, third round):
//
//   - the retired cloud_limits.json carried a "version" that nothing read, so a
//     synced document of unknown age replaced the shipped one unconditionally —
//     including with the numbers the embedded copy had already corrected, and
//     including with zeros (a row's Context is consumed without a check at
//     tier_routing.go and codex.go, so context 0 became MAX_CONTEXT_TOKENS=0);
//   - a row that states no window at all counted as a limit, so every
//     "0 means unknown" rule the sibling catalogs follow was inverted here.
//
// The limits live in the overlay now (catalog_overlay.go), merged per key at
// read time, so the version-bump gate the old provider catalog carried is not
// needed and not wanted: a correction lands when it is stated, and a document
// that states a zero cannot blank a number it does not name. What is left for
// this file is the zero rule, stated from both sides.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeCloudLimitsCache writes an overlay whose limits section is the given
// rows, under the test HOME — the shape `oaica remote sync` leaves behind.
func writeCloudLimitsCache(t *testing.T, limits map[string]map[string]int) {
	t.Helper()
	m := make(map[string]any, len(limits))
	for id, l := range limits {
		m[id] = l
	}
	body, err := json.Marshal(map[string]any{
		"version":   1,
		"providers": []any{},
		"limits":    m,
	})
	if err != nil {
		t.Fatal(err)
	}
	writeOverlayCache(t, string(body))
}

// F1: a row with no context window is not a limit. Nothing downstream rejects
// one: tier_routing.go writes l.Context into the env pair verbatim.
func TestCloudLimitsCatalogIgnoresARowWithNoContext(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeCloudLimitsCache(t, map[string]map[string]int{
		"no-window-alias": {"context": 0, "output": 4096},
		"output-only":     {"output": 4096},
	})

	got := cloudLimitsFromCatalog()
	if l, ok := got["no-window-alias"]; ok {
		t.Errorf("catalog accepted %q with %+v — a zero context flows into CLAUDE_CODE_MAX_CONTEXT_TOKENS=0 and turns auto-compact off instead of on", "no-window-alias", l)
	}
	if l, ok := got["output-only"]; ok {
		t.Errorf("catalog accepted %q with %+v", "output-only", l)
	}
	for name, l := range got {
		if l.Context <= 0 {
			t.Errorf("catalog accepted %q with context %d", name, l.Context)
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

// F1/F3: the synced overlay corrects what it states and cannot blank what it
// omits. Both halves matter and they pull in opposite directions, which is why
// they are asserted together: a zero must leave the embedded number standing,
// and a stated number must be taken — a declared window is a measurement, and
// the synced copy is the newer document.
func TestCloudLimitsCacheCorrectsStatedWindowsAndCannotBlankUnstatedOnes(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	embedded := cloudLimitsFromCatalog()
	kimi, ok := embedded["kimi-k2.6"]
	if !ok {
		t.Fatal("setup: the embedded overlay no longer lists kimi-k2.6")
	}

	// Zeros state nothing: the shipped row stands. This is the shape a stale
	// cache takes — it was fetched when the number was wrong, and the row is
	// listed without a window.
	writeCloudLimitsCache(t, map[string]map[string]int{
		"kimi-k2.6": {"context": 0, "output": 0},
	})
	if got := cloudLimitsFromCatalog(); got["kimi-k2.6"] != kimi {
		t.Errorf("a synced row blanked the embedded window: got %+v, want %+v (a zero states nothing)", got["kimi-k2.6"], kimi)
	}

	// A stated number wins, with no version to bump: the merge is per key at
	// READ time, so a correction is simply a correction — there is no gate
	// holding it back and none is wanted (the file it would gate is ours, and
	// an upstream correction must not be blocked by it).
	writeCloudLimitsCache(t, map[string]map[string]int{
		"glm-5": {"context": 4096, "output": 4096},
	})
	if got := cloudLimitsFromCatalog(); got["glm-5"] != (cloudModelLimit{Context: 4096, Output: 4096}) {
		t.Errorf("a synced row that states a window did not take effect: got %+v, want 4096/4096", got["glm-5"])
	}

	// Correcting one field keeps the other: a document that says the context and
	// nothing about the output must not blank the output it does not restate.
	shipped := embedded["glm-5.3-flash"]
	if shipped.Output == 0 {
		t.Fatal("setup: the embedded overlay no longer states glm-5.3-flash's output")
	}
	writeCloudLimitsCache(t, map[string]map[string]int{
		"glm-5.3-flash": {"context": 99999},
	})
	got := cloudLimitsFromCatalog()
	if got["glm-5.3-flash"].Context != 99999 {
		t.Errorf("glm-5.3-flash context = %d, want the synced 99999", got["glm-5.3-flash"].Context)
	}
	if got["glm-5.3-flash"].Output != shipped.Output {
		t.Errorf("glm-5.3-flash output = %d, want %d: a field the document does not state keeps the value the row already carried, it is not blanked", got["glm-5.3-flash"].Output, shipped.Output)
	}

	// Additive keys land: sync exists to add aliases the binary does not ship.
	writeCloudLimitsCache(t, map[string]map[string]int{
		"brand-new-alias": {"context": 131072, "output": 8192},
	})
	if l, ok := cloudLimitsFromCatalog()["brand-new-alias"]; !ok || l.Context != 131072 {
		t.Errorf("a synced row for an alias the embedded overlay does not have did not land: %+v, ok=%v", l, ok)
	}
}

// An unparseable cache is not an overlay — it must not take the embedded
// default down with it.
func TestCloudLimitsUnparseableCacheIsIgnored(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	path, err := oaicaOverlayCachePath()
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
