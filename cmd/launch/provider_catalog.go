package launch

// provider_catalog.go — the provider directory (endpoint, wire, tool format,
// plan label, credential names) as DATA, not Go code. Adding a provider, a new
// billing plan for an existing one (e.g. z.ai's Coding Plan alongside its
// pay-per-token API), or fixing an endpoint URL is an overlay edit (or an
// upstream models.dev correction), never a recompile — same principle as
// model_sync.go's hosted model catalog, applied to the OTHER thing that used to
// live only in Go source (catalogProviders used to be a literal slice here; see
// git history around 2026-09-17 for the before/after).
//
// Two layers, lowest priority first:
//  1. the ported models.dev catalog (catalog_modelsdev.go), cached at
//     ~/.oaica/cache/catalog/modelsdev.json by `oaica model catalog sync` —
//     third-party data we never edit;
//  2. the oaica overlay (providers/oaica.json), embedded and applied at read
//     time (catalog_overlay.go), which corrects upstream where it is wrong for
//     us and adds the rows and plans models.dev does not carry.
//
// This file also used to hold a third mechanism: a synced providers.json whose
// document could REDEFINE a shipped row if it bumped its own "version" above
// the embedded one. The overlay's per-key merge replaces that entirely — a
// correction lands at read time with no bump to make, and a document of unknown
// age can never blank a field it does not name. See catalog_overlay.go's header
// for the 2026-09-25 incident that version-bump machinery was built around.
//
// A user's own ~/.oaica/remotes.json entry of the same name still wins over
// BOTH layers (loadUserRemotes' existing dedupe) — the catalog only ever
// supplies a default, never overrides a user's explicit config.

import (
	"sort"
	"strings"
)

// Named env-var/id constants for the handful of builtins tests and other
// code reference by identifier. These are NOT the provider table — that's
// providers/oaica.json — just readable aliases for strings that also happen to
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

// providerCatalogEntry is one provider row — the overlay's own per-row shape,
// and what a models.dev provider becomes on the way in. A strict subset of
// userRemote's fields (only what a provider DEFAULT should carry; things like
// api_key or weight are a user's own choice, never shipped here).
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
	// Env is the provider's ordered list of credential environment variables,
	// ported from models.dev. The gate scans it in order and takes the first
	// variable that is SET, not the first listed — upstream orders these by
	// popularity, and taking env[0] would hide any provider whose listed-first
	// variable is the less common one.
	Env []string `json:"env,omitempty"`
	// Hidden marks a provider that cannot work through a plain base URL (the
	// SDK-signed group: Bedrock, Vertex, Azure, and friends). Listed nowhere in
	// the picker unless a user's own remotes.json names it, which always wins.
	Hidden bool `json:"hidden,omitempty"`
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

// providerCatalog returns the merged provider directory: the ported models.dev
// catalog as the base layer, corrected per field by the oaica overlay, with
// overlay-only rows appended. The overlay is where our own plans live —
// models.dev does not carry zai-coding-plan or opencode-go — and it is also the
// only way a row of ours can exist before upstream learns about it.
//
// Order is stable: the catalog's provider ids sorted, then the overlay's rows
// in file order. The picker, `oaica auth list` and the doctor all paint these
// rows, and a reshuffle between two runs of the same binary reads as a change.
//
// Errors reading either source are swallowed — a corrupt cache or a bad embed
// should degrade to "fewer providers listed", never crash the picker.
func providerCatalog() []providerCatalogEntry {
	byName := map[string]int{}
	out := []providerCatalogEntry{}

	if f, ok := loadModelsDevCatalog(); ok {
		ids := make([]string, 0, len(f.Providers))
		for id := range f.Providers {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			e := providerEntryFromModelsDev(id, f.Providers[id])
			if e.Name == "" {
				continue
			}
			if _, exists := byName[e.Name]; !exists {
				byName[e.Name] = len(out)
				out = append(out, e)
			}
		}
	}

	for _, p := range oaicaOverlay().Providers {
		p.Name = strings.TrimSpace(p.Name)
		if p.Name == "" {
			continue
		}
		if i, ok := byName[p.Name]; ok {
			// Field-level: the overlay's correction wins where it is set, and
			// whatever it does not mention keeps the catalog's value.
			out[i] = mergeProviderEntry(out[i], p)
			continue
		}
		byName[p.Name] = len(out)
		out = append(out, p)
	}

	// A row with no endpoint at all cannot work, and the picker must not offer
	// one that fails at launch: drop it silently. Hidden rows are exempt — an
	// SDK-signed provider has no plain base URL by design — and so are rows
	// upstream gives credentials for, which are real vendors whose endpoint
	// models.dev states per model rather than at the provider root
	// (providerEntryFromModelsDev).
	kept := out[:0]
	for _, e := range out {
		if strings.TrimSpace(e.BaseURL) == "" && !e.Hidden && len(e.Env) == 0 {
			continue
		}
		kept = append(kept, e)
	}
	return kept
}

