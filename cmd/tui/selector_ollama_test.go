package tui

import (
	"strings"
	"testing"

	"github.com/ollama/ollama/cmd/launch"
)

// Regression (2026-09-01): ReorderItems bucketed OllamaCloud rows into an
// `olc` slice that its final append never included, so every "ollama/<name>"
// cloud-catalog row vanished from the picker — filtering for "ollama" showed
// only the daemon's own 2 entries while the 18 catalog rows were silently
// dropped between the flat list and the render.
func TestReorderItemsKeepsOllamaCloudSection(t *testing.T) {
	items := []launch.SelectionItem{
		{Name: "oaica-35b-a3b-vision", Recommended: true, Remote: true},
		{Name: "ollama/gpt-oss", Recommended: true, OllamaCloud: true},
		{Name: "ollama/glm-5.3", Recommended: true, OllamaCloud: true},
		{Name: "ollama/glm-5.3-flash:cloud", Remote: true},
	}
	m := selectorModelWithCurrent("Select model for Claude Code:", ReorderItems(ConvertItems(items)), "")
	content := m.renderContent()
	for _, want := range []string{
		"Ollama (daemon + cloud)",
		"ollama/gpt-oss",
		"ollama/glm-5.3",
		"OAICA Models",
		"Remote Models",
		"ollama/glm-5.3-flash:cloud",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("render missing %q:\n%s", want, content)
		}
	}
	// Catalog rows must NOT leak into the Remote section: both sections are
	// rendered from the same flat list, so an ordering bug that misfiles one
	// row shows up as a duplicate.
	if got := strings.Count(content, "ollama/gpt-oss"); got != 1 {
		t.Fatalf("ollama/gpt-oss rendered %d times, want 1:\n%s", got, content)
	}
}