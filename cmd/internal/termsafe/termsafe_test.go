package termsafe

import (
	"strings"
	"testing"
	"unicode/utf8"
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

// F129-L1-5: raw 8-bit C1 bytes (invalid UTF-8) are neutralised like their U+009x spellings.
func TestTextNeutralisesRawC1Bytes(t *testing.T) {
	for _, in := range []string{"\x9b2J", "ok \x9d52;c;cHduZWQ=\x9c", "a\xffb"} {
		out := Text(in)
		if out == in || !utf8.ValidString(out) {
			t.Errorf("%q passed through as %q", in, out)
		}
	}
	if Text("模型") != "模型" {
		t.Errorf("valid multi-byte text was altered")
	}
}
