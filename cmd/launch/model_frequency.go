package launch

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/ollama/ollama/cmd/internal/fileutil"
)

// maxModelPickEntries bounds the counts file, which had nothing pruning it:
// every model name ever launched stayed, for the life of the installation. The
// picker reads the top 5 by count (models.go) and never needs a count to be
// exact, so the bound costs nothing but the caveat in boundModelPickCounts.
const maxModelPickEntries = 500

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

// loadModelPickCounts is the READ side: a missing or unusable file is no counts
// at all, which costs the picker its "frequently used" section and nothing
// else. Only the writer quarantines; see modelPickCountsForWrite.
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

// modelPickCountsForWrite reads the counts file for a writer that holds the
// store's lock. ok is false when the caller must NOT write: the file is there
// and could not be read (permissions, a device error), and publishing a fresh
// map over bytes nothing could read would be guessing at a file that might be a
// mount the user cares about — the distinction loadAuthStore draws.
//
// A file that is readable but not this shape is moved aside instead, and the
// user is told. json.Unmarshal into map[string]int rejects any shape this
// version does not recognise — a newer file, a hand-edit, a truncated write —
// and the caller used to start from an empty map and publish that: ONE pick
// replaced every model's history, with no notice and no copy. The bytes are all
// there, so keeping them under a name that says what they are costs nothing,
// and refusing instead would mean one stray byte made every launch's pick fail
// for ever while the counts it held stayed unreadable anyway (2026-09-26 audit,
// eleventh round).
func modelPickCountsForWrite(path string) (map[string]int, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]int{}, true
		}
		fmt.Fprintf(os.Stderr, "warning: %s could not be read (%v) — leaving it alone rather than replacing it with this pick.\n", path, err)
		return nil, false
	}
	counts := map[string]int{}
	if err := json.Unmarshal(b, &counts); err != nil {
		moved, qerr := quarantineModelPicks(path)
		if qerr != nil {
			fmt.Fprintf(os.Stderr, "warning: %s is not model pick counts (%v), and it could not be moved aside either: %v — leaving it alone rather than replacing it.\n", path, err, qerr)
			return nil, false
		}
		fmt.Fprintf(os.Stderr, "warning: %s is not model pick counts (%v).\n", path, err)
		fmt.Fprintf(os.Stderr, "warning: it has been kept as %s and a new one written. The picks it held are in that file, which nothing has deleted.\n", moved)
		return map[string]int{}, true
	}
	if counts == nil {
		// A file whose whole content is `null` decodes to a nil map, and
		// incrementing a nil map panics.
		counts = map[string]int{}
	}
	return counts, true
}

// recordModelPick increments the pick count for a chosen model name. Best
// effort: a failure here must never block launch — it only affects future
// picker ordering, not the launch that's in progress.
//
// The lock covers the whole load-mutate-save (fileutil.WithFileLock, the same
// helper every ~/.oaica store uses). The picker records a pick on every `oaica
// launch`, so two launches that overlap read the same snapshot, each increments
// its own model and writes the whole map back — the rename that landed last
// decided which picks survived, and the "frequently used" section was ordered
// from a history that quietly loses entries (2026-09-26 audit, eleventh round).
func recordModelPick(name string) {
	if name == "" {
		return
	}
	path, err := modelPickHistoryPath()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	_ = fileutil.WithFileLock(path, func() error {
		counts, ok := modelPickCountsForWrite(path)
		if !ok {
			return nil
		}
		counts[name]++
		b, err := json.Marshal(boundModelPickCounts(counts, name))
		if err != nil {
			return nil
		}
		// Unique temp + rename (2026-09-26 audit) — same reason as the picker
		// cache: one fixed temp name is a shared buffer between processes.
		return fileutil.WriteFileAtomic(path, b, 0o600)
	})
}

// quarantineModelPicks moves an unparseable counts file aside and returns where
// it went, claiming a free name first so two quarantines cannot land on one
// destination and destroy the earlier one's bytes (quarantineAuthStore,
// auth_store.go, for the same promise). Its caller holds the store's lock, so
// no other oaica is claiming a name in this directory at the same time.
func quarantineModelPicks(path string) (string, error) {
	base := fmt.Sprintf("%s.unreadable-%s", path, quarantineStamp())
	dest := base
	for n := 2; ; n++ {
		_, err := os.Lstat(dest)
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err != nil {
			return "", err
		}
		dest = fmt.Sprintf("%s-%d", base, n)
	}
	if err := os.Rename(path, dest); err != nil {
		return "", err
	}
	return dest, nil
}

// boundModelPickCounts trims the counts to maxModelPickEntries: the name just
// picked, plus the most-picked others, ties by name so the result is stable.
//
// The pick being recorded is never the one dropped — without that, a model
// outside the bound could be pruned on every record and never accumulate a
// count at all, which is the ordering this file exists to provide.
//
// The caveat, stated rather than hidden: this format has no timestamps, so once
// the map is over the bound a model that is not among the most-picked can lose
// its count and start again from one. That is the same order of consequence as
// the rest of this file — a pinned section in a picker, not a record — and it
// is what a bound costs short of inventing a format this launcher would then
// have to migrate.
func boundModelPickCounts(counts map[string]int, current string) map[string]int {
	if len(counts) <= maxModelPickEntries {
		return counts
	}
	names := make([]string, 0, len(counts))
	for name := range counts {
		if name != current {
			names = append(names, name)
		}
	}
	sort.Slice(names, func(i, j int) bool {
		if counts[names[i]] != counts[names[j]] {
			return counts[names[i]] > counts[names[j]]
		}
		return names[i] < names[j]
	})
	out := make(map[string]int, maxModelPickEntries)
	for _, name := range names {
		if len(out) >= maxModelPickEntries-1 {
			break
		}
		out[name] = counts[name]
	}
	out[current] = counts[current]
	return out
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
