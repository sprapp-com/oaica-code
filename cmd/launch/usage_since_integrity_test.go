package launch

// usage_since_integrity_test.go — `oaica usage --since` accepted a value that
// inverts the filter (2026-09-26 audit, fourth round).
//
// The cutoff is now MINUS the duration, and time.ParseDuration accepts a
// leading sign, so `--since -24h` asked for the last 24 hours and got a cutoff
// 24 hours in the FUTURE. LoadUsageStatsCountingUnreadable drops every row
// whose timestamp is before the cutoff, so the command printed an empty
// report, exit 0, no warning — from a cron job or a health check that is
// indistinguishable from a machine that sent no traffic at all. `--since 0`
// is the same silent-empty answer.

import (
	"strings"
	"testing"
	"time"
)

func TestUsageSinceCutoffRejectsADurationThatWouldEmptyTheReport(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

	for _, bad := range []string{"-24h", "-1m", "-1ns", "0", "0s"} {
		got, err := UsageSinceCutoff(bad, now)
		if err == nil {
			t.Errorf("UsageSinceCutoff(%q) = %v with no error — the cutoff is now-minus-the-duration, so this lands at or after now and every logged row is filtered out: `oaica usage` prints an empty report and exit 0, which a health check cannot tell from a machine that sent no traffic", bad, got)
			continue
		}
		if !strings.Contains(err.Error(), "--since") || !strings.Contains(err.Error(), "positive") {
			t.Errorf("UsageSinceCutoff(%q) error %q does not name the flag and the fix", bad, err)
		}
		if !got.IsZero() {
			t.Errorf("UsageSinceCutoff(%q) returned a cutoff alongside its error: %v", bad, got)
		}
	}

	// A plain unparseable value keeps its own (already-correct) message.
	if _, err := UsageSinceCutoff("yesterday", now); err == nil || !strings.Contains(err.Error(), "yesterday") {
		t.Errorf("UsageSinceCutoff(\"yesterday\") error = %v, want the parse error quoting the value", err)
	}
}

func TestUsageSinceCutoffIsNowMinusTheDuration(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

	got, err := UsageSinceCutoff("30m", now)
	if err != nil {
		t.Fatal(err)
	}
	if want := now.Add(-30 * time.Minute); !got.Equal(want) {
		t.Errorf("UsageSinceCutoff(\"30m\") = %v, want %v", got, want)
	}

	// Empty string is "all time" — the zero cutoff, which the filter reads as
	// "no filter at all".
	for _, empty := range []string{"", "   "} {
		got, err := UsageSinceCutoff(empty, now)
		if err != nil {
			t.Fatalf("UsageSinceCutoff(%q): %v", empty, err)
		}
		if !got.IsZero() {
			t.Errorf("UsageSinceCutoff(%q) = %v, want the zero time (all time)", empty, got)
		}
	}
}

// The harm the guard prevents, stated as the behavior it would have allowed: a
// cutoff in the future drops rows that were just written, silently.
func TestAFutureCutoffWouldDropEveryFreshRow(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeLogBytes(t, logRowJSON(t, "m", "zai", 200))

	future, err := UsageSinceCutoff("-1h", time.Now())
	if err == nil {
		t.Fatalf("a negative --since produced a cutoff in the future: %v", future)
	}
	future = time.Now().Add(time.Hour)

	rows, _, err := LoadUsageStatsCountingUnreadable(UsageStatsFilter{Since: future})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("a future cutoff kept %d rows; the test's premise (that it empties the report) no longer holds", len(rows))
	}
}
