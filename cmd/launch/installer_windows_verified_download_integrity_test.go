package launch

// installer_windows_verified_download_integrity_test.go — claude's and hermes'
// Windows installers still fetched a remote script and evaluated it in place
// (2026-09-26 audit, thirteenth round).
//
// kimi, qwen and muse were moved onto installer_dl.go's verified download —
// fetch to a temp file, enforce a SHA-256 pin when one exists, then execute the
// FILE — one agent at a time. Two Windows arms were left behind:
//
//	claude:  powershell -Command "irm https://claude.ai/install.ps1 | iex"
//	hermes:  powershell -Command "& ([scriptblock]::Create((irm <url>))) -SkipSetup"
//	hermes:  the constant "curl -fsSL <url> | bash -s -- --skip-setup" (unix)
//
// Each downloads and evaluates in one step: no temp file, no hash, no pin, no
// unpinned warning, no bytes-to-review. Whatever the network answers runs with
// the user's privileges, so a compromised CDN, a DNS hijack or a MITM executes
// attacker code — on the exact platforms where that is the only install path.
//
// These tests pin the two properties the other three installers already have:
// the file that runs is the file installer_dl.go verified, and a refused
// download runs nothing at all.

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestClaudeWindowsInstallerRunsTheVerifiedDownload pins claude's Windows argv
// to the file installer_dl.go verified, with nothing that fetches or evaluates
// a remote script.
func TestClaudeWindowsInstallerRunsTheVerifiedDownload(t *testing.T) {
	restore := stubFetchInstallerScript(t)
	defer restore()

	fetched := ""
	oldFetch := fetchInstallerScriptFn
	fetchInstallerScriptFn = func(u string) (string, error) {
		fetched = u
		return stubInstallerPath, nil
	}
	t.Cleanup(func() { fetchInstallerScriptFn = oldFetch })

	bin, args, err := claudeInstallerCommand("windows")
	if err != nil {
		t.Fatalf("claudeInstallerCommand(windows) error = %v", err)
	}
	joined := strings.Join(append([]string{bin}, args...), " ")
	if fetched == "" {
		t.Fatalf("claudeInstallerCommand(windows) never went through installer_dl.go's verified download; it built %q, which executes a remote script that was never hashed", joined)
	}
	if !strings.Contains(fetched, "install.ps1") {
		t.Errorf("verified download fetched %q, want claude's Windows install script", fetched)
	}
	if bin != "powershell" {
		t.Errorf("bin = %q, want powershell", bin)
	}
	for _, want := range []string{"-Command", "Copy-Item", stubInstallerPath, "& $installer"} {
		if !strings.Contains(joined, want) {
			t.Errorf("installer command %q is missing %q — it has to run the verified file", joined, want)
		}
	}
	for _, banned := range []string{"|", "Invoke-Expression", "Invoke-RestMethod", "Invoke-WebRequest", "curl", "-OutFile"} {
		if strings.Contains(joined, banned) {
			t.Errorf("installer command %q still contains %q: the script must be executed from the verified file, never fetched or evaluated inline", joined, banned)
		}
	}
}

// TestClaudeInstallerCleansUpOnlyWhatItLeftBehind: the Windows arm's
// PowerShell removes both the verified download and its .ps1 copy, so the Go
// caller must not try to delete the last argv element — on Windows that is the
// -Command script text, not a path.
func TestClaudeInstallerCleansUpOnlyWhatItLeftBehind(t *testing.T) {
	restore := stubFetchInstallerScript(t)
	defer restore()

	_, winArgs, err := claudeInstallerCommand("windows")
	if err != nil {
		t.Fatalf("claudeInstallerCommand(windows) error = %v", err)
	}
	if got := claudeInstallerTempFile("windows", winArgs); got != "" {
		t.Errorf("claudeInstallerTempFile(windows) = %q, want \"\" — the Windows command removes its own files, and the last argument is the -Command script, so deleting it would remove the wrong (or no) path", got)
	}

	_, unixArgs, err := claudeInstallerCommand("linux")
	if err != nil {
		t.Fatalf("claudeInstallerCommand(linux) error = %v", err)
	}
	if got := claudeInstallerTempFile("linux", unixArgs); got != stubInstallerPath {
		t.Errorf("claudeInstallerTempFile(linux) = %q, want the fetched installer path %q", got, stubInstallerPath)
	}
}

