package termsafe

import (
	"strings"
	"testing"
)

func TestTextNeutralisesTerminalControls(t *testing.T) {
	in := "ok\n\tline \x1b]52;c;cHduZWQ=\a \x1b[2J \r\x1b[2K \u009b31m \x7f ‮ done"
	out := Text(in)
	for _, bad := range []string{"\x1b", "\a", "\r", "\u009b", "\x7f", "‮"} {
		if strings.Contains(out, bad) {
			t.Errorf("%q survived in %q", bad, out)
		}
	}
	if !strings.Contains(out, "ok\n\tline ") || !strings.Contains(out, "␛]52") {
		t.Errorf("ordinary text or the visible escape marker was lost: %q", out)
	}
	if got := Text("plain\r\ntext"); got != "plain\ntext" {
		t.Errorf("CRLF line ends became %q", got)
	}
	if Text("模型 ünï") != "模型 ünï" {
		t.Errorf("non-ASCII text was altered")
	}
}
