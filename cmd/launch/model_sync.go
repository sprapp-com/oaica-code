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

	"github.com/ollama/ollama/cmd/internal/fileutil"
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
	// caller, so a non-zero SavedAt means "written by a successful fetch" —
	// and a successful fetch of a document that HAS models. The second half
	// matters as much as the first: a cache holding an empty catalog is not a
	// last-good copy, and serving it to an offline `--prune` deleted every
	// synced entry. The router cache in oaica_models.go gates its fallbacks on
	// exactly this pair.
	return !c.SavedAt.IsZero() && len(c.Catalog.Models) > 0
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
	// display is the URL as it may be written down or printed. The request
	// itself must keep the credential (that is how a mirror behind
	// `--url https://KEY@mirror/models.json` authenticates), but the cache
	// file, the manifest's source_url, the report line `oaica model sync`
	// echoes, and every error string are all places the key must not reach —
	// docs/ENTERPRISE.md names the catalog caches explicitly ("A credential
	// must not reach a log, a cache, a support bundle, or a process argument
	// list"), and this is the documented mirror setup.
	display := redactBaseURL(url)
	display = strings.TrimRight(display, "/")

	catalog, fromCache, err := fetchModelCatalog(url, display)
	if err != nil {
		return ModelSyncReport{URL: display}, err
	}

	m, err := loadModelManifest()
	if err != nil {
		return ModelSyncReport{URL: display}, err
	}

	report := ModelSyncReport{URL: display, FromCache: fromCache}
	// Membership is checked by TRIMMED id, because that is what the add loop
	// stores (see the TrimSpace below). Looking up the raw catalog key made
	// --prune delete the very row the same run had just added whenever a
	// catalog key carried whitespace — one command reporting a model as both
	// added and pruned (2026-09-26 audit).
	inCatalog := make(map[string]bool, len(catalog.Models))
	for listed := range catalog.Models {
		if id := strings.TrimSpace(listed); id != "" {
			inCatalog[id] = true
		}
	}
	for _, listed := range catalog.SortedIDs() {
		// The manifest keys on the exact id, and `model add` trims while this
		// loop used to store the catalog key verbatim — so a stray space in a
		// hand-authored catalog produced a second entry for one model, one of
		// which no command could address (rm/show do not trim either).
		id := strings.TrimSpace(listed)
		if id == "" {
			report.Skipped = append(report.Skipped, fmt.Sprintf("%q: empty model id", listed))
			continue
		}
		e := catalog.Models[listed]
		e.ID = id
		if err := validateModelManifestEntry(e); err != nil {
			report.Skipped = append(report.Skipped, fmt.Sprintf("%s: %v", id, err))
			continue
		}
		prev, existed := m.Get(id)
		switch {
		case !existed:
			e.Source = "sync"
			e.SourceURL = display
			m.Put(e)
			report.Added = append(report.Added, id)
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
			report.Updated = append(report.Updated, id)
		default:
			e.Source = "sync"
			e.SourceURL = display
			// Local field notes win unless the catalog ships its own.
			if e.Notes == "" {
				e.Notes = prev.Notes
			}
			// ModelPath is the merge policy's standing exception (see
			// fillFromCatalog): a path comes from the filesystem or the user,
			// never from a remote document. The catalog never carries one, so
			// replacing the entry wholesale silently cleared a path the user
			// had recorded — and every later sync erased it again.
			if e.ModelPath == "" {
				e.ModelPath = prev.ModelPath
			}
			m.Put(e)
			report.Updated = append(report.Updated, id)
		}
	}

	if prune {
		switch {
		case len(catalog.Models) == 0:
			// A document that declares no models is not evidence that every
			// model the user has was withdrawn. The dangerous shape is not
			// hypothetical: any 200 whose body is JSON without a "models"
			// member — a CDN or captive-portal error page, `{}`, or the
			// literal null — parsed as "zero models", so `--prune` deleted
			// every synced entry and reported it as a successful sync.
			report.Skipped = append(report.Skipped, "prune: the fetched catalog declares no models — nothing pruned")
		default:
			for _, id := range m.SortedIDs() {
				entry := m.Models[id]
				if entry.Source != "sync" {
					continue
				}
				// Only entries from THIS catalog are this catalog's to
				// withdraw. An internal mirror synced with --url would
				// otherwise have everything it supplied deleted by the next
				// plain `model sync --prune`, which cannot know the mirror's
				// entries at all.
				if entry.SourceURL != "" && entry.SourceURL != display {
					continue
				}
				if inCatalog[id] {
					continue
				}
				m.Remove(id)
				report.Pruned = append(report.Pruned, id)
			}
		}
	}

	if err := m.save(); err != nil {
		return report, err
	}
	return report, nil
}

