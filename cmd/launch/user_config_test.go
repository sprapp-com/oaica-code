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

func TestUserConfig_HaikuRoundTripKeepsSonnet(t *testing.T) {
	home := withTestHome(t)
	if got := UserConfigHaikuModel(); got != "" {
		t.Fatalf("fresh config haiku = %q, want empty", got)
	}
	if err := UserConfigSetSonnetModel("oaica-35b-a3b-vision"); err != nil {
		t.Fatalf("set sonnet: %v", err)
	}
	if err := UserConfigSetHaikuModel("zai-coding-plan/glm-4.5-air"); err != nil {
		t.Fatalf("set haiku: %v", err)
	}
	// Writing one key must not drop the other: the whole file is rewritten
	// from the loaded struct every time.
	if got := UserConfigSonnetModel(); got != "oaica-35b-a3b-vision" {
		t.Fatalf("sonnet = %q after setting haiku, want it preserved", got)
	}
	if got := UserConfigHaikuModel(); got != "zai-coding-plan/glm-4.5-air" {
		t.Fatalf("haiku = %q", got)
	}
	c, err := UserConfigLoad()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.SonnetModel != "oaica-35b-a3b-vision" || c.HaikuModel != "zai-coding-plan/glm-4.5-air" {
		t.Fatalf("config = %+v, want both keys", c)
	}
	b, err := os.ReadFile(home + "/.oaica/config.json")
	if err != nil {
		t.Fatalf("config file missing: %v", err)
	}
	if len(b) > 0 && b[len(b)-1] == '\n' {
		t.Errorf("config written with trailing newline; keep bytes exact for atomic rename")
	}
	if err := UserConfigSetHaikuModel(""); err != nil {
		t.Fatalf("clear haiku: %v", err)
	}
	if got := UserConfigHaikuModel(); got != "" {
		t.Fatalf("cleared haiku = %q, want empty", got)
	}
}

func TestUserConfig_CorruptFileMeansNoHaikuTier(t *testing.T) {
	home := withTestHome(t)
	os.MkdirAll(home+"/.oaica", 0o700)
	os.WriteFile(home+"/.oaica/config.json", []byte("{not json"), 0o600)
	if got := UserConfigHaikuModel(); got != "" {
		t.Fatalf("corrupt config haiku = %q, want empty", got)
	}
}

// The precedence itself: a flag (or a plan) wins, the saved preference fills
// only what is still empty.
func TestStandingTierModels_PreferenceFillsOnlyTheGaps(t *testing.T) {
	withTestHome(t)
	if err := UserConfigSetSonnetModel("zai-coding-plan/glm-4.6"); err != nil {
		t.Fatal(err)
	}
	if err := UserConfigSetHaikuModel("zai-coding-plan/glm-4.5-air"); err != nil {
		t.Fatal(err)
	}
	sonnet, haiku := standingTierModels("", "")
	if sonnet != "zai-coding-plan/glm-4.6" || haiku != "zai-coding-plan/glm-4.5-air" {
		t.Fatalf("standingTierModels(\"\", \"\") = %q, %q, want the saved pair", sonnet, haiku)
	}
	sonnet, haiku = standingTierModels("box/kat-awq", "")
	if sonnet != "box/kat-awq" || haiku != "zai-coding-plan/glm-4.5-air" {
		t.Fatalf("standingTierModels(flag, \"\") = %q, %q — a flag must win, config fills the rest", sonnet, haiku)
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
