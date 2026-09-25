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
// override (synced entries win by key). Errors reading/parsing either
// source are swallowed — same "degrade to fewer known limits, never crash"
// stance as providerCatalog().
func cloudLimitsFromCatalog() map[string]cloudModelLimit {
	out := map[string]cloudModelLimit{}
	add := func(b []byte) {
		var f cloudLimitsCatalogFile
		if json.Unmarshal(b, &f) != nil {
			return
		}
		for k, v := range f.Limits {
			out[k] = v
		}
	}
	add(cloudLimitsEmbeddedDefault)
	if path, err := cloudLimitsCatalogCachePath(); err == nil {
		if b, err := os.ReadFile(path); err == nil {
			add(b)
		}
	}
	return out
}
