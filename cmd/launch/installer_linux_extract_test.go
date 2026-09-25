package launch

// installer_linux_extract_test.go — docs/ENTERPRISE.md tells a reviewer what
// the installer does, in order:
//
//	"extract `bin/oaica` into a temporary directory, then install it as
//	 `/usr/local/bin/oaica` (mode `755`), using `sudo` when that directory is
//	 not writable. ... It does not touch your shell profile, does not install
//	 a service, and writes nothing outside the temp dir and the binary's
//	 destination."
//
// The macOS branch matched. The Linux branch did not: it handed
// download_and_extract the install PREFIX ($OAICA_INSTALL_DIR = dirname of
// $BINDIR) and unpacked with `$SUDO tar -xf - -C "$dest_dir"` — an archive
// written into /usr/local as root, outside the temp dir, leaving a
// half-written tree in the prefix if the download was truncated (2026-09-26
// audit). It now unpacks into $TEMP_DIR unprivileged and installs the single
// verified binary, which is what the prose says.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func installerScript(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "scripts", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInstallerLinuxExtractsIntoTheTempDir(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "ENTERPRISE.md"))
	if err != nil {
		t.Fatal(err)
	}
	const promise = "extract `bin/oaica` into a temporary directory, then install"
	if !strings.Contains(string(doc), promise) {
		t.Fatalf("docs/ENTERPRISE.md no longer carries %q — re-read the prose before trusting this test", promise)
	}

	script := installerScript(t)

	var call string
	for _, l := range strings.Split(script, "\n") {
		if strings.Contains(l, `download_and_extract "$DOWNLOAD_BASE"`) {
			call = strings.TrimSpace(l)
			break
		}
	}
	if call == "" {
		t.Fatal("scripts/install.sh no longer calls download_and_extract on the Linux path — re-read it before trusting this test")
	}
	if !strings.Contains(script, `UNPACK_DIR="$TEMP_DIR/`) {
		t.Errorf("the Linux path no longer unpacks into a directory under $TEMP_DIR, which is what the prose promises:\n  %s", call)
	}
	if !strings.Contains(script, `install -o0 -g0 -m755 "$UNPACK_DIR/bin/oaica" "$BINDIR/oaica"`) {
		t.Errorf("the Linux path no longer installs the extracted binary into $BINDIR with an explicit mode — the prose says it installs `bin/oaica` as `/usr/local/bin/oaica` (mode 755)")
	}
	// No extraction as root: the whole point of unpacking into the temp dir
	// is that the archive never reaches the prefix except as one installed
	// file. Comment lines are skipped — the fix's own note names the old
	// `$SUDO tar` command it replaced.
	for _, l := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(l)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, "tar -x") && strings.Contains(trimmed, "$SUDO") {
			t.Errorf("an extraction runs through $SUDO:\n  %s\n— the archive must be unpacked as the calling user, inside the temp dir", trimmed)
		}
	}
}
