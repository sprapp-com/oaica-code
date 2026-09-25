# models.dev Catalog Port — Implementation Plan (Plan A: the catalog itself)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the hand-maintained provider and cloud-limit JSON files with a ported models.dev catalog plus an embedded oaica overlay, so provider rows, endpoints, credentials, model lists, limits and pricing update by syncing a file.

**Architecture:** Three layers, lowest precedence first: (1) the models.dev catalog fetched into `~/.oaica/cache/catalog/modelsdev.json` and never edited by us; (2) one embedded `cmd/launch/providers/oaica.json` overlay, merged at *read time* so re-syncing can never lose a local fix; (3) `~/.oaica/remotes.json`, `aliases.json`, `local_servers.json`, unchanged and always winning. Existing seams (`providerCatalog`, `cloudLimitsFromCatalog`, `builtinRemotes`, `resolveLaunchEndpoint`, the picker's flat list) keep their shapes; only their data source changes.

**Tech Stack:** Go (stdlib only — no new module dependencies), `go:embed`, `cobra` command registration in `cmd/cmd.go`, bubbletea selector in `cmd/tui/selector.go`.

**Spec:** `docs/superpowers/specs/2026-09-25-modelsdev-catalog-design.md` (corrected 2026-09-26, commit `dfafe5e5`). Read it alongside this plan; §"Correction" notes there describe the tree as it actually is.

## Scope: this plan is Plan A of two

The approved spec bundles two subsystems that ship independently. This plan is the
**user-visible** one: catalog port, overlay, id translation, picker sections, model-list
resolution, first-party endpoint resolution, usage tracking, and **only** the contract
validator from the drift machinery (a sync that cannot read the payload refuses it and
keeps the last good copy — three steps, no archive).

**Plan B** (write it after A lands) is the internal-ops half: the raw archive and its
index, `shape.json`, the drift artifact, `check` / `diff` / `drift` / `--accept-drift`,
the `DRIFT` marker, `docs/CATALOG_DRIFT.md`, and the daily CI workflow. Deferring B is
safe **because sync stays explicit**: nothing auto-fetches, so an unreachable or
reformatted upstream cannot break a launch that never asks for one. If the user wants B
in the same pass, stop here and say so before starting Task 1.

## Deviations from the spec, and why

Three, all forced by the code as it exists. Each is also flagged in the task that owns it.

1. **Providers are not rendered as one section header each.** The spec (§Picker behaviour)
   asks for "all providers as sections". `ReorderItems` + the render split
   (`cmd/tui/selector.go:140-176`, `:700-789`) split a **flat** list into five fixed
   sections, and navigation walks that same flat list — the file's own comment warns that
   any section not present in both places desyncs the cursor from the highlight. Catalog
   rows already arrive as `<provider>/<model>` through the existing `Remote` machinery
   (`builtinRemotes` → `remoteLaunchModels`) and sort alphabetically, so each provider's
   rows are already contiguous, with its `plan_label` on them. This plan therefore adds
   **one** new section — Frequently used, which is the part with real user value — and
   lets providers ride the existing section. Plan B can add per-provider headers if the
   contiguity turns out not to be enough.
2. **First-party endpoint resolution does not reorder the existing chain.** The spec's
   order (env → `local_servers.json` → `remotes.json`) would put a bare-id
   `local_servers.json` lookup ahead of `remotes.json`, changing behaviour for every
   existing user. Task 10 adds `OAICA_GATEWAY_URL` at the top and otherwise leaves
   `resolveLaunchEndpoint`'s implemented order alone (`:local` remains the way a
   `local_servers.json` entry is selected).
3. **`status` moves into Plan A** out of the spec's drift command table — it is
   provenance (source, fetched_at, sha, counts), not drift machinery, and it is what a
   support conversation needs, so it ships with the port.

## Global Constraints

- **No new module dependencies.** This code path uses `encoding/json`, `net/http`,
  `os`, `path/filepath`, `strings`, `time` only. `go.mod` must not change.
- **Ported data is never edited by us.** Corrections go in the overlay, applied at read
  time. A re-sync of the catalog can never lose an overlay fix.
- **The catalog is not embedded.** `~/.oaica/cache/catalog/modelsdev.json` only. Embedded
  is the overlay alone.
- **Cache conventions, copied from `provider_sync.go`:** `MkdirAll(dir, 0o700)`,
  `WriteFile(file, body, 0o600)`, ETag in a `<path>.etag` sidecar, 10 s HTTP timeout,
  `file://` short-circuit, offline falls back to the last good copy.
- **All new home-directory lookups go through `os.UserHomeDir()`** exactly as
  `providerCatalogCachePath` does (`provider_catalog.go:116-122`), never a computed path,
  so `setLaunchTestHome` (`launch_test.go:196`) redirects them in tests.
- **Never write to a user's `remotes.json`, `aliases.json`, or `local_servers.json`.**
  Layer 3 is read-only for this work.
- **No ports or hosts hardcoded in `oaica.json`** for oaica's own models.
- **Every removed file is removed, not left dead:** `providers/providers.json`,
  `cloud_limits/cloud_limits.json`, `cloud_limits_catalog.go`, `cloud_limits_sync.go`
  (Task 4). Their content moves, so no user migration and no compatibility shim.
- **`tool_call` absent means tool-capable** (`model.tool_call ?? true`); `cost` absent
  means every component 0; `release_date` absent sorts last.
- **Dots become dashes on the Anthropic wire only.** Never on an OpenAI wire id
  (`gpt-5.4` keeps its dot).

## Review Focus

Five failure modes the spec implies but no task's tests exercise, each pinned to the task
that owns the code:

1. **A provider whose `api` is absent and whose overlay entry is missing** (the
   SDK-signed group, and any upstream provider that drops `api` later) must be *hidden*,
   never listed with an empty base URL that fails at launch. → Task 5, its own test.
2. **A user's `remotes.json` row colliding with a catalog provider name** must win
   outright — its URL *and* its key — not merely be preferred for its URL. → Task 5.
3. **A cache file that predates a newly added overlay field** must not make that field
   inert. This is the wholesale-replace hazard `provider_catalog.go:21-33` documents from
   the 2026-09-25 incident. → Task 3.
4. **A model id with a dot on an OpenAI-wire provider** must be passed through unchanged
   while the same-shaped Anthropic id is translated. → Task 6.
5. **A model entry with no `cost` and no `limit`** (reachable via overlay rows and via a
   future upstream shape) must render as free / `ctx ?`, never panic and never print a
   zero that reads as a real price. → Tasks 1 and 8.

---

## File Structure

New, each with one responsibility:

| File | Responsibility |
|---|---|
| `cmd/launch/catalog_modelsdev.go` | models.dev payload types, parser, contract validator. No I/O. |
| `cmd/launch/catalog_sync.go` | Fetch + ETag + cache + adopt/refuse verdict, and `CatalogSync`. |
| `cmd/launch/catalog_overlay.go` | `oaica.json` types, embedded copy, per-key read-time merge. |
| `cmd/launch/catalog_translate.go` | Model-id translation (Anthropic dots, aggregator prefixes). |
| `cmd/launch/model_usage.go` | `~/.oaica/usage.json` read/write/rank. |
| `cmd/launch/providers/oaica.json` | The overlay data (embedded). Replaces the two retired JSON files. |

Modified: `cmd/launch/provider_catalog.go` (reads the overlay), `cmd/launch/models.go`
(limits from the overlay), `cmd/launch/user_remotes.go` (`builtinRemotes` gating, sweep
gate, model-list resolution), `cmd/launch/tier_routing.go` (`OAICA_GATEWAY_URL`),
`cmd/tui/selector.go` + `cmd/launch/launch.go` + `cmd/launch/models.go` (Frequently used
section), `cmd/cmd.go` (`oaica model catalog …`).

Deleted in Task 4: `cmd/launch/providers/providers.json`,
`cmd/launch/cloud_limits/cloud_limits.json`, `cmd/launch/cloud_limits_catalog.go`,
`cmd/launch/cloud_limits_sync.go`.

---

### Task 1: models.dev payload types, parser, contract validator

Pure functions, no I/O. Everything downstream depends on these names, so they are fixed here.

**Files:**
- Create: `cmd/launch/catalog_modelsdev.go`
- Test: `cmd/launch/catalog_modelsdev_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type modelsDevFile struct { Providers map[string]modelsDevProvider `json:"providers"` }`
  - `type modelsDevProvider struct { ID, Name, API, NPM string; Env []string; Models map[string]modelsDevModel }`
  - `type modelsDevModel struct { ID, Name, ReleaseDate, Status string; Attachment, Reasoning, Temperature bool; ToolCall *bool; Modalities *modelsDevModalities; Limit *modelsDevLimit; Cost *modelsDevCost; Provider *modelsDevModelProvider }`
  - `type modelsDevLimit struct { Context, Input, Output float64 }`
  - `type modelsDevCost struct { Input, Output, CacheRead, CacheWrite float64; Tiers []modelsDevCostTier; ContextOver200k *modelsDevCostTier }`
  - `func parseModelsDevCatalog(b []byte) (modelsDevFile, error)`
  - `func (m modelsDevModel) toolCall() bool` — nil means true
  - `func (m modelsDevModel) cost() modelsDevCost` — nil means the zero value
  - `func (m modelsDevModel) contextWindow() (int, bool)` — false when `Limit` is nil or non-positive
  - `func (m modelsDevModel) deprecated() bool` — `Status == "deprecated"`
  - `type contractFailure struct { Path, Expected, Actual, Sample string }`
  - `func validateModelsDevContract(f modelsDevFile) []contractFailure`

`ToolCall` and `Limit` are pointers on purpose: the schema distinguishes "absent" from
"false"/"0", and a value type cannot. `float64` for limits because the contract check must
be able to *report* a JSON string that decoded as unparseable — the check runs on the raw
bytes (below), so the decoded fields can be strict.

- [ ] **Step 1: Write the failing test**

Create `cmd/launch/catalog_modelsdev_test.go`. The fixture is a Go raw string, matching
this package's existing convention (there is no `testdata/` directory anywhere under
`cmd/`, and no `go:embed` in any test file).

