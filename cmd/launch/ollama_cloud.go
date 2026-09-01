package launch

// ollama_cloud.go — Ollama's public CLOUD catalog (ollama.com/search?c=cloud)
// as extra picker entries. Every "<name>:cloud" id the daemon's own Ollama
// account can serve (gpt-oss:cloud, qwen3.5:cloud, ...) becomes selectable
// alongside OAICA router models, user remotes and local servers.
//
// There is no JSON API for the cloud catalog — the source of truth is the
// search page, which lists one /library/<name> link per cloud model. The
// fetch is therefore a scrape, kept resilient and cached like every other
// inventory source in this package:
//
//   - in-proc memo (process lifetime — a launch runs seconds, one fetch)
//   - disk cache ~/.oaica/cache/models/ollama_cloud.json, 1h fresh / 6h
//     grace (stale copy still paints the menu if ollama.com is down)
//   - 2-minute error cache so a dead network doesn't cost a full HTTP
//     timeout on every launch in a burst
//
// Failure is never fatal: an unreachable ollama.com only costs the Ollama
// entries, the same way one dead user remote costs only its own models.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	ollamaCloudSearchURL = "https://ollama.com/search?c=cloud"
	ollamaCloudCacheTTL  = time.Hour
	ollamaCloudErrorTTL  = 2 * time.Minute
)

// ollamaCloudCache mirrors remoteModelsCacheFile/userRemote cache shape —
// same ~/.oaica/cache/models directory, same atomic-write convention.
type ollamaCloudCache struct {
	SavedAt   time.Time `json:"saved_at"`
	TTLSecond float64   `json:"ttl_seconds"`
	Error     string    `json:"error,omitempty"`
	IDs       []string  `json:"ids,omitempty"`
}

func ollamaCloudCachePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".oaica", "cache", "models", "ollama-cloud.json"), nil
}

// ollamaLibraryHrefRe matches /library/<name> links in the search page.
// Names are restricted to what Ollama itself allows in model names, so the
// regex can't be tricked into matching arbitrary paths.
var ollamaLibraryHrefRe = regexp.MustCompile(`href="/library/([a-z0-9][a-z0-9._-]*)"`)

// ollamaCloudModelIDs fetches the cloud catalog and returns bare model
// names (e.g. "gpt-oss" — callers add the ":cloud" tag). In-proc memoized
// for the process lifetime; disk-cached across launches.
func ollamaCloudModelIDs() []string {
	ids, _ := ollamaCloudModelIDsErr()
	return ids
}

func ollamaCloudModelIDsErr() ([]string, error) {
	path, _ := ollamaCloudCachePath()
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			var c ollamaCloudCache
			if json.Unmarshal(b, &c) == nil {
				age := time.Since(c.SavedAt)
				if c.Error != "" && age < ollamaCloudErrorTTL {
					return nil, fmt.Errorf("%s", c.Error)
				}
				if c.Error == "" && age < ollamaCloudCacheTTL && len(c.IDs) > 0 {
					return c.IDs, nil
				}
				// Stale-but-within-grace copies still serve — a menu hours
				// old beats no menu; refresh happens next launch.
				if c.Error == "" && age < 6*ollamaCloudCacheTTL && len(c.IDs) > 0 {
					return c.IDs, nil
				}
			}
		}
	}

	ids, err := ollamaCloudModelIDsUncached()
	if err != nil && path != "" {
		_ = writeAtomic(path, mustMarshal(ollamaCloudCache{SavedAt: time.Now(), TTLSecond: ollamaCloudErrorTTL.Seconds(), Error: err.Error()}))
		return nil, err
	}
	if path != "" {
		_ = writeAtomic(path, mustMarshal(ollamaCloudCache{SavedAt: time.Now(), TTLSecond: ollamaCloudCacheTTL.Seconds(), IDs: ids}))
	}
	return ids, nil
}

func ollamaCloudModelIDsUncached() ([]string, error) {
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Get(ollamaCloudSearchURL)
	if err != nil {
		return nil, fmt.Errorf("couldn't reach ollama.com: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from ollama.com/search", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("reading ollama.com/search: %w", err)
	}
	seen := map[string]bool{}
	var ids []string
	for _, m := range ollamaLibraryHrefRe.FindAllStringSubmatch(string(body), -1) {
		name := m[1]
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		ids = append(ids, name)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("no /library/ links found on %s (page layout changed?)", ollamaCloudSearchURL)
	}
	return ids, nil
}

// ollamaCloudEntries converts the fetched catalog into picker entries.
// Skipped entirely when OAICA_HOST is pinned (same rule as :local entries —
// the user asked for ONE host, don't mix catalogs in).
// ollamaCloudEntriesFn is var-indirected for tests (set to nil to skip the
// catalog fetch entirely — no cache write, no HTTP).
var ollamaCloudEntriesFn = ollamaCloudEntries

func ollamaCloudEntries() []oaicaModelEntry {
	if strings.TrimSpace(os.Getenv("OAICA_HOST")) != "" {
		return nil
	}
	ids := ollamaCloudModelIDs()
	entries := make([]oaicaModelEntry, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, oaicaModelEntry{
			ID:          ollamaCloudPickerPrefix + id,
			Upstream:    id + ":cloud",
			Description: "Ollama cloud — served via the Ollama daemon's own account",
		})
	}
	return entries
}

// ollamaCloudPickerPrefix names ollama-cloud catalog models in the picker:
// the provider sits IN FRONT ("ollama/gpt-oss") instead of a ":cloud" tag
// hanging off the back, so non-OAICA entries read "<provider>/<model>" like
// user remotes do. ollamaCloudUpstreamFor maps such a display id back to
// the daemon's tagged name; "" for anything else.
const ollamaCloudPickerPrefix = "ollama/"

func ollamaCloudUpstreamFor(displayID string) string {
	rest, ok := strings.CutPrefix(displayID, ollamaCloudPickerPrefix)
	if !ok || rest == "" || strings.Contains(rest, "/") {
		return ""
	}
	return rest + ":cloud"
}

// mustMarshal is json.Marshal with the error collapsed — cache writes are
// best-effort throughout this package.
func mustMarshal(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
