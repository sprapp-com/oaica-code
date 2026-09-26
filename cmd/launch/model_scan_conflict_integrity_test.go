package launch

// model_scan_conflict_integrity_test.go — when two model files claimed the same
// manifest id, the scan registered one and silently discarded the other: not
// added, not updated, not reported in Ignored or Invalid, with the winner
// decided by walk order alone. `oaica model scan` printed "2 added" over three
// files with no hint that one of them is now invisible to `oaica model list`
// (2026-09-26 audit).
//
// The discipline is the same one the existing path-conflict case already
// applies against a stored entry: never repoint a model, but say so. The
// second file is not a duplicate the user can be assumed to not care about —
// it may be a different quant, a different size, or a stale copy they forgot
// about, and nothing else in the toolkit will ever mention it again.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeModelFile(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestASecondFileClaimingTheSameIDIsReportedNotSilentlyDropped(t *testing.T) {
	home := withTempHome(t)
	first := writeModelFile(t, filepath.Join(home, "models-a"), "kat-35b.pqm")
	second := writeModelFile(t, filepath.Join(home, "models-b"), "kat-35b.pqm")

	rep, err := ModelScan([]string{filepath.Join(home, "models-a"), filepath.Join(home, "models-b")})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Added) != 1 {
		t.Fatalf("added = %v, want just kat-35b — the two files share one id", rep.Added)
	}
	// The higher-priority directory wins, and it must be the winner every run:
	// discovery order is the only thing that decides it, so the result cannot
	// depend on map iteration or an unstable sort.
	if got := mustModelManifest(t).Models["kat-35b"].ModelPath; got != first {
		t.Errorf("model_path = %q, want %q (the first directory listed)", got, first)
	}
	reported := strings.Join(rep.Conflicts, "\n")
	if !strings.Contains(reported, second) {
		t.Errorf("the scan registered %s and never mentioned %s — that file is now invisible to `oaica model list` and `oaica model scan` prints a count that says nothing was skipped (conflicts = %v)", first, second, rep.Conflicts)
	}
	if !strings.Contains(reported, first) {
		t.Errorf("the conflict does not name the path that was kept, so the user cannot tell which copy won: %v", rep.Conflicts)
	}
}

// The same conflict seen one run later: the id is already in the manifest
// pointing at another path. That case was counted in Ignored, whose reasons
// `oaica model scan` never prints — the count line just says "already known",
// which reads as "nothing to do".
func TestAConflictWithAStoredEntryIsReportedNotCountedAsAlreadyKnown(t *testing.T) {
	home := withTempHome(t)
	kept := writeModelFile(t, filepath.Join(home, "models"), "kat-35b.pqm")
	other := writeModelFile(t, filepath.Join(home, "elsewhere"), "kat-35b.pqm")

	if _, err := ModelScan([]string{filepath.Join(home, "models")}); err != nil {
		t.Fatal(err)
	}
	rep, err := ModelScan([]string{filepath.Join(home, "elsewhere")})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Conflicts) == 0 {
		t.Fatalf("rescanning a second copy reported no conflict (ignored=%v) — the scan refuses to repoint the entry, correctly, but the file it refused is silently unregistered", rep.Ignored)
	}
	if got := mustModelManifest(t).Models["kat-35b"].ModelPath; got != kept {
		t.Errorf("model_path = %q, want the stored %q left alone", got, kept)
	}
	if !strings.Contains(strings.Join(rep.Conflicts, "\n"), other) {
		t.Errorf("the conflict does not name %s: %v", other, rep.Conflicts)
	}
}

// The control: rescanning the identical file is not a conflict. A scan that
// reported one on every run would make the real conflicts unreadable.
func TestRescanningTheSameFileReportsNoConflict(t *testing.T) {
	home := withTempHome(t)
	dir := filepath.Join(home, "models")
	writeModelFile(t, dir, "kat-35b.pqm")

	if _, err := ModelScan([]string{dir}); err != nil {
		t.Fatal(err)
	}
	rep, err := ModelScan([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Conflicts) != 0 {
		t.Errorf("a plain rescan reported conflicts: %v", rep.Conflicts)
	}
	if len(rep.Ignored) != 1 {
		t.Errorf("ignored = %v, want the one already-registered file", rep.Ignored)
	}
}

// Overlapping directories are one file seen twice, not two files claiming one
// id — the same path must not be reported as a conflict just because both a
// directory and a subdirectory of it are on the scan list.
func TestTheSameFileFoundTwiceIsNotAConflict(t *testing.T) {
	home := withTempHome(t)
	dir := filepath.Join(home, "models")
	writeModelFile(t, filepath.Join(dir, "sub"), "kat-35b.pqm")

	rep, err := ModelScan([]string{dir, filepath.Join(dir, "sub")})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Conflicts) != 0 {
		t.Errorf("one file reached by two directories was reported as a conflict: %v", rep.Conflicts)
	}
	if len(rep.Added) != 1 {
		t.Errorf("added = %v, want one entry", rep.Added)
	}
}
