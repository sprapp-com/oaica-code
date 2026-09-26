package launch

// kimi_installer_verified_download_integrity_test.go — the Windows arm of
// kimi's installer piped a remote script into Invoke-Expression
// (2026-09-26 audit, tenth round).
//
// The unix arm already downloads through installer_dl.go, which hashes the
// bytes and refuses to execute them when a SHA-256 pin (built-in, or
// OAICA_INSTALL_SHA256_<URL>) does not match. The Windows arm was
//
//	powershell -Command "Invoke-RestMethod <url> | Invoke-Expression"
//
// — no temp file, no hash, no pin, no unpinned warning. Whatever the network
// answered ran with the user's privileges: a compromised CDN, a DNS hijack or
// a MITM executed attacker code, and there was no bytes-to-review step at all.
// This is the same defect qwen's installer had, fixed in af24ebb3; these tests
// pin the two properties that fix has to have here: the file that runs is the
// file installer_dl.go verified, and a refused download executes nothing.

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const kimiTestVerifiedPath = "/tmp/oaica-kimi-verified-installer.sh"

// TestKimiInstallerRunsTheVerifiedDownload pins the argv to the file
// installer_dl.go verified: bash with that one argument on unix, and on Windows
// a PowerShell command that copies the verified file somewhere it can run —
// with nothing that fetches or evaluates a remote script.
func TestKimiInstallerRunsTheVerifiedDownload(t *testing.T) {
	cases := []struct {
		goos      string
		wantBin   string
		wantURLIn string
		wantParts []string
	}{
		{"linux", "bash", "install.sh", []string{kimiTestVerifiedPath}},
		{"darwin", "bash", "install.sh", []string{kimiTestVerifiedPath}},
		{"windows", "powershell", "install.ps1", []string{"-Command", "Copy-Item", kimiTestVerifiedPath, "& $installer"}},
	}
	for _, tc := range cases {
		t.Run(tc.goos, func(t *testing.T) {
			var fetchedURL string
			oldFetch := fetchInstallerScriptFn
			fetchInstallerScriptFn = func(u string) (string, error) {
				fetchedURL = u
				return kimiTestVerifiedPath, nil
			}
			t.Cleanup(func() { fetchInstallerScriptFn = oldFetch })

			bin, args, err := kimiInstallerCommand(tc.goos)
			if err != nil {
				t.Fatalf("kimiInstallerCommand(%s) error = %v", tc.goos, err)
			}
			if fetchedURL == "" {
				t.Fatalf("kimiInstallerCommand(%s) never went through installer_dl.go's verified download; it built %q %s, which executes a remote script that was never hashed",
					tc.goos, bin, strings.Join(args, " "))
			}
			if !strings.Contains(fetchedURL, tc.wantURLIn) {
				t.Errorf("verified download fetched %q, want the %s install script", fetchedURL, tc.wantURLIn)
			}
			if bin != tc.wantBin {
				t.Errorf("bin = %q, want %q", bin, tc.wantBin)
			}
			joined := strings.Join(append([]string{bin}, args...), " ")
			for _, part := range tc.wantParts {
				if !strings.Contains(joined, part) {
					t.Errorf("installer command %q is missing %q — it has to run the verified file", joined, part)
				}
			}
			for _, banned := range []string{"|", "Invoke-Expression", "Invoke-RestMethod", "Invoke-WebRequest", "curl", "-OutFile"} {
				if strings.Contains(joined, banned) {
					t.Errorf("installer command %q still contains %q: the script must be executed from the verified file, never fetched or evaluated inline", joined, banned)
				}
			}
		})
	}
}

// TestKimiInstallerDoesNotExecuteWhenTheDownloadIsRefused is the behavioural
// half: when installer_dl.go refuses the bytes (a wrong or missing pin), the
// install must stop. The old Windows argv never asked the gate at all — it
// built an Invoke-RestMethod command from the URL alone — so a refusal could
// not change anything about what ran.
func TestKimiInstallerDoesNotExecuteWhenTheDownloadIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell fake binaries")
	}
	setTestHome(t, t.TempDir())

	tmp := t.TempDir()
	t.Setenv("PATH", tmp)

	oldGOOS := kimiGOOS
	kimiGOOS = "windows"
	t.Cleanup(func() { kimiGOOS = oldGOOS })

	// A fake powershell that records any invocation: the Windows arm runs
	// this binary, and nothing may run when the download was refused.
	ran := filepath.Join(tmp, "ran.log")
	script := fmt.Sprintf("#!/bin/sh\necho powershell >> %q\nexit 0\n", ran)
	if err := os.WriteFile(filepath.Join(tmp, "powershell"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake powershell: %v", err)
	}
	// The unix arm's shell is on the same PATH, for the same reason.
	if err := os.WriteFile(filepath.Join(tmp, "bash"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bash: %v", err)
	}

	oldFetch := fetchInstallerScriptFn
	fetchInstallerScriptFn = func(string) (string, error) {
		return "", fmt.Errorf("installer kimi failed SHA-256 verification (got aa, want bb) — refusing to execute")
	}
	t.Cleanup(func() { fetchInstallerScriptFn = oldFetch })

	oldConfirm := DefaultConfirmPrompt
	DefaultConfirmPrompt = func(string, ConfirmOptions) (bool, error) { return true, nil }
	t.Cleanup(func() { DefaultConfirmPrompt = oldConfirm })

	_, err := ensureKimiInstalled()

	logged, _ := os.ReadFile(ran)
	if len(logged) > 0 {
		t.Fatalf("the installer executed %s even though the verified download refused the bytes — a refusal that does not stop execution is not a gate", strings.TrimSpace(string(logged)))
	}
	if err == nil || !strings.Contains(err.Error(), "refusing to execute") {
		t.Fatalf("ensureKimiInstalled with a refused download = %v, want the refusal", err)
	}
}
