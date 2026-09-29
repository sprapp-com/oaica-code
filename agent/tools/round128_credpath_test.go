package tools

import "testing"

// F128-L1-3: the denylist is case-insensitive, as bash's is (a case-insensitive filesystem serves .SSH/ID_RSA).
func TestRound128RefuseCredentialPathIsCaseInsensitive(t *testing.T) {
	for _, p := range []string{"/home/u/.SSH/ID_RSA", "/home/u/.ssh/id_ed25519", "/Users/u/.AWS/Credentials", "/etc/SHADOW"} {
		if refuseCredentialPath("/work", p) == nil {
			t.Errorf("%s was not refused", p)
		}
	}
	if err := refuseCredentialPath("/work", "notes/readme.md"); err != nil {
		t.Errorf("an ordinary path was refused: %v", err)
	}
}
