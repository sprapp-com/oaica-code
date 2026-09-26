package launch

// installer_temp_file_cleanup_integrity_test.go — round 16 taught the qwen
// Windows arm to randomise its %TEMP% copy name and to clean up in a finally.
// The other three Windows arms — claude, kimi, hermes — kept both defects, and
// claude's unix arm leaked the verified download whenever the install failed
// (2026-09-27 audit, round 17).
//
// Two properties, two kinds of test. The Windows one-liners cannot run on this
// host, so their shape is pinned the way TestQwenWindowsInstallerDoesNotLeave
// ItsFilesInTemp pins qwen's (see that file for why a fixed name in %TEMP% is
// the defect: %TEMP% is shared, so the path is predictable and pre-creatable,
// and Copy-Item -Force onto an existing reparse point writes through it, so the
// bytes `& $installer` runs need not be the bytes installer_dl.go verified).
// The unix leak is reachable here, so it is a real behavioural test: the
// installer is made to fail and the fetched file must be gone anyway.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// unixInstallerArm names the Windows-arm builder, a representative URL, and the
// fixed %TEMP% name each arm used to write, so one table covers all three.
type unixInstallerArm struct {
	name       string
	command    func(string) (string, []string, error)
	url        string
	fixedName  string
	extraAfter string // the installer's own flags between the run and the cleanup
}

func windowsInstallerArms() []unixInstallerArm {
	return []unixInstallerArm{
		{
			name:      "claude",
			command:   claudeInstallerCommand,
			url:       "https://claude.ai/install.ps1",
			fixedName: "install-claude.ps1",
		},
		{
			name:      "kimi",
			command:   kimiInstallerCommand,
			url:       kimiInstallScriptURLPS,
			fixedName: "install-kimi.ps1",
		},
		{
			name: "hermes",
			command: func(string) (string, []string, error) {
				return hermesWindowsInstallerCommand()
			},
			url:        hermesWindowsInstallURL,
			fixedName:  "install-hermes.ps1",
			extraAfter: "-SkipSetup",
		},
	}
}

func TestWindowsInstallerArmsRandomiseAndAlwaysCleanUp(t *testing.T) {
	verified := `C:\Users\someone\AppData\Local\Temp\oaica-install-123.sh`
	for _, arm := range windowsInstallerArms() {
		t.Run(arm.name, func(t *testing.T) {
			oldFetch := fetchInstallerScriptFn
			fetchInstallerScriptFn = func(string) (string, error) { return verified, nil }
			t.Cleanup(func() { fetchInstallerScriptFn = oldFetch })

			bin, args, err := arm.command("windows")
			if err != nil {
				t.Fatalf("%sInstallerCommand(windows) error = %v", arm.name, err)
			}
			if bin != "powershell" && bin != "powershell.exe" {
				t.Fatalf("bin = %q, want powershell", bin)
			}
			cmd := strings.Join(args, " ")

			if strings.Contains(cmd, "'"+arm.fixedName+"'") {
				t.Errorf("the installer copy is written to the fixed path %%TEMP%%\\%s, which another local process can predict and pre-create — Copy-Item -Force onto a reparse point there writes through it, so the bytes that run need not be the ones installer_dl.go verified:\n%s", arm.fixedName, cmd)
			}
			if !strings.Contains(cmd, "[System.IO.Path]::GetRandomFileName()") {
				t.Errorf("the installer copy's destination is not randomised:\n%s", cmd)
			}

			// Cleanup has to survive an installer that exits or throws part way,
			// which is exactly when a bare trailing Remove-Item does not run. The
			// caller cannot cover it: on this arm the copy's path never leaves the
			// child process, and the verified file is not the command's argument.
			if !strings.Contains(cmd, "try {") || !strings.Contains(cmd, "finally {") {
				t.Errorf("the run and its cleanup are not in a try/finally, so an installer that fails part way leaves its temp files behind:\n%s", cmd)
			}
			for _, want := range []string{
				"Remove-Item -LiteralPath $installer",
				"Remove-Item -LiteralPath $verified",
			} {
				if !strings.Contains(cmd, want) {
					t.Errorf("the command writes a temp file it never removes — %q is absent:\n%s", want, cmd)
				}
			}

			// Both removals must sit in the finally, not before it, or a failure
			// earlier in the pipeline skips them.
			runAt := strings.Index(cmd, "& $installer")
			finallyAt := strings.Index(cmd, "finally {")
			installerRmAt := strings.Index(cmd, "Remove-Item -LiteralPath $installer")
			verifiedRmAt := strings.Index(cmd, "Remove-Item -LiteralPath $verified")
			if runAt < 0 || finallyAt < 0 {
				t.Fatalf("the command does not run the copied installer inside the try:\n%s", cmd)
			}
			if finallyAt < runAt {
				t.Errorf("the finally precedes the run, so it is not a cleanup of it:\n%s", cmd)
			}
			if installerRmAt < finallyAt || verifiedRmAt < finallyAt {
				t.Errorf("a removal sits outside the finally, so a failing install skips it:\n%s", cmd)
			}

			// The verified-download contract is unchanged by this fix.
			if !strings.Contains(cmd, verified) {
				t.Errorf("the command no longer names the file installer_dl.go verified:\n%s", cmd)
			}
		})
	}
}

