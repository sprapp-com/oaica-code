package launch

// store_concurrency_integrity_test.go — the stores under ~/.oaica are
// load-mutate-write, and an atomic rename does not stop a LOST UPDATE
// (2026-09-26 audit, fourth round).
//
// Reproduced against the real CLI before the fix: eleven concurrent
// `oaica auth login <provider> --key ...` printed eleven successes and left
// ONE credential; eight concurrent `oaica remote add` left one remote. The
// user sees "Logged in to anthropic" and at launch time an unexplained
// "needs key".
//
// These run real child processes, not goroutines: the defect is between
// processes, and an in-process test would pass against a mutex.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// storeChildEnv marks the re-executed copy of this test binary. The value is
// "<kind>|<storePath>|<id>".
const storeChildEnv = "LAUNCH_STORE_TEST_CHILD"

// storeEnvVar is the path variable each store resolves through.
var storeEnvVar = map[string]string{
	"auth":     "OAICA_AUTH_FILE",
	"remote":   "OAICA_REMOTES_FILE",
	"plan":     "OAICA_PLANS_FILE",
	"manifest": "OAICA_MODELS_FILE",
	"alias":    "OAICA_ALIASES_FILE",
}

// TestStoreWriterChildHelper is not a test of its own: the parent re-executes
// the test binary with storeChildEnv set and this performs one store write.
//
// The store path comes through the spec and is installed HERE, after TestMain,
// because TestMain owns HOME (it repoints it at a shared scratch directory
// before any test runs) and masks OAICA_AUTH_FILE / OAICA_REMOTES_FILE. Setting
// the variable in the child is the only point in a test binary that outranks
// those.
func TestStoreWriterChildHelper(t *testing.T) {
	spec := os.Getenv(storeChildEnv)
	if spec == "" {
		t.Skip("helper for the store concurrency tests; runs in a child process")
	}
	parts := strings.SplitN(spec, "|", 3)
	if len(parts) != 3 {
		t.Fatalf("bad child spec %q", spec)
	}
	kind, store, id := parts[0], parts[1], parts[2]
	env, ok := storeEnvVar[kind]
	if !ok {
		t.Fatalf("unknown kind %q", kind)
	}
	os.Setenv(env, store)

	var err error
	switch kind {
	case "auth":
		err = AuthLogin(os.Stdout, id, "sk-test-"+id)
	case "remote":
		_, err = RemoteAdd(RemoteAddOptions{Name: id, BaseURL: "https://api-" + id + ".example.com"})
	case "plan":
		err = PlanSet(id, TierPlanProfile{Model: "m"})
	case "manifest":
		_, err = ModelAdd(ModelAddOptions{ID: id, Engine: "vllm", ModelPath: "/m/" + id})
	case "alias":
		err = ModelAliasSet(id, "target-"+id)
	}
	if err != nil {
		t.Fatalf("%s %s: %v", kind, id, err)
	}
}

// storePathFor is the scratch store path for a kind.
func storePathFor(t *testing.T, home, kind string) string {
	t.Helper()
	name, ok := map[string]string{
		"auth": "auth.json", "remote": "remotes.json", "plan": "plans.json",
		"manifest": "models.json", "alias": "aliases.json",
	}[kind]
	if !ok {
		t.Fatalf("unknown store kind %q", kind)
	}
	return filepath.Join(home, ".oaica", name)
}

// runStoreWriters starts n children of the given kind against one store path
// and waits for all of them.
func runStoreWriters(t *testing.T, kind, store string, ids []string) {
	t.Helper()
	cmds := make([]*exec.Cmd, 0, len(ids))
	outs := make([]*bytes.Buffer, 0, len(ids))
	for _, id := range ids {
		cmd := exec.Command(os.Args[0], "-test.run=TestStoreWriterChildHelper")
		cmd.Env = append(os.Environ(), storeChildEnv+"="+kind+"|"+store+"|"+id)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Start(); err != nil {
			t.Fatalf("start %s child %s: %v", kind, id, err)
		}
		cmds = append(cmds, cmd)
		outs = append(outs, &out)
	}
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("child %d failed: %v\n%s", i, err, outs[i].String())
		}
	}
}

