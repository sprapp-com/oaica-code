package tui

// selector_frequent_test.go — the picker's "Frequently used" section.
//
// It leads every other section, in ranking order (the order the rows arrive in
// IS the ranking, so ReorderItems must not sort it), and it is a partition flag
// like Local/Remote/Recommended: a row marked Frequent renders in that section
// and nowhere else. The render split and the flat order must be changed
// together — they are read by two different callers, and a cursor that walks a
// flat list which disagrees with the drawn sections lands on the wrong row.

import (
	"strings"
	"testing"
)

func namesOf(items []SelectItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Name)
	}
	return out
}

func TestReorderItems_FrequentSectionLeadsInItsGivenOrder(t *testing.T) {
	in := []SelectItem{
		{Name: "zeta/remote", Remote: true},
		{Name: "local-model", Local: true},
		{Name: "second-most", Frequent: true},
		{Name: "most-used", Frequent: true},
	}
	out := ReorderItems(in)
	if len(out) != 4 {
		t.Fatalf("got %v, want four rows", namesOf(out))
	}
	if out[0].Name != "second-most" || out[1].Name != "most-used" {
		t.Fatalf("Frequent must lead in the order given (that order is the ranking), got %v", namesOf(out))
	}
	if out[2].Name != "local-model" {
		t.Fatalf("Local must follow Frequent, got %v", namesOf(out))
	}
}

// Frequent beats the other section flags: a model this machine runs is what the
// user wants first, whatever else it is.
func TestReorderItems_FrequentBeatsOtherSectionFlags(t *testing.T) {
	in := []SelectItem{
		{Name: "recommended", Recommended: true},
		{Name: "both", Frequent: true, Remote: true, Recommended: true},
	}
	out := ReorderItems(in)
	if len(out) != 2 || out[0].Name != "both" {
		t.Fatalf("got %v, want the Frequent row first", namesOf(out))
	}
}

// The section is a partition: the row renders once, under its own header.
func TestRender_FrequentRowRendersOnceUnderItsOwnHeader(t *testing.T) {
	m := selectorModel{
		title: "Pick:",
		items: []SelectItem{
			{Name: "ollama/glm-5.2:cloud", Remote: true},
			{Name: "hot-model", Frequent: true, Remote: true},
			{Name: "local-model", Local: true},
		},
	}
	view := m.View()
	if !strings.Contains(view, "Frequently used") {
		t.Fatalf("render must show the section:\n%s", view)
	}
	if got := strings.Count(view, "hot-model"); got != 1 {
		t.Fatalf("a Frequent row must render exactly once, rendered %d times:\n%s", got, view)
	}
	if freq, loc := strings.Index(view, "Frequently used"), strings.Index(view, "Local Models"); freq < 0 || loc < 0 || freq > loc {
		t.Fatalf("Frequently used must lead Local Models:\n%s", view)
	}
}

// The scrollable "More" section starts after every pinned row — a Frequent row
// is pinned, so it must not count as scrollable or the cursor's scroll math
// desyncs from what is drawn.
func TestOtherStart_PinsFrequentRows(t *testing.T) {
	m := selectorModel{
		items: []SelectItem{
			{Name: "hot-model", Frequent: true},
			{Name: "local-model", Local: true},
			{Name: "other-01"},
		},
	}
	if got := m.otherStart(); got != 2 {
		t.Fatalf("otherStart() = %d, want 2 (both pinned rows before the scrollable one)", got)
	}
}
