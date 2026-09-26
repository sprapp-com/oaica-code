package launch

// usage_phantom_row_and_since_drop_integrity_test.go — two reader defects in
// `oaica usage`'s log fold (2026-09-26 audit, ninth round, auditor B).
//
//  1. `{}` and `null` parse into a zero requestLogEntry WITHOUT error, and a
//     zero row was aggregated like any other: one request under an empty model
//     and an empty backend, i.e. a phantom ERROR line in the report that no
//     turn produced. A line with no row in it is unreadable, not a turn.
//
//  2. With --since, a row whose timestamp cannot be parsed was dropped by the
//     same branch as a row legitimately outside the window — so it vanished
//     from the report with no `unreadable` warning, which is the one place the
//     report promises to say that rows are missing. Without --since the
//     timestamp is not needed, so such a row is still counted.

import (
	"encoding/json"
	"testing"
	"time"
)

// (1) An empty JSON object or the literal null is not a turn.
func TestAnEmptyLogLineIsNotAPhantomTurn(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeLogBytes(t,
		"{}",
		"null",
		"   ",
		logRowJSON(t, "zai", "remote", 200),
	)

	rows, unreadable, err := LoadUsageStatsCountingUnreadable(UsageStatsFilter{})
	if err != nil {
		t.Fatalf("LoadUsageStatsCountingUnreadable: %v", err)
	}
	total := 0
	for _, r := range rows {
		total += r.Requests
		if r.Model == "" && r.Backend == "" {
			t.Errorf("the report has a row with no model and no backend (%+v) — an empty log line was aggregated as a turn", r)
		}
	}
	if total != 1 {
		t.Errorf("the report counts %d request(s) for one real row plus three empty lines, want 1", total)
	}
	if unreadable != 3 {
		t.Errorf("unreadable = %d, want 3 — the empty lines are silently gone from the report instead of warned about", unreadable)
	}
}

// (2) A row the --since window needs a timestamp from, and cannot get one, is
// reported as unreadable rather than silently dropped.
func TestSinceReportsRowsItCannotTimestamp(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	b, err := json.Marshal(requestLogEntry{Timestamp: "not-a-time", Model: "zai", Backend: "remote", StatusCode: 200})
	if err != nil {
		t.Fatal(err)
	}
	writeLogBytes(t, string(b), logRowJSON(t, "kat", "local", 200))

	// Control: without --since the row needs no timestamp to be counted.
	rows, unreadable, err := LoadUsageStatsCountingUnreadable(UsageStatsFilter{})
	if err != nil {
		t.Fatalf("LoadUsageStatsCountingUnreadable: %v", err)
	}
	total := 0
	for _, r := range rows {
		total += r.Requests
	}
	if total != 2 || unreadable != 0 {
		t.Errorf("without --since: %d requests / %d unreadable, want 2 / 0 — a row is countable without its timestamp", total, unreadable)
	}

	// With --since the timestamp decides, and a row that has none must be
	// reported, not quietly discarded.
	rows, unreadable, err = LoadUsageStatsCountingUnreadable(UsageStatsFilter{Since: time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatalf("LoadUsageStatsCountingUnreadable: %v", err)
	}
	total = 0
	for _, r := range rows {
		total += r.Requests
	}
	if total != 1 {
		t.Errorf("with --since: %d request(s), want 1 (the row with a readable timestamp)", total)
	}
	if unreadable != 1 {
		t.Errorf("with --since: unreadable = %d, want 1 — the row with an unparseable timestamp is dropped from the report without the warning that says rows are missing", unreadable)
	}
}