// providerEntryFromModelsDev ports one models.dev provider row. The per-model
// provider.api / provider.npm overrides are not resolved here — they belong to
// the model, not the provider, and are read where a model is chosen.
func providerEntryFromModelsDev(id string, p modelsDevProvider) providerCatalogEntry {
	name := strings.TrimSpace(p.ID)
	if name == "" {
		name = strings.TrimSpace(id)
	}
	return providerCatalogEntry{
		Name:    name,
		BaseURL: strings.TrimSpace(p.API),
		Wire:    wireFromNPM(p.NPM),
		Env:     p.Env,
	}
}

// wireFromNPM maps an AI-SDK package name to the wire oaica speaks. Everything
// that is not the Anthropic SDK goes over the OpenAI-compatible wire, which is
// what opencode does too (npm ?? "@ai-sdk/openai-compatible").
func wireFromNPM(npm string) string {
	if strings.HasPrefix(npm, "@ai-sdk/anthropic") {
		return "anthropic"
	}
	return "openai"
}

// providerCatalogFromCatalogOnly is the catalog layer without overlay
// corrections, used to tell "upstream carries this provider" from "our overlay
// does" — a distinction the drift work (Plan B) needs and tests pin.
func providerCatalogFromCatalogOnly() map[string]providerCatalogEntry {
	out := map[string]providerCatalogEntry{}
	f, ok := loadModelsDevCatalog()
	if !ok {
		return out
	}
	for id, p := range f.Providers {
		e := providerEntryFromModelsDev(id, p)
		if e.Name != "" {
			out[e.Name] = e
		}
	}
	return out
}

// mergeDeclaredModelLimits merges a provider's declared model windows. kept is
// the row being built, incoming the other document's declared windows: the
// result holds every id either side lists, with the INCOMING numbers winning
// wherever it states one.
//
// The direction is what this function exists to pin down. Callers pass the
// newer, less-trusted document as `incoming` (the synced copy, or the overlay's
// correction), because a declared window is a measurement, not a preference,
// and the fetched copy is the newer one wherever it comes from. The version-bump
// branch this replaced called fillEmptyProviderFields(dst, prev) — src = the
// EMBEDDED row — so the one field a correction is most often used to fix was
// exactly the field the shipped row overruled: a vendor that revised a context
// window down (the honest direction) kept being over-reported by every host, and
// CLAUDE_CODE_MAX_CONTEXT_TOKENS / the proxy's context-fit clamp never learned.
//
// A zero is treated as "states nothing", not as a value: Context and Output are
// 0-means-unknown throughout, so a document that lists an id without stating a
// window (or with the field at 0) leaves the other side's number standing rather
// than blanking it — and a cache correcting one window field keeps the other.
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

// providerCatalogAsUserRemotes converts the merged catalog to userRemote
// values for builtinRemotes() to gate by env-var presence exactly like
// every other builtin.
// userRemoteFromCatalogEntry converts one catalog row to the remote oaica
// routes through. The env[] list is NOT copied: a row with no api_key_env is
// gated by firstSetEnv at the call site, which then records the variable that
// is actually set (builtinRemotes).
func userRemoteFromCatalogEntry(e providerCatalogEntry) userRemote {
	return userRemote{
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
	}
}

func providerCatalogAsUserRemotes() []userRemote {
	entries := providerCatalog()
	out := make([]userRemote, 0, len(entries))
	for _, e := range entries {
		out = append(out, userRemoteFromCatalogEntry(e))
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
