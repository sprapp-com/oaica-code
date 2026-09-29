package agent

import (
	"strings"
	"testing"
)

// F130-L1-5 (2026-09-29 audit, round 130): output that begins with the omission marker's words is output, not
// the marker.
func TestRound130GenuineOutputIsNotTheOmissionMarker(t *testing.T) {
	s := &Session{}
	genuine := toolOutputFullOmissionPrefix + " this is a file the user cat'ed"
	msg := s.toolMessageWithBudget("bash", "c1", genuine, RunOptions{}, 0, 0)
	if toolOutputFullyOmitted(msg.Content) {
		t.Errorf("genuine output passes for the omission marker: %q", msg.Content)
	}
	if !strings.Contains(msg.Content, "this is a file") {
		t.Errorf("genuine output lost: %q", msg.Content)
	}
}
