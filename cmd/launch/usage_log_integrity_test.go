package launch

// usage_log_integrity_test.go — `oaica usage` reads an append-only log that
// nothing rotates and that every concurrent session appends to. Three ways the
// reader and the writer disagreed, all silent, all exit 0 (2026-09-26 audit,
// third round):
//
//   - appendRequestLog wrote the row and its newline in TWO Write calls, so two
//     concurrent sessions could interleave and a reader saw "…}{…" on one line;
//   - LoadUsageStats used bufio.Scanner, which ABORTS the whole read on one
//     over-long line ("token too long") — one bad row bricked the command
//     forever, reporting zero requests;
//   - `--json` on an empty log printed `null`, not the documented row list.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// writeLogBytes writes raw bytes to this HOME's requests.log.
func writeLogBytes(t *testing.T, lines ...string) string {
	t.Helper()
	path, err := requestLogPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func logRowJSON(t *testing.T, model, backend string, status int) string {
	t.Helper()
	e := requestLogEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Model:     model, Backend: backend, StatusCode: status,
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// S4: one over-long line must cost that one row, not the whole report.
func TestUsageStatsSkipsAnOverLongRowInsteadOfAborting(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	huge := strings.Repeat("x", 4<<20) // far past the 1 MiB line cap
	writeLogBytes(t,
		logRowJSON(t, "good-before", "zai", 200),
		`{"model":"`+huge+`"}`,
		logRowJSON(t, "good-after", "zai", 200),
	)

	rows, unreadable, err := LoadUsageStatsCountingUnreadable(UsageStatsFilter{})
	if err != nil {
		t.Fatalf("LoadUsageStatsCountingUnreadable returned %v — one over-long line aborted the whole read, which is what bufio.Scanner's \"token too long\" did: `oaica usage` then reported zero requests forever with exit 0", err)
	}

	got := map[string]int{}
	for _, r := range rows {
		got[r.Model] = r.Requests
	}
	if got["good-before"] != 1 || got["good-after"] != 1 {
		t.Errorf("rows = %+v, want one request each for good-before and good-after — a row that cannot be read must not take its neighbours with it", got)
	}
	if unreadable == 0 {
		t.Error("the over-long line was silently dropped with no count — a report read as authoritative must say how many lines it could not read")
	}
}

// The same via the public entry point, which is what cmd.go actually calls.
func TestLoadUsageStatsSurvivesAnOverLongRow(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeLogBytes(t, `{"model":"`+strings.Repeat("y", 4<<20)+`"}`, logRowJSON(t, "m1", "zai", 200))
	rows, err := LoadUsageStats(UsageStatsFilter{})
	if err != nil {
		t.Fatalf("LoadUsageStats: %v", err)
	}
	if len(rows) != 1 || rows[0].Model != "m1" {
		t.Errorf("rows = %+v, want the single readable row", rows)
	}
}

// S11: `--json` on a log with no rows is `[]`, never `null`.
func TestUsageStatsJSONOfAnEmptyLogIsAnEmptyArray(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	for _, tc := range []struct {
		name string
		mk   bool
	}{
		{name: "no log file at all"},
		{name: "log file with no rows", mk: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.mk {
				writeLogBytes(t)
			}
			rows, err := LoadUsageStats(UsageStatsFilter{})
			if err != nil {
				t.Fatal(err)
			}
			if rows == nil {
				t.Fatal("LoadUsageStats returned a nil slice — `oaica usage --json` encodes that as null, not the empty row list it documents")
			}
			b, err := json.Marshal(rows)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != "[]" {
				t.Errorf("json.Marshal(rows) = %s, want []", b)
			}
		})
	}
}

// S3: a row and its newline leave in one Write, so two sessions appending
// concurrently cannot interleave into a line no reader can parse.
//
// The assertion is on the WRITER's observable contract rather than a real
// race (a race is not reproducible on demand): every line of the file is a
// complete JSON object, checked after many concurrent appends.
func TestRequestLogRowsAreNeverInterleaved(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	const writers, each = 8, 25
	done := make(chan struct{})
	for w := 0; w < writers; w++ {
		go func(w int) {
			defer func() { done <- struct{}{} }()
			for i := 0; i < each; i++ {
				appendRequestLog(requestLogEntry{
					Timestamp:  time.Now().UTC().Format(time.RFC3339),
					Model:      fmt.Sprintf("model-%d", w),
					Backend:    "zai",
					StatusCode: 200,
				})
			}
		}(w)
	}
	for w := 0; w < writers; w++ {
		<-done
	}

	path, err := requestLogPath()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines, bad := 0, 0
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		lines++
		var e requestLogEntry
		if json.Unmarshal([]byte(line), &e) != nil {
			bad++
			if bad == 1 {
				t.Errorf("a non-JSON line in requests.log — two writers interleaved their bytes (the row and its newline must leave in ONE Write; two Writes are not atomic even under O_APPEND):\n%.200s", line)
			}
		}
	}
	if lines != writers*each {
		t.Errorf("requests.log has %d lines, want %d — rows were lost or merged", lines, writers*each)
	}
	stats, err := LoadUsageStats(UsageStatsFilter{})
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, r := range stats {
		total += r.Requests
	}
	if total != writers*each {
		t.Errorf("`oaica usage` accounts for %d of %d requests — it read a lower count than was written", total, writers*each)
	}
}
