package launch

import (
	"errors"
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
	sonnet, haiku, sonnetSaved, haikuSaved := standingTierModels("", "")
	if sonnet != "zai-coding-plan/glm-4.6" || haiku != "zai-coding-plan/glm-4.5-air" {
		t.Fatalf("standingTierModels(\"\", \"\") = %q, %q, want the saved pair", sonnet, haiku)
	}
	if !sonnetSaved || !haikuSaved {
		t.Fatalf("standingTierModels(\"\", \"\") reported saved=%v/%v, want both true (they came from the file)", sonnetSaved, haikuSaved)
	}
	sonnet, haiku, sonnetSaved, haikuSaved = standingTierModels("box/kat-awq", "")
	if sonnet != "box/kat-awq" || haiku != "zai-coding-plan/glm-4.5-air" {
		t.Fatalf("standingTierModels(flag, \"\") = %q, %q — a flag must win, config fills the rest", sonnet, haiku)
	}
	// The flags matter downstream: a value the user typed is not forgiven by
	// the buildTierPlan retry, so the saved-marker must be false for it.
	if sonnetSaved || !haikuSaved {
		t.Fatalf("standingTierModels(flag, \"\") reported saved=%v/%v, want false/true", sonnetSaved, haikuSaved)
	}
}

// Which saved key a launch failure may drop. The retry's whole reason for
// existing is that a config value can rot; its whole reason for being per-tier
// is that BOTH keys are set across the fleet, so dropping the healthy one
// alongside the stale one silently re-bills Claude Code's background work at
// the primary's price.
func TestSavedTiersToDrop(t *testing.T) {
	sonnetErr := errors.New("--sonnet-model: no model named \"oaica/retired\" anywhere")
	haikuErr := errors.New("--haiku-model: remote \"zai\" is not configured")
	primaryErr := errors.New("no model named \"box/kat-awq\" anywhere")
	// The failing leg's own error interpolates the model name it could not
	// resolve, so a value that merely contains flag-like text must not make
	// the failure look like it named the other tier — that would cost the
	// healthy saved key for the launch.
	haikuErrEmbeddingSonnetFlag := errors.New(
		`--haiku-model: no model named "ghost--sonnet-model:x" anywhere: not a user remote; not on https://api.oaica.com; not pulled on the local daemon`)
	cases := []struct {
		name                    string
		err                     error
		savedSonnet, savedHaiku bool
		wantSonnet, wantHaiku   bool
		wantAttributed          bool
	}{
		{"sonnet leg fails, both saved", sonnetErr, true, true, true, false, true},
		{"haiku leg fails, both saved", haikuErr, true, true, false, true, true},
		{"sonnet leg fails, only haiku saved", sonnetErr, false, true, false, false, true},
		{"nothing saved", sonnetErr, false, false, false, false, false},
		// Unattributable: the warning must not claim the failing value came
		// from config.json, but a stale key still may not break the launch.
		{"primary fails, both saved", primaryErr, true, true, true, true, false},
		// The leg prefix decides, not a substring elsewhere in the message:
		// the healthy sonnet key survives a haiku value that reads like a
		// sonnet flag.
		{"haiku fails on a value containing --sonnet-model:", haikuErrEmbeddingSonnetFlag, true, true, false, true, true},
		{"no error at all", nil, true, true, false, false, false},
	}
	for _, c := range cases {
		gotSonnet, gotHaiku, gotAttributed := savedTiersToDrop(c.err, c.savedSonnet, c.savedHaiku)
		if gotSonnet != c.wantSonnet || gotHaiku != c.wantHaiku || gotAttributed != c.wantAttributed {
			t.Errorf("%s: savedTiersToDrop = %v/%v/%v, want %v/%v/%v",
				c.name, gotSonnet, gotHaiku, gotAttributed, c.wantSonnet, c.wantHaiku, c.wantAttributed)
		}
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
