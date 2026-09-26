package launch

// foreign_store_concurrency_integrity_test.go — oaica's writers over stores
// OTHER tools own were an unlocked load → mutate-in-memory →
// write-whole-document, so two overlapping oaica processes each published a
// snapshot and the loser's edit vanished while BOTH reported success
// (2026-09-26 audit, eleventh round).
//
// The stores under ~/.oaica got the cross-process lock in the fourth round
// (fileutil.WithFileLock, auth_store.go:152). A store oaica does not own needs
// the same treatment for the same reason: the lock is what makes oaica
// processes take turns instead of reading the same snapshot.
//
// The concrete path the auditor demonstrated: `oaica signin opencode:deepseek`
// and `oaica signin opencode:openrouter` from one provisioning script. Both
// read all of opencode's ~/.local/share/opencode/auth.json, each sets its own
// provider, and the rename that lands last publishes a document with the other
// provider's entry deleted — while both print "Saved API key for opencode
// provider ...". If the deleted entry was opencode's own OAuth login, the
// refresh token is gone and oaica cannot re-mint it (auth_external.go is
// deliberately read-only on OAuth), so the user has to redo the browser flow.
// The same shape covers opencode's model.json (one launch's recently-used
// models dropped by a concurrent launch's publish).
//
// These run real child processes: the defect is between processes, and an
// in-process test would pass against a mutex. The children are released from a
// rendezvous file so that the overlap is not left to luck — the race IS what is
// being tested, and a broken writer that happened to serialise would pass.

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ollama/ollama/cmd/internal/fileutil"
)

// foreignChildEnv marks the re-executed copy of this test binary. The value is
// "<kind>|<store or home>|<id>|<rendezvous file>".
const foreignChildEnv = "LAUNCH_FOREIGN_STORE_TEST_CHILD"

// TestForeignStoreWriterChildHelper is not a test of its own: the parent
// re-executes the test binary with foreignChildEnv set and this performs one
// write through the real writer.
//
// The store path comes through the spec and is installed HERE, after TestMain,
// because TestMain owns HOME and masks OPENCODE_AUTH_FILE — the child's own
// environment is the only point in a test binary that outranks those.
func TestForeignStoreWriterChildHelper(t *testing.T) {
	spec := os.Getenv(foreignChildEnv)
	if spec == "" {
		t.Skip("helper for the foreign-store concurrency tests; runs in a child process")
	}
	parts := strings.SplitN(spec, "|", 4)
	if len(parts) != 4 {
		t.Fatalf("bad child spec %q", spec)
	}
	kind, target, id, release := parts[0], parts[1], parts[2], parts[3]

	if kind == "opencode-auth" {
		os.Setenv("OPENCODE_AUTH_FILE", target)
	} else {
		// opencode's model.json and VS Code's chatLanguageModels.json resolve
		// through the home directory, not a variable of their own.
		os.Setenv("HOME", target)
	}
	waitForRelease(t, release)

	var err error
	switch kind {
	case "opencode-auth":
		err = SaveOpencodeAPIKey(id, "sk-"+id)
	case "opencode-recent":
		err = (&OpenCode{}).Edit([]LaunchModel{{Name: id}})
	case "vscode":
		err = (&VSCode{}).Edit([]LaunchModel{{Name: id}})
	default:
		t.Fatalf("unknown kind %q", kind)
	}
	if err != nil {
		t.Fatalf("%s %s: %v", kind, id, err)
	}
}

// waitForRelease blocks until the parent has started every writer. Polling, not
// a fixed sleep: a sleep long enough to cover a loaded CI box is a sleep the
// writers on a quiet box waste, and the point of the rendezvous is that all of
// them are released together.
func waitForRelease(t *testing.T, path string) {
	t.Helper()
	if path == "" {
		return
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the parent never released the writers")
		}
		time.Sleep(time.Millisecond)
	}
}

// startForeignWriter starts one child, held at the rendezvous until release.
func startForeignWriter(t *testing.T, kind, target, id, release string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestForeignStoreWriterChildHelper")
	cmd.Env = append(os.Environ(), foreignChildEnv+"="+kind+"|"+target+"|"+id+"|"+release)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s child %s: %v", kind, id, err)
	}
	return cmd, &out
}

// runForeignWriters starts one child per id and releases them TOGETHER. Started
// one at a time and left to run, the first child usually finishes before the
// last has read the store, and the test would pass against the broken writer.
func runForeignWriters(t *testing.T, kind, target string, ids []string) {
	t.Helper()
	release := filepath.Join(t.TempDir(), "release")
	cmds := make([]*exec.Cmd, 0, len(ids))
	outs := make([]*bytes.Buffer, 0, len(ids))
	for _, id := range ids {
		cmd, out := startForeignWriter(t, kind, target, id, release)
		cmds = append(cmds, cmd)
		outs = append(outs, out)
	}
	if err := os.WriteFile(release, []byte("go"), 0o600); err != nil {
		t.Fatalf("release the writers: %v", err)
	}
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("child %d failed: %v\n%s", i, err, outs[i].String())
		}
	}
}

// foreignStoreLockPathFor mirrors foreignStoreLockBase (opencode_auth.go) for
// the test below that holds the lock itself. The IDENTITY of the lock is part
// of the contract that test pins — two oaica processes must meet on the same
// one — so a change to the naming is meant to break it.
func foreignStoreLockPathFor(t *testing.T, home, store string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(store))
	return filepath.Join(home, ".oaica", "locks", fmt.Sprintf("%s-%x", filepath.Base(store), sum[:6]))
}

