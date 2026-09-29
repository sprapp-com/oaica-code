package server

// F118-L1-1 (2026-09-29 audit, round 118): Serve hands the web_search loop the server's own handler.
// Serve cannot be run here, so the wiring is pinned at the source: without it the loop dials the
// listener a drain has closed.

import (
	"os"
	"strings"
	"testing"
)

func TestRound118ServeGivesTheLoopItsOwnHandler(t *testing.T) {
	b, err := os.ReadFile("routes.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "middleware.SetFollowUpHandler(h)") {
		t.Fatal("Serve does not call middleware.SetFollowUpHandler: a web_search turn in flight fails when the drain closes the listener")
	}
}
