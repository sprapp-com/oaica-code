package launch

// agent_config_store_race_test.go — the machinery shared by the per-area
// `*_store_race_integrity_test.go` files that cover the launch integrations'
// own config stores.
//
// The defect class: a read-modify-write of a config file done without a lock —
// read the whole document, mutate it in memory, publish the whole document
// back. Two oaica commands whose writers overlap read the same snapshot, both
// publish, and the rename that lands last silently deletes the other's change
// while both report success. The stores under ~/.oaica got the fix in the
// fourth audit round (fileutil.WithFileLock, auth_store.go:152) and the stores
// oaica does not own followed (foreignStoreLockBase, opencode_auth.go:42); the
// launch integrations in this file's area were still publishing unlocked
// (2026-09-26 audit, twelfth round).
//
// What these tests pin, in two halves:
//
//   - The store's lock is what orders oaica's writers, so a writer that has
//     been released while another holder has the lock must NOT rewrite the
//     file. That is the assertion that fails against an unlocked writer, and
//     it fails deterministically: an unlocked writer publishes within
//     milliseconds of its release, so the polling second below cannot miss it
//     the way a two-writer race can be missed by a lucky interleaving.
//   - The writer's READ must be inside the lock, or the lock orders only the
//     publishes. The second half arranges that: the parent's write lands while
//     it holds the lock and before the child can take it, so a writer that
//     reads after acquiring publishes a document that still holds it, while
//     one that lock-wraps only its write publishes a snapshot taken before the
//     parent's entry existed and deletes it.
//
// The child writes through the real entry point in a real process: the defect
// is between processes, and an in-process test would pass against a mutex.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ollama/ollama/cmd/internal/fileutil"
)

// agentStoreChildEnv marks the re-executed copy of this test binary. The value
// is "<kind>|<home>|<id>|<rendezvous file>".
const agentStoreChildEnv = "LAUNCH_AGENT_CONFIG_STORE_TEST_CHILD"

// The writer kinds one child can be asked to run. One constant per entry point
// a lock was added to.
const (
	agentStorePi                     = "pi"
	agentStoreCodexCleanup           = "codex-cleanup"
	agentStoreCodexApp               = "codex-app"
	agentStoreCodexAppRestore        = "codex-app-restore"
	agentStoreOpenclawEdit           = "openclaw-edit"
	agentStoreOpenclawWebSearch      = "openclaw-web-search"
	agentStoreOpenclawDeviceScopes   = "openclaw-device-scopes"
	agentStoreOpenclawSessionOverrid = "openclaw-session-override"
	agentStoreDsh                    = "dsh"
	agentStoreHermes                 = "hermes"
	agentStoreOmpModels              = "omp-models"
	agentStoreOmpAgent               = "omp-agent"
)

// TestAgentConfigStoreWriterChildHelper is not a test of its own: the parent
// re-executes the test binary with agentStoreChildEnv set and this performs one
// write through the real entry point named by the spec.
//
// HOME is installed HERE, after TestMain, because TestMain owns HOME (it
// repoints it at a shared scratch directory before any test runs) — the child's
// own environment is the only point in a test binary that outranks that.
func TestAgentConfigStoreWriterChildHelper(t *testing.T) {
	spec := os.Getenv(agentStoreChildEnv)
	if spec == "" {
		t.Skip("helper for the launch-integration config-store race tests; runs in a child process")
	}
	parts := strings.SplitN(spec, "|", 4)
	if len(parts) != 4 {
		t.Fatalf("bad child spec %q", spec)
	}
	kind, home, id, release := parts[0], parts[1], parts[2], parts[3]
	os.Setenv("HOME", home)
	os.Setenv("USERPROFILE", home)
	// Nothing in a child may reach the network: the integrations below resolve
	// model inventories and web-search state through these.
	os.Setenv("OAICA_HOST", "http://127.0.0.1:1")
	os.Setenv("OAICA_REMOTES_FILE", filepath.Join(home, "no-remotes.json"))
	os.Setenv("OAICA_AUTH_FILE", filepath.Join(home, "no-auth.json"))

	waitForAgentStoreRelease(t, release)
	if err := runAgentStoreWriter(kind, id); err != nil {
		t.Fatalf("%s %s: %v", kind, id, err)
	}
}

// runAgentStoreWriter is the one place a child maps a kind onto a production
// writer. Every entry point here is the real one — the same function an
// `oaica launch <integration>` calls.
func runAgentStoreWriter(kind, id string) error {
	switch kind {
	case agentStorePi:
		return (&Pi{}).Edit([]LaunchModel{{Name: id}})
	case agentStoreCodexCleanup:
		path, err := codexConfigPath()
		if err != nil {
			return err
		}
		return cleanupCodexLegacyProfileConfig(path)
	case agentStoreCodexApp:
		return (&CodexApp{}).ConfigureWithModels(id, []LaunchModel{fallbackLaunchModel(id)})
	case agentStoreCodexAppRestore:
		path, err := codexConfigPath()
		if err != nil {
			return err
		}
		return codexAppRestoreConfig(path)
	case agentStoreOpenclawEdit:
		return (&Openclaw{}).Edit([]LaunchModel{{Name: id}})
	case agentStoreOpenclawWebSearch:
		configureOllamaWebSearch()
		return nil
	case agentStoreOpenclawDeviceScopes:
		patchDeviceScopes()
		return nil
	case agentStoreOpenclawSessionOverrid:
		clearSessionModelOverride(id)
		return nil
	case agentStoreDsh:
		return (&DeepSeekHarness{}).ConfigureWithModels(id, []LaunchModel{fallbackLaunchModel(id)})
	case agentStoreHermes:
		return (&Hermes{}).Configure(id)
	case agentStoreOmpModels:
		return writeOMPModelsConfig(id, []LaunchModel{fallbackLaunchModel(id)})
	case agentStoreOmpAgent:
		return writeOMPAgentConfig()
	}
	return fmt.Errorf("unknown launch-integration writer kind %q", kind)
}