// The unix arms run the fetched file with bash. Claude's removed it only after
// a successful run, so every failed install left the verified download in the
// user's temp dir; kimi and qwen `defer os.Remove(args[0])` and opencode did
// not remove it at all (2026-09-27 audit, round 18).
type unixEnsureArm struct {
	name   string
	ensure func() (string, error)
}

func unixEnsureArms() []unixEnsureArm {
	return []unixEnsureArm{
		{name: "claude", ensure: ensureClaudeInstalled},
		{name: "opencode", ensure: ensureOpenCodeInstalled},
	}
}

func TestUnixInstallerArmsRemoveTheirTempFileWhenTheInstallFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell fake binaries")
	}
	for _, arm := range unixEnsureArms() {
		t.Run(arm.name, func(t *testing.T) {
			runFailedUnixInstall(t, arm.name, arm.ensure)
		})
	}
}

func runFailedUnixInstall(t *testing.T, name string, ensure func() (string, error)) {
	t.Helper()

	setTestHome(t, t.TempDir())
	tmpDir := t.TempDir()
	t.Setenv("PATH", tmpDir)
	writeFakeBinary(t, tmpDir, "curl")
	// bash exits nonzero, which is what a failed install looks like here.
	if err := os.WriteFile(filepath.Join(tmpDir, "bash"), []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatalf("write fake bash: %v", err)
	}

	fetched := filepath.Join(t.TempDir(), name+"-install.sh")
	if err := os.WriteFile(fetched, []byte("#!/bin/sh\nexit 3\n"), 0o600); err != nil {
		t.Fatalf("write fetched installer: %v", err)
	}
	oldFetch := fetchInstallerScriptFn
	fetchInstallerScriptFn = func(string) (string, error) { return fetched, nil }
	t.Cleanup(func() { fetchInstallerScriptFn = oldFetch })

	oldConfirm := DefaultConfirmPrompt
	DefaultConfirmPrompt = func(string, ConfirmOptions) (bool, error) { return true, nil }
	t.Cleanup(func() { DefaultConfirmPrompt = oldConfirm })

	if _, err := ensure(); err == nil {
		t.Fatalf("expected the failing installer to be reported")
	}
	if _, err := os.Stat(fetched); err == nil {
		t.Errorf("the fetched installer %s is still on disk after a failed install — every failed attempt accumulates one copy of a script oaica downloaded:\n%s", fetched, fetched)
	} else if !os.IsNotExist(err) {
		t.Errorf("stat %s: %v", fetched, err)
	}
}
