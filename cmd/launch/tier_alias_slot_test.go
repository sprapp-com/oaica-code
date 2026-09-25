package launch

// tier_alias_slot_test.go — a user alias (~/.oaica/aliases.json) has to mean
// the same thing in every tier slot (round-4 audit, 2026-09-26).
//
// resolveLaunchEndpoint applies an alias FIRST, before every other source, so
// the primary slot has always honored one. resolveSecondaryEndpoint did not:
// the alias NAME was treated as a literal upstream id and, at the bottom of
// that function, prefixed with the primary's remote — so `--sonnet-model fast`
// ran the sonnet tier on the primary's host with the primary's credential,
// while `--model fast` went to the remote the alias names.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestAliasResolvesInTheSecondaryTierSlot(t *testing.T) {
	withTempOaicaHome(t)
	writeRemotes(t, `{"remotes":[
	  {"name":"box","base_url":"http://box.invalid/v1","api_key":"KEY_BOX"},
	  {"name":"zai","base_url":"http://zai.invalid/v1","api_key":"KEY_ZAI"}
	]}`)
	stubBareIndex(t, map[string][]string{})
	stubUserRemoteModels(t, nil, nil)
	stubCloudFetch(t, nil, nil)
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:1")
	if err := ModelAliasSet("fast", "zai/glm-air"); err != nil {
		t.Fatalf("ModelAliasSet: %v", err)
	}

	// The contract, in the slot that always had it.
	prim, err := resolveLaunchEndpoint("fast")
	if err != nil {
		t.Fatalf("resolveLaunchEndpoint(fast): %v", err)
	}
	if prim.BaseURL != "http://zai.invalid/v1" || prim.Token != "KEY_ZAI" || prim.UpstreamModel != "glm-air" {
		t.Fatalf("premise changed: the primary slot no longer resolves the alias: %+v", prim)
	}

	plan, err := buildTierPlan("box/primary-model", "fast", "", false)
	if err != nil {
		t.Fatalf("buildTierPlan: %v", err)
	}
	if plan.Secondary.BaseURL != "http://zai.invalid/v1" {
		t.Errorf("--sonnet-model fast (an alias for zai/glm-air) routed to %q with key %q and upstream model %q; "+
			"the primary slot routes the SAME name to http://zai.invalid/v1/glm-air — the alias was passed through "+
			"as a literal model id on the primary's remote",
			plan.Secondary.BaseURL, plan.Secondary.Token, plan.Secondary.UpstreamModel)
	}
	if plan.Secondary.Token != "KEY_ZAI" {
		t.Errorf("the sonnet tier carries the primary remote's credential %q for a model the alias places on zai",
			plan.Secondary.Token)
	}
	if plan.Secondary.UpstreamModel != "glm-air" {
		t.Errorf("sonnet upstream model = %q, want the alias target's id", plan.Secondary.UpstreamModel)
	}

	// The same slot through the third tier: --haiku-model shares this resolver.
	plan, err = buildTierPlan("box/primary-model", "", "fast", false)
	if err != nil {
		t.Fatalf("buildTierPlan (haiku): %v", err)
	}
	if plan.Haiku.BaseURL != "http://zai.invalid/v1" || plan.Haiku.Token != "KEY_ZAI" {
		t.Errorf("--haiku-model fast routed to %q with key %q, want zai/glm-air with KEY_ZAI",
			plan.Haiku.BaseURL, plan.Haiku.Token)
	}
}

// A remote named "ollama" is legal (`oaica remote add ollama --base-url ...`)
// and resolveRemoteEndpoint lets it win over the daemon source prefix, so
// opencode's inline provider config must not merge it with the local daemon's
// block: two groups under one id meant whichever was added first owned the
// block, and the other backend's models were declared under its base URL and
// credential — local traffic exported to a third-party remote, or the remote's
// model pointed at the daemon.
func TestOpencodeKeepsALocalAndARemoteNamedOllamaApart(t *testing.T) {
	withTempOaicaHome(t)
	writeRemotes(t, `{"remotes":[
	  {"name":"ollama","base_url":"http://other-box.invalid/v1","api_key":"KEY_OTHER_BOX","tool_format":"tool_calls"}
	]}`)
	stubBareIndex(t, map[string][]string{})
	stubUserRemoteModels(t, nil, nil)
	stubCloudFetch(t, nil, nil)
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:11434")
	t.Setenv("OAICA_HOST", "http://127.0.0.1:1")

	local := LaunchModel{Name: "llama3.2"}
	remoteModel := LaunchModel{Name: "ollama/remote-model"}

	// Premise: the remote resolves on its own, with its own key.
	ep, ok := resolveRemoteEndpoint("ollama/remote-model")
	if !ok {
		t.Fatal("premise changed: ollama/remote-model must resolve to the user remote named \"ollama\"")
	}
	if ep.Token != "KEY_OTHER_BOX" {
		t.Fatalf("premise changed: expected KEY_OTHER_BOX, got %q", ep.Token)
	}

	// Both orderings: whichever is added first used to own the merged block.
	for _, order := range [][]LaunchModel{{local, remoteModel}, {remoteModel, local}} {
		content, err := buildInlineConfig(order[0], order)
		if err != nil {
			t.Fatalf("buildInlineConfig: %v", err)
		}
		var cfg struct {
			Provider map[string]struct {
				Options struct {
					BaseURL string `json:"baseURL"`
					APIKey  string `json:"apiKey"`
				} `json:"options"`
				Models map[string]any `json:"models"`
			} `json:"provider"`
			Model string `json:"model"`
		}
		if err := json.Unmarshal([]byte(content), &cfg); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		// Walk every provider block and check each model sits under the base
		// URL its own endpoint resolves to.
		seen := map[string]string{}
		for id, p := range cfg.Provider {
			for name := range p.Models {
				seen[name] = fmt.Sprintf("%s (baseURL %s, apiKey %q)", id, p.Options.BaseURL, p.Options.APIKey)
			}
		}
		if got, ok := seen["remote-model"]; !ok || !strings.Contains(got, "http://other-box.invalid/v1") {
			t.Errorf("order %v: the remote's model \"remote-model\" is declared as %s, want the remote it resolves to (http://other-box.invalid/v1)", order, got)
		}
		if got, ok := seen["llama3.2"]; !ok || !strings.Contains(got, "http://127.0.0.1:11434/v1") {
			t.Errorf("order %v: the LOCAL daemon model \"llama3.2\" is declared as %s, want the daemon's own base URL — local traffic must not be exported to a third-party remote", order, got)
		}
		if _, ok := cfg.Provider[cfg.Model[:strings.Index(cfg.Model, "/")]]; !ok {
			t.Errorf("order %v: the top-level model %q names a provider block that does not exist", order, cfg.Model)
		}
	}
}
