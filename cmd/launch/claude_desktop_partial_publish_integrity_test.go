package launch

// claude_desktop_partial_publish_integrity_test.go — the record of a gateway key
// oaica injected has to reach disk BEFORE the profile it describes is
// overwritten (2026-09-27 audit, round 18).
//
// Windows has two third-party profile roots and ConfigureAutodiscovery walks
// them in one critical section, recording each profile's prior credential and
// then writing oaica's. The state file was published once, after the whole walk
// returned nil — so when the second root's write failed (a locked file, a
// permissions problem, a path shadowed by a file) the first root was already
// holding oaica's secret with no record of it anywhere:
//
//	oaica launch claude-desktop   → error, root 1 configured
//	oaica launch --restore        → root 1 keeps oaica's key forever
//
// because restoreClaudeDesktopOllamaProfile treats an unrecorded profile as one
// oaica never wrote to and leaves its credential alone. The user sees a failed
// configure and a secret of oaica's on disk, and no way to tell restore about
// it. The fix publishes the state inside the same lock right after each record,
// before the overwrite: a record written for a profile oaica then fails to
// touch is harmless (restore puts the same value back, or deletes a key that is
// not there), while the reverse is the leak above.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// claudeDesktopPartialPublishEnv stands up the Windows write path with a second
// third-party profile root that cannot be written to, and returns the paths of
// the first (writable) root.
func claudeDesktopPartialPublishEnv(t *testing.T) claudeDesktopPaths {
	t.Helper()
	tmpDir := t.TempDir()
	setTestHome(t, tmpDir)
	withClaudeDesktopPlatform(t, "windows")
	local := filepath.Join(tmpDir, "LocalAppData")
	t.Setenv("LOCALAPPDATA", local)
	t.Setenv("OLLAMA_API_KEY", "oaica-injected-key")
	withClaudeDesktopProcessHooks(t, func() bool { return false }, func() error { return nil }, func() error { return nil })
	withClaudeDesktopValidation(t, func(context.Context, string) error { return nil })

	paths, err := claudeDesktopConfigPaths()
	if err != nil {
		t.Fatal(err)
	}
	if paths.profile == "" {
		t.Fatal("no profile path for the first third-party root")
	}
	return paths
}

func TestClaudeDesktopConfigureRecordsAKeyItInjectedBeforeALaterRootFails(t *testing.T) {
	paths := claudeDesktopPartialPublishEnv(t)

	// The second third-party root's parent is a regular file, so every write
	// under it fails the way a locked or unwritable profile directory does.
	blocked := filepath.Join(os.Getenv("LOCALAPPDATA"), "Claude Nest-3p")
	if err := os.MkdirAll(filepath.Dir(blocked), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := (&ClaudeDesktop{}).ConfigureAutodiscovery(); err == nil {
		t.Fatal("ConfigureAutodiscovery succeeded with an unwritable second profile root")
	}

	key, ok := claudeDesktopProfileGatewayKey(t, paths.profile)
	if !ok || key != "oaica-injected-key" {
		t.Fatalf("the first root was not configured before the failure (%q, %v) — the test no longer sets up a partial write", key, ok)
	}

	// Clear the obstruction so restore can run to completion, then ask for the
	// user's config back. Restore only knows what to take back from the record
	// the failed configure should have left behind.
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	if err := (&ClaudeDesktop{}).Restore(); err != nil {
		t.Fatalf("Restore returned error: %v", err)
	}

	if key, ok := claudeDesktopProfileGatewayKey(t, paths.profile); ok {
		t.Fatalf("restore left the gateway API key oaica injected into the first profile: %q — the record was lost when the second root failed, so the secret cannot be taken back", key)
	}
}
