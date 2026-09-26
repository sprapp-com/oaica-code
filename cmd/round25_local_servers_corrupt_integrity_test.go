package cmd

// round25_local_servers_corrupt_integrity_test.go — an unreadable
// ~/.oaica/local_servers.json was rewritten from a nil snapshot (2026-09-27
// audit, round 25).
//
// oaicaReadLocalServers answered nil both for "no registry yet" and for "the
// file is there but cannot be read", and oaicaRegisterLocalServer publishes the
// document it read plus its own entry. So one corrupt byte anywhere in the file
// — a truncated write from an older build, a hand edit, a full disk — made the
// next `oaica serve` startup replace the registry with a single row: every
// other running server's entry disappeared from the picker, along with the
// --api-key recorded beside it, which is the credential the launcher's
// translation proxy sends for a "<model>:local" launch. The servers themselves
// keep running; nothing on the machine knows about them any more.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedLocalServers writes body to the registry path under a temporary HOME and
// returns the path.
func seedLocalServers(t *testing.T, body string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".oaica")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "local_servers.json")
	if body == "" {
		return path
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRegisterRefusesToReplaceAnUnreadableRegistry(t *testing.T) {
	// A truncated document: exactly what a killed write leaves behind.
	const corrupt = `[{"model":"kat","origin":"http://127.0.0.1:30001","pid":4242,"api_key":"sk-kat-0123456789"},{"model":"bons`
	path := seedLocalServers(t, corrupt)

	err := oaicaRegisterLocalServer("newmodel", "http://127.0.0.1:30009", "sk-new")
	if err == nil {
		t.Fatal("registering a server over an unreadable registry returned nil: the write replaces the file whole, so the servers it described — and their api keys — are gone from the machine's only record of them")
	}
	if !strings.Contains(err.Error(), "local_servers.json") {
		t.Errorf("the refusal does not say which file it protected: %v", err)
	}

	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != corrupt {
		t.Errorf("the registry was rewritten despite being unreadable:\n got: %s\nwant: %s", got, corrupt)
	}
}

// The teardown half needs no test of its own (2026-09-27 audit, round 26): the
// rewrite in oaicaDropLocalServers is gated on `dropped`, so a document that
// cannot be parsed — in which nothing can match — never reaches the write at
// all. A test over a corrupt registry passes whether or not the strict read is
// there, which is what made the earlier one vacuous and why it was removed
// rather than kept as decoration. The rule it would have pinned is pinned by
// construction, in the code, where the gate is.

// Control: the two states that are NOT "unreadable" keep working — an absent
// registry is an empty one (the first `oaica serve` on a box creates it), and a
// readable one keeps the other servers' rows and keys.
func TestRegisterStillWorksOverAbsentAndReadableRegistries(t *testing.T) {
	path := seedLocalServers(t, "")
	if err := oaicaRegisterLocalServer("newmodel", "http://127.0.0.1:30009", "sk-new"); err != nil {
		t.Fatalf("register over an absent registry = %v, want a fresh file", err)
	}
	entries := oaicaReadLocalServers(path)
	if len(entries) != 1 || entries[0].Model != "newmodel" {
		t.Fatalf("entries after registering over an absent registry = %+v, want one row for newmodel", entries)
	}

	path = seedLocalServers(t, `[{"model":"kat","origin":"http://127.0.0.1:30001","pid":4242,"api_key":"sk-kat-0123456789"}]`)
	if err := oaicaRegisterLocalServer("newmodel", "http://127.0.0.1:30009", "sk-new"); err != nil {
		t.Fatalf("register over a readable registry = %v", err)
	}
	entries = oaicaReadLocalServers(path)
	if len(entries) != 2 {
		t.Fatalf("entries = %+v, want the other server's row kept beside the new one", entries)
	}
	var kat *oaicaLocalServerEntry
	for i := range entries {
		if entries[i].Model == "kat" {
			kat = &entries[i]
		}
	}
	if kat == nil || kat.APIKey != "sk-kat-0123456789" {
		t.Errorf("the other server's row lost its api key: %+v", entries)
	}
}
