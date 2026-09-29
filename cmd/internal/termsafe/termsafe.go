// Package termsafe makes text written by a model, a tool or a vendor safe to put on a terminal.
//
// Escape sequences in such text are commands to the user's terminal: OSC 52 writes the clipboard, OSC 0
// retitles the window, CSI 2J clears the screen so the approval prompt can be forged over it, and a CR
// with an erase-line redraws a line already printed (2026-09-29 audit, round 128, F128-L1-1/2).
package termsafe

import "strings"

// Text keeps newlines, tabs and ordinary text and neutralises every other control character: ESC becomes
// the visible symbol U+241B (so an ANSI sequence shows as "␛[31m" instead of running), a lone CR is
// dropped, and the other C0/C1 controls, DEL and the bidirectional overrides become U+FFFD.
func Text(s string) string {
	if !needsWork(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r == '\r':
			// dropped: "\r\n" line ends stay line ends, and a bare CR cannot redraw a line
		case r == 0x1b:
			b.WriteRune('␛')
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || isBidi(r):
			b.WriteRune('�')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isBidi(r rune) bool {
	return (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) || r == 0x200e || r == 0x200f || r == 0x061c
}

func needsWork(s string) bool {
	for _, r := range s {
		if (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f || (r >= 0x80 && r <= 0x9f) || isBidi(r) {
			return true
		}
	}
	return false
}
