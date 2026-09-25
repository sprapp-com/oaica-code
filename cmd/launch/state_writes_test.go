package launch

// state_writes_test.go — durability of the state files oaica owns
// (~/.oaica/remotes.json, plans.json, config.json, the caches). Two shapes
// were audited on 2026-09-26 and both are destructive:
//
//  1. remotes.json and local_servers.json were written IN PLACE
//     (os.WriteFile → O_TRUNC on the live path). A crash, a power loss, or a
//     second oaica process in the truncation window leaves a zero-byte or
//     half-written file, and loadUserRemotes turns a parse failure into a
//     hard error — so every user remote, and any inline api_key, is gone
//     (not "unset": an error).
//
//  2. The temp+rename writers all used ONE fixed temp name (path + ".tmp").
//     Two writers open the same temp with O_TRUNC; the first rename
//     publishes whatever that path holds at that instant — including the
//     other writer's half-written buffer — and the second rename fails
//     ENOENT. Reproducible with two terminals: `oaica plan set` while a
//     launch wizard saves a plan.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// A reader must never see a torn or absent remotes.json, no matter how many
// writers are running. The file holds credentials a DIFFERENT command reads,
// so a lost file is a lost login, not a cosmetic glitch.
func TestSaveUserRemotes_ConcurrentWritersNeverTearTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remotes.json")
	t.Setenv("OAICA_REMOTES_FILE", path)
	// Seed it: this test is about a file that EXISTS being torn by
	// concurrent writers, not about the first-ever write (a reader that runs
	// before any writer has published legitimately reports no remotes).
	if err := saveUserRemotesFile(userRemotesFile{Remotes: []userRemote{{Name: "box", BaseURL: "http://box:8080/v1"}}}, path); err != nil {
		t.Fatal(err)
	}

	const writers, rounds = 6, 40
	var wg sync.WaitGroup
	errCh := make(chan error, writers*rounds+1)

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				f := userRemotesFile{Remotes: []userRemote{{
					Name:    "box",
					BaseURL: "http://box:8080/v1",
					APIKey:  strings.Repeat("k", 64+256*w+i),
				}}}
				if err := saveUserRemotesFile(f, path); err != nil {
					errCh <- err
				}
			}
		}(w)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < writers*rounds; i++ {
			f, _, err := loadUserRemotesFileRaw()
			if err != nil {
				errCh <- err
				return
			}
			if len(f.Remotes) != 1 {
				errCh <- errTorn(path, "a load returned %d remotes", len(f.Remotes))
				return
			}
		}
	}()

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent write/read of remotes.json: %v", err)
	}
}

// Same contract for plans.json, which is the file a launch wizard and
// `oaica plan set` both write.
func TestPlanSet_ConcurrentWritersNeverTearTheFile(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	path := filepath.Join(home, ".oaica", "plans.json")

	const writers, rounds = 6, 30
	var wg sync.WaitGroup
	errCh := make(chan error, writers*rounds+1)

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				name := string(rune('a'+w)) + strings.Repeat("x", i%32)
				if err := PlanSet(name, TierPlanProfile{Model: "box/kat-awq"}); err != nil {
					errCh <- err
				}
			}
		}(w)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < writers*rounds; i++ {
			b, err := os.ReadFile(path)
			if err != nil {
				if os.IsNotExist(err) {
					continue // no writer has published yet
				}
				errCh <- err
				return
			}
			var p tierPlanProfiles
			if err := json.Unmarshal(b, &p); err != nil {
				errCh <- errTorn(path, "%d-byte read does not parse: %v", len(b), err)
				return
			}
		}
	}()

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent write/read of plans.json: %v", err)
	}
}

