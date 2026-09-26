package launch

// cloud_limits_catalog.go — context/output token limits for Ollama ":cloud"
// alias base names (kimi-k2.6, glm-5.1, qwen3.5, ...) as DATA, not a Go
// literal. Same two-layer shape as provider_catalog.go: an embedded default
// (works offline, ships with the binary) overridden by whatever `oaica
// model cloud-limits sync` has pulled into
// ~/.oaica/cache/cloud_limits/cloud_limits.json. Correcting or adding an
// alias's limits is a cloud_limits.json edit + sync, never a recompile.
//
// This only covers Ollama's OWN upstream ":cloud" catalog — a user's local
// Ollama daemon exposing e.g. "kimi-k2.6:cloud". It has nothing to do with
// OAICA's own self-hosted models (those specify context size via `oaica
// model add --context-window` or the synced models/models.json catalog,
// model_manifest.go/model_sync.go) — different domain, same principle.
//
// A genuinely live number always wins over both layers here: launch.go's
// setDynamicCloudModelLimits sets per-launch limits straight from the OAICA
// router's own /v1/models response (lookupCloudModelLimit checks that
// first). This catalog is the fallback for names the live router doesn't
// know about — Ollama's own aliases running on someone's local daemon,
// which the router never sees.

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
)

//go:embed cloud_limits/cloud_limits.json
var cloudLimitsEmbeddedDefault []byte

type cloudLimitsCatalogFile struct {
	Version int                        `json:"version"`
	Limits  map[string]cloudModelLimit `json:"limits"`
}

func cloudLimitsCatalogCachePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".oaica", "cache", "cloud_limits", "cloud_limits.json"), nil
}

// cloudLimitsFromCatalog merges the embedded default with the synced
// override. Errors reading/parsing either source are swallowed — same
// "degrade to fewer known limits, never crash" stance as providerCatalog().
//
// The merge follows provider_catalog.go's two rules, which this catalog was
// missing (2026-09-26 audit):
//
//   - a synced row ADDS aliases the embedded default does not list, at any
//     version — that is what sync is for;
//   - a synced row REPLACES a value the embedded default already states only
//     when the synced document bumps its own version above the embedded one.
//     A synced file is of unknown age; without the gate a cache fetched when a
//     window was wrong outranked the binary forever, and nothing could correct
//     it (provider_catalog.go: "treating it as newer than the binary is how
//     both a dropped field and a stale endpoint reached real hosts").
//
// Zero means "states nothing" throughout these catalogs (see
// mergeDeclaredModelLimits), and here it is also what keeps a row out of the
// table: a limit with no context is not a limit, and its Context was written
// verbatim into CLAUDE_CODE_MAX_CONTEXT_TOKENS / AUTO_COMPACT_WINDOW /
// codex's context_window — where 0 turns auto-compact off instead of on.
func cloudLimitsFromCatalog() map[string]cloudModelLimit {
	embedded, embeddedVersion := parseCloudLimitsCatalog(cloudLimitsEmbeddedDefault)
	if embedded == nil {
		embedded = map[string]cloudModelLimit{}
	}
	merged := make(map[string]cloudModelLimit, len(embedded))
	for k, v := range embedded {
		merged[k] = v
	}

	var synced map[string]cloudModelLimit
	syncedVersion := 0
	if path, err := cloudLimitsCatalogCachePath(); err == nil {
		if b, err := os.ReadFile(path); err == nil {
			synced, syncedVersion = parseCloudLimitsCatalog(b)
		}
	}
	newer := syncedVersion > embeddedVersion
	for name, l := range synced {
		if l.Context <= 0 || l.Output <= 0 {
			// Zero states nothing: keep whatever the embedded row said, and if
			// there is no embedded row this alias simply has no known window.
			continue
		}
		if prev, ok := merged[name]; ok && !newer {
			// A same-or-older document may only fill in a field the embedded
			// row left unstated, never overrule one it carries.
			if prev.Context <= 0 {
				prev.Context = l.Context
			}
			if prev.Output <= 0 {
				prev.Output = l.Output
			}
			merged[name] = prev
			continue
		}
		merged[name] = l
	}

	// A row that states no usable window is dropped rather than kept as a
	// zero-valued limit any caller would act on.
	for name, l := range merged {
		if l.Context <= 0 || l.Output <= 0 {
			delete(merged, name)
		}
	}
	return merged
}

// parseCloudLimitsCatalog parses a catalog document, returning its rows and
// its declared version. A document that does not parse yields a nil map and
// version 0 — the caller keeps the other side rather than losing everything.
func parseCloudLimitsCatalog(b []byte) (map[string]cloudModelLimit, int) {
	var f cloudLimitsCatalogFile
	if json.Unmarshal(b, &f) != nil {
		return nil, 0
	}
	return f.Limits, f.Version
}
