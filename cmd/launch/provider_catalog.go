package launch

// provider_catalog.go — the built-in provider directory (endpoint, wire,
// tool format, plan label) as DATA, not Go code. Adding a provider, a new
// billing plan for an existing one (e.g. z.ai's Coding Plan alongside its
// pay-per-token API), or fixing an endpoint URL is a providers.json edit +
// `oaica provider sync`, never a recompile — same principle as
// model_sync.go's hosted model catalog, applied to the OTHER thing that
// used to live only in Go source (catalogProviders used to be a literal
// slice here; see git history around 2026-09-17 for the before/after).
//
// Two layers, lowest priority first:
//  1. providersEmbeddedDefault (go:embed providers/providers.json) — ships
//     inside the binary so a fresh install works fully offline.
//  2. ~/.oaica/cache/providers/providers.json — pulled by `oaica provider
//     sync` from the hosted URL, same ETag/offline-fallback shape as
//     model_sync.go. Present entries override the embedded default by
//     name; the embedded list still fills in anything sync hasn't fetched
//     (or never will, e.g. an air-gapped host).
//
// A user's own ~/.oaica/remotes.json entry of the same name still wins
// over BOTH layers (loadUserRemotes' existing dedupe) — the catalog only
// ever supplies a default, never overrides a user's explicit config.

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

//go:embed providers/providers.json
var providersEmbeddedDefault []byte

// Named env-var/id constants for the handful of builtins tests and other
// code reference by identifier. These are NOT the provider table — that's
// providers.json — just readable aliases for strings that also happen to
// live there; changing a provider's actual endpoint/wire/label never
// touches this block.
const (
	zaiName           = "zai"
	zaiEnvKey         = "Z_AI_API_KEY"
	openrouterName    = "openrouter"
	openrouterEnvKey  = "OPENROUTER_API_KEY"
	ollamaCloudName   = "ollama-cloud"
	ollamaCloudEnvKey = "OLLAMA_API_KEY"
)

// providerCatalogEntry is providers.json's per-row shape — a strict subset
// of userRemote's fields (only what a provider DEFAULT should carry; things
// like api_key or weight are a user's own choice, never shipped here).
type providerCatalogEntry struct {
	Name       string `json:"name"`
	BaseURL    string `json:"base_url"`
	Version    string `json:"version,omitempty"`
	Wire       string `json:"wire,omitempty"`
	ToolFormat string `json:"tool_format,omitempty"`
	APIKeyEnv  string `json:"api_key_env"`
	// PlanLabel is the picker's billing annotation for this provider's rows
	// (see billingPlanLabel) — e.g. "API Plan (Z.AI — per-token key)". Empty
	// means no label is shown, matching every provider that isn't a known
	// shared/subscription/per-token-key arrangement worth calling out.
	PlanLabel string `json:"plan_label,omitempty"`
	// PlanLabelModelPrefix scopes PlanLabel to model ids with this prefix
	// (after the "<provider>/" split) instead of every model the provider
	// serves — e.g. opencode-go's shared Coding Plan label only applies to
	// its "glm-*" rows, not every model routed through that aggregator.
	// Empty means the label applies to all of the provider's models.
	PlanLabelModelPrefix string `json:"plan_label_model_prefix,omitempty"`
	// KeyURL is where a user without a key yet can get one — surfaced by
	// ensureRemoteAPIKeyForModel's interactive prompt. Optional.
	KeyURL string `json:"key_url,omitempty"`
	Notes  string `json:"notes,omitempty"`
}

type providerCatalogFile struct {
	Version   int                    `json:"version"`
	Providers []providerCatalogEntry `json:"providers"`
}

func providerCatalogCachePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".oaica", "cache", "providers", "providers.json"), nil
}

// providerCatalog returns the merged provider directory: synced entries
// (by name) override the embedded default; anything sync hasn't supplied
// falls back to the shipped default. Errors reading/parsing either source
// are swallowed — a corrupt cache or a bad embed should degrade to "fewer
// providers listed", never crash the picker.
func providerCatalog() []providerCatalogEntry {
	byName := map[string]providerCatalogEntry{}
	order := []string{}

	add := func(entries []providerCatalogEntry) {
		for _, e := range entries {
			e.Name = strings.TrimSpace(e.Name)
			if e.Name == "" || strings.TrimSpace(e.BaseURL) == "" {
				continue
			}
			if _, exists := byName[e.Name]; !exists {
				order = append(order, e.Name)
			}
			byName[e.Name] = e
		}
	}

	add(parseProviderCatalogBytes(providersEmbeddedDefault))
	if path, err := providerCatalogCachePath(); err == nil {
		if b, err := os.ReadFile(path); err == nil {
			add(parseProviderCatalogBytes(b))
		}
	}

	out := make([]providerCatalogEntry, 0, len(order))
	for _, name := range order {
		out = append(out, byName[name])
	}
	return out
}

func parseProviderCatalogBytes(b []byte) []providerCatalogEntry {
	var f providerCatalogFile
	if json.Unmarshal(b, &f) != nil {
		return nil
	}
	return f.Providers
}

// providerCatalogAsUserRemotes converts the merged catalog to userRemote
// values for builtinRemotes() to gate by env-var presence exactly like
// every other builtin.
func providerCatalogAsUserRemotes() []userRemote {
	entries := providerCatalog()
	out := make([]userRemote, 0, len(entries))
	for _, e := range entries {
		out = append(out, userRemote{
			Name:       e.Name,
			BaseURL:    e.BaseURL,
			Version:    e.Version,
			Wire:       e.Wire,
			ToolFormat: e.ToolFormat,
			APIKeyEnv:  e.APIKeyEnv,
		})
	}
	return out
}

// providerPlanLabel looks up the catalog's plan_label for a "<provider>/
// <model>" picker id — the data-driven replacement for billingPlanLabel's
// old hardcoded per-vendor string matches. modelRest is the part after the
// first "/"; entries with a PlanLabelModelPrefix only label rows whose
// model id starts with it (e.g. opencode-go's Coding Plan label is
// "glm-*"-only, not every model that aggregator proxies).
func providerPlanLabel(providerName, modelRest string) string {
	for _, e := range providerCatalog() {
		if e.Name != providerName {
			continue
		}
		if e.PlanLabelModelPrefix != "" && !strings.HasPrefix(modelRest, e.PlanLabelModelPrefix) {
			return ""
		}
		return e.PlanLabel
	}
	return ""
}

// providerKeyURL looks up the catalog's key_url for a provider name, for
// ensureRemoteAPIKeyForModel's "get one at ..." prompt line.
func providerKeyURL(providerName string) string {
	for _, e := range providerCatalog() {
		if e.Name == providerName {
			return e.KeyURL
		}
	}
	return ""
}
