package launch

// usage_line_boundary_integrity_test.go — a log line of EXACTLY the 1 MiB cap
// was counted as unreadable although nothing was lost, so `oaica usage` warned
// about a truncated row on a log that was perfectly fine (2026-09-26 audit).
//
// readLogLine appends what fits in the remaining room and sets truncated when
// the chunk it got back is larger. The chunk from ReadSlice includes the line
// TERMINATOR, so a line of exactly maxLogLineBytes content plus its '\n' was
// one byte "too long" — the only byte dropped was the newline that gets
// stripped anyway. The reader then reported one unreadable line per such row,
// which is a false alarm on the count the user is told to trust; at a
// maxLogLineBytes-sized write (a long tool result recorded verbatim) it fires
// every time.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// rowOfExactSize returns a valid log row whose JSON is exactly n bytes.
func rowOfExactSize(t *testing.T, n int, model string) string {
	t.Helper()
	e := requestLogEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Model:     model, Backend: "zai", StatusCode: 200,
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > n {
		t.Fatalf("the base row is %d bytes, longer than the %d requested", len(b), n)
	}
	return string(b) + strings.Repeat(" ", n-len(b))
}

func TestALogLineOfExactlyTheCapIsNotCountedUnreadable(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	line := rowOfExactSize(t, maxLogLineBytes, "exactly-at-the-cap")
	if len(line) != maxLogLineBytes {
		t.Fatalf("test setup: line is %d bytes, want %d", len(line), maxLogLineBytes)
	}
	writeLogBytes(t, line, logRowJSON(t, "after", "zai", 200))

	rows, unreadable, err := LoadUsageStatsCountingUnreadable(UsageStatsFilter{})
	if err != nil {
		t.Fatalf("LoadUsageStatsCountingUnreadable: %v", err)
	}
	got := map[string]int{}
	for _, r := range rows {
		got[r.Model] = r.Requests
	}
	if got["exactly-at-the-cap"] != 1 || got["after"] != 1 {
		t.Errorf("rows = %+v, want both the exactly-at-the-cap row and the row after it — a line whose only dropped byte is its newline lost nothing and must be read like any other", got)
	}
	if unreadable != 0 {
		t.Errorf("unreadable = %d, want 0: the line is exactly %d bytes and its own terminator was counted as overflow, so `oaica usage` warns about a truncated row on a healthy log", unreadable, maxLogLineBytes)
	}
}

// The control: one byte over the cap IS a truncated line and must still be
// counted, or the cap stops protecting the read.
func TestALogLineOverTheCapIsStillCountedUnreadable(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeLogBytes(t, rowOfExactSize(t, maxLogLineBytes+1, "one-over"), logRowJSON(t, "after", "zai", 200))

	rows, unreadable, err := LoadUsageStatsCountingUnreadable(UsageStatsFilter{})
	if err != nil {
		t.Fatalf("LoadUsageStatsCountingUnreadable: %v", err)
	}
	if unreadable != 1 {
		t.Errorf("unreadable = %d, want 1 for a line one byte past the %d-byte cap", unreadable, maxLogLineBytes)
	}
	for _, r := range rows {
		if r.Model == "one-over" {
			t.Errorf("a line past the cap was read as a row (%+v)", r)
		}
	}
}
