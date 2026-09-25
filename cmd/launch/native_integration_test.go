package launch

import (
	"strings"
	"testing"
)

// A native run must never carry oaica's own profile, config overrides or
// catalog path: any of those would point the vendor CLI at our endpoint and
// spend the router's key instead of the user's own subscription. Checked
// across the whole table, so the next integration added cannot quietly omit
// the property.
func TestNativeIntegrations_NeverCarryOAICAFlags(t *testing.T) {
	banned := []string{"--profile", "-c ", "model_catalog_json", "ollama-launch", "ANTHROPIC_BASE_URL"}
	for _, n := range nativeIntegrations() {
		if n.Tool == "" || n.Binary == "" || len(n.Prefixes) == 0 || len(n.Models) == 0 {
			t.Fatalf("integration %+v is incomplete — a tool with no binary, prefix or row cannot be selected", n)
		}
		if n.ModelValue == nil || n.Args == nil {
			t.Fatalf("integration %q has no model mapping or argv builder", n.Tool)
		}
		for _, item := range n.Models {
			value, ok := n.ModelValue(item.Name)
			if !ok {
				t.Fatalf("%s row %q is not recognized by its own ModelValue", n.Tool, item.Name)
			}
			joined := strings.Join(n.Args(value, []string{"--extra"}), " ")
			for _, bad := range banned {
				if strings.Contains(joined, bad) {
					t.Fatalf("%s native args = %q, must not contain %q", n.Tool, joined, bad)
				}
			}
			if !strings.Contains(joined, "--extra") {
				t.Fatalf("%s native args = %q, dropped the user's own args", n.Tool, joined)
			}
		}
	}
}

// The picker row set for an integration comes from the table, so a tool with
// no entry gets nothing and a tool with one gets exactly its rows.
func TestNativePickerItemsFor(t *testing.T) {
	codex := nativePickerItemsFor("codex")
	if len(codex) == 0 || codex[0].Name != codexNativePrefix {
		t.Fatalf("nativePickerItemsFor(codex) = %v, want the codex/native row", codex)
	}
	if got := nativePickerItemsFor("claude"); len(got) != len(nativeClaudePickerModels) {
		t.Fatalf("nativePickerItemsFor(claude) has %d rows, want the table's %d", len(got), len(nativeClaudePickerModels))
	}
	// An integration with no native path (a plain model runner, or the generic
	// select path's "") must get no rows rather than someone else's.
	for _, tool := range []string{"", "opencode", "gemini", "qwen"} {
		if got := nativePickerItemsFor(tool); len(got) != 0 {
			t.Fatalf("nativePickerItemsFor(%q) = %v, want none", tool, got)
		}
	}
}

// Every prefix an integration declares must route to that integration, and no
// prefix may be claimed twice — two entries matching one id would make the
// dispatch order decide behavior silently.
func TestNativeIntegrations_PrefixesAreExclusive(t *testing.T) {
	seen := map[string]string{}
	for _, n := range nativeIntegrations() {
		for _, p := range n.Prefixes {
			if prev, ok := seen[p]; ok {
				t.Fatalf("prefix %q is claimed by both %s and %s", p, prev, n.Tool)
			}
			seen[p] = n.Tool
			// The bare prefix routes here; so does a qualified id, whose
			// separator is the prefix's own trailing slash when it has one
			// ("claude/opus"), else one added ("codex/native/gpt-6-sol").
			// A prefix must end at that boundary: "codex/nativex" is not the
			// codex integration (pinned in codex_native_test.go).
			qualified := strings.TrimSuffix(p, "/") + "/sample"
			for _, sample := range []string{p, qualified} {
				if got := nativeToolForModel(sample); got != n.Tool {
					t.Fatalf("nativeToolForModel(%q) = %q, want %q", sample, got, n.Tool)
				}
			}
		}
	}
}

// isNativeModel is the single gate the launcher uses, so it must agree with
// each per-tool helper.
func TestIsNativeModel_AgreesWithPerToolHelpers(t *testing.T) {
	for _, n := range nativeIntegrations() {
		for _, item := range n.Models {
			if !isNativeModel(item.Name) {
				t.Fatalf("isNativeModel(%q) = false for a %s picker row", item.Name, n.Tool)
			}
			switch n.Tool {
			case "claude":
				if !isNativeClaudeModel(item.Name) || isNativeCodexModel(item.Name) {
					t.Fatalf("per-tool helpers disagree for %q", item.Name)
				}
			case "codex":
				if !isNativeCodexModel(item.Name) || isNativeClaudeModel(item.Name) {
					t.Fatalf("per-tool helpers disagree for %q", item.Name)
				}
			}
		}
	}
	// A plain model id is never native.
	for _, name := range []string{"ollama/glm-5.3-flash", "zai/glm-5.3", "qwen3:8b", "plan/fast"} {
		if isNativeModel(name) {
			t.Fatalf("isNativeModel(%q) = true, want false", name)
		}
	}
}
