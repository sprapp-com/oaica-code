package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// F131-L2-1 (2026-09-29 audit, round 131): temp files of pulls that are gone are swept; a live one is not.
func TestRound131StalePullTempsAreSwept(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "m.gguf")
	stale := filepath.Join(dir, "m.gguf.partial-111")
	live := filepath.Join(dir, "m.gguf.partial-222")
	for _, p := range []string{stale, live} {
		os.WriteFile(p, []byte("x"), 0o600)
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(stale, old, old)
	f, stop, err := createPullTemp(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	defer f.Close()
	if _, err := os.Stat(stale); err == nil {
		t.Error("a stale temp file survived the sweep")
	}
	if _, err := os.Stat(live); err != nil {
		t.Error("a live pull's temp file was swept")
	}
}

// F131-L2-3: a failing pull's body is redacted and printable.
func TestRound131PullErrorBodyIsQuotedAndRedacted(t *testing.T) {
	t.Setenv("OAICA_LICENSE_KEY", "sk-license-9f3c")
	got := readPullErrorBody(strings.NewReader("bad auth header: Bearer sk-license-9f3c\x1b]0;pwned\a\x1b[2K"))
	if strings.ContainsAny(got, "\x1b\a") || strings.Contains(got, "sk-license-9f3c") {
		t.Errorf("pull error body leaks: %q", got)
	}
}

// F132-L2-1: pulling model `m` must not delete the installed file of model `m.gguf.partial-v2`.
func TestRound132SweepLeavesOtherModelsAlone(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(dir, "m.gguf.partial-v2.gguf")
	os.WriteFile(other, []byte("x"), 0o600)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(other, old, old)
	f, stop, err := createPullTemp(filepath.Join(dir, "m.gguf"))
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	defer f.Close()
	if _, err := os.Stat(other); err != nil {
		t.Errorf("an installed model of another name was swept: %v", err)
	}
}

// F132-L2-4: a body that echoes the licence key across the cap does not print the part before the cut.
func TestRound132PullErrorBodyRedactsAcrossTheCap(t *testing.T) {
	t.Setenv("OAICA_LICENSE_KEY", "sk-license-abcdefghijklmnop")
	body := strings.Repeat("x", int(maxPullErrorBodyBytes)-7-16) + "Bearer sk-license-abcdefghijklmnop tail"
	got := readPullErrorBody(strings.NewReader(body))
	if strings.Contains(got, "sk-license-abc") {
		t.Errorf("part of the licence key survived the cut: %q", got[len(got)-60:])
	}
}

// F133-L1-3: what `oaica activate` saved is what `oaica pull` sends.
func TestRound133PullSendsTheActivatedLicence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("OAICA_LICENSE_KEY", "")
	os.MkdirAll(filepath.Join(home, ".oaica"), 0o700)
	os.WriteFile(filepath.Join(home, ".oaica", "license.json"), []byte(`{"key":"oaica-lic-`+strings.Repeat("a", 32)+`","instance_id":"i"}`), 0o600)
	if got := oaicaLicenseKey(); got != "oaica-lic-"+strings.Repeat("a", 32) {
		t.Errorf("oaicaLicenseKey() = %q after activation, want the activated key", got)
	}
	os.WriteFile(filepath.Join(home, ".oaica", "license_key"), []byte("explicit-file-key\n"), 0o600)
	if got := oaicaLicenseKey(); got != "explicit-file-key" {
		t.Errorf("license_key file must win over license.json: %q", got)
	}
}
