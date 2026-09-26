package launch

// usage_stats.go — reads ~/.oaica/requests.log (request_log.go) and
// aggregates it for `oaica usage`. No token counts exist in the log (see
// request_log.go's doc comment — it was scoped for flashplan classifier
// tuning, not billing), so this reports request counts, status/error
// counts, and message char-length sums per (model, backend) pair — the
// same shape support has manually grepped/python'd out of the log by hand
// more than once; this is that script promoted to a real command.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// usageKey is one (model, backend) aggregate bucket.
type usageKey struct{ model, backend string }

// UsageStatsRow is one (model, backend) aggregate.
type UsageStatsRow struct {
	Model    string
	Backend  string
	Requests int
	OK       int
	Errors   int
	CharsSum int64
	LastSeen time.Time
}

// UsageStatsFilter narrows which log rows count toward the aggregate. Zero
// values mean "no filter" for that dimension.
type UsageStatsFilter struct {
	Since time.Time // zero = all time
	Model string    // exact match, empty = any
}

// UsageSinceCutoff turns the `oaica usage --since` value into the cutoff
// instant, measured back from now. Empty means "all time" (the zero time).
//
// time.ParseDuration accepts a leading sign, and the cutoff is now MINUS the
// duration: `--since -24h` put the cutoff 24 hours in the FUTURE, so every row
// was filtered out and `oaica usage` printed an empty report with no error —
// from a cron job or a health check, indistinguishable from a machine that
// sent no traffic (2026-09-26 audit, fourth round). A zero duration is the
// same silent-empty answer by a shorter route, so both are refused.
func UsageSinceCutoff(sinceStr string, now time.Time) (time.Time, error) {
	if strings.TrimSpace(sinceStr) == "" {
		return time.Time{}, nil
	}
	d, err := time.ParseDuration(sinceStr)
	if err != nil {
		return time.Time{}, fmt.Errorf("--since %q: %w (examples: 1h, 30m, 24h)", sinceStr, err)
	}
	if d <= 0 {
		where := "in the future"
		if d == 0 {
			where = "at the current instant"
		}
		return time.Time{}, fmt.Errorf("--since %q: the duration must be positive (examples: 1h, 30m, 24h) — it is subtracted from the current time, so this puts the cutoff %s: every row is excluded and `oaica usage` reports an empty history as if the machine had sent no traffic", sinceStr, where)
	}
	return now.Add(-d), nil
}

// LoadUsageStats reads and aggregates requestLogPath() (best-effort: a
// missing file returns an empty result, not an error — a fresh install has
// no log yet). Malformed lines are skipped rather than aborting the whole
// read, matching appendRequestLog's own best-effort logging.
//
// Never returns a nil slice: `oaica usage --json` on a machine with no traffic
// printed `null` instead of `[]`, which breaks any consumer that iterates the
// documented row list (2026-09-26 audit, third round).
func LoadUsageStats(filter UsageStatsFilter) ([]UsageStatsRow, error) {
	rows, _, err := LoadUsageStatsCountingUnreadable(filter)
	return rows, err
}

// LoadUsageStatsCountingUnreadable is LoadUsageStats plus the number of log
// lines it could not read. Callers that print a total should surface that
// count — a report the user reads as authoritative must not silently drop
// rows (2026-09-26 audit, third round).
func LoadUsageStatsCountingUnreadable(filter UsageStatsFilter) ([]UsageStatsRow, int, error) {
	rows := []UsageStatsRow{}
	path, err := requestLogPath()
	if err != nil {
		return rows, 0, err
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return rows, 0, nil
		}
		return rows, 0, err
	}
	defer f.Close()

	agg := map[usageKey]*UsageStatsRow{}
	order := []usageKey{}
	unreadable := 0

	// bufio.Scanner aborted the whole read on one over-long line
	// ("token too long"), permanently bricking `oaica usage` on an
	// append-only log that is never rotated. A line we cannot read is one
	// row lost, not a reason to report zero rows for the whole file
	// (2026-09-26 audit, third round).
	r := bufio.NewReaderSize(f, 64*1024)
	for {
		line, truncated, rerr := readLogLine(r)
		if len(line) > 0 {
			var e requestLogEntry
			switch {
			case truncated:
				unreadable++
			case json.Unmarshal(line, &e) != nil:
				unreadable++
			case isEmptyLogRow(e):
				// `{}` and `null` parse into a zero row without error, and a
				// zero row counted as one request under an empty model and an
				// empty backend — a phantom ERROR line in the report, invented
				// by whatever wrote an empty line (2026-09-26 audit, ninth
				// round). A line with no row in it is unreadable, not a turn.
				unreadable++
			default:
				if aggregateUsageRow(agg, &order, filter, e) == rowUnreadable {
					unreadable++
				}
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			return rows, unreadable, rerr
		}
	}

	rows = make([]UsageStatsRow, 0, len(order))
	for _, k := range order {
		rows = append(rows, *agg[k])
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Requests > rows[j].Requests })
	return rows, unreadable, nil
}