// `oaica remote add` is documented as an edit, not a reset: the fields it
// cannot name must survive. route_policy, weight and auth_via have no flag on
// `remote add` at all, so wiping them is not a reset a user can undo — the
// value is unreachable afterwards, and each one silently changes routing
// (weight 0 drops a leg from a weighted split; a lost route_policy reverts
// remote-only to local-first; a lost auth_via re-breaks credential reuse).
func TestRemoteAdd_PreservesFieldsItHasNoFlagFor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remotes.json")
	t.Setenv("OAICA_REMOTES_FILE", path)

	seed := userRemotesFile{Remotes: []userRemote{{
		Name:        "box",
		BaseURL:     "http://box:8080/v1",
		APIKey:      "sk-existing",
		RoutePolicy: string(RouteRemoteOnly),
		Weight:      3,
		AuthVia:     "opencode",
	}}}
	b, err := json.MarshalIndent(seed, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := RemoteAdd(RemoteAddOptions{Name: "box", BaseURL: "http://box:9090/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.RoutePolicy != string(RouteRemoteOnly) || got.Weight != 3 || got.AuthVia != "opencode" {
		t.Errorf("remote add dropped flagless fields: route_policy=%q weight=%d auth_via=%q",
			got.RoutePolicy, got.Weight, got.AuthVia)
	}

	f, _, err := loadUserRemotesFileRaw()
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Remotes) != 1 {
		t.Fatalf("remotes = %d, want 1", len(f.Remotes))
	}
	r := f.Remotes[0]
	if r.BaseURL != "http://box:9090/v1" {
		t.Errorf("base_url = %q, want the new one — the edit did not happen", r.BaseURL)
	}
	if r.RoutePolicy != string(RouteRemoteOnly) || r.Weight != 3 || r.AuthVia != "opencode" {
		t.Errorf("on disk: route_policy=%q weight=%d auth_via=%q", r.RoutePolicy, r.Weight, r.AuthVia)
	}
}

// errTorn builds an error that also reports the on-disk SIZE, so a zero-byte
// file (a truncation window) is distinguishable from a partially written one.
// The interactive key prompt stores the typed key and NOTHING else. Going
// through RemoteAdd (what it used to do) meant the mutually-exclusive
// api_key/api_key_env check deleted the row's api_key_env — so a user whose
// credential deliberately lived in an env var, prompted once because that
// variable was unset, ended up with the secret in plaintext in remotes.json.
// Every other hand-tuned field went the same way (wire, version, tool_format,
// route_policy, weight, auth_via, prices).
func TestSavePromptedRemoteKey_ChangesOnlyTheKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remotes.json")
	t.Setenv("OAICA_REMOTES_FILE", path)

	reliable := true
	seed := userRemotesFile{Remotes: []userRemote{{
		Name:            "mybox",
		BaseURL:         "https://box.example.com",
		APIKeyEnv:       "MYBOX_KEY",
		Version:         "v4",
		Wire:            "anthropic",
		ToolFormat:      "xml",
		ToolReliable:    &reliable,
		ForceTools:      true,
		PriceInputPerM:  1.5,
		PriceOutputPerM: 6,
		RoutePolicy:     string(RouteRemoteOnly),
		Weight:          2,
		AuthVia:         "opencode",
	}}}
	b, err := json.MarshalIndent(seed, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := savePromptedRemoteKey("mybox", "sk-typed"); err != nil {
		t.Fatal(err)
	}

	f, _, err := loadUserRemotesFileRaw()
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Remotes) != 1 {
		t.Fatalf("remotes = %d, want 1", len(f.Remotes))
	}
	got := f.Remotes[0]
	want := seed.Remotes[0]
	want.APIKey = "sk-typed"
	if got.APIKey != want.APIKey {
		t.Errorf("api_key = %q, want %q", got.APIKey, want.APIKey)
	}
	if got.APIKeyEnv != want.APIKeyEnv {
		t.Errorf("api_key_env = %q, want %q — the env indirection (a secret kept OFF disk) was deleted",
			got.APIKeyEnv, want.APIKeyEnv)
	}
	if got.Version != want.Version || got.Wire != want.Wire || got.ToolFormat != want.ToolFormat ||
		got.ForceTools != want.ForceTools || got.PriceInputPerM != want.PriceInputPerM ||
		got.PriceOutputPerM != want.PriceOutputPerM || got.RoutePolicy != want.RoutePolicy ||
		got.Weight != want.Weight || got.AuthVia != want.AuthVia {
		t.Errorf("the prompt rewrote fields it was not asked to touch:\ngot  %+v\nwant %+v", got, want)
	}
	if got.ToolReliable == nil || *got.ToolReliable != reliable {
		t.Errorf("tool_reliable = %v, want %v", got.ToolReliable, reliable)
	}
}

func errTorn(path, format string, args ...any) error {
	n := -1
	if b, err := os.ReadFile(path); err == nil {
		n = len(b)
	}
	return fmt.Errorf("%s (%s, %d bytes on disk)", fmt.Sprintf(format, args...), filepath.Base(path), n)
}
