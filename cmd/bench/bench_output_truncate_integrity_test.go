package main

// bench_output_truncate_integrity_test.go — the -output file was opened
// O_CREATE|O_WRONLY with no O_TRUNC, so writes started at offset 0 and the file
// kept its previous length. Re-running a benchmark over a longer earlier report
// produced a results file whose tail was the PREVIOUS run's measurements: a
// 2-epoch run over a 10-epoch report left eight stale rows behind, exit 0, and
// nothing marking where the new run ends (2026-09-26 audit).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAShorterBenchRunLeavesNoPreviousResultsBehind(t *testing.T) {
	path := filepath.Join(t.TempDir(), "results.csv")
	previous := strings.Repeat("epoch,metric,stale-value\n", 40)
	if err := os.WriteFile(path, []byte(previous), 0o644); err != nil {
		t.Fatal(err)
	}

	f, err := benchOutputWriter(path)
	if err != nil {
		t.Fatalf("benchOutputWriter: %v", err)
	}
	// Before a single byte of the new run is written the file must already be
	// empty: opening for write is what discards the old report, and if it does
	// not, the new report cannot overwrite the old one's tail either.
	if fi, err := f.Stat(); err != nil {
		t.Fatal(err)
	} else if fi.Size() != 0 {
		t.Errorf("the output file is %d bytes after opening, want 0 — the previous run's report survives the new one, so a shorter run produces a file whose last rows are stale measurements from a different run", fi.Size())
	}
	if _, err := f.WriteString("epoch,metric\n1,ok\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "epoch,metric\n1,ok\n" {
		t.Errorf("results file = %q, want only this run's rows — stale measurements from a previous run are indistinguishable from current ones", got)
	}
}
