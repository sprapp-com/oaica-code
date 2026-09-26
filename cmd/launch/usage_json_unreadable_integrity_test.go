package launch

// usage_json_unreadable_integrity_test.go — `oaica usage --json` printed rows
// that silently excluded every log line it could not read. The human path warns
// about those lines; the machine path did not, and the machine path is the one
// a dashboard, cron job or cost report consumes — it sees a total that is lower
// than the log it claims to summarize, with nothing in the output to say so
// (2026-09-26 audit).
//
// The warning goes to stderr, not into the JSON: stdout has to stay the
// documented array of rows, or the fix breaks every existing consumer.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestUsageJSONWarnsAboutUnreadableLinesOnStderr(t *testing.T) {
	var out, warn bytes.Buffer
	rows := []UsageStatsRow{{Model: "kat-35b", Backend: "zai", Requests: 2, OK: 2}}
	if err := WriteUsageStatsJSON(&out, &warn, rows, 3); err != nil {
		t.Fatalf("WriteUsageStatsJSON: %v", err)
	}

	var got []UsageStatsRow
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("stdout is no longer parseable JSON (%q): %v", out.String(), err)
	}
	if len(got) != 1 || got[0].Requests != 2 {
		t.Errorf("rows = %+v, want the aggregate unchanged", got)
	}
	if !strings.Contains(warn.String(), "3") {
		t.Errorf("stderr = %q, want it to report the 3 lines that could not be read — the JSON total is lower than the log it summarizes and nothing else says so", warn.String())
	}
}

func TestUsageJSONSaysNothingOnStderrWhenNoLineWasDropped(t *testing.T) {
	var out, warn bytes.Buffer
	if err := WriteUsageStatsJSON(&out, &warn, []UsageStatsRow{}, 0); err != nil {
		t.Fatalf("WriteUsageStatsJSON: %v", err)
	}
	if warn.Len() != 0 {
		t.Errorf("stderr = %q, want it empty: a warning on every clean report is one nobody reads", warn.String())
	}
	if strings.TrimSpace(out.String()) != "[]" {
		t.Errorf("stdout = %q, want [] — an empty report still encodes as an empty array", out.String())
	}
}
