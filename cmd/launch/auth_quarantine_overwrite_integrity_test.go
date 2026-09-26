package launch

// auth_quarantine_overwrite_integrity_test.go — quarantining an unreadable auth
// store overwrote its own evidence (2026-09-26 audit, tenth round).
//
// quarantineAuthStore named the file it moved "<path>.unreadable-<UTC timestamp
// to the second>", and the doc comment promised "two runs cannot overwrite each
// other's evidence". Within the same second they can: the second os.Rename
// replaces the first quarantine file, and the credentials stored in it are
// gone. Two corruptions a moment apart is not exotic — a provisioning script
// that writes the store twice, or a hand-edit repaired and re-broken — and the
// whole point of the quarantine (rather than a delete) is that the bytes are
// still on disk afterwards.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestASecondQuarantineDoesNotOverwriteTheFirst(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)

	// Pin the stamp so both quarantines land on the same name — the case the
	// old code destroyed. The seam exists for exactly this: without it the
	// test would depend on two calls landing inside one wall-clock second.
	old := quarantineStamp
	quarantineStamp = func() string { return "20260101-000000" }
	t.Cleanup(func() { quarantineStamp = old })

	path := authStorePath()
	if path == "" {
		t.Fatal("no auth store path")
	}
	const first = "{ this is not JSON: the user's first store "
	const second = "{{{ and this is the second, written after the repair "
	if err := os.WriteFile(path, []byte(first), 0o600); err != nil {
		t.Fatal(err)
	}

	var warn bytes.Buffer
	if err := updateAuthStore(&warn, func(f *authStoreFile) error {
		f.Providers["one"] = authCredential{Type: authCredentialTypeAPIKey, Key: "sk-one"}
		return nil
	}); err != nil {
		t.Fatalf("first update: %v", err)
	}
	if err := os.WriteFile(path, []byte(second), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := updateAuthStore(&warn, func(f *authStoreFile) error {
		f.Providers["two"] = authCredential{Type: authCredentialTypeAPIKey, Key: "sk-two"}
		return nil
	}); err != nil {
		t.Fatalf("second update: %v", err)
	}

	// Every quarantined store is still there, with its own bytes.
	dir := filepath.Dir(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the store directory %s: %v", dir, err)
	}
	var quarantined []string
	for _, e := range entries {
		if strings.Contains(e.Name(), ".unreadable-") {
			quarantined = append(quarantined, e.Name())
		}
	}
	if len(quarantined) < 2 {
		t.Fatalf("only %d quarantined store(s) left: %v — the second rename replaced the first, so the credentials in it are gone and the warning that promised they were kept is false",
			len(quarantined), quarantined)
	}

	kept := map[string]bool{}
	for _, name := range quarantined {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		kept[string(data)] = true
	}
	if !kept[first] || !kept[second] {
		t.Errorf("the quarantined stores do not hold both original documents: kept %v", kept)
	}
}
