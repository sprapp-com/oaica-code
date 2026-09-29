package tui

import (
	"regexp"
	"strings"
	"testing"
)

var sgrRe = regexp.MustCompile("\x1b\\[[0-9;]*m")

// F132-L1-8: model names and descriptions from a remote's catalogue are quoted before they reach the terminal.
func TestRound132SelectorRendersHostileNamesSafely(t *testing.T) {
	m := selectorModel{title: "Pick:", items: []SelectItem{{Name: "evil\x1b]52;c;AAAA\a\x1b[2J", Description: "d\x1b]52;c;AAAA\a", Remote: true}}}
	view := m.View()
	if strings.ContainsAny(sgrRe.ReplaceAllString(view, ""), "\x1b\a") {
		t.Errorf("selector carries a control sequence: %q", view)
	}
}