// fetchModelCatalog GETs the catalog body, honoring If-None-Match via
// the sync cache. file:// URLs bypass HTTP entirely (no ETag).
func fetchModelCatalog(url, display string) (modelManifest, bool, error) {
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
	if b, err := os.ReadFile(cachePath); err == nil && json.Unmarshal(b, &cached) == nil && cached.URL == display {
		// valid cache only if it came from the same URL
	} else {
		cached = modelSyncCache{}
	}

	// NewRedactedRequest, not http.NewRequest: its error text quotes the URL,
	// and for a credential-carrying --url that is the key in a parse error
	// before any request exists.
	req, err := NewRedactedRequest(http.MethodGet, url, nil)
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
		return modelManifest{}, false, fmt.Errorf("couldn't reach %s: %w", display, redactErr(err))
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotModified:
		if cached.usable() {
			cached.SavedAt = time.Now()
			if b, jerr := json.Marshal(cached); jerr == nil {
				_ = os.MkdirAll(filepath.Dir(cachePath), 0o700)
				_ = fileutil.WriteFileAtomic(cachePath, b, 0o600)
			}
			return cached.Catalog, true, nil
		}
		return modelManifest{}, false, fmt.Errorf("%s returned 304 but no cached copy from that URL is on disk", display)
	case resp.StatusCode != http.StatusOK:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return modelManifest{}, false, fmt.Errorf("%s: HTTP %d: %s", display, resp.StatusCode, strings.TrimSpace(string(body)))
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
	if b, jerr := json.Marshal(modelSyncCache{SavedAt: time.Now(), URL: display, ETag: etag, Catalog: catalog}); jerr == nil {
		_ = os.MkdirAll(filepath.Dir(cachePath), 0o700)
		_ = fileutil.WriteFileAtomic(cachePath, b, 0o600)
	}
	return catalog, false, nil
}

// parseModelCatalog decodes a catalog body. The hosted file is the same
// modelManifest shape as the local manifest; version 0 (absent) is
// tolerated so hand-rolled catalogs don't need to remember "version": 1.
//
// A document with no "models" MEMBER is rejected rather than read as an empty
// catalog. Without that, `{"message":"Not Found"}` from a proxy, `{}`, or the
// literal `null` all decoded cleanly into "this catalog has zero models" — and
// since the prune compares the manifest against exactly that map, one such
// response over HTTP 200 deleted every synced entry in one go.
func parseModelCatalog(data []byte) (modelManifest, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		return modelManifest{}, fmt.Errorf("parse catalog: %w", err)
	}
	if _, ok := members["models"]; !ok {
		return modelManifest{}, fmt.Errorf("parse catalog: the document has no \"models\" member, so it is not a catalog (refusing to read it as an empty one)")
	}
	var c modelManifest
	if err := json.Unmarshal(data, &c); err != nil {
		return modelManifest{}, fmt.Errorf("parse catalog: %w", err)
	}
	if c.Models == nil {
		return modelManifest{}, fmt.Errorf("parse catalog: \"models\" is null, not a model map")
	}
	return c, nil
}
