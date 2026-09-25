package cmd

// site_installer_mirror_test.go — site/README.md states plainly that
// "install.sh and install.ps1 there are copies of scripts/{install.sh,
// install.ps1}", and docs/ENTERPRISE.md tells a reviewer what that installer
// does (unpack into a temporary directory, then install the verified binary).
//
// The copy drifted: scripts/install.sh was fixed to unpack into $TEMP_DIR, and
// site/install.sh — the file the Pages project actually serves — still piped
// the archive into `$SUDO tar -xf - -C /usr/local`, reintroducing exactly the
// root-owned half-written tree the fix removed, while both documents went on
// describing the fixed behaviour (2026-09-26 audit). A mirror nothing checks
// is a mirror that drifts.

import (
	"os"
	"testing"
)

func TestSiteInstallScriptsMirrorTheRepoCopies(t *testing.T) {
	for _, name := range []string{"install.sh", "install.ps1"} {
		want, err := os.ReadFile("../scripts/" + name)
		if err != nil {
			t.Fatalf("reading scripts/%s: %v", name, err)
		}
		got, err := os.ReadFile("../site/" + name)
		if err != nil {
			t.Fatalf("reading site/%s: %v — site/README.md promises this file is a copy of scripts/%s", name, err, name)
		}
		if string(got) != string(want) {
			t.Errorf("site/%s has drifted from scripts/%s — site/README.md promises they are copies, and site/ is what the install URL serves: copy the script across and redeploy the Pages project", name, name)
		}
	}
}
