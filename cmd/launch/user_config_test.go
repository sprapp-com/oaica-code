package launch

import (
	"os"
	"testing"
)

func withTestHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	return dir
}

func TestUserConfig_RoundTrip(t *testing.T) {
	home := withTestHome(t)
	if got := UserConfigSonnetModel(); got != "" {
		t.Fatalf("fresh config sonnet = %q, want empty", got)
	}
	if err := UserConfigSetSonnetModel("oaica-35b-a3b-vision"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := UserConfigSonnetModel(); got != "oaica-35b-a3b-vision" {
		t.Fatalf("sonnet = %q, want oaica-35b-a3b-vision", got)
	}
	if err := UserConfigSetSonnetModel(""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := UserConfigSonnetModel(); got != "" {
		t.Fatalf("cleared sonnet = %q, want empty", got)
	}
	b, err := os.ReadFile(home + "/.oaica/config.json")
	if err != nil {
		t.Fatalf("config file missing: %v", err)
	}
	if len(b) > 0 && b[len(b)-1] == '\n' {
		t.Errorf("config written with trailing newline; keep bytes exact for atomic rename")
	}
}

func TestUserConfig_MissingFileIsZeroConfig(t *testing.T) {
	withTestHome(t)
	c, err := UserConfigLoad()
	if err != nil {
		t.Fatalf("load on first run: %v", err)
	}
	if c.SonnetModel != "" {
		t.Fatalf("sonnet = %q, want empty", c.SonnetModel)
	}
}

func TestUserConfig_CorruptFileDoesNotBlockLaunch(t *testing.T) {
	home := withTestHome(t)
	os.MkdirAll(home+"/.oaica", 0o700)
	os.WriteFile(home+"/.oaica/config.json", []byte("{not json"), 0o600)
	if got := UserConfigSonnetModel(); got != "" {
		t.Fatalf("corrupt config sonnet = %q, want empty (unreadable config must not block a launch)", got)
	}
}