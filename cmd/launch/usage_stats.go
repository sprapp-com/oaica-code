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
			default:
				aggregateUsageRow(agg, &order, filter, e)
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

// aggregateUsageRow folds one parsed row into the aggregate, reporting whether
// it counted toward any bucket (false when a filter excluded it).
func aggregateUsageRow(agg map[usageKey]*UsageStatsRow, order *[]usageKey, filter UsageStatsFilter, e requestLogEntry) bool {
	if filter.Model != "" && e.Model != filter.Model {
		return false
	}
	ts, terr := time.Parse(time.RFC3339, e.Timestamp)
	if !filter.Since.IsZero() {
		if terr != nil || ts.Before(filter.Since) {
			return false
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
	return true
}
