package launch

// catalog.go — the data SHAPE for a future public model catalog page (an
// oaica.com/library analogous to ollama.com/library), decoupled from the
// per-user manifest in model_manifest.go. A manifest entry answers "what
// did I configure on my box"; a CatalogEntry answers "what SKU exists and
// what would a customer read about it before choosing it" — no business
// content is invented here (no prices, no license terms, no marketing
// copy), only the FIELDS such a page would need, so the catalog data file
// can be authored later without a schema migration.
//
// Nothing in this file is wired into a live HTTP handler or CLI command —
// it is intentionally inert scaffolding for workstream 5 ("packaging/
// catalog/licensing groundwork"), not a shipped feature.

// CatalogVariant is one SKU in the "canonical N-variant lineup" shape
// sprapp-prism/README.md already uses for the OAICA-* model line (e.g.
// OAICA-700M_LOOPED MM) — deliberately generic enough to also describe a
// supported third-party model (kat-awq) on the same page.
type CatalogVariant struct {
	// ID is the model id customers would pass to `oaica run`/`oaica launch`
	// — the same namespace as ModelManifestEntry.ID and plan Model fields.
	ID string `json:"id"`

	// DisplayName is the human-facing SKU name (e.g. "OAICA 700M", "kat-awq").
	DisplayName string `json:"display_name"`

	// Tagline is a one-line description, analogous to a catalog card
	// subtitle. Content TBD by whoever owns product copy — left empty here.
	Tagline string `json:"tagline,omitempty"`

	// ParamCount, ContextWindow, DeploySizeGB mirror the sizing columns in
	// sprapp-prism's variant table so the same numbers can drive both the
	// research doc and a public catalog page without hand-copying.
	ParamCount    string `json:"param_count,omitempty"`    // free text: "700M", "13B" — matches how the source doc labels these, not always a clean int
	ContextWindow int    `json:"context_window,omitempty"`
	DeploySizeGB  float64 `json:"deploy_size_gb,omitempty"`

	// Multimodal mirrors the "all 5 ship multimodal" column (vision/audio/OCR).
	Multimodal bool `json:"multimodal,omitempty"`

	// PrimaryUseCase mirrors the sprapp-prism variant table's use-case column.
	PrimaryUseCase string `json:"primary_use_case,omitempty"`

	// License is a machine-readable identifier ("apache-2.0", "proprietary",
	// etc) for the page to display and for a future entitlement check to key
	// off of — no license TEXT is authored here, only where a future one
	// would be labeled.
	License string `json:"license,omitempty"`

	// Availability distinguishes "you can run this today" from roadmap
	// entries the source doc already tracks (sprapp-prism/README.md:
	// "7B+ variants are roadmap"). Kept as a free string, not an enum, since
	// the real state names (in-training, roadmap, ga) aren't decided here.
	Availability string `json:"availability,omitempty"`

	// ManifestID, when set, is the ~/.oaica/models.json entry a customer
	// gets after `oaica model add` for this variant — the seam between the
	// public catalog (this file) and the per-user manifest
	// (model_manifest.go). Empty for a roadmap/unavailable entry.
	ManifestID string `json:"manifest_id,omitempty"`
}

// Catalog is the top-level document shape for a future
// oaica.com/library data file (JSON or embedded Go literal — undecided).
type Catalog struct {
	Version  int              `json:"version"`
	Variants []CatalogVariant `json:"variants"`
}

const catalogVersion = 1

// NewCatalog returns an empty, correctly-versioned catalog — a starting
// point for whoever authors the real variant list, not sample content.
func NewCatalog() Catalog {
	return Catalog{Version: catalogVersion}
}