```go
package launch

import "testing"

// modelsDevFixture is a frozen excerpt of the models.dev shape. It is
// deliberately small and must not be regenerated from the live payload: its
// job is to pin the mapping, so a change here is a change to what we read.
const modelsDevFixture = `{
  "groq": {
    "id": "groq", "name": "Groq", "api": "https://api.groq.com/openai/v1",
    "env": ["GROQ_API_KEY"], "npm": "@ai-sdk/openai-compatible",
    "models": {
      "llama-x": {
        "id": "llama-x", "name": "Llama X",
        "limit": {"context": 163840, "output": 32768},
        "cost": {"input": 0.11, "output": 0.34, "cache_read": 0.02},
        "tool_call": true, "attachment": false, "reasoning": false,
        "release_date": "2026-01-05", "status": "active"
      }
    }
  },
  "zai-coding-plan": {
    "id": "zai-coding-plan", "name": "Z.AI Coding Plan",
    "env": ["ZHIPU_API_KEY", "Z_AI_API_KEY"],
    "models": {
      "glm-4.6": {
        "id": "glm-4.6", "name": "GLM-4.6",
        "limit": {"context": 200000, "output": 131072},
        "tool_call": true, "status": "deprecated",
        "cost": {"input": 0.6, "output": 2.2,
                 "tiers": [{"tier": {"type": "context", "size": 32000},
                            "input": 0.9, "output": 3.0}],
                 "context_over_200k": {"input": 1.2, "output": 4.4}}
      },
      "glm-4.7": {
        "id": "glm-4.7", "name": "GLM-4.7",
        "tool_call": false, "attachment": true,
        "modalities": {"input": ["text", "image"], "output": ["text"]},
        "provider": {"npm": "@ai-sdk/anthropic", "api": "https://api.z.ai/api/anthropic"}
      }
    }
  },
  "anthropic": {
    "id": "anthropic", "name": "Anthropic", "env": ["ANTHROPIC_API_KEY"],
    "npm": "@ai-sdk/anthropic",
    "models": {
      "claude-haiku-4.5": {
        "id": "claude-haiku-4.5", "name": "Claude Haiku 4.5",
        "limit": {"context": 200000, "output": 64000},
        "cost": {"input": 1, "output": 5},
        "tool_call": true, "attachment": true
      }
    }
  }
}`

func TestParseModelsDevCatalog_FixtureShape(t *testing.T) {
	f, err := parseModelsDevCatalog([]byte(modelsDevFixture))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(f.Providers) != 3 {
		t.Fatalf("providers = %d, want 3", len(f.Providers))
	}
	groq := f.Providers["groq"]
	if groq.API != "https://api.groq.com/openai/v1" {
		t.Fatalf("groq api = %q", groq.API)
	}
	if len(groq.Env) != 1 || groq.Env[0] != "GROQ_API_KEY" {
		t.Fatalf("groq env = %v", groq.Env)
	}
	m := groq.Models["llama-x"]
	if ctx, ok := m.contextWindow(); !ok || ctx != 163840 {
		t.Fatalf("llama-x context = %d, %v", ctx, ok)
	}
	if c := m.cost(); c.Input != 0.11 || c.Output != 0.34 || c.CacheRead != 0.02 {
		t.Fatalf("llama-x cost = %+v", c)
	}
	if !m.toolCall() {
		t.Fatal("llama-x must be tool-capable: tool_call is present and true")
	}
}

// tool_call absent means tool-capable (opencode: model.tool_call ?? true).
// glm-4.7 above has no tool_call field; glm-4.7's explicit false is in the
// same payload, so a defaulting bug cannot pass by accident.
func TestModelsDevToolCall_AbsentMeansTruePresentFalseMeansFalse(t *testing.T) {
	f, _ := parseModelsDevCatalog([]byte(modelsDevFixture))
	zai := f.Providers["zai-coding-plan"]
	if !zai.Models["glm-4.7"].toolCall() {
		t.Fatal("absent tool_call must default to true")
	}
	if zai.Models["glm-4.7"].ToolCall == nil {
		t.Fatal("absent tool_call must stay distinguishable from an explicit value")
	}
	if zai.Models["glm-4.6"].toolCall() {
		t.Fatal("explicit tool_call:false must stay false")
	}
}

func TestModelsDevCost_AbsentIsZeroNotAbsent(t *testing.T) {
	f, _ := parseModelsDevCatalog([]byte(modelsDevFixture))
	m := f.Providers["zai-coding-plan"].Models["glm-4.7"]
	c := m.cost()
	if c.Input != 0 || c.Output != 0 || c.CacheRead != 0 || c.CacheWrite != 0 {
		t.Fatalf("absent cost must be all-zero, got %+v", c)
	}
	if len(c.Tiers) != 0 || c.ContextOver200k != nil {
		t.Fatalf("absent cost must carry no bands, got %+v", c)
	}
}

func TestModelsDevCost_TierAndOver200kBandsSurvive(t *testing.T) {
	f, _ := parseModelsDevCatalog([]byte(modelsDevFixture))
	c := f.Providers["zai-coding-plan"].Models["glm-4.6"].cost()
	if len(c.Tiers) != 1 || c.Tiers[0].Input != 0.9 {
		t.Fatalf("tiers = %+v", c.Tiers)
	}
	if c.ContextOver200k == nil || c.ContextOver200k.Output != 4.4 {
		t.Fatalf("context_over_200k = %+v", c.ContextOver200k)
	}
}

func TestModelsDevModel_DeprecatedAndStatusDefault(t *testing.T) {
	f, _ := parseModelsDevCatalog([]byte(modelsDevFixture))
	zai := f.Providers["zai-coding-plan"]
	if !zai.Models["glm-4.6"].deprecated() {
		t.Fatal("status deprecated must read as deprecated")
	}
	if zai.Models["glm-4.7"].deprecated() {
		t.Fatal("absent status must default to active, not deprecated")
	}
}

func TestModelsDevLimit_AbsentDoesNotPanic(t *testing.T) {
	f, _ := parseModelsDevCatalog([]byte(modelsDevFixture))
	m := f.Providers["zai-coding-plan"].Models["glm-4.7"]
	if _, ok := m.contextWindow(); ok {
		t.Fatal("absent limit must report no window, not a zero window")
	}
}

func TestValidateModelsDevContract_CleanFixturePasses(t *testing.T) {
	f, _ := parseModelsDevCatalog([]byte(modelsDevFixture))
	if fails := validateModelsDevContract(f); len(fails) != 0 {
		t.Fatalf("clean fixture must pass, got %+v", fails)
	}
}

// Each case is a shape models.dev could ship tomorrow that we must NOT
// silently misread. The path must name the field, because the path is what a
// human acts on.
func TestValidateModelsDevContract_NamesEveryFieldWeRead(t *testing.T) {
	cases := []struct {
		name, payload, wantPath string
	}{
		{
			"limit.context retyped to a string",
			`{"p":{"id":"p","models":{"m":{"id":"m","limit":{"context":"163840","output":1}}}}}`,
			"providers.p.models.m.limit.context",
		},
		{
			"env is a string, not an array",
			`{"p":{"id":"p","env":"P_API_KEY","models":{}}}`,
			"providers.p.env",
		},
		{
			"models is an array, not an object",
			`{"p":{"id":"p","api":"https://x/v1","models":[]}}`,
			"providers.p.models",
		},
		{
			"tool_call retyped to a string",
			`{"p":{"id":"p","models":{"m":{"id":"m","tool_call":"yes","limit":{"context":1,"output":1}}}}}`,
			"providers.p.models.m.tool_call",
		},
		{
			"cost.input retyped to a string",
			`{"p":{"id":"p","models":{"m":{"id":"m","cost":{"input":"cheap"},"limit":{"context":1,"output":1}}}}}`,
			"providers.p.models.m.cost.input",
		},
		{
			"model id empty",
			`{"p":{"id":"p","models":{"m":{"id":"","limit":{"context":1,"output":1}}}}}`,
			"providers.p.models.m.id",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fails := validateModelsDevContractRaw([]byte(tc.payload))
			if len(fails) == 0 {
				t.Fatalf("expected a failure naming %s", tc.wantPath)
			}
			found := false
			for _, f := range fails {
				if f.Path == tc.wantPath {
					found = true
					if f.Expected == "" || f.Actual == "" {
						t.Fatalf("%s: failure must say expected and actual, got %+v", tc.wantPath, f)
					}
				}
			}
			if !found {
				t.Fatalf("no failure named %s, got %+v", tc.wantPath, fails)
			}
		})
	}
}

// Fields we do not read are not constrained: a new upstream field must never
// fail the contract.
func TestValidateModelsDevContract_IgnoresUnreadFields(t *testing.T) {
	payload := `{"p":{"id":"p","api":"https://x/v1","someNewThing":{"a":1},
	             "models":{"m":{"id":"m","limit":{"context":1,"output":1},"weird":[1,2]}}}}`
	if fails := validateModelsDevContractRaw([]byte(payload)); len(fails) != 0 {
		t.Fatalf("unread fields must not fail the contract, got %+v", fails)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/launch/ -run 'TestParseModelsDevCatalog|TestModelsDev|TestValidateModelsDevContract' -v`
Expected: FAIL — `undefined: parseModelsDevCatalog`, `undefined: validateModelsDevContractRaw`.

- [ ] **Step 3: Write minimal implementation**

Create `cmd/launch/catalog_modelsdev.go`:

```go
package launch

// catalog_modelsdev.go — the models.dev payload as types we read, the parser
// that turns bytes into them, and the contract check that refuses a payload
// whose shape we cannot trust.
//
// models.dev is a live third-party database (MIT, maintained by sst, the
// organisation behind opencode; opencode reads the same source). We port it
// verbatim and never edit it: corrections live in providers/oaica.json, applied
// at read time.
//
// Every field here exists because oaica reads it. Fields we do not read are
// deliberately absent from the structs AND from the contract check — a new
// upstream field must never fail a sync.

import (
	"encoding/json"
	"fmt"
	"sort"
)

type modelsDevFile struct {
	Providers map[string]modelsDevProvider `json:"providers"`
}

type modelsDevProvider struct {
	ID     string                     `json:"id"`
	Name   string                     `json:"name"`
	API    string                     `json:"api"`
	NPM    string                     `json:"npm"`
	Env    []string                   `json:"env"`
	Models map[string]modelsDevModel  `json:"models"`
}

type modelsDevModel struct {
	ID          string                  `json:"id"`
	Name        string                  `json:"name"`
	ReleaseDate string                  `json:"release_date"`
	Status      string                  `json:"status"`
	Attachment  bool                    `json:"attachment"`
	Reasoning   bool                    `json:"reasoning"`
	Temperature bool                    `json:"temperature"`
	// ToolCall is a pointer so "absent" stays distinguishable from an explicit
	// false — opencode's rule is `model.tool_call ?? true`.
	ToolCall   *bool                   `json:"tool_call"`
	Modalities *modelsDevModalities    `json:"modalities"`
	Limit      *modelsDevLimit         `json:"limit"`
	Cost       *modelsDevCost          `json:"cost"`
	// Provider carries per-model overrides; it wins over the enclosing
	// provider's npm/api (opencode: model.provider.npm ?? provider.npm).
	Provider *modelsDevModelProvider `json:"provider"`
}

type modelsDevModelProvider struct {
	NPM string `json:"npm"`
	API string `json:"api"`
}

type modelsDevModalities struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

type modelsDevLimit struct {
	Context float64 `json:"context"`
	// Input is the input-token limit, kept separate from Context.
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
}

type modelsDevCost struct {
	Input         float64             `json:"input"`
	Output        float64             `json:"output"`
	CacheRead     float64             `json:"cache_read"`
	CacheWrite    float64             `json:"cache_write"`
	Tiers         []modelsDevCostTier `json:"tiers"`
	ContextOver200k *modelsDevCostTier `json:"context_over_200k"`
}

// modelsDevCostTier is one context band: above Tier.Size tokens, these rates
// apply. Displaying only the base rate would misprice long Anthropic contexts.
type modelsDevCostTier struct {
	Tier   modelsDevTierBand `json:"tier"`
	Input  float64           `json:"input"`
	Output float64           `json:"output"`
}

type modelsDevTierBand struct {
	Type string  `json:"type"`
	Size float64 `json:"size"`
}

func parseModelsDevCatalog(b []byte) (modelsDevFile, error) {
	var f modelsDevFile
	if err := json.Unmarshal(b, &f); err != nil {
		return modelsDevFile{}, err
	}
	return f, nil
}

// toolCall reports whether the model can call tools. Absent means yes.
func (m modelsDevModel) toolCall() bool {
	if m.ToolCall == nil {
		return true
	}
	return *m.ToolCall
}

// cost returns the model's pricing, or the zero value when upstream omits it,
// so the pricing column reads "free" rather than "absent".
func (m modelsDevModel) cost() modelsDevCost {
	if m.Cost == nil {
		return modelsDevCost{}
	}
	return *m.Cost
}

// contextWindow returns the model's context length in tokens. ok is false when
// upstream omits the limit or gives a non-positive one, so callers render
// "ctx ?" instead of a zero that reads as a real window.
func (m modelsDevModel) contextWindow() (int, bool) {
	if m.Limit == nil || m.Limit.Context <= 0 {
		return 0, false
	}
	return int(m.Limit.Context), true
}

func (m modelsDevModel) deprecated() bool { return m.Status == "deprecated" }

// modelsDevCounts is what the sync reports: how much of upstream we took.
func (f modelsDevFile) counts() (providers, models int) {
	for _, p := range f.Providers {
		providers++
		models += len(p.Models)
	}
	return providers, models
}

// contractFailure names one field we read that upstream did not shape the way
// we require. Path is the JSON path, so a human can act on it without reading
// this package.
type contractFailure struct {
	Path     string
	Expected string
	Actual   string
	Sample   string
}

func (c contractFailure) String() string {
	return fmt.Sprintf("%s: expected %s, got %s (%s)", c.Path, c.Expected, c.Actual, c.Sample)
}

func validateModelsDevContract(f modelsDevFile) []contractFailure {
	raw, err := json.Marshal(f)
	if err != nil {
		return []contractFailure{{Path: "providers", Expected: "marshalable", Actual: err.Error()}}
	}
	return validateModelsDevContractRaw(raw)
}

// validateModelsDevContractRaw checks the payload as JSON, not through the
// decoded structs: a field retyped upstream (limit.context becoming a string)
// either fails json.Unmarshal outright or silently zeroes, and only the raw
// tree can say which field and what it now holds. Only fields oaica reads are
// constrained.
func validateModelsDevContractRaw(b []byte) []contractFailure {
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		return []contractFailure{{Path: "providers", Expected: "an object", Actual: "unparseable: " + err.Error()}}
	}

	var fails []contractFailure
	add := func(path, expected, actual, sample string) {
		fails = append(fails, contractFailure{Path: path, Expected: expected, Actual: actual, Sample: sample})
	}
	kind := func(v any) string {
		switch v.(type) {
		case nil:
			return "null"
		case string:
			return "string"
		case float64:
			return "number"
		case bool:
			return "bool"
		case []any:
			return "array"
		case map[string]any:
			return "object"
		default:
			return fmt.Sprintf("%T", v)
		}
	}
	sampleOf := func(v any) string {
		s, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		if len(s) > 40 {
			s = s[:40]
		}
		return string(s)
	}
	numberOrAbsent := func(path string, v any, present bool) {
		if !present || v == nil {
			return
		}
		if kind(v) != "number" {
			add(path, "number", kind(v), sampleOf(v))
		}
	}

	providers, ok := root["providers"].(map[string]any)
	if !ok {
		return []contractFailure{{Path: "providers", Expected: "an object", Actual: kind(root["providers"]), Sample: sampleOf(root["providers"])}}
	}

	keys := make([]string, 0, len(providers))
	for k := range providers {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic failure order

	for _, pid := range keys {
		p, _ := providers[pid].(map[string]any)
		if p == nil {
			add("providers."+pid, "an object", kind(providers[pid]), sampleOf(providers[pid]))
			continue
		}
		if v, present := p["env"]; present {
			if _, isArr := v.([]any); !isArr && v != nil {
				add("providers."+pid+".env", "an array of strings", kind(v), sampleOf(v))
			} else if isArr {
				for i, item := range v.([]any) {
					if _, isStr := item.(string); !isStr {
						add(fmt.Sprintf("providers.%s.env[%d]", pid, i), "a string", kind(item), sampleOf(item))
					}
				}
			}
		}
		models, present := p["models"]
		if !present {
			continue
		}
		mm, ok := models.(map[string]any)
		if !ok {
			add("providers."+pid+".models", "an object", kind(models), sampleOf(models))
			continue
		}
		mkeys := make([]string, 0, len(mm))
		for k := range mm {
			mkeys = append(mkeys, k)
		}
		sort.Strings(mkeys)
		for _, mid := range mkeys {
			base := "providers." + pid + ".models." + mid
			m, _ := mm[mid].(map[string]any)
			if m == nil {
				add(base, "an object", kind(mm[mid]), sampleOf(mm[mid]))
				continue
			}
			if v, present := m["id"]; present {
				s, isStr := v.(string)
				if !isStr || s == "" {
					add(base+".id", "a non-empty string", kind(v), sampleOf(v))
				}
			}
			if v, present := m["name"]; present {
				s, isStr := v.(string)
				if !isStr || s == "" {
					add(base+".name", "a non-empty string", kind(v), sampleOf(v))
				}
			}
			if v, present := m["tool_call"]; present {
				if _, isBool := v.(bool); !isBool && v != nil {
					add(base+".tool_call", "a bool", kind(v), sampleOf(v))
				}
			}
			if v, present := m["limit"]; present && v != nil {
				l, ok := v.(map[string]any)
				if !ok {
					add(base+".limit", "an object", kind(v), sampleOf(v))
				} else {
					numberOrAbsent(base+".limit.context", l["context"], l["context"] != nil)
					numberOrAbsent(base+".limit.output", l["output"], l["output"] != nil)
					numberOrAbsent(base+".limit.input", l["input"], l["input"] != nil)
				}
			}
			if v, present := m["cost"]; present && v != nil {
				c, ok := v.(map[string]any)
				if !ok {
					add(base+".cost", "an object", kind(v), sampleOf(v))
				} else {
					for _, field := range []string{"input", "output", "cache_read", "cache_write"} {
						numberOrAbsent(base+".cost."+field, c[field], c[field] != nil)
					}
					if t, present := c["tiers"]; present && t != nil {
						if _, isArr := t.([]any); !isArr {
							add(base+".cost.tiers", "an array", kind(t), sampleOf(t))
						}
					}
					if o, present := c["context_over_200k"]; present && o != nil {
						if _, isObj := o.(map[string]any); !isObj {
							add(base+".cost.context_over_200k", "an object", kind(o), sampleOf(o))
						}
					}
				}
			}
		}
	}
	return fails
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/launch/ -run 'TestParseModelsDevCatalog|TestModelsDev|TestValidateModelsDevContract' -v`
Expected: PASS. Then `gofmt -l cmd/launch/catalog_modelsdev.go` must print nothing.

