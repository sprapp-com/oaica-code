package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ollama/ollama/cmd/launch"
)

// The 16-row Ollama cloud catalog must render inside a window instead of
// pushing the top sections off-screen (2026-09-01 overflow report).
func TestPinnedSectionWindowing(t *testing.T) {
	var items []launch.SelectionItem
	for i := 0; i < 16; i++ {
		items = append(items, launch.SelectionItem{Name: fmt.Sprintf("ollama/m%02d", i), OllamaCloud: true, Recommended: true})
	}
	m := selectorModelWithCurrent("t", ReorderItems(ConvertItems(items)), "ollama/m12")
	content := m.renderContent()

	if lines := strings.Count(content, "\n"); lines > 20 {
		t.Fatalf("render too tall (%d lines):\n%s", lines, content)
	}
	if !strings.Contains(content, "▸ ollama/m12") {
		t.Fatalf("cursor row not visible:\n%s", content)
	}
	if !strings.Contains(content, "... 5 above") || !strings.Contains(content, "... 3 more") {
		t.Fatalf("window hints missing:\n%s", content)
	}
}
