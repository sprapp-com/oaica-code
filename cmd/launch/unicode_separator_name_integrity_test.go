package launch

// unicode_separator_name_integrity_test.go — a name carrying a Unicode line
// separator was accepted and printed unquoted (2026-09-26 audit, tenth round).
//
// isControlRune was ASCII-only (r < 0x20 || r == 0x7f), but the class it stands
// for — "moves the cursor or starts a new line" — is not. U+2028 (LINE
// SEPARATOR), U+2029 (PARAGRAPH SEPARATOR) and U+0085 (NEL) are line breaks to
// most consumers, and U+202E (RIGHT-TO-LEFT OVERRIDE) moves the cursor. All of
// them passed both the add-time check and printableName, so a name like
// "mine evil openai tool_calls none" was accepted and `remote list`
// printed the row split — the same forged-row failure the ASCII class was added
// for (ninth round), reached through the gap the ASCII range leaves.
//
// The zero-width and bidi-formatting ranges come along because they are the
// same hazard one step further: a character that is invisible in a terminal can
// reorder or hide the rest of a line, and a name is printed in tables people
// read.

import (
	"strings"
	"testing"
)

// separatorNames are the escapes that have to be refused, each named by what it
// does rather than by its number.
var separatorNames = []struct {
	what string
	name string
}{
	{"LINE SEPARATOR", "mine" + string(rune(0x2028)) + "zai-coding-plan  ********1234  ready"},
	{"PARAGRAPH SEPARATOR", "mine" + string(rune(0x2029)) + "evil openai tool_calls none"},
	{"NEL", "mine" + string(rune(0x85)) + "evil"},
	{"RIGHT-TO-LEFT OVERRIDE", "mine" + string(rune(0x202E)) + "evil"},
	{"LEFT-TO-RIGHT OVERRIDE", "mine" + string(rune(0x202D)) + "evil"},
	{"ZERO WIDTH SPACE", "mine" + string(rune(0x200B)) + "evil"},
	{"LEFT-TO-RIGHT MARK", "mine" + string(rune(0x200E)) + "evil"},
	{"BYTE ORDER MARK", "mine" + string(rune(0xFEFF)) + "evil"},
}

func TestAUnicodeSeparatorNameIsRejectedAndQuoted(t *testing.T) {
	withTempRemotesFile(t)

	for _, tc := range separatorNames {
		t.Run(tc.what, func(t *testing.T) {
			_, err := RemoteAdd(RemoteAddOptions{Name: tc.name, BaseURL: "https://api.example.com"})
			if err == nil {
				t.Errorf("`remote add` accepted a name carrying %s — the name is printed in `oaica remote list`/`show`, and this is a character those readers treat as a line break or a cursor move", tc.what)
			}

			// And a name that reached remotes.json anyway (hand-edited, or
			// written by an older version) is quoted, so no printer can render
			// it as structure.
			got := printableName(tc.name)
			if !strings.HasPrefix(got, `"`) {
				t.Errorf("printableName printed %q raw — remotes.json is hand-editable, so add-time validation cannot be the only line of defence", tc.name)
			}
			if strings.ContainsRune(got, firstRuneAfterASCII(tc.name)) {
				t.Errorf("printableName's output still carries the character itself: %q", got)
			}
		})
	}

	// Control: ordinary names are untouched, including the accents and CJK a
	// real provider name may carry.
	for _, name := range []string{"zai-coding-plan", "münchen-box", "東京リモート", "acme/v1"} {
		if got := printableName(name); got != name {
			t.Errorf("an ordinary name was rewritten: %q -> %q", name, got)
		}
	}
}

// firstRuneAfterASCII is the character under test: the first rune that is not
// plain ASCII (these names are otherwise ASCII, so it is the one at fault).
func firstRuneAfterASCII(s string) rune {
	for _, r := range s {
		if r > 0x7f {
			return r
		}
	}
	return 0
}