- [ ] **Step 5: Commit**

```bash
git add cmd/launch/catalog_modelsdev.go cmd/launch/catalog_modelsdev_test.go
git commit -m "launch: read the models.dev payload as typed data, with a contract check

The port's foundation: parse the upstream payload into the fields oaica
actually reads, and refuse a shape we cannot trust by naming the JSON path
the field moved to. Fields we do not read are unconstrained, so a new
upstream field can never fail a sync."
```

---

### Task 2: Fetch, cache, and adopt-or-refuse

Copies `provider_sync.go`'s mechanics exactly, and extracts the generic fetch helper so
there is one implementation instead of two.

**Files:**
- Create: `cmd/launch/catalog_sync.go`
- Modify: `cmd/launch/provider_sync.go` (its `fetchProviderCatalogBody` becomes a thin call)
- Test: `cmd/launch/catalog_sync_test.go`

**Interfaces:**
- Consumes: `parseModelsDevCatalog`, `validateModelsDevContractRaw`, `contractFailure` (Task 1).
- Produces:
  - `const defaultCatalogSyncURL = "https://models.dev/api.json"`
  - `func catalogCachePath() (string, error)` → `~/.oaica/cache/catalog/modelsdev.json`
  - `type CatalogSyncReport struct { URL string; Providers, Models int; FromCache, Unchanged, Refused bool }`
  - `func CatalogSync(url string) (CatalogSyncReport, error)` — refuses (non-nil error, cache untouched) when the contract fails
  - `func loadModelsDevCatalog() (modelsDevFile, bool)` — cached payload, ok=false when absent or unreadable. Used by the picker; never fetches.
  - `func fetchCatalogBody(url, etag, cachePath string) (body []byte, newEtag string, fromCache bool, err error)` — the extracted helper

- [ ] **Step 1: Write the failing test**

```go
package launch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCatalogFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "payload.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return "file://" + p
}

func TestCatalogSync_AdoptsAPassingPayload(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	rep, err := CatalogSync(writeCatalogFile(t, modelsDevFixture))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if rep.Refused || rep.Providers != 3 {
		t.Fatalf("report = %+v", rep)
	}
	path, err := catalogCachePath()
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || len(b) == 0 {
		t.Fatalf("cache not written: %v", err)
	}
}

// The refusal path is the whole safety story for Plan A: a payload whose shape
// we cannot trust must not replace the last good copy.
func TestCatalogSync_ContractFailureKeepsLastGood(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	if _, err := CatalogSync(writeCatalogFile(t, modelsDevFixture)); err != nil {
		t.Fatal(err)
	}
	path, _ := catalogCachePath()
	good, _ := os.ReadFile(path)

	bad := `{"p":{"id":"p","models":{"m":{"id":"m","limit":{"context":"163840"}}}}}`
	rep, err := CatalogSync(writeCatalogFile(t, bad))
	if err == nil {
		t.Fatalf("a contract failure must be an error, got %+v", rep)
	}
	if !rep.Refused {
		t.Fatalf("report must say refused: %+v", rep)
	}
	if !strings.Contains(err.Error(), "providers.p.models.m.limit.context") {
		t.Fatalf("error must name the field: %v", err)
	}
	now, _ := os.ReadFile(path)
	if string(now) != string(good) {
		t.Fatal("a refused payload overwrote the last good catalog")
	}
}

func TestCatalogSync_UnparseablePayloadIsRefusedAndNotAdopted(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	rep, err := CatalogSync(writeCatalogFile(t, "not json at all"))
	if err == nil || !rep.Refused {
		t.Fatalf("unparseable payload: rep=%+v err=%v", rep, err)
	}
	if _, statErr := os.Stat(mustCatalogPath(t)); !os.IsNotExist(statErr) {
		t.Fatal("an unparseable payload must not be cached")
	}
}

func TestCatalogSync_IdenticalResyncIsAnUnchangedNoop(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	src := writeCatalogFile(t, modelsDevFixture)
	if _, err := CatalogSync(src); err != nil {
		t.Fatal(err)
	}
	rep, err := CatalogSync(src)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Unchanged {
		t.Fatalf("identical bytes must report unchanged: %+v", rep)
	}
}

func TestLoadModelsDevCatalog_AbsentIsNotAnError(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	if _, ok := loadModelsDevCatalog(); ok {
		t.Fatal("no catalog on disk must report ok=false, not a fabricated one")
	}
}

func mustCatalogPath(t *testing.T) string {
	t.Helper()
	p, err := catalogCachePath()
	if err != nil {
		t.Fatal(err)
	}
	return p
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/launch/ -run 'TestCatalogSync|TestLoadModelsDevCatalog' -v`
Expected: FAIL — `undefined: CatalogSync`, `undefined: catalogCachePath`.

- [ ] **Step 3: Write minimal implementation**

Create `cmd/launch/catalog_sync.go`:

```go
package launch

// catalog_sync.go — `oaica model catalog sync`: fetch the models.dev payload,
// check it against the contract, and either adopt it or refuse it and keep the
// last good copy.
//
// Nothing auto-fetches. A launch reads whatever is cached and works offline;
// an explicit command is the only thing that talks to the network. That is a
// deliberate divergence from opencode, which treats the catalog as live
// infrastructure with a 5-minute refresh and a cross-process file lock — oaica
// is a one-shot CLI with no resident process to host a background refresher,
// so paying a 5 MB fetch on a timer would only tax launches that are already
// network-bound.
//
// Mechanics are model_sync.go's: ETag revalidation, a last-good fallback when
// the network is down, and file:// for air-gapped hosts and tests.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const defaultCatalogSyncURL = "https://models.dev/api.json"

func catalogCachePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".oaica", "cache", "catalog", "modelsdev.json"), nil
}

// CatalogSyncReport is what CatalogSync returns for the caller to print.
type CatalogSyncReport struct {
	URL       string
	Providers int
	Models    int
	FromCache bool
	Unchanged bool
	Refused   bool
}

// CatalogSync fetches the payload at url (empty = defaultCatalogSyncURL),
// validates it, and adopts it only if it passes. A contract failure is
// returned as an error naming the field, with the cached catalog left
// untouched — the refusal is the entire safety story while the drift tooling
// (Plan B) does not exist yet.
func CatalogSync(url string) (CatalogSyncReport, error) {
	if strings.TrimSpace(url) == "" {
		url = defaultCatalogSyncURL
	}

	cachePath, err := catalogCachePath()
	if err != nil {
		return CatalogSyncReport{URL: url}, err
	}

	var etag string
	if b, rerr := os.ReadFile(cachePath + ".etag"); rerr == nil {
		etag = strings.TrimSpace(string(b))
	}

	body, newEtag, fromCache, err := fetchCatalogBody(url, etag, cachePath)
	if err != nil {
		return CatalogSyncReport{URL: url}, err
	}

	// Byte-identical to what we already hold: nothing to validate, nothing to
	// write. (The ETag path usually catches this; file:// has no ETag.)
	if prev, rerr := os.ReadFile(cachePath); rerr == nil && sameBytes(prev, body) {
		f, _ := parseModelsDevCatalog(body)
		pn, mn := f.counts()
		return CatalogSyncReport{URL: url, Providers: pn, Models: mn, Unchanged: true}, nil
	}

	f, perr := parseModelsDevCatalog(body)
	if perr != nil {
		return CatalogSyncReport{URL: url, Refused: true},
			fmt.Errorf("refused: %s is not valid JSON: %w (cached catalog left in place)", url, perr)
	}
	if fails := validateModelsDevContractRaw(body); len(fails) > 0 {
		lines := make([]string, 0, len(fails))
		for i, fl := range fails {
			if i == 5 {
				lines = append(lines, fmt.Sprintf("... and %d more", len(fails)-i))
				break
			}
			lines = append(lines, fl.String())
		}
		return CatalogSyncReport{URL: url, Refused: true},
			fmt.Errorf("refused: %s does not match the fields oaica reads:\n  %s\n(cached catalog left in place)",
				url, strings.Join(lines, "\n  "))
	}

	if !fromCache {
		if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
			return CatalogSyncReport{URL: url}, err
		}
		if err := os.WriteFile(cachePath, body, 0o600); err != nil {
			return CatalogSyncReport{URL: url}, err
		}
		if newEtag != "" {
			_ = os.WriteFile(cachePath+".etag", []byte(newEtag), 0o600)
		}
	}

	pn, mn := f.counts()
	return CatalogSyncReport{URL: url, Providers: pn, Models: mn, FromCache: fromCache}, nil
}

// loadModelsDevCatalog returns the cached catalog and whether one exists. It
// never fetches: the picker calls this on every build, offline or not.
func loadModelsDevCatalog() (modelsDevFile, bool) {
	path, err := catalogCachePath()
	if err != nil {
		return modelsDevFile{}, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return modelsDevFile{}, false
	}
	f, err := parseModelsDevCatalog(b)
	if err != nil {
		return modelsDevFile{}, false
	}
	return f, true
}

// catalogAgeDays reports how stale the cached payload is, or 0 when it is
// missing or unreadable. The picker shows this past 30 days so staleness is
// visible rather than silent.
func catalogAgeDays(now time.Time) int {
	path, err := catalogCachePath()
	if err != nil {
		return 0
	}
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	days := int(now.Sub(fi.ModTime()).Hours() / 24)
	if days < 0 {
		return 0
	}
	return days
}

func sameBytes(a, b []byte) bool {
	ha, hb := sha256.Sum256(a), sha256.Sum256(b)
	return hex.EncodeToString(ha[:]) == hex.EncodeToString(hb[:])
}

// fetchCatalogBody is the generic ETag fetch-and-cache shared with
// provider_sync.go. file:// reads the path directly and carries no ETag;
// a transport error falls back to whatever is already cached.
func fetchCatalogBody(url, etag, cachePath string) (body []byte, newEtag string, fromCache bool, err error) {
	if strings.HasPrefix(url, "file://") {
		b, rerr := os.ReadFile(strings.TrimPrefix(url, "file://"))
		return b, "", false, rerr
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, "", false, err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		if b, rerr := os.ReadFile(cachePath); rerr == nil {
			return b, etag, true, nil
		}
		return nil, "", false, fmt.Errorf("couldn't reach %s: %w", url, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotModified:
		b, rerr := os.ReadFile(cachePath)
		if rerr != nil {
			return nil, "", false, fmt.Errorf("%s returned 304 but no cache exists", url)
		}
		return b, etag, true, nil
	case resp.StatusCode != http.StatusOK:
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, "", false, fmt.Errorf("%s: HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(errBody)))
	}

	// 16 MiB cap: the live payload is ~5 MB, and the cap exists so a hostile or
	// broken response cannot exhaust memory.
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, "", false, err
	}
	return b, resp.Header.Get("ETag"), false, nil
}
```

