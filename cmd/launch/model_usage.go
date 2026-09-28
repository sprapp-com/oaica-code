package launch

// model_usage.go — which models this machine actually runs, for the picker's
// "Frequently used" section.
//
// Machine-local by design (~/.oaica/usage.json), never synced and never
// uploaded: there is no pin list to configure and no fleet state to keep in
// step. Written on a successful launch only, so a failed attempt cannot
// promote a model.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	// frequentMinUses keeps a one-off experiment out of the section.
	frequentMinUses = 2
	// frequentMaxEntries is the section's cap.
	frequentMaxEntries = 8
)

type modelUsage struct {
	Count int    `json:"n"`
	Last  string `json:"last"`
}

type usageFile struct {
	Version int                   `json:"version"`
	Usage   map[string]modelUsage `json:"usage"`
}

const usageVersion = 1

func usageCachePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".oaica", "usage.json"), nil
}

func loadUsage() usageFile {
	path, err := usageCachePath()
	if err != nil {
		return usageFile{Version: usageVersion, Usage: map[string]modelUsage{}}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return usageFile{Version: usageVersion, Usage: map[string]modelUsage{}}
	}
	var f usageFile
	if json.Unmarshal(b, &f) != nil || f.Usage == nil {
		return usageFile{Version: usageVersion, Usage: map[string]modelUsage{}}
	}
	return f
}

// recordModelUse counts one successful launch of a model. Best-effort: a
// machine where ~/.oaica is not writable must still launch.
func recordModelUse(model string) {
	if model == "" {
		return
	}
	path, err := usageCachePath()
	if err != nil {
		return
	}
	f := loadUsage()
	u := f.Usage[model]
	u.Count++
	u.Last = time.Now().UTC().Format(time.RFC3339)
	f.Usage[model] = u
	f.Version = usageVersion

	b, err := json.Marshal(f)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	_ = os.WriteFile(path, b, 0o600)
}

// frequentModels returns up to limit model ids this machine runs, most-used
// first and most-recent as tiebreak. Models used fewer than frequentMinUses
// times are omitted.
func frequentModels(limit int) []string {
	if limit <= 0 {
		limit = frequentMaxEntries
	}
	f := loadUsage()
	type row struct {
		name string
		u    modelUsage
	}
	rows := make([]row, 0, len(f.Usage))
	for name, u := range f.Usage {
		if u.Count < frequentMinUses {
			continue
		}
		rows = append(rows, row{name, u})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].u.Count != rows[j].u.Count {
			return rows[i].u.Count > rows[j].u.Count
		}
		if rows[i].u.Last != rows[j].u.Last {
			return rows[i].u.Last > rows[j].u.Last
		}
		return rows[i].name < rows[j].name
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.name)
	}
	return out
}
