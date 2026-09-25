package launch

// model_sync.go — `oaica model sync`: pull the hosted canonical model
// catalog (same shape as ~/.oaica/models.json) and upsert its entries
// into the local manifest, so a model-config change on our side reaches
// every user with one command and never a reinstall.
//
// Design mirrors oaica_models.go's router cache: GET with If-None-Match,
// 304 falls back to the cached body, offline falls back to the last good
// copy. Merge policy turns on who owns the entry: an entry this sync brought
// in (Source=="sync") is replaced by the catalog, while an entry the user
// owns — hand-added (`oaica model add`) or registered by the local scan — is
// only ever filled in where it is missing, never rewritten (fillFromCatalog).
// `--prune` removes only entries this sync brought in that have since
// disappeared from the catalog; hand-added and scanned entries are never
// touched by automation.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// defaultModelSyncURL is the hosted canonical catalog. Overridable with
// --url; file:// paths are accepted too (tests and air-gapped hosts).
const defaultModelSyncURL = "https://raw.githubusercontent.com/sprapp-com/oaica-code/main/models/models.json"

// modelSyncCache mirrors routerCacheFile's ETag shape for the catalog.
type modelSyncCache struct {
	SavedAt time.Time     `json:"saved_at"`
	URL     string        `json:"url"`
	ETag    string        `json:"etag,omitempty"`
	Catalog modelManifest `json:"catalog"`
}

// usable reports whether this is a copy this sync actually fetched. It is the
// gate for both fallbacks (a 304 body, and a host that cannot be reached).
//
// It deliberately does NOT test Catalog.Version: that field is the catalog
// document's own schema version, and parseModelCatalog documents version 0 as
// tolerated ("hand-rolled catalogs don't need to remember version: 1"). So a
// perfectly good fetched copy of a versionless catalog used to fail both
// fallbacks with "no cache exists" while that copy sat on disk — and, once
// the network hiccuped, a working sync turned into a hard error.
func (c modelSyncCache) usable() bool {
	// A cache file whose URL differs from the requested one is zeroed by the
	// caller, so a non-zero SavedAt means "written by a successful fetch".
	return !c.SavedAt.IsZero()
}

func modelSyncCachePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".oaica", "cache", "models", "sync.json"), nil
}

// ModelSyncReport is what ModelSync returns for the caller to print.
type ModelSyncReport struct {
	URL       string
	Added     []string
	Updated   []string
	Pruned    []string
	Skipped   []string // entries that failed validation (id: reason)
	FromCache bool     // served from the 304/last-good cache, not a fresh 200
}

// fillFromCatalog copies only the fields a user-owned manifest entry is
// missing, never overwriting one it already carries. `oaica model sync` runs
// against a file the user also edits by hand and that `oaica model scan`
// writes to; a field the entry already has is by definition someone's
// decision. ModelPath is the deliberate exception in the other direction: it
// stays untouched, because a path comes from the filesystem (the scan) or
// from the user, never from a remote document.
func fillFromCatalog(dst *ModelManifestEntry, src ModelManifestEntry) {
	if dst.Engine == "" {
		dst.Engine = src.Engine
	}
	if dst.Arch == "" {
		dst.Arch = src.Arch
	}
	if dst.Quant == "" {
		dst.Quant = src.Quant
	}
	if dst.ContextWindow == 0 {
		dst.ContextWindow = src.ContextWindow
	}
	if dst.DefaultMaxOutputTokens == 0 {
		dst.DefaultMaxOutputTokens = src.DefaultMaxOutputTokens
	}
	if dst.GPUMemGB == 0 {
		dst.GPUMemGB = src.GPUMemGB
	}
	if dst.RAMGB == 0 {
		dst.RAMGB = src.RAMGB
	}
	if len(dst.LaunchFlags) == 0 {
		dst.LaunchFlags = src.LaunchFlags
	}
	if dst.Notes == "" {
		dst.Notes = src.Notes
	}
}