// WriteUsageStatsJSON writes rows to w as the JSON array `oaica usage --json`
// documents, and — when unreadable > 0 — a warning to warn. Both, because a
// consumer of that JSON gets a total lower than the log it summarizes and has
// no other way to learn that lines were dropped; the human path warns, and
// this is the same warning for the machine path. It goes to a second stream,
// never into the array: stdout has to stay exactly the row list, or the fix
// breaks every existing consumer (2026-09-26 audit).
func WriteUsageStatsJSON(w, warn io.Writer, rows []UsageStatsRow, unreadable int) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rows); err != nil {
		return err
	}
	if unreadable > 0 && warn != nil {
		fmt.Fprintf(warn, "warning: %d log line(s) could not be read and are NOT included in this report (over-long or corrupt entries).\n", unreadable)
	}
	return nil
}

// PrintableCell renders one store-held value for a listing row: an ordinary
// value as-is, one carrying a control character in Go-quoted form, and any
// byte sequence that is not valid UTF-8 sanitised first. It is the exported
// form of manifestCell (model_manifest_cli.go), i.e. the same rule
// printableName (remote_cli.go) applies to a remote name.
//
// Exported because package cmd prints the same kind of value in its own
// listings — alias names and targets, plan names/models/descriptions, config
// model ids, and the router registry's name/upstream model — and had no such
// call at all: `oaica model alias list` printed a stored name carrying a
// newline as a fabricated extra row naming a model the file never held
// (2026-09-26 audit, eleventh round). The rule lives in ONE place so the two
// packages cannot drift apart on what "safe to print" means.
func PrintableCell(s string) string { return manifestCell(s) }

// WriteUsageStatsTable writes the human table `oaica usage` prints. It lives
// here, beside the reader, rather than in the command's RunE, because the model
// and backend cells are printed rows of a report nothing else re-checks: the
// model is the CLIENT's request body (request_log.go), and the backend is a
// base URL from the user's own remotes.json (redactBaseURL). Either can carry a
// newline or a control character, and a raw one forges an extra row — a
// traffic line the log never recorded, complete with request counts — in the
// command a user runs to find out what their traffic actually was. Both cells
// are printed through printableName, the same quoting `remote list` and
// `doctor` use.
func WriteUsageStatsTable(w io.Writer, rows []UsageStatsRow, unreadable int) {
	var totalReqs, totalErrs int
	var totalChars int64
	fmt.Fprintf(w, "%-28s %-45s %8s %8s %6s %14s\n", "MODEL", "BACKEND", "REQS", "OK", "ERR", "CHARS")
	for _, r := range rows {
		fmt.Fprintf(w, "%-28s %-45s %8d %8d %6d %14d\n", printableName(r.Model), printableName(r.Backend), r.Requests, r.OK, r.Errors, r.CharsSum)
		totalReqs += r.Requests
		totalErrs += r.Errors
		totalChars += r.CharsSum
	}
	fmt.Fprintf(w, "\ntotal requests: %d  errors: %d  chars: %d\n", totalReqs, totalErrs, totalChars)
	// A dropped row must not be silent: this report is read as authoritative
	// over an append-only log nothing rotates (2026-09-26 audit, third round).
	if unreadable > 0 {
		fmt.Fprintf(w, "warning: %d log line(s) could not be read and are NOT counted above (over-long or corrupt entries).\n", unreadable)
	}
	fmt.Fprintln(w, "(request counts and message char-length only — no token counts locally; real token/$ cost lives on the gateway's usage ledger)")
}

