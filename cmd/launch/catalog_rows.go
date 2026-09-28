package launch

// catalog_rows.go — one model, as the picker shows it: window, price per
// million (with the bands upstream carries), and capability marks. Display
// only; routing reads the catalog directly.
//
// The list is the models.dev catalog plus the ids the oaica overlay declares
// that upstream lacks, and the overlay corrects per field — the same two-layer
// read provider_catalog.go does for providers, for the same reason: the
// subscription endpoints (z.ai's Coding Plan, MiniMax's) serve no /v1/models at
// all, so for them the declared list is not a correction but the only list
// there is.

import (
	"fmt"
	"sort"
	"strings"
)

type catalogModelRow struct {
	ID         string
	Name       string
	Context    int
	Output     int
	HasContext bool
	Cost       modelsDevCost
	ToolCall   bool
	Reasoning  bool
	Vision     bool
	Deprecated bool
}

// providerNameOf is a models.dev provider's oaica name. Upstream keys its map
// by id and also states an id inside the row; the stated one wins, so an
// upstream that renames a key does not rename every row that reads it.
func providerNameOf(id string, p modelsDevProvider) string {
	return firstNonEmpty(strings.TrimSpace(p.ID), strings.TrimSpace(id))
}

// catalogRowsFor returns a provider's models.dev models, plus any the overlay
// declares that upstream lacks, sorted by release date descending (newest
// first) with a stable id tiebreak. Deprecated rows are marked, not hidden: a
// model the vendor still serves is a model the picker must still be able to
// launch.
func catalogRowsFor(providerName string) []catalogModelRow {
	rows := map[string]catalogModelRow{}
	released := map[string]string{}

	if f, ok := loadModelsDevCatalog(); ok {
		for id, p := range f.Providers {
			if providerNameOf(id, p) != providerName {
				continue
			}
			for _, m := range p.Models {
				mid := strings.TrimSpace(m.ID)
				if mid == "" {
					continue
				}
				ctx, hasCtx := m.contextWindow()
				row := catalogModelRow{
					ID:         mid,
					Name:       firstNonEmpty(m.Name, mid),
					Context:    ctx,
					HasContext: hasCtx,
					Cost:       m.cost(),
					ToolCall:   m.toolCall(),
					Reasoning:  m.Reasoning,
					Vision:     m.Attachment || hasImageInput(m.Modalities),
					Deprecated: m.deprecated(),
				}
				if m.Limit != nil {
					row.Output = int(m.Limit.Output)
				}
				rows[mid] = row
				released[mid] = m.ReleaseDate
			}
		}
	}

	// The overlay's declared windows correct the catalog's per field, and add
	// the ids upstream does not carry at all (mergeDeclaredModelLimits' rule:
	// a zero states nothing, so it never blanks the other side's number).
	for id, lim := range providerCatalogDeclaredModels(providerName) {
		row, ok := rows[id]
		if !ok {
			row = catalogModelRow{ID: id, Name: id, ToolCall: true}
		}
		if lim.Context > 0 {
			row.Context, row.HasContext = lim.Context, true
		}
		if lim.Output > 0 {
			row.Output = lim.Output
		}
		rows[id] = row
	}

	out := make([]catalogModelRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		ri, rj := released[out[i].ID], released[out[j].ID]
		if ri != rj {
			return ri > rj // newest first; absent release_date sorts last
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func hasImageInput(m *modelsDevModalities) bool {
	if m == nil {
		return false
	}
	for _, in := range m.Input {
		if in == "image" {
			return true
		}
	}
	return false
}

// catalogRowDescription is the row's right-hand text: window, price per
// million, then capability marks. Banded pricing is shown when upstream carries
// it — displaying only the base rate would misprice a long context, which is
// exactly the case the bands exist for.
func catalogRowDescription(r catalogModelRow) string {
	var parts []string
	if r.HasContext {
		parts = append(parts, fmt.Sprintf("ctx %d", r.Context))
	} else {
		parts = append(parts, "ctx ?")
	}

	c := r.Cost
	switch {
	case c.Input == 0 && c.Output == 0 && len(c.Tiers) == 0 && c.ContextOver200k == nil:
		// A vendor that charges nothing and a row that states no price at all
		// are different claims, but a picker cannot tell them apart, and
		// printing "$0.00/$0.00 per M" for the second asserts a price nobody
		// stated. Say the weaker thing.
		parts = append(parts, "free")
	default:
		parts = append(parts, fmt.Sprintf("$%.2f/$%.2f per M", c.Input, c.Output))
	}
	for _, t := range c.Tiers {
		parts = append(parts, fmt.Sprintf(">%dK $%.2f/$%.2f", int(t.Tier.Size/1000), t.Input, t.Output))
	}
	if c.ContextOver200k != nil {
		parts = append(parts, fmt.Sprintf(">200K $%.2f/$%.2f", c.ContextOver200k.Input, c.ContextOver200k.Output))
	}

	var marks []string
	if r.ToolCall {
		marks = append(marks, "tools")
	}
	if r.Reasoning {
		marks = append(marks, "reasoning")
	}
	if r.Vision {
		marks = append(marks, "vision")
	}
	if r.Deprecated {
		marks = append(marks, "deprecated")
	}
	if len(marks) > 0 {
		parts = append(parts, strings.Join(marks, " · "))
	}
	return strings.Join(parts, " · ")
}

// remoteIsSweepable reports whether a remote has any credential mechanism at
// all. A remote with none is genuinely unauthenticated, so sweeping it spends a
// timeout on an answer we would not trust.
func remoteIsSweepable(r userRemote) bool {
	if r.APIKey != "" || r.APIKeyEnv != "" || r.AuthVia != "" {
		return true
	}
	return r.key() != ""
}

// remoteNeedsSweep is the question the launch actually asks, and it is not the
// one above. "Not worth sweeping" is a claim about a VENDOR: keyless, it
// answers 401 or a list nothing can use — and the catalog already lists it. A
// user's own box is the opposite case: a llama.cpp on the LAN is legitimately
// open, the catalog has never heard of it, and skipping its sweep would leave
// its picker section empty. So only a catalog row may be skipped for want of a
// key.
func remoteNeedsSweep(r userRemote) bool {
	if r.CatalogOrigin && !remoteIsSweepable(r) {
		return false
	}
	return true
}
