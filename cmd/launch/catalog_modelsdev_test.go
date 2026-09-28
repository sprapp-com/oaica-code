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
        "tool_call": false, "status": "deprecated",
        "cost": {"input": 0.6, "output": 2.2,
                 "tiers": [{"tier": {"type": "context", "size": 32000},
                            "input": 0.9, "output": 3.0}],
                 "context_over_200k": {"input": 1.2, "output": 4.4}}
      },
      "glm-4.7": {
        "id": "glm-4.7", "name": "GLM-4.7",
        "attachment": true,
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
// glm-4.7 above has no tool_call field; glm-4.6's explicit false is in the
// same payload, so a defaulting bug cannot pass by accident.
func TestModelsDevToolCall_AbsentMeansTruePresentFalseMeansFalse(t *testing.T) {
	f, _ := parseModelsDevCatalog([]byte(modelsDevFixture))
	zai := f.Providers["zai-coding-plan"]
	if !zai.Models["glm-4.7"].toolCall() {
		t.Fatal("absent tool_call must default to true")
	}
	if zai.Models["glm-4.7"].ToolCall != nil {
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