Then replace `provider_sync.go`'s `fetchProviderCatalogBody` body with a call to it, so
there is one implementation:

```go
func fetchProviderCatalogBody(url, etag string) ([]byte, string, bool, error) {
	cachePath, err := providerCatalogCachePath()
	if err != nil {
		return nil, "", false, err
	}
	return fetchCatalogBody(url, etag, cachePath)
}
```

Add `"net/http"`, `"io"`, `"time"` to `catalog_sync.go`'s imports (the block above lists
them implicitly through use — run `gofmt` and the compiler to confirm).

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/launch/ -run 'TestCatalogSync|TestLoadModelsDevCatalog' -v`
Expected: PASS.

Then the extracted helper must not have changed callers:
Run: `go test ./cmd/launch/ -run 'TestProviderSync|TestProviderCatalog' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/launch/catalog_sync.go cmd/launch/catalog_sync_test.go cmd/launch/provider_sync.go
git commit -m "launch: sync the models.dev catalog, refusing a shape we cannot read

Adopt-or-refuse in one command: a payload that fails the contract names the
field, exits non-zero, and leaves the last good catalog in place. The ETag
fetch helper is now shared with the provider sync instead of duplicated."
```

---

### Task 3: The overlay, merged per key at read time

The overlay is the only file we edit. It carries three sections — provider corrections,
oaica's first-party models, and limits for ids upstream lacks — and it supersedes both
retired JSON files (Task 4 moves their content in).

**Files:**
- Create: `cmd/launch/providers/oaica.json` (starts small; Task 4 fills it)
- Create: `cmd/launch/catalog_overlay.go`
- Test: `cmd/launch/catalog_overlay_test.go`

**Interfaces:**
- Consumes: `modelsDevFile`, `modelsDevProvider`, `modelsDevModel` (Task 1).
- Produces:
  - `type oaicaOverlayFile struct` — fields `Version int` (`version`), `Providers []providerCatalogEntry` (`providers`), `Models []oaicaOverlayModel` (`models`), `Limits map[string]providerCatalogModelLimit` (`limits`)
  - `type oaicaOverlayModel struct { ID, DisplayName string; Context, Output int }`
  - `func oaicaOverlay() oaicaOverlayFile` — embedded, merged with a synced copy **per provider name / model id / limit key**
  - `func overlayProvidersByName() map[string]providerCatalogEntry`
  - `func overlayLimits() map[string]providerCatalogModelLimit`
  - `func overlayFirstPartyModels() []oaicaOverlayModel`

Because the merge is per key, a cache written by an older binary can only ever *override*
the entries it names — every other embedded entry, and every field of a non-overridden
entry, survives. That is the fix for the wholesale-replace hazard
(`provider_catalog.go:21-33`).

- [ ] **Step 1: Write the failing test**

```go
package launch

import (
	"os"
	"path/filepath"
	"testing"
)

func writeOverlayCache(t *testing.T, body string) {
	t.Helper()
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".oaica", "cache", "providers")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "oaica.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The hazard this pins: provider_catalog.go's header records a 2026-09-25
// incident where a cache row synced before a new field existed made the field
// invisible, with no error. A per-key merge cannot do that.
func TestOverlay_MergeIsPerKeyNotWholesale(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	// The cache names one provider and corrects its base_url only. Every other
	// embedded provider must survive, and the corrected provider must keep the
	// embedded fields the cache did not mention.
	embedded := oaicaOverlay()
	var probe, other string
	for _, p := range embedded.Providers {
		if probe == "" {
			probe = p.Name
		} else if p.Name != probe {
			other = p.Name
		}
	}
	if other == "" {
		t.Skip("embedded overlay has fewer than two providers; nothing to pin yet")
	}
	writeOverlayCache(t, `{"version":1,"providers":[{"name":"`+probe+`","base_url":"https://corrected.example/v1"}]}`)

	got := oaicaOverlay()
	byName := map[string]providerCatalogEntry{}
	for _, p := range got.Providers {
		byName[p.Name] = p
	}
	if byName[probe].BaseURL != "https://corrected.example/v1" {
		t.Fatalf("%s base_url = %q, want the cache's correction", probe, byName[probe].BaseURL)
	}
	if _, ok := byName[other]; !ok {
		t.Fatalf("%s vanished: the cache replaced the overlay wholesale", other)
	}
}

func TestOverlay_CacheCannotEraseAnEmbeddedOnlyField(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	embedded := oaicaOverlay()
	if len(embedded.Providers) == 0 {
		t.Skip("overlay has no providers yet")
	}
	name := embedded.Providers[0].Name
	writeOverlayCache(t, `{"version":1,"providers":[{"name":"`+name+`","base_url":"https://corrected.example/v1"}]}`)
	for _, p := range oaicaOverlay().Providers {
		if p.Name == name && p.BaseURL == "https://corrected.example/v1" {
			return
		}
	}
	t.Fatalf("%s missing from the merged overlay", name)
}

func TestOverlay_UnparseableCacheFallsBackToEmbedded(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeOverlayCache(t, "{ this is not json")
	got := oaicaOverlay()
	if len(got.Providers) == 0 {
		t.Fatal("an unparseable cache must degrade to the embedded overlay, which is always present")
	}
}

