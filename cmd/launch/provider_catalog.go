package launch

// provider_catalog.go — the built-in provider directory (endpoint, wire,
// tool format, plan label) as DATA, not Go code. Adding a provider, a new
// billing plan for an existing one (e.g. z.ai's Coding Plan alongside its
// pay-per-token API), or fixing an endpoint URL is a providers.json edit +
// `oaica remote sync`, never a recompile — same principle as
// model_sync.go's hosted model catalog, applied to the OTHER thing that
// used to live only in Go source (catalogProviders used to be a literal
// slice here; see git history around 2026-09-17 for the before/after).
//
// Two layers, lowest priority first:
//  1. providersEmbeddedDefault (go:embed providers/providers.json) — ships
//     inside the binary so a fresh install works fully offline.
//  2. ~/.oaica/cache/providers/providers.json — pulled by `oaica remote
//     sync` from the hosted URL, same ETag/offline-fallback shape as
//     model_sync.go. The embedded list still fills in anything sync hasn't
//     fetched (or never will, e.g. an air-gapped host).
//
// The layers MERGE PER FIELD, and the synced copy is additive: it supplies
// providers, models and fields the embedded default does not have, and cannot
// replace a value the embedded row already carries unless the fetched document
// bumps its own "version" above the embedded one. See providerCatalog for why
// (the short version: a synced file is of unknown age, and treating it as
// newer than the binary is how both a dropped field and a stale endpoint
// reached real hosts, 2026-09-26).
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
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	Version string `json:"version,omitempty"`
	// ModelsPath overrides the model-list path for a vendor whose list is
	// versioned differently from its chat endpoint (perplexity: chat at
	// "/chat/completions", list at "/v1/models"). See userRemote.modelsURL.
	ModelsPath string `json:"models_path,omitempty"`
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
	// AuthVia names an external credential store whose login for THIS
	// provider id may be reused instead of asking for a key again
	// (auth_external.go) — "opencode" today. Opt-in per row, deliberately:
	// two tools naming a provider the same thing is a coincidence, not
	// evidence that one's stored key belongs to the other, so a row that says
	// nothing here can never source a credential from another tool's disk.
	AuthVia string `json:"auth_via,omitempty"`
	// Models declares this provider's model ids and their windows, keyed by
	// the bare id the provider expects upstream. It exists for providers
	// whose /v1/models sweep cannot answer: the Anthropic-compatible
	// subscription endpoints (z.ai's Coding Plan, MiniMax's) serve no model
	// list at all, and a key-gated sweep can be refused, so without this a
	// billed plan's rows would never reach the picker. Providers that do
	// answer /v1/models need nothing here — the sweep still supplies them,
	// and its ids win when both know a model.
	Models map[string]providerCatalogModelLimit `json:"models,omitempty"`
	Notes  string                               `json:"notes,omitempty"`
}

// EndpointBase resolves this row the same way a userRemote's does, so a
// message that names a provider's endpoint cannot disagree with the request
// oaica sends. BaseURL alone is a prefix on any row with a version segment.
func (e providerCatalogEntry) EndpointBase() string {
	return userRemote{BaseURL: e.BaseURL, Version: e.Version}.openAIBase()
}

// providerCatalogModelLimit is one declared model's windows. Tool calling is
// not a field because every entry currently shipped supports it (models.dev
// agrees); add one here rather than assuming if that ever stops being true.
type providerCatalogModelLimit struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}

