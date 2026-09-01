package launch

// installer_dl.go — shared plumbing for third-party installers (audit L3).
// The integrations invoke upstream "curl | bash" install scripts. Piping a
// fetched script straight into bash gives no integrity check at all: a
// compromised CDN, DNS hijack, or MITM on the guest's network executes
// attacker code with user privileges. This helper downloads the script to
// a temp file first, verifies its SHA-256 when a pin is known (or when the
// user pins one via OAICA_INSTALL_SHA256_<SCHEME>), warns loudly when
// unpinned, and only then executes it from the file — so the exact bytes
// fetched are the bytes reviewed/hashable, and a failure mid-download can
// no longer truncate into a syntactically-valid partial script.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Test seams: production points these at the real implementations; tests
// stub them to avoid network access.
var (
	fetchInstallerScriptFn = fetchInstallerScript
	runInstallerScriptFn   = runInstallerScript
)

// installerSHAPins maps a script URL to its expected SHA-256. Pins here are
// advisory-only hardening for the rare case an upstream publishes a stable
// script; the common escape hatch is the OAICA_INSTALL_SHA256 env var.
var installerSHAPins = map[string]string{}

// installerSHAEnv turns a URL into the env var name a user can set to pin
// the script's hash for this run: https://claude.ai/install.sh →
// OAICA_INSTALL_SHA256_CLAUDE_AI_INSTALL_SH.
func installerSHAEnv(u string) string {
	var b strings.Builder
	b.WriteString("OAICA_INSTALL_SHA256_")
	host := strings.ReplaceAll(strings.ReplaceAll(u, "https://", ""), "http://", "")
	host = strings.ReplaceAll(host, ".", "_")
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r - 32)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// fetchInstallerScript downloads scriptURL to a temp file and verifies its
// SHA-256 when a pin exists (built-in map or OAICA_INSTALL_SHA256_* env).
// Returns the temp file path; the caller runs it and removes it.
func fetchInstallerScript(scriptURL string) (string, error) {
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Get(scriptURL)
	if err != nil {
		return "", fmt.Errorf("download installer %s: %w", scriptURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download installer %s: HTTP %d", scriptURL, resp.StatusCode)
	}
	tmp, err := os.CreateTemp("", "oaica-install-*.sh")
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", fmt.Errorf("download installer %s: %w", scriptURL, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	sum := hex.EncodeToString(h.Sum(nil))

	want := installerSHAPins[scriptURL]
	if want == "" {
		want = os.Getenv(installerSHAEnv(scriptURL))
	}
	if want != "" {
		if !strings.EqualFold(sum, strings.TrimSpace(want)) {
			os.Remove(tmp.Name())
			return "", fmt.Errorf("installer %s failed SHA-256 verification (got %s, want %s) — refusing to execute", scriptURL, sum, want)
		}
	} else {
		// Unpinned: not a refusal (upstreams rotate their scripts with
		// releases and we have no trusted hash source), but the user gets
		// the hash to eyeball against upstream docs before it runs.
		fmt.Fprintf(os.Stderr, "%swarning: executing %s WITHOUT integrity pinning (sha256 %s). Pin it with %s=<sha256>%s\n",
			ansiYellow, scriptURL, sum, installerSHAEnv(scriptURL), ansiReset)
	}
	return tmp.Name(), nil
}

// runInstallerScript downloads scriptURL, optionally verifies its SHA-256
// (see fetchInstallerScript), and executes it with bash, passing args
// through. The downloaded file is removed afterward.
func runInstallerScript(scriptURL string, args ...string) error {
	path, err := fetchInstallerScript(scriptURL)
	if err != nil {
		return err
	}
	defer os.Remove(path)
	full := append([]string{path}, args...)
	cmd := exec.Command("bash", full...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}
