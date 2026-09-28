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
	ID     string                    `json:"id"`
	Name   string                    `json:"name"`
	API    string                    `json:"api"`
	NPM    string                    `json:"npm"`
	Env    []string                  `json:"env"`
	Models map[string]modelsDevModel `json:"models"`
}

type modelsDevModel struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ReleaseDate string `json:"release_date"`
	Status      string `json:"status"`
	Attachment  bool   `json:"attachment"`
	Reasoning   bool   `json:"reasoning"`
	Temperature bool   `json:"temperature"`
	// ToolCall is a pointer so "absent" stays distinguishable from an explicit
	// false — opencode's rule is `model.tool_call ?? true`.
	ToolCall   *bool                `json:"tool_call"`
	Modalities *modelsDevModalities `json:"modalities"`
	Limit      *modelsDevLimit      `json:"limit"`
	Cost       *modelsDevCost       `json:"cost"`
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
	Input           float64             `json:"input"`
	Output          float64             `json:"output"`
	CacheRead       float64             `json:"cache_read"`
	CacheWrite      float64             `json:"cache_write"`
	Tiers           []modelsDevCostTier `json:"tiers"`
	ContextOver200k *modelsDevCostTier  `json:"context_over_200k"`
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

// parseModelsDevCatalog turns the payload into typed providers.
//
// models.dev/api.json — the URL the sync fetches — is keyed by provider id at
// the TOP LEVEL ({"deepinfra": {...}, "groq": {...}}); it carries no
// "providers" wrapper. The struct's own tag names the wrapper because that is
// what a marshalled modelsDevFile looks like, and a payload that does wrap its
// map is read the same way, so both shapes land in Providers.
func parseModelsDevCatalog(b []byte) (modelsDevFile, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(b, &members); err != nil {
		return modelsDevFile{}, err
	}
	if _, wrapped := members["providers"]; wrapped {
		var f modelsDevFile
		if err := json.Unmarshal(b, &f); err != nil {
			return modelsDevFile{}, err
		}
		return f, nil
	}
	var providers map[string]modelsDevProvider
	if err := json.Unmarshal(b, &providers); err != nil {
		return modelsDevFile{}, err
	}
	return modelsDevFile{Providers: providers}, nil
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

	// The provider map is the payload's top level (models.dev's real shape, see
	// parseModelsDevCatalog); a payload that wraps it in "providers" is checked
	// the same way. A "providers" member that is present but is not an object is
	// a shape we cannot read at all, and is reported as such.
	providers, ok := root["providers"].(map[string]any)
	if !ok {
		if v, wrapped := root["providers"]; wrapped {
			return []contractFailure{{Path: "providers", Expected: "an object", Actual: kind(v), Sample: sampleOf(v)}}
		}
		providers = root
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
