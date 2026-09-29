package agent

import (
	"strings"
	"testing"
)

// F130-L1-4: the model's argument KEYS reach the approval line quoted.
func TestRound130ApprovalLineQuotesHostileKeys(t *testing.T) {
	got := compactMap(map[string]any{"command": "ls", "\x1b[8m~\r\x1b[2K": ""})
	if strings.ContainsAny(got, "\x1b\r") {
		t.Errorf("approval line carries a control sequence: %q", got)
	}
}
