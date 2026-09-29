package launch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The store the client reads (OAICA_REMOTES_FILE, README-documented) vs the file
// doctor --report describes for the 0700/0600 layout check.
func TestRound117DoctorDescribesTheStoreInUse(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	shared := t.TempDir()
	rf := filepath.Join(shared, "remotes.json")
	os.WriteFile(rf, []byte(`{"remotes":[{"name":"acme","base_url":"https://api.acme.example/v1","api_key":"sk-acme-0123456789abcdef"}]}`), 0o644)
	os.Chmod(rf, 0o644)
	t.Setenv("OAICA_REMOTES_FILE", rf)
	af := filepath.Join(shared, "auth.json")
	os.WriteFile(af, []byte(`{"version":1,"providers":{"zai":{"key":"sk-zai-0123456789abcdef"}}}`), 0o644)
	os.Chmod(af, 0o644)
	t.Setenv("OAICA_AUTH_FILE", af)

	rs, err := loadUserRemotes()
	t.Logf("client reads remotes from %s: %d remote(s) err=%v", userRemotesPath(), len(rs), err)
	st, _, aerr := loadAuthStore()
	t.Logf("client reads auth from %s: %d provider(s) err=%v", authStorePath(), len(st.Providers), aerr)
	rep, _ := buildDoctorReport()
	for _, l := range strings.Split(rep, "\n") {
		if strings.Contains(l, "remotes.json") || strings.Contains(l, "auth.json") {
			t.Logf("report: %s", l)
		}
	}
	if !strings.Contains(rep, rf) || !strings.Contains(rep, "readable by other users") {
		t.Errorf("RED: the report's layout check describes ~/.oaica/{remotes,auth}.json, not the 0644 stores the client actually reads (%s, %s)", rf, af)
	}
}