// TestOpencodeSigninKeepsEveryConcurrentProvidersEntry is the auditor's
// scenario: a provisioning script signs in several opencode providers at once.
// Every one of them reports success, so every one of them must still be in
// opencode's store afterwards — including entries the signins did not touch.
func TestOpencodeSigninKeepsEveryConcurrentProvidersEntry(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "opencode-auth.json")
	// A live OAuth login is the entry whose loss oaica cannot repair: the
	// refresh token is not something auth_external.go re-mints, so a lost entry
	// here costs the user a browser flow, not a paste.
	seed := `{"opencode-go":{"type":"oauth","access":"at-live","refresh":"rt-live","expires":4102444800000},` +
		`"zai":{"type":"api","key":"sk-seed"}}`
	if err := os.WriteFile(store, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	ids := []string{"anthropic", "openai", "openrouter", "moonshot", "mistral", "groq", "together", "cerebras"}
	runForeignWriters(t, "opencode-auth", store, ids)

	doc := ocRawDoc(t, store)
	for _, id := range ids {
		if _, ok := doc[id]; !ok {
			t.Errorf("provider %q is missing from opencode's auth.json after its `oaica signin opencode:%s` reported success (%d entries stored, %d signins) — a provisioning script that signs in several providers at once leaves the ones that lost the race unconfigured", id, id, len(doc), len(ids))
		}
	}
	if got, _ := doc["zai"]["key"].(string); got != "sk-seed" {
		t.Errorf("the credential that was already in the store did not survive: zai key = %q, want sk-seed", got)
	}
	if got, _ := doc["opencode-go"]["refresh"].(string); got != "rt-live" {
		t.Errorf("opencode's own OAuth login did not survive the concurrent signins: refresh = %q, want rt-live — oaica does not re-mint that token, so the user has to redo the browser flow", got)
	}
}

// TestOpencodeLaunchKeepsEveryConcurrentLaunchesRecentModels is the same
// read-modify-write on opencode's model.json: Edit rewrites `recent` from a
// snapshot it read, preserving the entries other launches put there. Two
// launches that overlap each published their own list, so one launch's
// recently-used models were gone from the picker while both launches looked
// fine.
func TestOpencodeLaunchKeepsEveryConcurrentLaunchesRecentModels(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	ids := []string{"fc-aa", "fc-bb", "fc-cc", "fc-dd", "fc-ee"}
	runForeignWriters(t, "opencode-recent", home, ids)

	statePath, err := openCodeStatePath()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("no opencode model state was written: %v", err)
	}
	var state struct {
		Recent []struct {
			ProviderID string `json:"providerID"`
			ModelID    string `json:"modelID"`
		} `json:"recent"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("opencode's model.json is not parseable: %v\n%s", err, raw)
	}
	seen := make(map[string]bool, len(state.Recent))
	for _, e := range state.Recent {
		seen[e.ModelID] = true
	}
	for _, id := range ids {
		if !seen[id] {
			t.Errorf("model %q is missing from opencode's recently-used list after a concurrent launch reported success (%d of %d entries) — the launch that renamed last published its own snapshot over the other's models", id, len(state.Recent), len(ids))
		}
	}
}

// TestVSCodeEditSerialisesWithAnotherWriterOfItsStore pins the property the
// lock gives this file, which is not a property of oaica's own rows: Edit
// writes exactly one deterministic `ollama` vendor row and removes only that
// row, so two oaica Edits cannot lose each other's entries. What they can do is
// publish concurrently, and what any writer of this file — including VS Code
// itself — can lose is a row that appeared between the read and the rename.
// The lock orders oaica's writers; it cannot order anyone else's (see
// foreignStoreLockBase).
func TestVSCodeEditSerialisesWithAnotherWriterOfItsStore(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	v := &VSCode{}
	clm := v.chatLanguageModelsPath()
	if err := os.MkdirAll(filepath.Dir(clm), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `[{"vendor":"copilot","name":"Copilot","url":"https://api.example.com"}]`
	if err := os.WriteFile(clm, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	release := filepath.Join(t.TempDir(), "release")
	cmd, out := startForeignWriter(t, "vscode", home, "locked-model", release)

	err := fileutil.WithFileLock(foreignStoreLockPathFor(t, home, clm), func() error {
		if err := os.WriteFile(release, []byte("go"), 0o600); err != nil {
			return err
		}
		// An Edit that does not take the lock publishes within milliseconds of
		// the release; one that does is still waiting here. A second of polling
		// makes the difference a behaviour rather than a timing accident.
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			b, rerr := os.ReadFile(clm)
			if rerr == nil && !bytes.Equal(b, []byte(seed)) {
				t.Errorf("an Edit rewrote chatLanguageModels.json while another writer held the store's lock — the two publishes overlap and the one that renames last decides what the file says")
				return nil
			}
			time.Sleep(20 * time.Millisecond)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("holding the store lock: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Edit child failed: %v\n%s", err, out.String())
	}

	raw, err := os.ReadFile(clm)
	if err != nil {
		t.Fatal(err)
	}
	var entries []map[string]any
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("chatLanguageModels.json is not parseable after the Edit: %v\n%s", err, raw)
	}
	var vendor, copilot int
	for _, e := range entries {
		switch e["vendor"] {
		case "ollama":
			vendor++
		case "copilot":
			copilot++
		}
	}
	if vendor != 1 {
		t.Errorf("the Edit left %d ollama entries, want exactly 1", vendor)
	}
	if copilot != 1 {
		t.Errorf("the copilot vendor row that was in the file is gone (%d found) — Edit removes only the ollama entry", copilot)
	}
}
