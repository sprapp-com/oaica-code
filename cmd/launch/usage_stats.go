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
	"encoding/json"
	"os"
	"sort"
	"time"
)

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

// LoadUsageStats reads and aggregates requestLogPath() (best-effort: a
// missing file returns an empty result, not an error — a fresh install has
// no log yet). Malformed lines are skipped rather than aborting the whole
// read, matching appendRequestLog's own best-effort logging.
func LoadUsageStats(filter UsageStatsFilter) ([]UsageStatsRow, error) {
	path, err := requestLogPath()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	type key struct{ model, backend string }
	agg := map[key]*UsageStatsRow{}
	order := []key{}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var e requestLogEntry
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if filter.Model != "" && e.Model != filter.Model {
			continue
		}
		ts, terr := time.Parse(time.RFC3339, e.Timestamp)
		if !filter.Since.IsZero() {
			if terr != nil || ts.Before(filter.Since) {
				continue
			}
		}
		k := key{e.Model, e.Backend}
		row, ok := agg[k]
		if !ok {
			row = &UsageStatsRow{Model: e.Model, Backend: e.Backend}
			agg[k] = row
			order = append(order, k)
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
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	rows := make([]UsageStatsRow, 0, len(order))
	for _, k := range order {
		rows = append(rows, *agg[k])
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Requests > rows[j].Requests })
	return rows, nil
}
