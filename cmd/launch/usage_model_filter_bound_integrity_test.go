package launch

// usage_model_filter_bound_integrity_test.go — `oaica usage --model <id>` could
// never match its own traffic for an id longer than maxLoggedModelBytes
// (2026-09-26 audit, eleventh round).
//
// request_log.go stores boundedModel(entry.Model) (maxLoggedModelBytes bytes
// plus "…"), while aggregateUsageRow compared the user's RAW --model argument
// against that stored value. An id over the bound logs truncated, the filter
// then excludes every row, and the command prints "No launch traffic logged
// yet" — a false zero reported as an empty log, with no error and no
// unreadable-line warning, for a machine that had just sent that traffic.
//
// The tests drive the real writer (appendRequestLog) and the real reader
// (LoadUsageStats / LoadUsageStatsCountingUnreadable), because the defect is
// exactly that the two disagreed about which space they compare in.

import (
	"net/http"
	"strings"
	"testing"
)

func totalUsageRequests(rows []UsageStatsRow) int {
	n := 0
	for _, r := range rows {
		n += r.Requests
	}
	return n
}

func TestUsageModelFilterCountsItsOwnTruncatedRows(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	long := "openrouter/" + strings.Repeat("anthropic/claude-sonnet-4-5-", 20) + "variant"
	if len(long) <= maxLoggedModelBytes {
		t.Fatalf("the fixture id is %d bytes, want it longer than the %d-byte bound", len(long), maxLoggedModelBytes)
	}
	appendRequestLog(requestLogEntry{
		Timestamp:  "2026-09-26T21:00:00Z",
		Model:      long,
		Path:       "/v1/messages",
		Backend:    "https://api.example.com",
		StatusCode: http.StatusOK,
	})

	rows, unreadable, err := LoadUsageStatsCountingUnreadable(UsageStatsFilter{Model: long})
	if err != nil {
		t.Fatalf("LoadUsageStatsCountingUnreadable: %v", err)
	}
	if unreadable != 0 {
		t.Errorf("unreadable = %d — the log holds one row this package wrote itself", unreadable)
	}
	if len(rows) != 1 || rows[0].Requests != 1 {
		t.Errorf("`oaica usage --model <a %d-byte id>` matched %d row(s) / %d request(s), want the 1 turn logged under that id: the stored model is bounded to %d bytes, so a filter compared against the raw argument excludes its own traffic and the command reports an empty log for a machine that just sent it",
			len(long), len(rows), totalUsageRequests(rows), maxLoggedModelBytes)
	}

	// The row stays counted when the filter the user reads back out of the
	// report (the truncated form) is what they pass.
	stored := boundedModel(long)
	if stored != long {
		rows, _, err = LoadUsageStatsCountingUnreadable(UsageStatsFilter{Model: stored})
		if err != nil {
			t.Fatalf("LoadUsageStatsCountingUnreadable(%q): %v", stored, err)
		}
		if totalUsageRequests(rows) != 1 {
			t.Errorf("filtering on the truncated id the report itself shows (%q) matched %d request(s), want 1", stored, totalUsageRequests(rows))
		}
	}

	// Control: a different over-long id must not be swept in by the bound.
	other := "openrouter/" + strings.Repeat("openai/gpt-5-", 25) + "x"
	if len(other) <= maxLoggedModelBytes {
		t.Fatalf("the control id is %d bytes, want it longer than the bound", len(other))
	}
	rows, _, err = LoadUsageStatsCountingUnreadable(UsageStatsFilter{Model: other})
	if err != nil {
		t.Fatalf("LoadUsageStatsCountingUnreadable: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("a filter for a different %d-byte id matched %d row(s) — the bound must not make unrelated ids equivalent", len(other), len(rows))
	}

	// Control: the unfiltered report still shows the turn.
	all, _, err := LoadUsageStatsCountingUnreadable(UsageStatsFilter{})
	if err != nil {
		t.Fatalf("LoadUsageStatsCountingUnreadable: %v", err)
	}
	if totalUsageRequests(all) != 1 {
		t.Errorf("the unfiltered report counts %d request(s), want 1 — the row itself is not in question", totalUsageRequests(all))
	}
}

// The bound is lossy, and so is the report: two ids sharing their first
// maxLoggedModelBytes bytes are stored as the SAME string, which is the
// aggregation key — one bucket holding both turns. Comparing the filter in the
// stored space therefore adds no ambiguity the report did not already have,
// and this pins that reading (rather than leaving it as an accident) so a
// future "exact match" change has to argue with it.
func TestUsageModelFilterMatchesWithinTheStoredSpace(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	prefix := strings.Repeat("p", maxLoggedModelBytes)
	ids := []string{prefix + "-alpha", prefix + "-beta"}
	for _, id := range ids {
		appendRequestLog(requestLogEntry{
			Timestamp:  "2026-09-26T21:00:00Z",
			Model:      id,
			Path:       "/v1/messages",
			Backend:    "https://api.example.com",
			StatusCode: http.StatusOK,
		})
	}

	for _, filter := range ids {
		rows, err := LoadUsageStats(UsageStatsFilter{Model: filter})
		if err != nil {
			t.Fatalf("LoadUsageStats(%q): %v", filter, err)
		}
		if len(rows) != 1 || rows[0].Requests != 2 {
			t.Errorf("filter %q matched %d row(s) / %d request(s), want the single bucket that holds both turns (their stored models are identical, so the unfiltered report already merges them)",
				filter, len(rows), totalUsageRequests(rows))
		}
	}
}
