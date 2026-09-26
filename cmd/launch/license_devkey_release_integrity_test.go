package launch

// license_devkey_release_integrity_test.go — the dev/test licence key shipped
// inside the release binary (2026-09-26 audit, fifth round).
//
// `const testLicenseKey = "OAICA-TEST-DEV-FREE"` was compiled into every build,
// and three paths honoured it with no Lemon Squeezy call: `oaica activate <key>`
// (license.go's activateLicenseLive returned a licence file with
// ActivatedAt/ValidatedAt = now), a stored ~/.oaica/license.json holding it
// (requireLicenseLive returned nil before any revalidation), and
// OAICA_LICENSE_KEY (requireLicenseFromEnv returned nil on the same check).
// So the paywall on the prebuilt artifact — the thing the licence gate exists
// to be for a binary whose source is public — was open to anyone who read the
// source and typed the string, on any machine, offline, forever.
//
// The key now lives behind `-tags devtest` (license_devkey_dev.go), which is
// how the maintainer still runs the flow without a purchase. These tests run
// against the DEFAULT build shape and are the reason the tag cannot be added
// back to the default path by accident.

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestReleaseBuildDoesNotHonourADevTestKey
//
// The premise is that nothing in this run has registered a dev/test key — the
// release shape. Every path that once accepted the published string must now
// refuse it and go to the licence server instead.
func TestReleaseBuildDoesNotHonourADevTestKey(t *testing.T) {
	if devTestKeyBuildTagged {
		// Built with -tags devtest, where registering the key IS the point.
		// The property below belongs to the shape every release artifact is
		// built in.
		t.Skip("built with -tags devtest: this is the opt-in dev build, not the release shape")
	}
	if len(devTestLicenseKeys) != 0 {
		t.Fatalf("premise: devTestLicenseKeys = %v in a build that did NOT opt in — a key compiled in here is a key every user of the shipped artifact can type, offline and without a purchase", devTestLicenseKeys)
	}
	if isTestLicenseKey(devTestKey) {
		t.Errorf("isTestLicenseKey(%q) = true in the default build: the string is published in this repository, so anyone reading it gets a perpetual licence on the prebuilt binary with no network call — the tag `devtest` exists to keep it out of that build", devTestKey)
	}
	// An empty key must never be mistaken for one: a release variant that
	// zeroed a sentinel would compare every stored and injected key against "".
	if isTestLicenseKey("") {
		t.Errorf("isTestLicenseKey(\"\") = true — a stored licence file holding {\"key\": \"\"} would then pass the launch gate")
	}

	// Path 1: `oaica activate <published string>`.
	called := false
	stubLemonSqueezy(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"activated":false,"error":"license_key not found"}`))
	})
	if _, err := activateLicenseLive(devTestKey, ""); err == nil {
		t.Errorf("activateLicenseLive(%q) succeeded locally in the default build — it must reach the licence server like any other key", devTestKey)
	}
	if !called {
		t.Errorf("activateLicenseLive(%q) never called the licence server: the check that keeps a shipped artifact's gate meaningful is gone", devTestKey)
	}

	// Path 2: a stored ~/.oaica/license.json holding the published string.
	setLaunchTestHome(t, t.TempDir())
	stale := time.Now().Add(-licenseRevalidateTTL - time.Hour)
	if err := saveLicenseFile(licenseFile{Key: devTestKey, InstanceID: "test", ValidatedAt: stale}); err != nil {
		t.Fatalf("saveLicenseFile: %v", err)
	}
	if err := requireLicenseLive(nil, nil); err == nil {
		t.Errorf("a stored licence whose key is %q passed the launch gate with a stale validation — the dev key exemption has to be gone from the default build, or a licence file can be hand-written with the published string", devTestKey)
	}

	// Path 3: OAICA_LICENSE_KEY carrying the published string.
	setLaunchTestHome(t, t.TempDir())
	t.Setenv("OAICA_LICENSE_KEY", devTestKey)
	if err := requireLicenseLive(nil, nil); err == nil {
		t.Errorf("OAICA_LICENSE_KEY=%q passed the launch gate — an injected env var is the easiest of the three to set, so it is the one a bypass would use", devTestKey)
	}
}

// The dev flow itself must still work — it moved, it did not disappear. A test
// registers a throwaway key and every honouring path accepts it again, which is
// what `-tags devtest` does for the maintainer's build.
func TestDevTestKeysAreHonouredWhenRegistered(t *testing.T) {
	key := withDevTestKey(t)
	if !isTestLicenseKey(key) {
		t.Fatalf("isTestLicenseKey(%q) = false after registration — the dev/test flow (`oaica activate` on a machine with no purchase) is what the tag enables", key)
	}
	stubLemonSqueezy(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a registered dev/test key must not call the licence server")
	})
	f, err := activateLicenseLive(key, "")
	if err != nil {
		t.Fatalf("activateLicenseLive(%q): %v", key, err)
	}
	if f.InstanceID != "test" || f.ValidatedAt.IsZero() {
		t.Errorf("got %+v, want the local test activation", f)
	}
}

// The tag file is the only place the string may appear outside a _test.go file.
// A grep-shaped guard is crude, but the failure it catches is silent and
// expensive: re-adding the constant to license.go restores the bypass with
// nothing else to notice.
func TestPublishedDevKeyIsNotInANonTestSourceFile(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" || len(name) > 8 && name[len(name)-8:] == "_test.go" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if name == "license_devkey_dev.go" {
			continue // the tagged file is the intended home
		}
		if bytes.Contains(b, []byte(devTestKey)) {
			t.Errorf("%s contains the published dev/test key: it is compiled into the default build, and `oaica activate %s` then hands out a perpetual licence offline — keep it in license_devkey_dev.go behind `-tags devtest`", name, devTestKey)
		}
	}
}

// TestTheEnterpriseDocDoesNotClaimTheDevKeyShipsInEveryBuild
//
// docs/ENTERPRISE.md's egress table is the document an enterprise reader uses
// to decide whether the binary can phone home or be unlocked without a
// purchase, and it said the published key "is compiled into the binary" — true
// before the tag, false after, and a threat model built on a stale sentence is
// worse than no sentence. The claim must still name the tag that gates it.
func TestTheEnterpriseDocDoesNotClaimTheDevKeyShipsInEveryBuild(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "ENTERPRISE.md"))
	if err != nil {
		t.Skipf("docs/ENTERPRISE.md not readable from here: %v", err)
	}
	text := string(doc)
	if !strings.Contains(text, devTestKey) {
		return // the doc no longer mentions the key at all — nothing to be wrong about
	}
	idx := strings.Index(text, devTestKey)
	// The sentence carrying the claim: back up to the start of its table row.
	start := strings.LastIndex(text[:idx], "\n|")
	if start < 0 {
		start = 0
	}
	paragraph := text[start:]
	if end := strings.Index(paragraph, "\n|"); end > 0 {
		paragraph = paragraph[:end]
	}
	if !strings.Contains(paragraph, "devtest") {
		t.Errorf("docs/ENTERPRISE.md documents %q without naming the `devtest` build tag that registers it:\n%s\n— the shipped binary does not honour this key, and a threat model built on the opposite claim is worse than none", devTestKey, paragraph)
	}
	if strings.Contains(paragraph, "is compiled into the binary") {
		t.Errorf("docs/ENTERPRISE.md still says %q is compiled into the binary:\n%s\n— it is registered only by license_devkey_dev.go behind `-tags devtest`, so that sentence is false for every released artifact", devTestKey, paragraph)
	}
}
