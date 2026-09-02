package launch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

// modelPickHistoryPath lives under ~/.oaica next to plans.json/remotes.json/
// picker_cache.json — same directory convention as the rest of the picker's
// local state.
func modelPickHistoryPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".oaica", "model_picks.json"), nil
}

func loadModelPickCounts() map[string]int {
	path, err := modelPickHistoryPath()
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var counts map[string]int
	if json.Unmarshal(b, &counts) != nil {
		return nil
	}
	return counts
}

// recordModelPick increments the pick count for a chosen model name. Best
// effort: a failure here must never block launch — it only affects future
// picker ordering, not the launch that's in progress.
func recordModelPick(name string) {
	if name == "" {
		return
	}
	path, err := modelPickHistoryPath()
	if err != nil {
		return
	}
	counts := loadModelPickCounts()
	if counts == nil {
		counts = make(map[string]int)
	}
	counts[name]++
	b, err := json.Marshal(counts)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// topFrequentModels returns up to n model names ordered by pick count
// descending, ties broken by name for stable output. Used by the picker to
// pin a user's most-used models to the top, above the OAICA recommendation
// section.
func topFrequentModels(n int) []string {
	counts := loadModelPickCounts()
	if len(counts) == 0 || n <= 0 {
		return nil
	}
	type entry struct {
		name  string
		count int
	}
	entries := make([]entry, 0, len(counts))
	for name, count := range counts {
		if count > 0 {
			entries = append(entries, entry{name, count})
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].count != entries[j].count {
			return entries[i].count > entries[j].count
		}
		return entries[i].name < entries[j].name
	})
	if len(entries) > n {
		entries = entries[:n]
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.name
	}
	return names
}
