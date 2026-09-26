package launch

// openclaw_edit_corrupt_config_integrity_test.go — Openclaw.Edit read the
// daemon's config with `_ = json.Unmarshal(...)`, so a document it could not
// parse was treated as an empty one and rewritten with just the two keys oaica
// knows about (models, agents.defaults.model). What the user lost: the gateway
// token, channels, plugins — and wizard.lastRunAt, whose absence makes the next
// launch decide the install was never onboarded and run onboarding over a
// config the user already had (2026-09-26 audit).
//
// A partial decode is worse than none: Go inserts map keys as it decodes, so
// the file that comes back is the half of the document that happened to
// precede the syntax error.
//
// The rest of this integration already takes the safe stance — muse.go refuses
// to write a launch file it cannot parse, and configureOllamaWebSearch returns
// without writing.

import (
	"os"
	"strings"
	"testing"
)

func TestEditRefusesToRewriteAnUnreadableOpenclawConfig(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	path := openclawConfigPath(t, home)
	// Truncated mid-object: readable, and unparseable.
	corrupt := `{"gateway":{"token":"gw-do-not-lose-me"},"channels":[{"name":"ops"}],"models":`
	if err := os.WriteFile(path, []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}

	c := &Openclaw{}
	err := c.Edit(testLaunchModels("model-a"))
	if err == nil {
		t.Errorf("Edit returned nil for a config it could not parse — it then rewrote the file from a half-decoded map, so the token and channels above are gone and the wizard marker with them (the next launch re-onboards)")
	}
	got, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != corrupt {
		t.Errorf("the unreadable config was overwritten:\n%s\nwant it left byte-identical at:\n%s", got, corrupt)
	}
}

// The control: a readable config with members oaica does not model is still
// updated, and every one of those members survives — including a number that
// cannot round-trip through float64.
func TestEditKeepsTheMembersOpenclawOwns(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	path := openclawConfigPath(t, home)
	fixture := `{
  "gateway": {"token": "gw-keep-me"},
  "wizard": {"lastRunAt": 9007199254740993},
  "channels": [{"name": "ops"}],
  "models": {"providers": {"other": {"baseUrl": "https://example.invalid"}}}
}`
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}

	c := &Openclaw{}
	if err := c.Edit(testLaunchModels("model-a")); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"gw-keep-me", "9007199254740993", `"ops"`, "example.invalid"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("%s did not survive the update:\n%s", want, got)
		}
	}
}
