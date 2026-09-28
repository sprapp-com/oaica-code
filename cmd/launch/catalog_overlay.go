package launch

// catalog_overlay.go — the oaica overlay: the ONE file we edit. It corrects the
// ported catalog, adds oaica's own first-party models (which models.dev does not
// know about), and carries limits for ids upstream lacks.
//
// Two copies, merged PER KEY, lowest priority first:
//  1. providers/oaica.json, embedded — ships in the binary, so a fresh install
//     works fully offline.
//  2. ~/.oaica/cache/providers/oaica.json — pulled by `oaica remote sync`.
//
// The merge is per provider name, per model id, and per limit key. That matters:
// the layer it replaces (provider_catalog.go) overrode WHOLESALE, so a field
// added to a newer binary was invisible on any host whose cache predated it —
// documented there as the cause of a 2026-09-25 "the feature is inert on the
// real box" incident. A per-key merge cannot lose a field it never named.
//
// Applied at READ time, so re-syncing the ported catalog can never lose a
// correction and an upstream update can never be blocked by one.

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

//go:embed providers/oaica.json
var oaicaOverlayEmbedded []byte

// oaicaOverlayModel is one first-party model: models.dev knows nothing about
// oaica-35b-a3b-vision or oaica-default, and they must stay first-class picker
// entries. Endpoints are NOT here — they are resolved at launch (tier_routing),
// because the gateway lives on a different port on every machine.
type oaicaOverlayModel struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Context     int    `json:"context"`
	Output      int    `json:"output"`
}

type oaicaOverlayFile struct {
	Version   int                                  `json:"version"`
	Providers []providerCatalogEntry               `json:"providers"`
	Models    []oaicaOverlayModel                  `json:"models"`
	Limits    map[string]providerCatalogModelLimit `json:"limits"`
}

func oaicaOverlayCachePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".oaica", "cache", "providers", "oaica.json"), nil
}

// oaicaOverlay returns the embedded overlay merged with the synced copy,
// per key. A missing or unparseable cache is not an error: the embedded copy is
// always present, so the overlay can never be empty.
func oaicaOverlay() oaicaOverlayFile {
	out := parseOverlayBytes(oaicaOverlayEmbedded)
	cache := oaicaOverlayFile{}
	if path, err := oaicaOverlayCachePath(); err == nil {
		if b, err := os.ReadFile(path); err == nil {
			cache = parseOverlayBytes(b)
		}
	}

	byName := make(map[string]int, len(out.Providers))
	for i, p := range out.Providers {
		byName[p.Name] = i
	}
	for _, p := range cache.Providers {
		if p.Name == "" {
			continue
		}
		if i, ok := byName[p.Name]; ok {
			// Field-level: the cache's correction wins where it is set, and
			// whatever it does not mention keeps the embedded value.
			out.Providers[i] = mergeProviderEntry(out.Providers[i], p)
			continue
		}
		byName[p.Name] = len(out.Providers)
		out.Providers = append(out.Providers, p)
	}

	byID := make(map[string]int, len(out.Models))
	for i, m := range out.Models {
		byID[m.ID] = i
	}
	for _, m := range cache.Models {
		if m.ID == "" {
			continue
		}
		if i, ok := byID[m.ID]; ok {
			if m.DisplayName != "" {
				out.Models[i].DisplayName = m.DisplayName
			}
			if m.Context > 0 {
				out.Models[i].Context = m.Context
			}
			if m.Output > 0 {
				out.Models[i].Output = m.Output
			}
			continue
		}
		byID[m.ID] = len(out.Models)
		out.Models = append(out.Models, m)
	}

	// Limits merge per key and per field, by the same rule the provider rows
	// follow (mergeDeclaredModelLimits): the sync's stated numbers win, a zero
	// states nothing, and a sync that corrects one window field keeps the other.
	// A limit's Context is written verbatim into CLAUDE_CODE_MAX_CONTEXT_TOKENS
	// and codex's context_window, so a document that lists an alias it has no
	// window for must leave the embedded number standing rather than blank it.
	synced := make(map[string]providerCatalogModelLimit, len(cache.Limits))
	for id, lim := range cache.Limits {
		if id == "" {
			continue
		}
		synced[id] = lim
	}
	out.Limits = mergeDeclaredModelLimits(out.Limits, synced)
	if out.Limits == nil {
		out.Limits = map[string]providerCatalogModelLimit{}
	}
	return out
}

// mergeProviderEntry layers override onto base field by field, so a cache that
// corrects one field cannot blank the rest.
//
// Every field of providerCatalogEntry is listed here deliberately: a field this
// function forgets is a field a synced cache can never correct, and the failure
// is silent — the embedded value just keeps winning. If a field is added to the
// struct, add it here in the same change.
func mergeProviderEntry(base, override providerCatalogEntry) providerCatalogEntry {
	// Judged on the TRIMMED value and assigned trimmed, for the reason the
	// retired provider catalog documented when a "   " endpoint survived as a
	// row's base_url: every URL built from the row became relative
	// ("/v4/chat/completions"), the provider 404s for every model, and the
	// error names no host. A whitespace endpoint is a hand-edit that lost its
	// content, not an endpoint.
	if v := strings.TrimSpace(override.BaseURL); v != "" {
		base.BaseURL = v
	}
	if override.Version != "" {
		base.Version = override.Version
	}
	if override.ModelsPath != "" {
		base.ModelsPath = override.ModelsPath
	}
	if override.Wire != "" {
		base.Wire = override.Wire
	}
	if override.ToolFormat != "" {
		base.ToolFormat = override.ToolFormat
	}
	if override.APIKeyEnv != "" {
		base.APIKeyEnv = override.APIKeyEnv
	}
	if override.PlanLabel != "" {
		base.PlanLabel = override.PlanLabel
	}
	if override.PlanLabelModelPrefix != "" {
		base.PlanLabelModelPrefix = override.PlanLabelModelPrefix
	}
	if override.KeyURL != "" {
		base.KeyURL = override.KeyURL
	}
	if override.AuthVia != "" {
		base.AuthVia = override.AuthVia
	}
	if override.Notes != "" {
		base.Notes = override.Notes
	}
	if len(override.Env) > 0 {
		base.Env = override.Env
	}
	// Delegated rather than open-coded: a document that corrects a model's
	// context window must not blank the output it does not restate, and a zero
	// is "states nothing" — the rule mergeDeclaredModelLimits pins (and the
	// reason this used to be a wholesale per-id assignment that let a cache
	// stating {"context":0} zero an embedded window).
	base.Models = mergeDeclaredModelLimits(base.Models, override.Models)
	// Hidden is a bool, so absence and false read the same: a bool cannot be
	// merged by "override wins where it is set". The override wins when it says
	// true, and an embedded true is never cleared by a cache that omits the
	// field. Hiding is the conservative direction — a hidden row is offered to
	// nobody, while an unhidden SDK-signed row is offered to everyone and fails
	// at launch — so this asymmetry is deliberate.
	if override.Hidden {
		base.Hidden = true
	}
	return base
}

func parseOverlayBytes(b []byte) oaicaOverlayFile {
	var f oaicaOverlayFile
	_ = json.Unmarshal(b, &f)
	return f
}

func overlayProvidersByName() map[string]providerCatalogEntry {
	out := map[string]providerCatalogEntry{}
	for _, p := range oaicaOverlay().Providers {
		out[p.Name] = p
	}
	return out
}

func overlayLimits() map[string]providerCatalogModelLimit {
	return oaicaOverlay().Limits
}

func overlayFirstPartyModels() []oaicaOverlayModel {
	return oaicaOverlay().Models
}