func TestAuthLoginSurvivesConcurrentWriters(t *testing.T) {
	home := t.TempDir()
	ids := []string{"anthropic", "openai", "together", "fireworks", "moonshot", "zhipu", "xai", "mistral", "groq", "cerebras"}

	store := storePathFor(t, home, "auth")
	runStoreWriters(t, "auth", store, ids)

	b, err := os.ReadFile(store)
	if err != nil {
		t.Fatalf("no auth.json was written: %v", err)
	}
	var f authStoreFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("auth.json is not parseable: %v\n%s", err, b)
	}
	for _, id := range ids {
		c, ok := f.Providers[id]
		if !ok || strings.TrimSpace(c.Key) == "" {
			t.Errorf("provider %q is missing from auth.json after its `auth login` reported success (%d of %d stored) — a provisioning script that logs in several providers in parallel leaves the ones that lost the race unconfigured", id, len(f.Providers), len(ids))
		}
	}
}

func TestRemoteAddSurvivesConcurrentWriters(t *testing.T) {
	home := t.TempDir()
	ids := []string{"rr-aa", "rr-bb", "rr-cc", "rr-dd", "rr-ee", "rr-ff", "rr-gg", "rr-hh"}

	store := storePathFor(t, home, "remote")
	runStoreWriters(t, "remote", store, ids)

	b, err := os.ReadFile(store)
	if err != nil {
		t.Fatalf("no remotes.json was written: %v", err)
	}
	var f userRemotesFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("remotes.json is not parseable: %v\n%s", err, b)
	}
	seen := map[string]bool{}
	for _, r := range f.Remotes {
		seen[strings.TrimSpace(r.Name)] = true
	}
	for _, id := range ids {
		if !seen[id] {
			t.Errorf("remote %q is missing after its `remote add` reported success (%d of %d stored)", id, len(f.Remotes), len(ids))
		}
	}
}

func TestPlanSetSurvivesConcurrentWriters(t *testing.T) {
	home := t.TempDir()
	ids := []string{"pp-aa", "pp-bb", "pp-cc", "pp-dd", "pp-ee", "pp-ff"}

	store := storePathFor(t, home, "plan")
	runStoreWriters(t, "plan", store, ids)

	b, err := os.ReadFile(store)
	if err != nil {
		t.Fatalf("no plans.json was written: %v", err)
	}
	var f tierPlanProfiles
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("plans.json is not parseable: %v\n%s", err, b)
	}
	for _, id := range ids {
		if _, ok := f.Profiles[id]; !ok {
			t.Errorf("plan %q is missing after its `plan set` reported success (%d of %d stored)", id, len(f.Profiles), len(ids))
		}
	}
}

func TestModelAddSurvivesConcurrentWriters(t *testing.T) {
	home := t.TempDir()
	ids := []string{"mm-aa", "mm-bb", "mm-cc", "mm-dd", "mm-ee", "mm-ff"}

	store := storePathFor(t, home, "manifest")
	runStoreWriters(t, "manifest", store, ids)

	b, err := os.ReadFile(store)
	if err != nil {
		t.Fatalf("no models.json was written: %v", err)
	}
	var m modelManifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("models.json is not parseable: %v\n%s", err, b)
	}
	for _, id := range ids {
		if _, ok := m.Models[id]; !ok {
			t.Errorf("model %q is missing after its `model add` reported success (%d of %d stored)", id, len(m.Models), len(ids))
		}
	}
}

func TestModelAliasSetSurvivesConcurrentWriters(t *testing.T) {
	home := t.TempDir()
	ids := []string{"al-aa", "al-bb", "al-cc", "al-dd", "al-ee", "al-ff"}

	store := storePathFor(t, home, "alias")
	runStoreWriters(t, "alias", store, ids)

	b, err := os.ReadFile(store)
	if err != nil {
		t.Fatalf("no aliases.json was written: %v", err)
	}
	var a modelAliases
	if err := json.Unmarshal(b, &a); err != nil {
		t.Fatalf("aliases.json is not parseable: %v\n%s", err, b)
	}
	for _, id := range ids {
		if a.Aliases[id] == "" {
			t.Errorf("alias %q is missing after its `model alias set` reported success (%d of %d stored)", id, len(a.Aliases), len(ids))
		}
	}
}

// A guard against the test going vacuous: if a child could not write at all,
// the assertions above would be the only signal. This checks the child path
// itself works — one child, one store.
func TestStoreWriterChildReallyWrites(t *testing.T) {
	home := t.TempDir()
	store := storePathFor(t, home, "remote")
	runStoreWriters(t, "remote", store, []string{fmt.Sprintf("single-%d", os.Getpid())})
	if _, err := os.Stat(store); err != nil {
		t.Fatalf("the child writer path wrote nothing: %v", err)
	}
}
