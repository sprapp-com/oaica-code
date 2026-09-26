package launch

// license_redact_integrity_test.go — the redaction meant to keep a licence key
// out of terminals and tickets echoed most of a short one (2026-09-26 audit).
//
// redactLicenseKey's rule was "show the first four characters and the last
// four". For a 36-character key that is 8 of 36 — a fingerprint. For a
// nine-character key it is EIGHT OF NINE: the "redacted" value is the secret
// with one character missing, printed into an error message, a log, a
// screenshot or a support ticket. Redaction has to know how much of the value
// it is allowed to spend, and for a short key that is nothing at all.

import (
	"strings"
	"testing"
)

func TestRedactingAShortLicenceKeyRevealsNothing(t *testing.T) {
	for _, key := range []string{"123456789", "abcdefghij", "0123456789abcde"} {
		got := redactLicenseKey(key)
		if got == key {
			t.Errorf("a %d-character key was printed back verbatim: %q", len(key), got)
			continue
		}
		for _, part := range []string{key[:4], key[len(key)-4:], key[:3]} {
			if len(part) >= 3 && strings.Contains(got, part) {
				t.Errorf("redacting the %d-character key %q produced %q, which contains %q — four characters at each end of a short key is most of the secret, and this value goes into error messages, logs and tickets",
					len(key), key, got, part)
			}
		}
		if !strings.Contains(got, "****") {
			t.Errorf("the redacted form of a %d-character key (%q) does not look redacted at all", len(key), got)
		}
	}
}

// The control: a key long enough to afford a fingerprint still gets one, so
// the fix cannot be "redact everything".
func TestALongLicenceKeyStillShowsItsFingerprint(t *testing.T) {
	key := "8f14e45f-ceea-467a-9e2b-1a2b3c4d5e6f"
	got := redactLicenseKey(key)
	if !strings.HasPrefix(got, "8f14") || !strings.HasSuffix(got, "5e6f") {
		t.Errorf("redactLicenseKey(%q) = %q, want the first four and last four so the user can tell which key this is", key, got)
	}
	if strings.Contains(got, "ceea") {
		t.Errorf("the middle of a long key was revealed: %q", got)
	}
}