// ModelSync fetches the catalog at url (empty = defaultModelSyncURL) and
// upserts it into ~/.oaica/models.json. prune=true drops catalog-sourced
// entries that are no longer in the catalog.
func ModelSync(url string, prune bool) (ModelSyncReport, error) {
	if strings.TrimSpace(url) == "" {
		url = defaultModelSyncURL
	}

	catalog, fromCache, err := fetchModelCatalog(url)
	if err != nil {
		return ModelSyncReport{URL: url}, err
	}

	m, err := loadModelManifest()
	if err != nil {
		return ModelSyncReport{URL: url}, err
	}

	report := ModelSyncReport{URL: url, FromCache: fromCache}
	for _, remote := range catalog.SortedIDs() {
		e := catalog.Models[remote]
		e.ID = remote
		if err := validateModelManifestEntry(e); err != nil {
			report.Skipped = append(report.Skipped, fmt.Sprintf("%s: %v", remote, err))
			continue
		}
		prev, existed := m.Get(remote)
		switch {
		case !existed:
			e.Source = "sync"
			m.Put(e)
			report.Added = append(report.Added, remote)
		case prev.Source != "sync":
			// The entry is the user's: hand-added (no Source) or registered
			// by the local scan. Automation must not rewrite it — the same
			// discipline the scan keeps when two files claim one id. Before
			// this, sync replaced the entry wholesale (`e.Source = "sync"`),
			// which both destroyed the user's model_path/launch_flags and
			// re-labelled the entry as sync-sourced, so the NEXT `--prune`
			// deleted a model the help promises is "never touched by
			// automation". Only fields the entry is missing are filled in.
			merged := prev
			fillFromCatalog(&merged, e)
			m.Put(merged)
			report.Updated = append(report.Updated, remote)
		default:
			e.Source = "sync"
			// Local field notes win unless the catalog ships its own.
			if e.Notes == "" {
				e.Notes = prev.Notes
			}
			m.Put(e)
			report.Updated = append(report.Updated, remote)
		}
	}

	if prune {
		for _, id := range m.SortedIDs() {
			if m.Models[id].Source != "sync" {
				continue
			}
			if _, inCatalog := catalog.Models[id]; inCatalog {
				continue
			}
			m.Remove(id)
			report.Pruned = append(report.Pruned, id)
		}
	}

	if err := m.save(); err != nil {
		return report, err
	}
	return report, nil
}

// fetchModelCatalog GETs the catalog body, honoring If-None-Match via
// the sync cache. file:// URLs bypass HTTP entirely (no ETag).
func fetchModelCatalog(url string) (modelManifest, bool, error) {
	if strings.HasPrefix(url, "file://") {
		path := strings.TrimPrefix(url, "file://")
		data, err := os.ReadFile(path)
		if err != nil {
			return modelManifest{}, false, err
		}
		c, perr := parseModelCatalog(data)
		return c, false, perr
	}

	cachePath, _ := modelSyncCachePath()
	var cached modelSyncCache
	if b, err := os.ReadFile(cachePath); err == nil && json.Unmarshal(b, &cached) == nil && cached.URL == url {
		// valid cache only if it came from the same URL
	} else {
		cached = modelSyncCache{}
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return modelManifest{}, false, err
	}
	if cached.ETag != "" {
		req.Header.Set("If-None-Match", cached.ETag)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		// Offline but we have a last-good copy: use it rather than failing.
		if cached.usable() {
			return cached.Catalog, true, nil
		}
		return modelManifest{}, false, fmt.Errorf("couldn't reach %s: %w", url, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotModified:
		if cached.usable() {
			cached.SavedAt = time.Now()
			if b, jerr := json.Marshal(cached); jerr == nil {
				_ = os.MkdirAll(filepath.Dir(cachePath), 0o700)
				_ = os.WriteFile(cachePath, b, 0o600)
			}
			return cached.Catalog, true, nil
		}
		return modelManifest{}, false, fmt.Errorf("%s returned 304 but no cached copy from that URL is on disk", url)
	case resp.StatusCode != http.StatusOK:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return modelManifest{}, false, fmt.Errorf("%s: HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return modelManifest{}, false, err
	}
	catalog, err := parseModelCatalog(data)
	if err != nil {
		return modelManifest{}, false, err
	}
	etag := resp.Header.Get("ETag")
	if b, jerr := json.Marshal(modelSyncCache{SavedAt: time.Now(), URL: url, ETag: etag, Catalog: catalog}); jerr == nil {
		_ = os.MkdirAll(filepath.Dir(cachePath), 0o700)
		_ = os.WriteFile(cachePath, b, 0o600)
	}
	return catalog, false, nil
}

// parseModelCatalog decodes a catalog body. The hosted file is the same
// modelManifest shape as the local manifest; version 0 (absent) is
// tolerated so hand-rolled catalogs don't need to remember "version": 1.
func parseModelCatalog(data []byte) (modelManifest, error) {
	var c modelManifest
	if err := json.Unmarshal(data, &c); err != nil {
		return modelManifest{}, fmt.Errorf("parse catalog: %w", err)
	}
	if c.Models == nil {
		c.Models = map[string]ModelManifestEntry{}
	}
	return c, nil
}
