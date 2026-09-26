package launch

// usage_table_forgery_integrity_test.go — `oaica usage` printed a
// client-supplied model id and a user-config backend raw (2026-09-26 audit,
// tenth round).
//
// The model cell is the CLIENT's request body (request_log.go), and the backend
// cell is a base URL out of the user's own remotes.json (redactBaseURL). A row
// carrying a newline therefore paints a second, completely invented traffic line
// — a model, a backend, request counts — into the report a user reads to find
// out what their traffic actually was. Over-long ids are already bounded
// (boundedModel); control characters were not.

import (
	"bytes"
	"strings"
	"testing"
)

func TestUsageTableDoesNotPrintARawModelOrBackend(t *testing.T) {
	rows := []UsageStatsRow{
		{
			// A model id the client chose: a newline plus a full forged row.
			Model:    "real-model\ngpt-5    https://evil.example   9999   9999      0       999999",
			Backend:  "https://box.example/v1",
			Requests: 1,
			OK:       1,
		},
		{
			Model:   "other",
			Backend: "https://box.example/v1\nthird    https://worse.example      1      1      0            1",
			Requests: 2,
			OK:       2,
		},
	}

	var out bytes.Buffer
	WriteUsageStatsTable(&out, rows, 0)
	got := out.String()

	if strings.Contains(got, "real-model\ngpt-5") {
		t.Errorf("the model cell was printed raw, so the rest of it reads as a traffic line of its own:\n%s", got)
	}
	if strings.Contains(got, "box.example/v1\nthird") {
		t.Errorf("the backend cell was printed raw, so the rest of it reads as a traffic line of its own:\n%s", got)
	}
	// Still listed, and the real counts are untouched.
	for _, want := range []string{"real-model", "other", "https://box.example/v1"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q vanished from the report instead of being quoted:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "total requests: 3  errors: 0") {
		t.Errorf("the totals are wrong — 2 rows, 3 requests are logged here:\n%s", got)
	}
	// No line may present the forged hosts as its own cells.
	for _, line := range strings.Split(got, "\n") {
		for _, host := range []string{"evil.example", "worse.example"} {
			if i := strings.Index(line, host); i >= 0 && !strings.Contains(line[:i], `"`) {
				t.Errorf("a forged traffic line survived: %q", line)
			}
		}
	}
}

// Control: an ordinary report still prints plainly — the fix cannot be passed
// by quoting every cell.
func TestUsageTableStillPrintsOrdinaryRows(t *testing.T) {
	var out bytes.Buffer
	WriteUsageStatsTable(&out, []UsageStatsRow{
		{Model: "kimi-k2", Backend: "https://box.example/v1", Requests: 4, OK: 3, Errors: 1, CharsSum: 120},
	}, 0)
	got := out.String()
	if !strings.Contains(got, "kimi-k2") || !strings.Contains(got, "https://box.example/v1") {
		t.Fatalf("the row is missing:\n%s", got)
	}
	if strings.Contains(got, `"kimi-k2"`) || strings.Contains(got, `"https://box.example/v1"`) {
		t.Errorf("an ordinary row was quoted:\n%s", got)
	}
	if !strings.Contains(got, "total requests: 4  errors: 1  chars: 120") {
		t.Errorf("the totals are wrong:\n%s", got)
	}
}