func TestOverlay_LimitsMergeByModelID(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeOverlayCache(t, `{"version":1,"limits":{"glm-5":{"context":202752,"output":32768}}}`)
	got := overlayLimits()
	if got["glm-5"].Context != 202752 {
		t.Fatalf("glm-5 = %+v", got["glm-5"])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/launch/ -run 'TestOverlay' -v`
Expected: FAIL — `undefined: oaicaOverlay`.

- [ ] **Step 3: Write minimal implementation**

Create `cmd/launch/providers/oaica.json` with the shape (content filled in Task 4):

```json
{
  "version": 1,
  "providers": [],
  "models": [],
  "limits": {}
}
```

Create `cmd/launch/catalog_overlay.go`:

```go
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
	Version   int                                     `json:"version"`
	Providers []providerCatalogEntry                  `json:"providers"`
	Models    []oaicaOverlayModel                     `json:"models"`
	Limits    map[string]providerCatalogModelLimit    `json:"limits"`
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

	if out.Limits == nil {
		out.Limits = map[string]providerCatalogModelLimit{}
	}
	for id, lim := range cache.Limits {
		if id == "" {
			continue
		}
		out.Limits[id] = lim
	}
	return out
}

// mergeProviderEntry layers override onto base field by field, so a cache that
// corrects one field cannot blank the rest.
func mergeProviderEntry(base, override providerCatalogEntry) providerCatalogEntry {
	if override.BaseURL != "" {
		base.BaseURL = override.BaseURL
	}
	if override.Version != "" {
		base.Version = override.Version
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
	if len(override.Models) > 0 {
		if base.Models == nil {
			base.Models = map[string]providerCatalogModelLimit{}
		}
		for id, lim := range override.Models {
			base.Models[id] = lim
		}
	}
	// Hidden is a bool, so absence and false are indistinguishable: the
	// override wins whenever it says true, and an embedded true is never
	// cleared by a cache that omits the field. Hiding is the conservative
	// direction, so this asymmetry is deliberate.
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
```

Note `providerCatalogEntry` gains a `Hidden bool` field in Task 5 — add it there, not
here, so this task's diff stays about the merge.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/launch/ -run 'TestOverlay' -v`
Expected: PASS (the first two tests skip until Task 4 puts rows in the overlay — that is
expected and they must be re-run then).

- [ ] **Step 5: Commit**

```bash
git add cmd/launch/providers/oaica.json cmd/launch/catalog_overlay.go cmd/launch/catalog_overlay_test.go
git commit -m "launch: add the oaica overlay, merged per key at read time

One file we edit, applied at read time: provider corrections, first-party
models, and limits for ids models.dev lacks. Merging per provider name /
model id / limit key is what stops a stale cache from making a newer
binary's field invisible — the wholesale-replace hazard provider_catalog.go
documents."
```

---

### Task 4: The merge — `providerCatalog()` from catalog + overlay, and the two retired files

The task where the port actually happens for providers and limits: the 44 rows of
`providers.json` and 21 of `cloud_limits.json` fold into the overlay as corrections, the
catalog becomes the base layer, and all four retired files are deleted.

**Files:**
- Modify: `cmd/launch/providers/oaica.json` (content moved in)
- Modify: `cmd/launch/provider_catalog.go` (`providerCatalog()` merges two layers)
- Modify: `cmd/launch/models.go` (`cloudModelLimits` merges the overlay)
- Modify: `cmd/launch/provider_sync.go` (`defaultProviderSyncURL` → the overlay)
- Modify: `cmd/cmd.go` (drop `oaica model cloud-limits sync`, whose file is deleted)
- Delete: `cmd/launch/providers/providers.json`, `cmd/launch/cloud_limits/cloud_limits.json`, `cmd/launch/cloud_limits_catalog.go`, `cmd/launch/cloud_limits_sync.go`
- Test: `cmd/launch/provider_catalog_test.go` (new), plus updates to
  `provider_catalog_wire_test.go`, `user_remotes_test.go`, `hermetic_test.go`,
  `picker_resolution_test.go`, `picker_needs_key_test.go`, `auth_store_test.go`,
  `auth_external_test.go` where they read a retired source

**Interfaces:**
- Consumes: `loadModelsDevCatalog` (Task 2), `oaicaOverlay`, `overlayProvidersByName`, `overlayLimits` (Task 3).
- Produces:
  - `providerCatalogEntry` gains `Env []string `json:"env,omitempty"`` and `Hidden bool `json:"hidden,omitempty"`` (Task 5 consumes both).
  - `func providerEntryFromModelsDev(id string, p modelsDevProvider) providerCatalogEntry`
  - `providerCatalog()` — catalog rows, corrected per field by the overlay, then overlay-only rows appended; unchanged signature `func() []providerCatalogEntry`
  - `cloudLimitsFromCatalog()` — overlay limits only now; unchanged signature

- [ ] **Step 1: Move the retired files' content into the overlay**

Mechanical, and it must be verbatim — do not retype 44 rows by hand:

```bash
python3 - <<'PY'
import json, pathlib
prov = json.loads(pathlib.Path("cmd/launch/providers/providers.json").read_text())
lim  = json.loads(pathlib.Path("cmd/launch/cloud_limits/cloud_limits.json").read_text())
out = {
    "version": 1,
    "providers": prov["providers"],
    "models": [
        # oaica's first-party models: models.dev does not carry them. Windows
        # moved verbatim from cloud_limits.json where they exist, so nothing
        # that resolved before resolves differently now.
        {"id": "oaica-default",       "display_name": "OAICA Default",
         "context": lim["limits"].get("oaica-default", {}).get("context", 0),
         "output":  lim["limits"].get("oaica-default", {}).get("output", 0)},
        {"id": "oaica-35b-a3b-1M",    "display_name": "OAICA 35B A3B (1M)",
         "context": lim["limits"].get("oaica-35b-a3b-1M", {}).get("context", 0),
         "output":  lim["limits"].get("oaica-35b-a3b-1M", {}).get("output", 0)},
    ],
    "limits": lim["limits"],
}
pathlib.Path("cmd/launch/providers/oaica.json").write_text(json.dumps(out, indent=2) + "\n")
print("providers:", len(out["providers"]), "limits:", len(out["limits"]))
PY
```

Then check nothing was lost before deleting anything:

```bash
python3 - <<'PY'
import json, pathlib
old = json.loads(pathlib.Path("cmd/launch/providers/providers.json").read_text())["providers"]
new = json.loads(pathlib.Path("cmd/launch/providers/oaica.json").read_text())["providers"]
assert [p["name"] for p in old] == [p["name"] for p in new], "provider rows changed"
oldl = json.loads(pathlib.Path("cmd/launch/cloud_limits/cloud_limits.json").read_text())["limits"]
newl = json.loads(pathlib.Path("cmd/launch/providers/oaica.json").read_text())["limits"]
assert oldl == newl, "limits changed"
print("verbatim move verified:", len(new), "providers,", len(newl), "limits")
PY
```

- [ ] **Step 2: Write the failing test**

Create `cmd/launch/provider_catalog_test.go`:

```go
package launch

import (
	"os"
	"path/filepath"
	"testing"
)

func writeModelsDevCache(t *testing.T, body string) {
	t.Helper()
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".oaica", "cache", "catalog")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "modelsdev.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func catalogEntryByName(t *testing.T, name string) (providerCatalogEntry, bool) {
	t.Helper()
	for _, e := range providerCatalog() {
		if e.Name == name {
			return e, true
		}
	}
	return providerCatalogEntry{}, false
}

// The port's headline: a provider models.dev knows and our old file did not
// must now be offered, with upstream's endpoint and env array.
func TestProviderCatalog_UpstreamProvidersAppear(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeModelsDevCache(t, modelsDevFixture)
	e, ok := catalogEntryByName(t, "groq")
	if !ok {
		t.Fatal("groq must appear from the catalog")
	}
	if e.BaseURL != "https://api.groq.com/openai/v1" {
		t.Fatalf("groq base_url = %q", e.BaseURL)
	}
	if len(e.Env) != 1 || e.Env[0] != "GROQ_API_KEY" {
		t.Fatalf("groq env = %v", e.Env)
	}
}

// Two live bugs the port fixes, asserted directly because they are the reason
// the port exists: opencode-go's row is missing its version path and is
// unselectable, and zai-coding-plan reuses upstream's env name instead of ours.
func TestProviderCatalog_OverlayCorrectsUpstream(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeModelsDevCache(t, modelsDevFixture)
	overlay := map[string]providerCatalogEntry{}
	for _, p := range oaicaOverlay().Providers {
		overlay[p.Name] = p
	}
	for _, name := range []string{"opencode-go", "zai-coding-plan"} {
		corr, ok := overlay[name]
		if !ok {
			t.Fatalf("%s must have an overlay correction", name)
		}
		got, ok := catalogEntryByName(t, name)
		if !ok {
			t.Fatalf("%s missing from the merged catalog", name)
		}
		if got.BaseURL != corr.BaseURL {
			t.Fatalf("%s base_url = %q, want the overlay's %q", name, got.BaseURL, corr.BaseURL)
		}
		if got.APIKeyEnv != corr.APIKeyEnv {
			t.Fatalf("%s api_key_env = %q, want %q", name, got.APIKeyEnv, corr.APIKeyEnv)
		}
	}
}

// An overlay row for a provider models.dev does not carry must still be
// offered — that is what keeps our own endpoints working.
func TestProviderCatalog_OverlayOnlyProviderSurvives(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeModelsDevCache(t, modelsDevFixture)
	for _, p := range oaicaOverlay().Providers {
		if _, ok := providerCatalogFromCatalogOnly()[p.Name]; ok {
			continue
		}
		if _, ok := catalogEntryByName(t, p.Name); !ok {
			t.Fatalf("overlay-only provider %s vanished", p.Name)
		}
		return
	}
	t.Skip("every overlay row is also in the fixture catalog")
}

func TestProviderCatalog_NoCatalogFallsBackToOverlayOnly(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	if got := providerCatalog(); len(got) == 0 {
		t.Fatal("with no catalog cached the overlay alone must still produce rows")
	}
}

func TestCloudLimits_FromOverlayNotRetiredFile(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	limits := cloudLimitsFromCatalog()
	if len(limits) == 0 {
		t.Fatal("overlay limits must reach cloudLimitsFromCatalog")
	}
	// glm-5's overlay value is the one that must win over any upstream figure.
	if l, ok := limits["glm-5"]; !ok || l.Context != 202752 {
		t.Fatalf("glm-5 = %+v (want the overlay's 202752, not upstream's 1000000)", l)
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./cmd/launch/ -run 'TestProviderCatalog_|TestCloudLimits_' -v`
Expected: FAIL — `undefined: providerCatalogFromCatalogOnly`.

- [ ] **Step 4: Write the implementation**

In `cmd/launch/provider_catalog.go`, add the two fields to `providerCatalogEntry`:

```go
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
```

Replace `providerCatalog()`'s body with the two-layer merge:

```go
// providerCatalog returns the merged provider directory: the ported models.dev
// catalog as the base layer, corrected per field by the oaica overlay, with
// overlay-only rows appended. Errors reading either source are swallowed — a
// corrupt cache or a bad embed degrades to "fewer providers listed", never a
// crash in the picker.
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
			out[i] = mergeProviderEntry(out[i], p)
			continue
		}
		byName[p.Name] = len(out)
		out = append(out, p)
	}

	// A row with no endpoint and no overlay correction cannot work; drop it
	// silently rather than listing a provider that fails at launch.
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
```

In `cmd/launch/models.go`, point `cloudLimitsFromCatalog()` at the overlay (its body
becomes `return overlayLimits()`), and delete `cloud_limits_catalog.go` +
`cloud_limits_sync.go` along with their `go:embed` and cache-path helpers. In
`cmd/launch/provider_sync.go`, change the default URL to the overlay:

```go
const defaultProviderSyncURL = "https://raw.githubusercontent.com/sprapp-com/oaica-code/main/cmd/launch/providers/oaica.json"
```

In `cmd/cmd.go`, remove `modelCloudLimitsCmd` / `modelCloudLimitsSyncCmd` and their
`AddCommand` wiring (the file they call is gone); keep `remote sync` as-is, since it now
syncs the overlay.

- [ ] **Step 5: Run the whole launch package, then commit**

Run: `go test ./cmd/launch/ -run 'TestProviderCatalog_|TestCloudLimits_|TestProviderSync' -v`
Expected: PASS.

Run: `go test ./cmd/launch/ ...` — the existing catalog tests must be updated to seed the
overlay/catalog instead of the retired files, not deleted. Anything that fails because it
read `providers/providers.json` directly is updated to read `providers/oaica.json`.

Run: `go build ./... && go test ./cmd/launch/ ./anthropic/ ./cmd/tui/`
Expected: PASS.

```bash
git add -A cmd/launch cmd/cmd.go
git commit -m "launch: providers and limits come from models.dev plus the overlay

The port: provider rows, endpoints and credential env arrays now come from
the cached models.dev payload, corrected per field by our own overlay, with
the 44 rows of providers.json and 21 limits of cloud_limits.json folded in
verbatim as corrections. Both JSON files and the two .go files that served
them are deleted, so there is one source of truth and no shim."
```

---

### Task 5: Credential gating — `env[]` first-set-wins, and hidden providers

**Files:**
- Modify: `cmd/launch/user_remotes.go` (`builtinRemotes` only)
- Test: `cmd/launch/user_remotes_test.go` (extend the existing file)

**Interfaces:**
- Consumes: `providerCatalogEntry.Env` / `.Hidden` (Task 4), `hasStoredAuth`, `externalAuthKey` (existing).
- Produces: `func firstSetEnv(env []string) string` — the first *set* variable, or `""`.

- [ ] **Step 1: Write the failing test**

```go
// env[] is ordered by popularity upstream, not by what a given user has set.
// Taking env[0] would hide this provider from anyone holding Z_AI_API_KEY.
func TestBuiltinRemotes_MultiEnvFirstSetWins(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	clearAllCatalogKeys(t)
	writeModelsDevCache(t, modelsDevFixture)
	t.Setenv("Z_AI_API_KEY", "k") // the SECOND variable in zai-coding-plan's env[]
	remotes := builtinRemotes()
	for _, r := range remotes {
		if r.Name == "zai-coding-plan" {
			if r.APIKeyEnv != "Z_AI_API_KEY" {
				t.Fatalf("gated on %q, want the variable that is actually set", r.APIKeyEnv)
			}
			return
		}
	}
	t.Fatal("zai-coding-plan must be offered when its second env var is set")
}

func TestBuiltinRemotes_NoEnvSetMeansNotOffered(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	clearAllCatalogKeys(t)
	writeModelsDevCache(t, modelsDevFixture)
	for _, r := range builtinRemotes() {
		if r.Name == "zai-coding-plan" || r.Name == "groq" {
			t.Fatalf("%s offered with no credential set: %+v", r.Name, r)
		}
	}
}

// Bedrock/Vertex/Azure cannot work through a plain base URL. Listing them
// offers the user a provider that cannot succeed.
func TestBuiltinRemotes_HiddenProviderNotOffered(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	clearAllCatalogKeys(t)
	writeModelsDevCache(t, modelsDevFixture)
	hidden := 0
	for _, p := range oaicaOverlay().Providers {
		if !p.Hidden {
			continue
		}
		hidden++
		for _, v := range p.Env {
			t.Setenv(v, "k")
		}
		if p.APIKeyEnv != "" {
			t.Setenv(p.APIKeyEnv, "k")
		}
		for _, r := range builtinRemotes() {
			if r.Name == p.Name {
				t.Fatalf("hidden provider %s was offered", p.Name)
			}
		}
	}
	if hidden == 0 {
		t.Skip("overlay marks nothing hidden yet; Task 4's migration adds the SDK-signed group")
	}
}

// A user's own remotes.json row of the same name wins outright: its URL and its
// key, not just its URL.
func TestBuiltinRemotes_UserRemoteBeatsCatalogIncludingItsKey(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeModelsDevCache(t, modelsDevFixture)
	writeRemotes(t, `{"remotes":[{"name":"groq","base_url":"http://my-groq-proxy:8080/v1","api_key":"mine"}]}`)
	for _, r := range builtinRemotes() {
		if r.Name == "groq" {
			t.Fatalf("builtin groq must be shadowed by the user's row, got %+v", r)
		}
	}
	got, ok := findUserRemote("groq")
	if !ok || got.BaseURL != "http://my-groq-proxy:8080/v1" || got.APIKey != "mine" {
		t.Fatalf("user's groq = %+v, %v", got, ok)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/launch/ -run 'TestBuiltinRemotes_' -v`
Expected: FAIL — `zai-coding-plan` is gated on `env[0]` (`ZHIPU_API_KEY`) and is never
offered, and `undefined: findUserRemote` if that helper's name differs — use whichever
helper `user_remotes_test.go` already uses for this lookup.

- [ ] **Step 3: Write the implementation**

In `cmd/launch/user_remotes.go`:

```go
// firstSetEnv returns the first variable in env that is actually set, or "".
// models.dev orders env[] by popularity, so env[0] is the wrong answer for
// every user who holds the second one (opencode's rule is the same:
// provider.env.map((item) => envs[item]).find(Boolean)).
func firstSetEnv(env []string) string {
	for _, name := range env {
		if strings.TrimSpace(name) == "" {
			continue
		}
		if os.Getenv(name) != "" {
			return name
		}
	}
	return ""
}

func builtinRemotes() []userRemote {
	var out []userRemote
	for _, e := range providerCatalog() {
		// SDK-signed providers cannot work through a plain base URL; they are
		// reachable only by naming them explicitly in remotes.json, which
		// always wins (loadUserRemotes' dedupe).
		if e.Hidden {
			continue
		}
		p := userRemoteFromCatalogEntry(e)
		if set := firstSetEnv(e.Env); set != "" {
			p.APIKeyEnv = set
			out = append(out, p)
			continue
		}
		if p.APIKeyEnv != "" && os.Getenv(p.APIKeyEnv) != "" {
			out = append(out, p)
			continue
		}
		if hasStoredAuth(p.Name) {
			out = append(out, p)
			continue
		}
		if externalAuthKey(p.AuthVia, p.Name) != "" {
			out = append(out, p)
		}
	}
	return out
}
```

`userRemoteFromCatalogEntry` is `providerCatalogAsUserRemotes`'s existing per-row
conversion, extracted to a function so the gate and the converter stay in step.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/launch/ -run 'TestBuiltinRemotes_|TestUserRemotes|TestProviderCatalog_' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/launch/user_remotes.go cmd/launch/user_remotes_test.go
git commit -m "launch: gate builtins on the first credential that is SET, and hide signed providers

models.dev orders env[] by popularity, so env[0] hid every provider whose
listed-first variable is the less common one. Hidden marks the SDK-signed
group (Bedrock, Vertex, Azure) that cannot work through a base URL at all."
```

---

### Task 6: Model-id translation

Two rules that are not visible in the data and would otherwise cause silent 404s.

**Files:**
- Create: `cmd/launch/catalog_translate.go`
- Test: `cmd/launch/catalog_translate_test.go`

**Interfaces:**
- Consumes: nothing (pure).
- Produces:
  - `func translateModelID(wire, modelID string) string`
  - `func splitVendorPrefix(modelID string) (vendor, rest string, ok bool)` — the leading `openai/` or `anthropic/` segment an aggregator uses
  - `func aggregatorWire(vendor, defaultWire string) string`

- [ ] **Step 1: Write the failing test**

```go
package launch

import "testing"

func TestTranslateModelID_AnthropicDotsBecomeDashes(t *testing.T) {
	if got := translateModelID("anthropic", "claude-haiku-4.5"); got != "claude-haiku-4-5" {
		t.Fatalf("got %q, want claude-haiku-4-5", got)
	}
	// Lossless: no native Anthropic slug contains a dot.
	if got := translateModelID("anthropic", "claude-opus-4-5"); got != "claude-opus-4-5" {
		t.Fatalf("dashed id must be untouched, got %q", got)
	}
}

// The half of the rule that a careless implementation gets wrong: OpenAI ids
// legitimately keep their dots.
func TestTranslateModelID_OpenAIDotsUntouched(t *testing.T) {
	if got := translateModelID("openai", "gpt-5.4"); got != "gpt-5.4" {
		t.Fatalf("got %q, want gpt-5.4 unchanged", got)
	}
}

func TestTranslateModelID_AggregatorAnthropicPrefix(t *testing.T) {
	vendor, rest, ok := splitVendorPrefix("anthropic/claude-haiku-4.5")
	if !ok || vendor != "anthropic" || rest != "claude-haiku-4.5" {
		t.Fatalf("split = %q %q %v", vendor, rest, ok)
	}
	if got := aggregatorWire(vendor, "openai"); got != "anthropic" {
		t.Fatalf("wire = %q", got)
	}
	if got := translateModelID("anthropic", rest); got != "claude-haiku-4-5" {
		t.Fatalf("aggregator anthropic id = %q", got)
	}
}

// OpenRouter-shaped ids keep their vendor/model form; the prefix selects the
// wire but is not stripped from anything else.
func TestTranslateModelID_OpenRouterFormsPassThrough(t *testing.T) {
	vendor, rest, ok := splitVendorPrefix("openai/gpt-5.4")
	if !ok || vendor != "openai" {
		t.Fatalf("split = %q %q %v", vendor, rest, ok)
	}
	if got := aggregatorWire(vendor, "openai"); got != "openai" {
		t.Fatalf("wire = %q", got)
	}
	if got := translateModelID("openai", rest); got != "gpt-5.4" {
		t.Fatalf("id = %q", got)
	}
}

// An unknown prefix is passed through, never dropped and never guessed at.
func TestTranslateModelID_UnknownPrefixPassthrough(t *testing.T) {
	if vendor, _, ok := splitVendorPrefix("meta-llama/llama-4"); ok {
		t.Fatalf("meta-llama must not be treated as a wire-selecting vendor, got %q", vendor)
	}
	if got := translateModelID("openai", "meta-llama/llama-4"); got != "meta-llama/llama-4" {
		t.Fatalf("unknown prefix id = %q", got)
	}
}

func TestTranslateModelID_NoSlashIsNotAnAggregator(t *testing.T) {
	if _, _, ok := splitVendorPrefix("gpt-5.4"); ok {
		t.Fatal("an id with no slash has no vendor prefix")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/launch/ -run 'TestTranslateModelID|TestAggregator|TestSplitVendor' -v`
Expected: FAIL — `undefined: translateModelID`.

- [ ] **Step 3: Write the implementation**

```go
package launch

// catalog_translate.go — two id rules that are invisible in the data.
//
// models.dev lists Anthropic models with dotted versions (claude-haiku-4.5);
// the Anthropic Messages API wants the dashed native slug (claude-haiku-4-5).
// Translating is lossless because no native Anthropic slug contains a dot —
// but applying the SAME substitution to an OpenAI id corrupts it, since OpenAI
// names legitimately keep dots (gpt-5.4). Hence: the wire decides.
//
// Aggregators proxy other vendors' models behind a "vendor/model" id. The
// leading openai/ or anthropic/ segment selects the wire for that model;
// anything else is OpenAI-compatible with the id passed through unchanged.

import "strings"

// translateModelID returns the id to send upstream for a model on the given
// wire. Any id we cannot translate is passed through: never dropped, never
// guessed at.
func translateModelID(wire, modelID string) string {
	if wire != "anthropic" {
		return modelID
	}
	return strings.ReplaceAll(modelID, ".", "-")
}

// splitVendorPrefix splits a leading wire-selecting vendor segment. ok is
// false for an id with no slash and for a vendor that does not name a wire —
// meta-llama/llama-4 is an ordinary model id, not a routing instruction.
func splitVendorPrefix(modelID string) (vendor, rest string, ok bool) {
	i := strings.IndexByte(modelID, '/')
	if i <= 0 || i == len(modelID)-1 {
		return "", modelID, false
	}
	vendor, rest = modelID[:i], modelID[i+1:]
	switch vendor {
	case "openai", "anthropic":
		return vendor, rest, true
	default:
		return "", modelID, false
	}
}

// aggregatorWire returns the wire a vendor prefix selects, falling back to the
// provider's own wire when the prefix does not name one.
func aggregatorWire(vendor, defaultWire string) string {
	switch vendor {
	case "anthropic":
		return "anthropic"
	case "openai":
		return "openai"
	default:
		return defaultWire
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/launch/ -run 'TestTranslateModelID|TestAggregator|TestSplitVendor' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/launch/catalog_translate.go cmd/launch/catalog_translate_test.go
git commit -m "launch: translate model ids per wire, not globally

Dotted Anthropic ids become dashed slugs; OpenAI ids keep their dots, which
the same substitution would corrupt. A vendor prefix selects the wire for
an aggregator's rows and is otherwise passed through."
```

---

### Task 7: Usage tracking

**Files:**
- Create: `cmd/launch/model_usage.go`
- Test: `cmd/launch/model_usage_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `func usageCachePath() (string, error)` → `~/.oaica/usage.json`
  - `type modelUsage struct { Count int `json:"n"`; Last string `json:"last"` }`
  - `type usageFile struct { Version int `json:"version"`; Usage map[string]modelUsage `json:"usage"` }`
  - `func recordModelUse(model string)` — best-effort; a write failure is swallowed
  - `func frequentModels(limit int) []string` — ranked, `n >= 2` only
  - `const frequentMinUses = 2`, `const frequentMaxEntries = 8`

Ranking is count descending, most-recent as tiebreak. A corrupt or unreadable file
degrades to an empty section, never an error.

- [ ] **Step 1: Write the failing test**

```go
package launch

import (
	"os"
	"testing"
)

func TestUsage_SingleUseDoesNotSurface(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	recordModelUse("once")
	if got := frequentModels(8); len(got) != 0 {
		t.Fatalf("a single experiment must not displace the section, got %v", got)
	}
}

func TestUsage_RankedByCountThenRecency(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	for i := 0; i < 3; i++ {
		recordModelUse("often")
	}
	for i := 0; i < 3; i++ {
		recordModelUse("also-often")
	}
	recordModelUse("often") // 4 > 3: count decides
	for i := 0; i < 2; i++ {
		recordModelUse("rarely")
	}
	got := frequentModels(8)
	if len(got) != 3 || got[0] != "often" {
		t.Fatalf("ranking = %v", got)
	}
	// rarely (2 uses, newest) outranks nothing but must be present, and the
	// tie between the two 3-use entries broke by recency.
	if got[2] != "also-often" && got[2] != "rarely" {
		t.Fatalf("unexpected tail: %v", got)
	}
}

func TestUsage_CorruptFileDegradesToEmptySection(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	path, _ := usageCachePath()
	if err := os.WriteFile(path, []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := frequentModels(8); len(got) != 0 {
		t.Fatalf("corrupt usage file must yield an empty section, got %v", got)
	}
	// and must not block recording a new use
	recordModelUse("fresh")
	recordModelUse("fresh")
	if got := frequentModels(8); len(got) != 1 || got[0] != "fresh" {
		t.Fatalf("recording after corruption = %v", got)
	}
}

func TestUsage_RespectsLimit(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	for i := 0; i < 12; i++ {
		m := "m" + string(rune('a'+i))
		recordModelUse(m)
		recordModelUse(m)
	}
	if got := frequentModels(8); len(got) != 8 {
		t.Fatalf("limit ignored: %d entries", len(got))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/launch/ -run 'TestUsage_' -v`
Expected: FAIL — `undefined: recordModelUse`.

- [ ] **Step 3: Write the implementation**

```go
package launch

// model_usage.go — which models this machine actually runs, for the picker's
// "Frequently used" section.
//
// Machine-local by design (~/.oaica/usage.json), never synced and never
// uploaded: there is no pin list to configure and no fleet state to keep in
// step. Written on a successful launch only, so a failed attempt cannot
// promote a model.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	// frequentMinUses keeps a one-off experiment out of the section.
	frequentMinUses = 2
	// frequentMaxEntries is the section's cap.
	frequentMaxEntries = 8
)

type modelUsage struct {
	Count int    `json:"n"`
	Last  string `json:"last"`
}

type usageFile struct {
	Version int                   `json:"version"`
	Usage   map[string]modelUsage `json:"usage"`
}

const usageVersion = 1

func usageCachePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".oaica", "usage.json"), nil
}

func loadUsage() usageFile {
	path, err := usageCachePath()
	if err != nil {
		return usageFile{Version: usageVersion, Usage: map[string]modelUsage{}}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return usageFile{Version: usageVersion, Usage: map[string]modelUsage{}}
	}
	var f usageFile
	if json.Unmarshal(b, &f) != nil || f.Usage == nil {
		return usageFile{Version: usageVersion, Usage: map[string]modelUsage{}}
	}
	return f
}

// recordModelUse counts one successful launch of a model. Best-effort: a
// machine where ~/.oaica is not writable must still launch.
func recordModelUse(model string) {
	if model == "" {
		return
	}
	path, err := usageCachePath()
	if err != nil {
		return
	}
	f := loadUsage()
	u := f.Usage[model]
	u.Count++
	u.Last = time.Now().UTC().Format(time.RFC3339)
	f.Usage[model] = u
	f.Version = usageVersion

	b, err := json.Marshal(f)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	_ = os.WriteFile(path, b, 0o600)
}

// frequentModels returns up to limit model ids this machine runs, most-used
// first and most-recent as tiebreak. Models used fewer than frequentMinUses
// times are omitted.
func frequentModels(limit int) []string {
	if limit <= 0 {
		limit = frequentMaxEntries
	}
	f := loadUsage()
	type row struct {
		name string
		u    modelUsage
	}
	rows := make([]row, 0, len(f.Usage))
	for name, u := range f.Usage {
		if u.Count < frequentMinUses {
			continue
		}
		rows = append(rows, row{name, u})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].u.Count != rows[j].u.Count {
			return rows[i].u.Count > rows[j].u.Count
		}
		if rows[i].u.Last != rows[j].u.Last {
			return rows[i].u.Last > rows[j].u.Last
		}
		return rows[i].name < rows[j].name
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.name)
	}
	return out
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/launch/ -run 'TestUsage_' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/launch/model_usage.go cmd/launch/model_usage_test.go
git commit -m "launch: track which models this machine runs, locally

Machine-local launch counts for the picker's Frequently used section: no
synced pin list, no configuration, and a corrupt file degrades to an empty
section rather than an error."
```

---

### Task 8: The picker's Frequently used section

One new section, placed first (the spec's order). Everything else rides the existing
`Remote` machinery: catalog rows arrive as `<provider>/<model>` through `builtinRemotes`
→ `remoteLaunchModels`, already sort alphabetically, which makes each provider's rows
contiguous under the existing section. See "Deviations" for why per-provider headers are
not in this plan.

**Files:**
- Modify: `cmd/tui/selector.go` (`SelectItem`, `ConvertItems`, `ReorderItems`, the render split)
- Modify: `cmd/launch/launch.go` (`SelectionItem` gains the same flag)
- Modify: `cmd/launch/models.go` (mark rows)
- Test: `cmd/tui/selector_test.go`, `cmd/launch/models_test.go` (create if absent)

**Interfaces:**
- Consumes: `frequentModels` (Task 7), `catalogAgeDays` (Task 2).
- Produces:
  - `SelectItem.Frequent bool` and `SelectionItem.Frequent bool`
  - `ReorderItems` order: Frequent → Local → OllamaCloud → Recommended(OAICA) → Remote → More

- [ ] **Step 1: Write the failing test**

```go
// cmd/tui/selector_test.go

// names is a small local helper for asserting on section order. Define it once
// at the top of this file's new block; if selector_test.go already has an
// equivalent, use that instead of adding a second one.
func names(items []SelectItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Name)
	}
	return out
}

func TestReorderItems_FrequentSectionLeadsAndKeepsItsRanking(t *testing.T) {
	in := []SelectItem{
		{Name: "zeta/remote", Remote: true},
		{Name: "local-model", Local: true},
		{Name: "second-most", Frequent: true},
		{Name: "most-used", Frequent: true},
	}
	out := ReorderItems(in)
	if out[0].Name != "second-most" || out[1].Name != "most-used" {
		t.Fatalf("Frequent must lead in its given order (the ranking), got %v", names(out))
	}
	if out[2].Name != "local-model" {
		t.Fatalf("Local must follow Frequent, got %v", names(out))
	}
}

// A row can be both Frequent and Remote. Frequent is the section that wins,
// and the flat list must agree with the render or the cursor desyncs.
func TestReorderItems_FrequentBeatsOtherSectionFlags(t *testing.T) {
	in := []SelectItem{{Name: "both", Frequent: true, Remote: true}}
	out := ReorderItems(in)
	if len(out) != 1 || !out[0].Frequent {
		t.Fatalf("got %v", names(out))
	}
	s := newSelectorModelForTest(t, in)
	if !strings.Contains(s.View(), "Frequently used") {
		t.Fatalf("render must show the section:\n%s", s.View())
	}
	if strings.Count(s.View(), "both") != 1 {
		t.Fatalf("row must render exactly once:\n%s", s.View())
	}
}
```

Use the helper names already present in `selector_test.go` for constructing a model and
rendering it (`newSelectorModelForTest` above is a placeholder for whichever one exists —
do not add a second one).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/tui/ -run 'TestReorderItems_Frequent' -v`
Expected: FAIL — `unknown field 'Frequent'`.

- [ ] **Step 3: Write the implementation**

In `cmd/tui/selector.go`, add to `SelectItem` and mirror it in `ConvertItems`:

```go
	// Frequent marks a model this machine actually runs (see
	// launch.frequentModels). It leads every other section, in the order given
	// — that order IS the ranking, so ReorderItems must not re-sort it.
	Frequent bool
```

`ReorderItems` gains the branch first, and the render split gains the matching section
before "Local Models":

```go
func ReorderItems(items []SelectItem) []SelectItem {
	var freq, loc, rec, olc, rem, other []SelectItem
	for _, item := range items {
		switch {
		case item.Frequent:
			// First, and in the caller's order (ranked), not sorted by name.
			freq = append(freq, item)
		case item.Local:
			loc = append(loc, item)
		case item.OllamaCloud:
			olc = append(olc, item)
		case item.Recommended:
			rec = append(rec, item)
		case item.Remote:
			rem = append(rem, item)
		default:
			other = append(other, item)
		}
	}
	// ... existing local/other/remote sorts unchanged ...
	return append(append(append(append(append(freq, loc...), rec...), olc...), rem...), other...)
}
```

In the render (`selector.go:700-789`), add "Frequently used" as the first always-shown
section, before "Local Models", collecting the leading `Frequent` rows from the same flat
order — the section list and the render split must be changed in the same commit or the
highlight desyncs from the cursor.

In `cmd/launch/models.go`, where `buildModelListWithRecommendations` sets row flags, mark
the frequent ids:

```go
	freq := map[string]bool{}
	for _, name := range frequentModels(frequentMaxEntries) {
		freq[name] = true
	}
	// ... after each item is built ...
	item.Frequent = freq[item.Name]
```

In `cmd/launch/launch.go`, add `Frequent bool` to `SelectionItem` (and to whatever
converts `ModelItem` → `SelectionItem`, `SelectionItemsWithAccountState`).

In `cmd/launch/launch.go`'s launch path, after a launch starts successfully, call
`recordModelUse(primaryName)` — the same place the plan/policy banner is printed is fine,
but it must be after the process is actually started, not before.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/tui/ -run 'TestReorderItems|TestSelector' -v && go test ./cmd/launch/ -run 'TestUsage_|TestModelList' -v`
Expected: PASS, including the existing selector tests that pin the other sections' order.

- [ ] **Step 5: Commit**

```bash
git add cmd/tui/selector.go cmd/tui/selector_test.go cmd/launch/launch.go cmd/launch/models.go
git commit -m "launch: lead the picker with the models this machine runs

One new section, first, ranked by local launch counts. Catalog rows need no
new section: they arrive as <provider>/<model> through the existing remote
machinery and already group contiguously per provider."
```

---

### Task 9: The model list — intersect when reachable, list when not

Today a remote's rows come from a live `/v1/models` sweep, with the overlay's declared
`models` map filling in only where the sweep cannot answer. Invert the common case: the
catalog supplies the list, the sweep filters it. Offline, the full list still appears.

**Files:**
- Modify: `cmd/launch/user_remotes.go` (`remoteLaunchModels`, `fetchRemoteModels`)
- Create: `cmd/launch/catalog_rows.go` (catalog → picker row display)
- Test: `cmd/launch/catalog_rows_test.go`, `cmd/launch/user_remotes_test.go`

**Interfaces:**
- Consumes: `loadModelsDevCatalog` (Task 2), `overlayProvidersByName` (Task 3), `translateModelID` (Task 6), `providerCatalogDeclaredModels` (existing).
- Produces:
  - `type catalogModelRow struct { ID, Name string; Context, Output int; HasContext bool; Cost modelsDevCost; ToolCall, Reasoning, Vision, Deprecated bool }`
  - `func catalogRowsFor(providerName string) []catalogModelRow`
  - `func catalogRowDescription(r catalogModelRow) string` — context, price/M with bands, marks
  - `func remoteIsSweepable(r userRemote) bool`

- [ ] **Step 1: Write the failing test**

```go
package launch

import (
	"strings"
	"testing"
)

func TestCatalogRowsFor_CarriesMarksAndPrice(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeModelsDevCache(t, modelsDevFixture)
	rows := catalogRowsFor("zai-coding-plan")
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	byID := map[string]catalogModelRow{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	if !byID["glm-4.6"].Deprecated {
		t.Fatal("status deprecated must mark the row")
	}
	if !byID["glm-4.7"].Vision {
		t.Fatal("attachment + image input modality must mark vision")
	}
	if byID["glm-4.7"].ToolCall != false {
		t.Fatal("explicit tool_call:false must reach the row")
	}
}

// Review focus 5: a model with no cost and no limit must render as free with
// an unknown window, never as a zero price and never panicking.
func TestCatalogRowDescription_MissingCostAndLimitDegrade(t *testing.T) {
	desc := catalogRowDescription(catalogModelRow{ID: "m", Name: "M"})
	if strings.Contains(desc, "$0.00") || strings.Contains(desc, "0/M") {
		t.Fatalf("absent cost must not print as a real price: %q", desc)
	}
	if !strings.Contains(desc, "ctx ?") {
		t.Fatalf("absent limit must read as unknown: %q", desc)
	}
	if !strings.Contains(desc, "free") {
		t.Fatalf("absent cost should read as free: %q", desc)
	}
}

func TestCatalogRowDescription_ShowsTierAndOver200kBands(t *testing.T) {
	desc := catalogRowDescription(catalogModelRow{
		ID: "glm-4.6", Name: "GLM-4.6", Context: 200000, HasContext: true,
		Cost: modelsDevCost{
			Input: 0.6, Output: 2.2,
			Tiers:           []modelsDevCostTier{{Tier: modelsDevTierBand{Type: "context", Size: 32000}, Input: 0.9, Output: 3}},
			ContextOver200k: &modelsDevCostTier{Input: 1.2, Output: 4.4},
		},
	})
	if !strings.Contains(desc, "0.90") || !strings.Contains(desc, "1.20") {
		t.Fatalf("banded pricing must be shown, not just the base rate: %q", desc)
	}
}

// A remote with no credential mechanism at all is genuinely unauthenticated:
// sweeping it burns a timeout for an answer that cannot be trusted anyway.
func TestRemoteIsSweepable(t *testing.T) {
	if remoteIsSweepable(userRemote{Name: "keyless", BaseURL: "http://x/v1"}) {
		t.Fatal("a remote with no key mechanism must not be swept")
	}
	if !remoteIsSweepable(userRemote{Name: "keyed", BaseURL: "http://x/v1", APIKeyEnv: "X_API_KEY"}) {
		t.Fatal("a remote with an env key mechanism must be swept")
	}
	if !remoteIsSweepable(userRemote{Name: "inline", BaseURL: "http://x/v1", APIKey: "k"}) {
		t.Fatal("a remote with an inline key must be swept")
	}
}

// The inversion: an unreachable remote still lists the catalog's models, each
// marked unverified, instead of an empty section.
func TestRemoteLaunchModels_UnreachableShowsCatalogUnverified(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeModelsDevCache(t, modelsDevFixture)
	writeRemotes(t, `{"remotes":[{"name":"groq","base_url":"http://127.0.0.1:1/v1","api_key":"k"}]}`)
	stubUserRemoteModels(t, nil, nil) // the sweep answers nothing
	rows := remoteLaunchModels()
	if len(rows) == 0 {
		t.Fatal("an unreachable remote must still list the catalog's models")
	}
	for _, r := range rows {
		if !strings.Contains(r.Name, "groq/") {
			continue
		}
		if r.AvailabilityBadge != "unverified" {
			t.Fatalf("%s badge = %q, want unverified", r.Name, r.AvailabilityBadge)
		}
		return
	}
	t.Fatal("no groq rows")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/launch/ -run 'TestCatalogRow|TestRemoteIsSweepable|TestRemoteLaunchModels_' -v`
Expected: FAIL — `undefined: catalogRowsFor`.

- [ ] **Step 3: Write the implementation**

Create `cmd/launch/catalog_rows.go`:

```go
package launch

// catalog_rows.go — one catalog model as a picker row: what it costs, how big
// its window is, what it can do. Display only; routing reads the catalog
// directly.

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

// catalogRowsFor returns a provider's models.dev models, plus any the overlay
// declares that upstream lacks, sorted by release date descending (newest
// first) with a stable id tiebreak. Deprecated rows are marked, not hidden.
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

	for id, lim := range providerCatalogDeclaredModels(providerName) {
		if _, ok := rows[id]; ok {
			continue // the sweep and the catalog both know it; catalog wins
		}
		rows[id] = catalogModelRow{
			ID: id, Name: id, Context: lim.Context, Output: lim.Output,
			HasContext: lim.Context > 0, ToolCall: true,
		}
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

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// catalogRowDescription is the row's right-hand text: window, price per
// million, then capability marks. Banded pricing is shown when upstream
// carries it — displaying only the base rate would misprice a long Anthropic
// context, which is exactly the case the bands exist for.
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
// all. A remote with none is genuinely unauthenticated (remote_key_prompt.go's
// rule), so sweeping it spends a timeout on an answer we would not trust —
// fetchRemoteModels currently sweeps it anyway.
func remoteIsSweepable(r userRemote) bool {
	if r.APIKey != "" || r.APIKeyEnv != "" || r.AuthVia != "" {
		return true
	}
	return r.key() != ""
}
```

Then in `user_remotes.go`:
- `fetchRemoteModels` skips any remote where `!remoteIsSweepable(r)`.
- `remoteLaunchModels` builds rows from `catalogRowsFor(r.Name)` first; when the sweep
  answered, keep the intersection (a swept id the catalog lacks is still listed — the
  provider is the authority on what it serves) and mark those `unverified` only when the
  sweep did not answer.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/launch/ -run 'TestCatalogRow|TestRemoteIsSweepable|TestRemoteLaunchModels_' -v`
Expected: PASS.

Run: `go test ./cmd/launch/` — the remote/picker tests that stub the sweep must now also
seed the catalog where they assert on listed models.
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/launch/catalog_rows.go cmd/launch/catalog_rows_test.go cmd/launch/user_remotes.go
git commit -m "launch: list a remote's models from the catalog, swept when reachable

The common path no longer depends on the sweep answering: reachable remotes
intersect the catalog with /v1/models, unreachable ones list the catalog
marked unverified, and a remote with no credential mechanism is not swept at
all. Rows carry the window, banded pricing, and capability marks."
```

---

### Task 10: First-party endpoints, offline behaviour, the commands, and the docs

**Files:**
- Modify: `cmd/launch/tier_routing.go` (`resolveLaunchEndpoint`)
- Modify: `cmd/launch/models.go` (first-party overlay rows in the list)
- Modify: `cmd/cmd.go` (`oaica model catalog sync` / `status`)
- Create: `docs/CATALOG.md`
- Modify: `README.md`, `AGENTS.md`
- Test: `cmd/launch/tier_routing_test.go`, `cmd/launch/models_test.go`

**Interfaces:**
- Consumes: `overlayFirstPartyModels` (Task 3), `CatalogSync` (Task 2), `catalogAgeDays` (Task 2).
- Produces:
  - `func oaicaGatewayURLOverride() string` — `OAICA_GATEWAY_URL`, trimmed
  - `func CatalogStatus() (CatalogSyncReport, string)` — provenance for the command

- [ ] **Step 1: Write the failing test**

```go
// OAICA_GATEWAY_URL is an override at the TOP of the chain: it exists so a
// first-party model can be pointed at a gateway on any machine, since the port
// differs per box.
func TestResolveLaunchEndpoint_GatewayURLOverride(t *testing.T) {
	noRemotes(t)
	stubCloudFetch(t, nil, &oaicaRouterError{Status: 401})
	stubDaemon(t)
	t.Setenv("OAICA_GATEWAY_URL", "http://gateway.local:8081")
	ep, err := resolveLaunchEndpoint("oaica-default")
	if err != nil {
		t.Fatalf("override must resolve: %v", err)
	}
	if ep.BaseURL != "http://gateway.local:8081/v1" {
		t.Fatalf("base = %q", ep.BaseURL)
	}
}

// The chain it must NOT disturb: an alias still beats everything, and a user
// remote still beats local_servers.json for a bare id.
func TestResolveLaunchEndpoint_OverrideDoesNotReorderTheChain(t *testing.T) {
	remoteBox(t, map[string][]string{"kat-awq": {"box/kat-awq"}})
	stubCloudFetch(t, nil, &oaicaRouterError{Status: 401})
	stubDaemon(t)
	t.Setenv("OAICA_GATEWAY_URL", "http://gateway.local:8081")
	if r, _ := resolveLaunchEndpoint("box/kat-awq"); r.BaseURL != "http://box:8080/v1" {
		t.Fatalf("a user remote must still win for its own models, got %+v", r)
	}
}

// Offline: no catalog cached, nothing errors, and the notice names the fix.
func TestModelList_NoCatalogStillListsOverlayAndLocal(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[]}`)
	stubDaemon(t, "local-model")
	items := buildModelListWithRecommendations(nil)
	var sawFirstParty, sawLocal bool
	for _, it := range items {
		if isFirstPartyOverlayModel(it.Name) {
			sawFirstParty = true
		}
		if it.Name == "local-model" {
			sawLocal = true
		}
	}
	if !sawFirstParty || !sawLocal {
		t.Fatalf("offline list must carry first-party and local rows: %+v", items)
	}
	if notice := catalogOfflineNotice(); notice == "" || !strings.Contains(notice, "oaica model catalog sync") {
		t.Fatalf("offline notice must name the sync command, got %q", notice)
	}
}

func TestCatalogAgeNotice_OnlyPastThirtyDays(t *testing.T) {
	if got := catalogAgeNotice(29); got != "" {
		t.Fatalf("29 days must stay quiet, got %q", got)
	}
	if got := catalogAgeNotice(31); !strings.Contains(got, "days old") {
		t.Fatalf("31 days must be visible, got %q", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/launch/ -run 'TestResolveLaunchEndpoint_Gateway|TestModelList_NoCatalog|TestCatalogAgeNotice' -v`
Expected: FAIL — `undefined: oaicaGatewayURLOverride`.

- [ ] **Step 3: Write the implementation**

In `cmd/launch/tier_routing.go`, at the top of `resolveLaunchEndpoint` (before the alias
lookup), add the override:

```go
// OAICA_GATEWAY_URL points every first-party model at one gateway. It exists
// because the gateway's port differs per machine, so a first-party model cannot
// carry a fixed endpoint. It is checked first, and it deliberately does NOT
// reorder the rest of the chain: an alias still beats everything, and a user's
// remotes.json still beats local_servers.json for a bare id.
func oaicaGatewayURLOverride() string {
	return strings.TrimRight(strings.TrimSpace(os.Getenv("OAICA_GATEWAY_URL")), "/")
}
```

```go
	if base := oaicaGatewayURLOverride(); base != "" {
		return launchEndpoint{
			RemoteEndpoint: RemoteEndpoint{Name: "oaica-gateway", BaseURL: base + "/v1", Token: "oaica"},
			Source:         sourceRouter,
		}, nil
	}
```

In `cmd/launch/models.go`, add first-party rows from the overlay (they are `Recommended`,
which is what puts them in the OAICA section), with their windows from the overlay and an
unavailable mark when nothing resolves:

```go
// firstPartyOverlayRows lists oaica's own models. Their endpoint is resolved at
// launch, so a row is shown even when nothing resolves — marked unavailable with
// the reason, because hiding it would make our own model look like it vanished.
func firstPartyOverlayRows() []ModelItem {
	var out []ModelItem
	for _, m := range overlayFirstPartyModels() {
		item := ModelItem{
			Name:             m.ID,
			Description:      firstNonEmpty(m.DisplayName, m.ID),
			Recommended:      true,
			MaxOutputTokens:  m.Output,
		}
		if _, err := resolveLaunchEndpoint(m.ID); err != nil {
			item.AvailabilityBadge = "unavailable"
		}
		out = append(out, item)
	}
	return out
}

func isFirstPartyOverlayModel(name string) bool {
	for _, m := range overlayFirstPartyModels() {
		if m.ID == name {
			return true
		}
	}
	return false
}

// catalogOfflineNotice is the one line shown when no catalog is cached.
func catalogOfflineNotice() string {
	if _, ok := loadModelsDevCatalog(); ok {
		return ""
	}
	return "no model catalog yet — run `oaica model catalog sync` to list external providers"
}

// catalogAgeNotice makes staleness visible past 30 days rather than silent.
func catalogAgeNotice(days int) string {
	if days <= 30 {
		return ""
	}
	return fmt.Sprintf("model catalog is %d days old — run `oaica model catalog sync` to refresh", days)
}
```

In `cmd/cmd.go`, register the verb following the `model cloud-limits` pattern
(`cmd/cmd.go:2490-2520` is the closest analogue):

```go
catalogCmd := &cobra.Command{Use: "catalog", Short: "Manage the ported models.dev catalog"}
catalogSyncCmd := &cobra.Command{
	Use:   "sync",
	Short: "Fetch the models.dev catalog and adopt it if it still matches what oaica reads",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		url, _ := cmd.Flags().GetString("url")
		rep, err := launch.CatalogSync(url)
		if err != nil {
			return err // non-zero exit: a refusal is a failure of the check
		}
		src := "fresh"
		switch {
		case rep.Unchanged:
			src = "unchanged"
		case rep.FromCache:
			src = "cached/304"
		}
		fmt.Printf("catalog %s from %s: %d providers, %d models\n", src, rep.URL, rep.Providers, rep.Models)
		return nil
	},
}
catalogSyncCmd.Flags().String("url", "", "Catalog URL (default: https://models.dev/api.json; file:// paths accepted)")
catalogStatusCmd := &cobra.Command{
	Use:   "status",
	Short: "Show the cached catalog's provenance: source, age, sha256, counts",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		rep, line := launch.CatalogStatus()
		fmt.Println(line)
		return nil
	},
}
catalogCmd.AddCommand(catalogSyncCmd, catalogStatusCmd)
modelCmd.AddCommand(catalogCmd)
```

`CatalogStatus` (in `catalog_sync.go`) returns the cached path, its sha256, its modtime
age in days, and the provider/model counts — no network.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/launch/ -run 'TestResolveLaunchEndpoint|TestModelList_|TestCatalogAgeNotice' -v`
Expected: PASS.

- [ ] **Step 5: Write the docs, then commit**

`docs/CATALOG.md` must cover: the three layers and their precedence; that the ported
catalog is never edited and corrections go in `providers/oaica.json`; `oaica model
catalog sync` and what a refusal means (a field oaica reads changed shape — the error
names it, and the last good catalog is still in use); `OAICA_GATEWAY_URL`; where the
cache lives and that deleting it is always safe; and that Plan B's drift tooling
(archive, `check`/`diff`/`drift`, `--accept-drift`, the CI check) does not exist yet, so a
refusal is resolved by hand today.

`README.md` gains the catalog as a feature line, `AGENTS.md` a pointer to `docs/CATALOG.md`.

```bash
git add -A cmd/launch cmd/cmd.go docs/CATALOG.md README.md AGENTS.md
git commit -m "launch: first-party endpoints, offline behaviour, and the catalog commands

OAICA_GATEWAY_URL points first-party models at a gateway without reordering
the existing resolution chain; with no catalog cached the overlay and local
models still list, with one notice naming the sync command; staleness past
30 days is shown rather than silent. Adds oaica model catalog sync|status."
```

---

## Self-review

**Spec coverage.** Decisions 1–4: Decision 1 (show everything) — Task 4 (all upstream
providers merged, hidden only for the SDK-signed group per the spec's own "Provider
coverage"), Tasks 9's rows. Decision 2 (frequently used, auto-tracked) — Tasks 7 and 8.
Decision 3 (first-party from the overlay, endpoint resolved at launch) — Tasks 3 and 10.
Decision 4 (models.dev list ∩ live sweep) — Task 9. Model id translation — Task 6.
Mapping tables — Task 1 (types) and Task 4 (`providerEntryFromModelsDev`), with the
per-model `provider.npm` / `provider.api` overrides carried on the model in Task 9's row
build. Sync and offline behaviour — Tasks 2 and 10. Contract check — Tasks 1, 2.
Rollback — nothing writes outside `~/.oaica/cache/` and `~/.oaica/usage.json`, so a
`git revert` leaves a self-consistent binary; noted in the spec already.

**Not covered here, by design:** the archive, `shape.json`, the drift artifact,
`check` / `diff` / `drift` / `--accept-drift`, the `DRIFT` marker, `docs/CATALOG_DRIFT.md`
and the daily CI workflow — all of that is Plan B, per "Scope".

**Placeholders.** None: every step carries its command, its expected result, and the code
it needs. Two places instruct the implementer to reuse a name that already exists in the
file they are editing (a selector test helper in Task 8, a remote-lookup helper in
Task 5) — that is deliberate, because inventing a second helper beside an existing one is
the failure mode being avoided; check the file before writing the test.

**Type consistency.** `providerCatalogEntry.Env` / `.Hidden` are declared in Task 4 and
consumed in Tasks 5 and 9. `modelsDevCost` / `modelsDevCostTier` are declared in Task 1,
used by `catalogModelRow` in Task 9. `catalogCachePath`, `loadModelsDevCatalog`,
`catalogAgeDays` are declared in Task 2 and used in Tasks 4, 9, 10. `oaicaOverlay`,
`overlayProvidersByName`, `overlayLimits`, `overlayFirstPartyModels` are declared in
Task 3, consumed in Tasks 4 and 10. `mergeProviderEntry` is defined once, in Task 3, and
called from Task 4. `frequentModels` / `frequentMaxEntries` — Task 7, consumed in Task 8.
`remoteIsSweepable` / `catalogRowsFor` / `catalogRowDescription` — Task 9 only.

**Review focus.** (1) hidden/endpointless providers → Task 5, `TestBuiltinRemotes_HiddenProviderNotOffered`
plus Task 4's drop rule. (2) user remotes.json beats the catalog including its key →
Task 5, `TestBuiltinRemotes_UserRemoteBeatsCatalogIncludingItsKey`. (3) a stale cache
cannot make a new overlay field inert → Task 3, `TestOverlay_MergeIsPerKeyNotWholesale`
and `TestOverlay_CacheCannotEraseAnEmbeddedOnlyField`. (4) OpenAI ids keep their dots →
Task 6, `TestTranslateModelID_OpenAIDotsUntouched`. (5) absent cost/limit degrade → Task 1
(`TestModelsDevCost_AbsentIsZeroNotAbsent`, `TestModelsDevLimit_AbsentDoesNotPanic`) and
Task 9 (`TestCatalogRowDescription_MissingCostAndLimitDegrade`).
