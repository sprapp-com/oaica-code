package launch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestUsage_SingleUseDoesNotSurface(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	recordModelUse("once")
	if got := frequentModels(8); len(got) != 0 {
		t.Fatalf("a single experiment must not displace the section, got %v", got)
	}
}

func TestUsage_RankedByCountThenRecency(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	for i := 0; i < 3; i++ {
		recordModelUse("often")
	}
	for i := 0; i < 3; i++ {
		recordModelUse("also-often")
	}
	recordModelUse("often") // 4 > 3: count decides
	for i := 0; i < 2; i++ {
		recordModelUse("rarely")
	}
	got := frequentModels(8)
	if len(got) != 3 {
		t.Fatalf("ranking = %v, want three entries", got)
	}
	if got[0] != "often" || got[1] != "also-often" || got[2] != "rarely" {
		t.Fatalf("ranking = %v, want [often also-often rarely]: count decides first, and a 2-use model still surfaces", got)
	}
}

// The tiebreak is recency, not the map's iteration order: two models with the
// same count are ordered by when they were last used, and the answer is the
// same on every read of the same file.
func TestUsage_TiesBreakByRecencyDeterministically(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	for i := 0; i < 2; i++ {
		recordModelUse("older")
	}
	path, err := usageCachePath()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Rewrite the older row's timestamp; the file is the only state.
	for i := 0; i < 2; i++ {
		recordModelUse("newer")
	}
	b2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	patched := patchUsageLast(t, string(b2), "older", "2000-01-01T00:00:00Z")
	if err := os.WriteFile(path, []byte(patched), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = b
	first := frequentModels(8)
	for i := 0; i < 5; i++ {
		again := frequentModels(8)
		if len(again) != len(first) || again[0] != first[0] || again[1] != first[1] {
			t.Fatalf("read %d changed the order: %v vs %v", i, first, again)
		}
	}
	if len(first) != 2 || first[0] != "newer" || first[1] != "older" {
		t.Fatalf("ranking = %v, want [newer older]: equal counts, so the more recently used model leads", first)
	}
}

func TestUsage_CorruptFileDegradesToEmptySection(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	path, _ := usageCachePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := frequentModels(8); len(got) != 0 {
		t.Fatalf("corrupt usage file must yield an empty section, got %v", got)
	}
	// and must not block recording a new use
	recordModelUse("fresh")
	recordModelUse("fresh")
	if got := frequentModels(8); len(got) != 1 || got[0] != "fresh" {
		t.Fatalf("recording after corruption = %v", got)
	}
}

func TestUsage_RespectsLimit(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	for i := 0; i < 12; i++ {
		m := "m" + string(rune('a'+i))
		recordModelUse(m)
		recordModelUse(m)
	}
	if got := frequentModels(8); len(got) != 8 {
		t.Fatalf("limit ignored: %d entries", len(got))
	}
}

// A zero limit means the section's own cap, not an empty section.
func TestUsage_ZeroLimitMeansTheCap(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	for i := 0; i < 12; i++ {
		m := "m" + string(rune('a'+i))
		recordModelUse(m)
		recordModelUse(m)
	}
	if got := frequentModels(0); len(got) != frequentMaxEntries {
		t.Fatalf("limit 0 returned %d entries, want the cap %d", len(got), frequentMaxEntries)
	}
}

// An unwritable usage file must not stop a launch, and must not be reported.
func TestUsage_RecordIsBestEffort(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	path, err := usageCachePath()
	if err != nil {
		t.Fatal(err)
	}
	// A FILE where the directory must be: every write inside it fails.
	if err := os.WriteFile(filepath.Dir(path), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	recordModelUse("anything") // must not panic
	if got := frequentModels(8); len(got) != 0 {
		t.Fatalf("unwritable store surfaced %v", got)
	}
}

// patchUsageLast rewrites one entry's "last" field in a usage file, so a test
// can place two entries in a known order without sleeping.
func patchUsageLast(t *testing.T, body, model, last string) string {
	t.Helper()
	var f usageFile
	if err := json.Unmarshal([]byte(body), &f); err != nil {
		t.Fatalf("usage file: %v (%s)", err, body)
	}
	u, ok := f.Usage[model]
	if !ok {
		t.Fatalf("%s missing from %s", model, body)
	}
	u.Last = last
	f.Usage[model] = u
	out, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