// TestHermesWindowsInstallerRunsTheVerifiedDownload is the same pin for
// hermes, whose Windows arm also has to keep passing -SkipSetup.
func TestHermesWindowsInstallerRunsTheVerifiedDownload(t *testing.T) {
	restore := stubFetchInstallerScript(t)
	defer restore()

	fetched := ""
	oldFetch := fetchInstallerScriptFn
	fetchInstallerScriptFn = func(u string) (string, error) {
		fetched = u
		return stubInstallerPath, nil
	}
	t.Cleanup(func() { fetchInstallerScriptFn = oldFetch })

	bin, args, err := hermesWindowsInstallerCommand()
	if err != nil {
		t.Fatalf("hermesWindowsInstallerCommand() error = %v", err)
	}
	joined := strings.Join(append([]string{bin}, args...), " ")
	if fetched != hermesWindowsInstallURL {
		t.Errorf("verified download fetched %q, want %q — hermes' Windows arm must ask installer_dl.go for the same URL it used to fetch itself", fetched, hermesWindowsInstallURL)
	}
	if bin != "powershell.exe" {
		t.Errorf("bin = %q, want powershell.exe", bin)
	}
	for _, want := range []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", "Copy-Item", stubInstallerPath, "-SkipSetup", "& $installer"} {
		if !strings.Contains(joined, want) {
			t.Errorf("installer command %q is missing %q", joined, want)
		}
	}
	for _, banned := range []string{"|", "Invoke-Expression", "Invoke-RestMethod", "Invoke-WebRequest", "irm ", "scriptblock", "curl", "-OutFile"} {
		if strings.Contains(joined, banned) {
			t.Errorf("installer command %q still contains %q: the script must be executed from the verified file, never fetched or evaluated inline", joined, banned)
		}
	}
}

// TestHermesWindowsInstallDoesNotExecuteWhenTheDownloadIsRefused is the
// behavioural half: hermes' old Windows argv never asked the gate at all — it
// built an Invoke-RestMethod command from the URL alone — so a refusal could
// not change what ran.
func TestHermesWindowsInstallDoesNotExecuteWhenTheDownloadIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell fake binary")
	}
	tmp := t.TempDir()
	setTestHome(t, tmp)
	withHermesPlatform(t, "windows")
	t.Setenv("PATH", tmp)

	ran := filepath.Join(tmp, "ran.log")
	fake := fmt.Sprintf("#!/bin/sh\necho powershell >> %q\nexit 0\n", ran)
	if err := os.WriteFile(filepath.Join(tmp, "powershell.exe"), []byte(fake), 0o755); err != nil {
		t.Fatalf("write fake powershell: %v", err)
	}

	oldFetch := fetchInstallerScriptFn
	fetchInstallerScriptFn = func(string) (string, error) {
		return "", fmt.Errorf("installer hermes failed SHA-256 verification — refusing to execute")
	}
	t.Cleanup(func() { fetchInstallerScriptFn = oldFetch })

	err := (&Hermes{}).runInstallScript()
	if logged, _ := os.ReadFile(ran); len(logged) > 0 {
		t.Fatalf("hermes' Windows installer executed %s even though the verified download refused the bytes — a refusal that does not stop execution is not a gate", strings.TrimSpace(string(logged)))
	}
	if err == nil || !strings.Contains(err.Error(), "refusing to execute") {
		t.Fatalf("runInstallScript with a refused download = %v, want the refusal", err)
	}
}
