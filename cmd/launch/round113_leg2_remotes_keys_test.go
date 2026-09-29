package launch

// round113_leg2_remotes_keys_test.go — leg 2, round 113 (2026-09-29 audit), F113-L2-3.
//
// loadUserRemotes decoded remotes.json with plain json.Unmarshal, and warnSkippedRemoteRow
// fired only for a row missing its name or base_url, so a hand-edited row with a misspelled
// key was dropped in silence, including keys that carry policy: `route_polcy: "local-only"`
// left traffic the operator meant to keep local free to leave, `api_key_evn` gave a later 401
// that named nothing, `force_tool` and `wieght` did nothing. A key that no field of the row
// reads is now warned about, naming the file, the row and the key. It is a warning, not a
// refusal: a newer oaica may have written a member this one does not model, and the rewrite
// paths deliberately keep such members.

import (
	"bytes"
	"strings"
	"testing"
)

func TestMine113AMisspelledRemoteKeysAreWarnedAbout(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"http://box:8080/v1","api_key_evn":"BOXKEY","route_polcy":"local-only","force_tool":true,"wieght":5,"zz\u001b[31mred":1}]}`)
	var out bytes.Buffer
	old := planNotices
	planNotices = &out
	t.Cleanup(func() { planNotices = old })
	rs, err := loadUserRemotes()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rs {
		if r.Name == "box" {
			found = true
		}
	}
	if !found {
		t.Fatalf("premise: the row was not loaded")
	}
	got := out.String()
	for _, key := range []string{"api_key_evn", "route_polcy", "force_tool", "wieght"} {
		if !strings.Contains(got, key) {
			t.Errorf("no warning names the unknown key %q — a misspelled policy key was dropped in silence (2026-09-29 audit, round 113, F113-L2-3)\n%s", key, got)
		}
	}
	if strings.Contains(got, "\x1b[31m") {
		t.Errorf("a key carrying a terminal escape was echoed raw and could forge output: %q (2026-09-29 audit, round 113, F113-L2-3)", got)
	}
	if !strings.Contains(got, "box") {
		t.Errorf("the warning does not name the row: %s (2026-09-29 audit, round 113, F113-L2-3)", got)
	}
}

func TestMine113AValidRemoteRowWarnsAboutNothing(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"http://box:8080/v1","api_key_env":"BOXKEY","Version":"v4"}]}`)
	var out bytes.Buffer
	old := planNotices
	planNotices = &out
	t.Cleanup(func() { planNotices = old })
	if _, err := loadUserRemotes(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "unknown") {
		t.Errorf("a valid row produced an unknown-key warning (a key differing only in case is one the decoder reads): %s (2026-09-29 audit, round 113, F113-L2-3)", out.String())
	}
}