// waitForAgentStoreRelease blocks until the parent has released the writer.
// Polling, not a fixed sleep: a sleep long enough to cover a loaded CI box is a
// sleep the writer on a quiet box wastes, and the parent's next step (taking the
// store's lock) is what the child must then wait for rather than race.
func waitForAgentStoreRelease(t *testing.T, path string) {
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
			t.Fatal("the parent never released the writer")
		}
		time.Sleep(time.Millisecond)
	}
}

func startAgentStoreChild(t *testing.T, kind, home, id, release string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestAgentConfigStoreWriterChildHelper")
	cmd.Env = append(os.Environ(), agentStoreChildEnv+"="+kind+"|"+home+"|"+id+"|"+release)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s child: %v", kind, err)
	}
	return cmd, &out
}

// agentStoreCase is one store and one writer over it.
type agentStoreCase struct {
	// name is the writer as a user would name it, for the failure messages.
	name string
	// kind is the child dispatch constant.
	kind string
	// id is the model name this launch writes into the store.
	id string
	// store is the file under test, relative to the hermetic HOME.
	store string
	// foreign is true when the store belongs to another program, so the lock is
	// keyed under ~/.oaica/locks (foreignStoreLockBase) rather than placed
	// beside the file.
	foreign bool
	// extraFiles are seeded alongside the store (openclaw's identity file, for
	// the writer that reads a device id out of it).
	extraFiles map[string]string
	// seed is the document the store starts as: what the user (or the program
	// that owns it) had already written.
	seed string
	// other is the same document with one entry this integration does not own —
	// another writer's completed publish, written while the parent holds the
	// store's lock.
	other string
	// want are substrings the document must hold after both writes.
	want []string
	// gone are substrings the writer must have removed or replaced, so a writer
	// that did not write at all cannot pass the `want` checks vacuously.
	gone []string
}

// runAgentStoreExclusion is the whole test for one store: a child writer is
// held at a rendezvous, the parent takes the store's lock and publishes its own
// entry, and the child is then released into the lock.
//
// The poll's one second is not a sleep that hopes: it is a window an unlocked
// writer cannot avoid publishing inside, and a locked one cannot publish inside
// at all. Reaching the end of it is the behaviour being asserted.
func runAgentStoreExclusion(t *testing.T, c agentStoreCase) {
	t.Helper()
	home := t.TempDir()
	setTestHome(t, home)

	store := filepath.Join(home, c.store)
	if err := os.MkdirAll(filepath.Dir(store), 0o700); err != nil {
		t.Fatal(err)
	}
	for rel, body := range c.extraFiles {
		path := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(store, []byte(c.seed), 0o600); err != nil {
		t.Fatal(err)
	}

	// The lock the writer must meet us on. WithFileLock locks "<path>.lock", so
	// the path handed to it is the store itself when oaica owns the store, and
	// the mirror of foreignStoreLockBase when another program does; the identity
	// of the lock is part of the contract, so a change to the naming is meant to
	// break these tests.
	lockPath := store
	if c.foreign {
		lockPath = foreignStoreLockPathFor(t, home, store)
	}

	release := filepath.Join(t.TempDir(), "release")
	cmd, out := startAgentStoreChild(t, c.kind, home, c.id, release)

	err := fileutil.WithFileLock(lockPath, func() error {
		if err := os.WriteFile(store, []byte(c.other), 0o600); err != nil {
			return err
		}
		if err := os.WriteFile(release, []byte("go"), 0o600); err != nil {
			return err
		}
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			got, rerr := os.ReadFile(store)
			if rerr != nil {
				return rerr
			}
			if !bytes.Equal(got, []byte(c.other)) {
				t.Errorf("%s rewrote %s while another writer held the store's lock.\n\nThat is the whole defect: the two publishes overlap, so whichever renames last decides what the file says and the other writer's entry is gone while both commands report success. The load-mutate-save must run under the store's lock.\n\nsaw:\n%s", c.name, store, got)
				return nil
			}
			time.Sleep(20 * time.Millisecond)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("holding %s: %v", lockPath, err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the %s writer failed: %v\n%s", c.name, err, out.String())
	}

	got, err := os.ReadFile(store)
	if err != nil {
		t.Fatalf("%s is gone after the write: %v", store, err)
	}
	for _, want := range c.want {
		if !strings.Contains(string(got), want) {
			t.Errorf("%s: %q is not in %s after a write that reported success.\n\nA writer that reads the store BEFORE it takes the lock publishes a snapshot taken before this entry landed, so the other writer's entry is deleted even though the lock was held the whole time.\n\nsaw:\n%s", c.name, want, store, got)
		}
	}
	for _, gone := range c.gone {
		if strings.Contains(string(got), gone) {
			t.Errorf("%s: %q is still in %s — this writer never published anything, so the assertions above proved nothing\n%s", c.name, gone, store, got)
		}
	}
}
