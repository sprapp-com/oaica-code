package launch

import (
	"context"
	"strings"
	"testing"
)

func TestIsNativeCodexModel(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"codex/native", true},
		{"codex/native/gpt-5.6-sol", true},
		{"codex/native/", true}, // empty id — Run's runNative passes no -m
		{"codex/gpt-5.3-codex", false},
		{"codex", false},
		{"native", false},
		{"openai/gpt-5.3-codex", false},
		{"claude/native", false},
		{"codex/nativex", false}, // prefix must end at a path boundary
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isNativeCodexModel(tt.name); got != tt.want {
				t.Fatalf("isNativeCodexModel(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

// The bare entry must pin NO model: `codex/native` leaves the choice to
// Codex's own config (~/.codex/config.toml's "model"), which is where a
// ChatGPT-plan user's own choice lives.
func TestNativeCodexModelID(t *testing.T) {
	tests := map[string]string{
		"codex/native":               "",
		"codex/native/":              "",
		"codex/native/gpt-5.6-sol":   "gpt-5.6-sol",
		"codex/native/x/y":           "x/y",
		"openai/gpt-5.3-codex":       "",
		"codex/native/gpt-5.3-codex": "gpt-5.3-codex",
	}
	for name, want := range tests {
		if got := nativeCodexModelID(name); got != want {
			t.Fatalf("nativeCodexModelID(%q) = %q, want %q", name, got, want)
		}
	}
}

// A native run must not carry the OAICA profile, the -c overrides, or the
// model-catalog path — any of those would point Codex at our endpoint and
// spend the router's key instead of the user's own ChatGPT plan.
func TestCodexNativeArgs(t *testing.T) {
	args := codexNativeArgs("gpt-5.6-sol", []string{"--dangerously-bypass-approvals-and-sandbox"})
	joined := strings.Join(args, " ")
	for _, banned := range []string{"--profile", "-c ", "model_catalog_json", "ollama-launch"} {
		if strings.Contains(joined, banned) {
			t.Fatalf("codexNativeArgs() = %q, must not contain %q", joined, banned)
		}
	}
	want := "-m gpt-5.6-sol --dangerously-bypass-approvals-and-sandbox"
	if joined != want {
		t.Fatalf("codexNativeArgs() = %q, want %q", joined, want)
	}

	// No model id → no -m at all, so Codex's own config decides.
	plain := strings.Join(codexNativeArgs("", nil), " ")
	if strings.Contains(plain, "-m") {
		t.Fatalf("codexNativeArgs(\"\", nil) = %q, want no -m", plain)
	}
}

func TestNativeCodexPickerModels(t *testing.T) {
	if len(nativeCodexPickerModels) == 0 {
		t.Fatal("no native Codex picker entries")
	}
	for _, item := range nativeCodexPickerModels {
		if !isNativeCodexModel(item.Name) {
			t.Fatalf("picker entry %q is not recognized by isNativeCodexModel", item.Name)
		}
		if strings.TrimSpace(item.Description) == "" {
			t.Fatalf("picker entry %q has no description", item.Name)
		}
		// Must not collide with the "codex/" remote-model namespace a user
		// remote could serve, nor with an integration alias.
		if hasSourcePrefix(item.Name) {
			t.Fatalf("picker entry %q collides with a source prefix", item.Name)
		}
	}
}

// A native entry has nothing to prepare: singleModelUsable must say so
// without consulting the inventory (there is no inventory row for it).
func TestSingleModelUsable_NativeCodex(t *testing.T) {
	c := &launcherClient{}
	if !c.singleModelUsable(context.Background(), "codex/native", nil) {
		t.Fatal("singleModelUsable(codex/native) = false, want true")
	}
	if !c.singleModelUsable(context.Background(), "codex/native/gpt-5.6-sol", nil) {
		t.Fatal("singleModelUsable(codex/native/gpt-5.6-sol) = false, want true")
	}
}

// The picker only offers the native rows to the codex integration: a Claude
// Code launch must not see "codex/native", and vice versa.
func TestNativePickerEntriesAreIntegrationScoped(t *testing.T) {
	for _, item := range nativeClaudePickerModels {
		if isNativeCodexModel(item.Name) {
			t.Fatalf("native Claude entry %q is also a native Codex model", item.Name)
		}
	}
	for _, item := range nativeCodexPickerModels {
		if isNativeClaudeModel(item.Name) {
			t.Fatalf("native Codex entry %q is also a native Claude model", item.Name)
		}
	}
}