// maxLogLineBytes caps how much of one line is held in memory. Beyond it the
// line is truncated and counted as unreadable — a corrupt write cannot make
// this read unbounded.
const maxLogLineBytes = 1 << 20

// readLogLine returns the next line without its terminator, whether it was
// truncated at maxLogLineBytes, and the read error that ended it (io.EOF at
// end of file).
func readLogLine(r *bufio.Reader) (line []byte, truncated bool, err error) {
	for {
		chunk, rerr := r.ReadSlice('\n')
		if rerr == nil {
			// The terminator is not part of the line: ReadSlice hands it back
			// with the data and it is stripped below. Counting it as content
			// made a line of exactly maxLogLineBytes bytes look one byte too
			// long, so the reader reported a truncated row for a log with
			// nothing wrong with it — the one byte "dropped" was the newline
			// nobody keeps (2026-09-26 audit).
			chunk = bytes.TrimSuffix(chunk, []byte("\n"))
			chunk = bytes.TrimSuffix(chunk, []byte("\r"))
		}
		if len(line) < maxLogLineBytes {
			room := maxLogLineBytes - len(line)
			if len(chunk) > room {
				line = append(line, chunk[:room]...)
				truncated = true
			} else {
				line = append(line, chunk...)
			}
		} else if len(chunk) > 0 {
			truncated = true
		}
		if rerr == nil {
			break
		}
		if rerr == bufio.ErrBufferFull {
			continue
		}
		line = bytes.TrimRight(line, "\r\n")
		return line, truncated, rerr
	}
	return bytes.TrimRight(line, "\r\n"), truncated, nil
}

// isEmptyLogRow reports whether a parsed line carries no row at all. Only `{}`,
// `null` and whitespace parse into that shape — every row this package writes
// has at least a timestamp and a backend.
func isEmptyLogRow(e requestLogEntry) bool {
	return e.Timestamp == "" && e.Model == "" && e.Path == "" && e.Backend == ""
}

// rowFold is what folding one parsed row into the aggregate did with it.
type rowFold int

const (
	rowCounted    rowFold = iota // folded into a bucket
	rowFiltered                  // excluded by the filter, as the user asked
	rowUnreadable                // excluded because its own timestamp cannot be read
)

// aggregateUsageRow folds one parsed row into the aggregate and reports what it
// did with it.
//
// A row whose timestamp cannot be PARSED is reported as unreadable when a
// --since filter needs it: it used to return the same "filtered" signal as a
// row legitimately outside the window, so a log holding rows with corrupt (or
// absent, as in a version-skewed writer) timestamps produced a report that
// silently omitted them with no `unreadable` warning — the one place the report
// promises to say that rows are missing (2026-09-26 audit, ninth round).
// Without --since the timestamp is not needed to count the row, so it is still
// counted.
func aggregateUsageRow(agg map[usageKey]*UsageStatsRow, order *[]usageKey, filter UsageStatsFilter, e requestLogEntry) rowFold {
	if filter.Model != "" && e.Model != filter.Model {
		return rowFiltered
	}
	ts, terr := time.Parse(time.RFC3339, e.Timestamp)
	if !filter.Since.IsZero() {
		if terr != nil {
			return rowUnreadable
		}
		if ts.Before(filter.Since) {
			return rowFiltered
		}
	}
	k := usageKey{e.Model, e.Backend}
	row, ok := agg[k]
	if !ok {
		row = &UsageStatsRow{Model: e.Model, Backend: e.Backend}
		agg[k] = row
		*order = append(*order, k)
	}
	row.Requests++
	if e.StatusCode == 200 {
		row.OK++
	} else {
		row.Errors++
	}
	row.CharsSum += int64(e.TotalMessagesLen)
	if terr == nil && ts.After(row.LastSeen) {
		row.LastSeen = ts
	}
	return rowCounted
}
