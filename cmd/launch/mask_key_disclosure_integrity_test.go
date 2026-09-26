package launch

// mask_key_disclosure_integrity_test.go — the masked key in `oaica auth login`
// / `auth list` / `signin` output disclosed eight characters of a nine-character
// key (2026-09-26 audit, fifth round).
//
// maskKey rendered `key[:4] + "********" + key[len-4:]`. For a key long enough
// that the two ends cannot overlap that is a sensible display hint; for a short
// one it is not a mask at all. Nine characters produced "1234********6789" —
// eight of the nine characters, printed by `oaica auth login`, listed by
// `oaica auth list`, and shown by `oaica signin`, in output the commands
// themselves document as pasteable into a bug report. A twelve-character key
// lost eight.
//
// The rule now scales: at most a fifth of the key is shown at each end, so what
// is hidden is always the larger part. Nothing here changes the long-key shape
// ("sk-a********1234"), which is what the display hint is for.

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestMaskKeyIsRuneSafe: the scale of the mask is a rune count, so it must be
// applied in runes. Slicing by byte cut a multibyte character in half — the
// printed hint was invalid UTF-8 and showed a partial character, which is both
// a garbled report line and a disclosure the ratio did not intend. It also
// miscounted: nine two-byte runes are eighteen bytes, so `len(key)/5` showed
// four characters at each end where the rule says one.
func TestMaskKeyIsRuneSafe(t *testing.T) {
	for _, n := range []int{9, 10, 17, 20, 30} {
		key := strings.Repeat("é", n)
		got := maskKey(key)

		if !utf8.ValidString(got) {
			t.Errorf("maskKey(%d × \"é\") = %q — the mask is not valid UTF-8: the slice cut a two-byte character in half", n, got)
			continue
		}
		visible := utf8.RuneCountInString(strings.ReplaceAll(got, "*", ""))
		if visible*2 > n {
			t.Errorf("maskKey(%d × \"é\") = %q discloses %d runes of %d — the ratio was computed in bytes, so a multibyte key is masked by halves", n, got, visible, n)
		}
	}
}

// TestMaskKeyNeverDisclosesMostOfTheKey sweeps every length from 1 to 64 and
// checks that what a reader can recover is never more than half the key.
func TestMaskKeyNeverDisclosesMostOfTheKey(t *testing.T) {
	for n := 1; n <= 64; n++ {
		key := ""
		for i := 0; i < n; i++ {
			key += string(rune('a' + i%26))
		}
		got := maskKey(key)

		if strings.Contains(got, key) && n > 4 {
			t.Errorf("maskKey(%q) = %q — the whole key is printed in output documented as pasteable into a bug report", key, got)
		}

		// Count the distinct characters of the key that survived. Distinct,
		// not positions: a repeated character is disclosed once whatever the
		// mask does, and the interesting quantity is how much of the secret a
		// reader learns.
		visible := 0
		for _, r := range key {
			if strings.ContainsRune(got, r) {
				visible++
			}
		}
		if visible*2 > n {
			t.Errorf("maskKey(%q) = %q discloses %d of the key's %d distinct characters — more than half, so the hidden part is the smaller part and the mask no longer protects the credential it exists to hide",
				key, got, visible, n)
		}
		// The asterisks must still be there: a "mask" with nothing hidden is
		// the same defect stated differently.
		if !strings.Contains(got, "*") {
			t.Errorf("maskKey(%q) = %q, want the hidden part rendered as asterisks", key, got)
		}
	}
}

// TestMaskKeyShortKeysAreFullyHidden pins the boundary the old code got right,
// so the scaled rule cannot regress it.
func TestMaskKeyShortKeysAreFullyHidden(t *testing.T) {
	for _, key := range []string{"k", "sk-1", "sk-1234", "12345678"} {
		got := maskKey(key)
		if got != strings.Repeat("*", len(key)) {
			t.Errorf("maskKey(%q) = %q, want every character hidden — a short key must not be partially printed", key, got)
		}
	}
}

// TestMaskKeyLongKeysKeepTheirShape is the other control: the display hint has
// to survive, or `auth list` stops being able to tell two keys apart.
func TestMaskKeyLongKeysKeepTheirShape(t *testing.T) {
	const key = "sk-abcdefghijklmnop-1234"
	if got, want := maskKey(key), "sk-a********1234"; got != want {
		t.Errorf("maskKey(%q) = %q, want %q — the ends are the diagnostic that makes the listing useful", key, got, want)
	}
}