type providerCatalogFile struct {
	// Version is the document's own revision, and it is load-bearing: a synced
	// (cached) copy may REDEFINE a shipped row — rather than only add to it —
	// exactly when its Version is strictly greater than the embedded file's.
	// Bump it on main when a change must reach binaries that are already
	// installed (a corrected endpoint, a moved path); new providers, new model
	// rows and newly added fields need no bump, because those merge additively
	// whatever this number says. Leaving it alone can never make a host worse
	// off: the failure mode of a stale cache is under-application, never a
	// clobbered shipped row.
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

// providerCatalog returns the merged provider directory. The embedded default
// is the shipped contract; the synced copy (fetched from main by
// `oaica remote sync`) may ADD to it — new providers, new model rows, fields
// the embedded row leaves empty — but may not silently REDEFINE it.
//
// That asymmetry is deliberate, and it is the whole of this function's
// contract. A synced copy is an untimestamped snapshot: nothing in it says
// whether it was fetched before or after the binary that is reading it, so a
// row replaced wholesale by it was a defect in both directions. A copy synced
// before a field existed blanked that field's value (a synced zhipu row lost
// its version and resolved to ".../paas/v4/v1/..." — a 404 on every request,
// with the picker cheerfully listing the models); a copy that predates an
// endpoint fix kept pointing at the old one. Both are the same bug: a document
// of unknown age was allowed to overwrite a known one.
//
// The one way to redefine a shipped row through sync is for the fetched
// document to declare a strictly greater providerCatalogFile.Version than the
// embedded one — an explicit "this is newer than any binary" statement, made
// on purpose by whoever edits providers.json on main. Absent that bump, the
// safe reading holds: additions land, corrections wait for a release.
//
// Errors reading/parsing either source are swallowed — a corrupt cache or a
// bad embed should degrade to "fewer providers listed", never crash the picker.
func providerCatalog() []providerCatalogEntry {
	byName := map[string]providerCatalogEntry{}
	order := []string{}

	add := func(e providerCatalogEntry) bool {
		e.Name = strings.TrimSpace(e.Name)
		if e.Name == "" || strings.TrimSpace(e.BaseURL) == "" {
			return false
		}
		if _, exists := byName[e.Name]; !exists {
			order = append(order, e.Name)
		}
		byName[e.Name] = e
		return true
	}

	embedded, embeddedVersion := parseProviderCatalogFileVersioned(providersEmbeddedDefault)
	for _, e := range embedded {
		add(e)
	}

	if path, err := providerCatalogCachePath(); err == nil {
		if b, err := os.ReadFile(path); err == nil {
			synced, syncedVersion := parseProviderCatalogFileVersioned(b)
			redefines := syncedVersion > embeddedVersion
			for _, e := range synced {
				prev, exists := byName[strings.TrimSpace(e.Name)]
				if !exists {
					add(e)
					continue
				}
				if !redefines {
					// Additive only: fill the embedded row's gaps.
					merged := prev
					fillEmptyProviderFields(&merged, e)
					// The declared windows come from the synced document where
					// it states them, in this direction too — a measurement,
					// not a preference (see mergeDeclaredModelLimits).
					merged.Models = mergeDeclaredModelLimits(prev.Models, e.Models)
					byName[merged.Name] = merged
					continue
				}
				// The fetched document declares itself newer than this
				// binary: its values win, and anything it omits is filled
				// from the embedded row so a redefinition cannot blank a
				// field the synced copy simply did not carry.
				merged := e
				merged.Name = prev.Name
				fillEmptyProviderFields(&merged, prev)
				// …including the declarations: prev supplies the ids the
				// synced row does not list at all, and nowhere else — the
				// synced document's own numbers win, which is the whole point
				// of bumping the version to correct a window.
				merged.Models = mergeDeclaredModelLimits(prev.Models, e.Models)
				byName[merged.Name] = merged
			}
		}
	}

	out := make([]providerCatalogEntry, 0, len(order))
	for _, name := range order {
		out = append(out, byName[name])
	}
	return out
}

// fillEmptyProviderFields copies only the fields dst is missing. Used in both
// merge directions for the same reason: a field the incoming row does not
// carry means "this document has nothing to say", not "unset what you know".
//
// "Missing" is judged on the TRIMMED value and the result is trimmed, because
// a base_url of "   " is a hand-edit that lost its content, not an endpoint:
// add() refuses such a row outright ("a row that cannot state an endpoint is
// dropped"), but the redefinition branch never went through add(), so a
// whitespace endpoint was accepted as a redefinition and every URL built from
// the row became relative ("/v4/chat/completions") — the provider 404s for
// every model, with an error that names no host.
func fillEmptyProviderFields(dst *providerCatalogEntry, src providerCatalogEntry) {
	if strings.TrimSpace(dst.BaseURL) == "" {
		dst.BaseURL = src.BaseURL
	}
	dst.BaseURL = strings.TrimSpace(dst.BaseURL)
	if dst.Version == "" {
		dst.Version = src.Version
	}
	if dst.ModelsPath == "" {
		dst.ModelsPath = src.ModelsPath
	}
	if dst.Wire == "" {
		dst.Wire = src.Wire
	}
	if dst.ToolFormat == "" {
		dst.ToolFormat = src.ToolFormat
	}
	if dst.APIKeyEnv == "" {
		dst.APIKeyEnv = src.APIKeyEnv
	}
	if dst.PlanLabel == "" {
		dst.PlanLabel = src.PlanLabel
	}
	if dst.PlanLabelModelPrefix == "" {
		dst.PlanLabelModelPrefix = src.PlanLabelModelPrefix
	}
	if dst.KeyURL == "" {
		dst.KeyURL = src.KeyURL
	}
	if dst.AuthVia == "" {
		dst.AuthVia = src.AuthVia
	}
	if dst.Notes == "" {
		dst.Notes = src.Notes
	}
}

// mergeDeclaredModelLimits merges a provider's declared model windows. kept is
// the row being built, incoming the other document's declared windows: the
// result holds every id either side lists, with the INCOMING numbers winning
// wherever it states one.
//
// The direction is what this function exists to pin down. Callers pass the
// SYNCED document as `incoming` in both merge directions, because a declared
// window is a measurement, not a preference, and the fetched copy is the
// newer one wherever it comes from. The redefinition branch used to call
// fillEmptyProviderFields(dst, prev) — src = the EMBEDDED row — so the one
// field a version bump is most often used to correct was exactly the field the
// shipped row overruled: a vendor that revised a context window down (the
// honest direction) kept being over-reported by every host, and
// CLAUDE_CODE_MAX_CONTEXT_TOKENS / the proxy's context-fit clamp never learned.
//
// A zero is treated as "states nothing", not as a value: Context and Output
// are 0-means-unknown throughout, so a document that lists an id without a
// window leaves the other side's number standing rather than blanking it.
func mergeDeclaredModelLimits(kept, incoming map[string]providerCatalogModelLimit) map[string]providerCatalogModelLimit {
	if len(kept) == 0 && len(incoming) == 0 {
		return nil
	}
	out := make(map[string]providerCatalogModelLimit, len(kept)+len(incoming))
	for id, limit := range kept {
		out[id] = limit
	}
	for id, limit := range incoming {
		prev, existed := out[id]
		if !existed {
			out[id] = limit
			continue
		}
		if limit.Context == 0 {
			limit.Context = prev.Context
		}
		if limit.Output == 0 {
			limit.Output = prev.Output
		}
		out[id] = limit
	}
	return out
}

func parseProviderCatalogBytes(b []byte) []providerCatalogEntry {
	entries, _ := parseProviderCatalogFileVersioned(b)
	return entries
}

// parseProviderCatalogFileVersioned decodes a catalog document and reports the
// document's own version, which says whether it is newer than the embedded
// default (see providerCatalog).
func parseProviderCatalogFileVersioned(b []byte) ([]providerCatalogEntry, int) {
	var f providerCatalogFile
	if json.Unmarshal(b, &f) != nil {
		return nil, 0
	}
	return f.Providers, f.Version
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
			ModelsPath: e.ModelsPath,
			Wire:       e.Wire,
			ToolFormat: e.ToolFormat,
			APIKeyEnv:  e.APIKeyEnv,
			AuthVia:    e.AuthVia,
			// Marks the row as the catalog's, which is what lets
			// remoteLaunchModels apply the vendor's declared model list to it
			// and to nothing else (see CatalogOrigin).
			CatalogOrigin: true,
		})
	}
	return out
}

// providerPlanLabel looks up the catalog's plan_label for a "<provider>/
// <model>" picker id — the data-driven replacement for billingPlanLabel's
// old hardcoded per-vendor string matches. modelRest is the part after the
// first "/"; entries with a PlanLabelModelPrefix only label rows whose
// model id starts with it (for a provider whose one subscription covers
// only part of what it proxies — set the prefix on THAT provider's row and
// the label stops at that boundary).
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

// providerCatalogDeclaredModels returns the model ids (and windows) the
// catalog declares for a provider, keyed by the bare upstream id. Empty for
// every provider whose /v1/models sweep can answer for itself.
func providerCatalogDeclaredModels(providerName string) map[string]providerCatalogModelLimit {
	for _, e := range providerCatalog() {
		if e.Name == providerName {
			return e.Models
		}
	}
	return nil
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
