package launch

// qwen_installer_temp_cleanup_integrity_test.go — the Windows install arm left
// its working files behind, at a name anyone could predict (2026-09-26 audit,
// round 16).
//
// cmd has to recognise a .bat extension and installer_dl.go always writes a
// .sh, so the Windows arm copies the verified download into %TEMP% before it
// runs it. That copy was made to the fixed name "install-qwen.bat" and was
// never removed — neither was the verified file, because the caller's
// `defer os.Remove(args[0])` is guarded to the unix arms (there the verified
// path IS argv[0]; here the arm runs a copy). Every install attempt therefore
// accumulated two files in the user's %TEMP%, one of them at a path any local
// process can predict and pre-create: Copy-Item -Force onto an existing
// reparse point writes through it, so the bytes `& $installer` runs need not
// be the bytes installer_dl.go hashed.
//
// A PowerShell one-liner cannot be executed on this host, so this pins its
// shape the way TestQwenInstallerCommand does — but to the two properties that
// are the defect: the destination must be randomised, and everything the
// command writes must be removed by the command itself.

import (
	"strings"
	"testing"
)

func TestQwenWindowsInstallerDoesNotLeaveItsFilesInTemp(t *testing.T) {
	oldFetch := fetchInstallerScriptFn
	fetchInstallerScriptFn = func(string) (string, error) { return qwenTestVerifiedPath, nil }
	t.Cleanup(func() { fetchInstallerScriptFn = oldFetch })

	bin, args, err := qwenInstallerCommand("windows")
	if err != nil {
		t.Fatalf("qwenInstallerCommand(windows) error = %v", err)
	}
	if bin != "powershell" {
		t.Fatalf("bin = %q, want powershell", bin)
	}
	cmd := strings.Join(args, " ")

	// The fixed destination name is gone: it is the predictable path in a
	// shared temp dir, and nothing about running the installer needs it.
	if strings.Contains(cmd, "'install-qwen.bat'") {
		t.Errorf("the installer copy is written to the fixed path %%TEMP%%\\install-qwen.bat, which another local process can pre-create (a reparse point there is written through by Copy-Item -Force, so the bytes that run need not be the verified ones):\n%s", cmd)
	}
	if !strings.Contains(cmd, "[System.IO.Path]::GetRandomFileName()") {
		t.Errorf("the installer copy's destination is not randomised:\n%s", cmd)
	}

	// Both files must be removed by the command that wrote them. The caller
	// cannot do it: on this arm it never sees the copy's path, and the
	// verified file is not args[0] either.
	if !strings.Contains(cmd, "finally") {
		t.Errorf("the command has no finally, so a failing or throwing installer leaves its temp files behind:\n%s", cmd)
	}
	for _, want := range []string{
		"Remove-Item -LiteralPath $installer",
		"Remove-Item -LiteralPath $verified",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("the command writes a temp file it never removes — %q is absent:\n%s", want, cmd)
		}
	}

	// The cleanup must be the last thing, or a failure before it skips it.
	if i, j := strings.Index(cmd, "& $installer"), strings.Index(cmd, "Remove-Item -LiteralPath $installer"); i < 0 || j < i {
		t.Errorf("the removal does not follow the run of the copied installer, so the file can outlive it:\n%s", cmd)
	}

	// The verified-download contract is unchanged by this fix.
	if !strings.Contains(cmd, qwenTestVerifiedPath) {
		t.Errorf("the command no longer names the file installer_dl.go verified:\n%s", cmd)
	}
}
