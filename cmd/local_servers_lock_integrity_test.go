package cmd

// local_servers_lock_integrity_test.go — the local server registry was a
// read-modify-write with no lock (2026-09-26 audit).
//
// ~/.oaica/local_servers.json is load → mutate in memory → write the whole
// file, and its writers are separate `oaica serve` processes: every server
// registers itself on startup and unregisters on teardown. WriteFileAtomic
// makes each write torn-free, which is why this survived the audit rounds that
// added it — an atomic rename stops a READER from seeing half a file, but it
// does not stop a LOST UPDATE. Two servers starting at once both read the same
// snapshot and whichever renames last wins with the other's entry absent, while
// both report a clean start. The launcher's picker then has no "bonsai:local"
// row for a server that is running and listening.
//
// The first test is the mechanism, stated so that it cannot pass by luck: while
// another process holds the registry's lock, a writer must WAIT. The second is
// the outcome the user sees. Both run real child processes — the defect is
// between processes, and an in-process test would pass against a mutex.

import (
	"bytes"
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

// localServerChildEnv marks the re-executed copy of this test binary; its value
// is "<register|drop>|<model>".
const localServerChildEnv = "OAICA_LOCAL_SERVER_TEST_CHILD"

// TestLocalServerChildHelper is not a test of its own: the parent re-executes
// the test binary with localServerChildEnv set and this performs one registry
// write, of the kind the spec names.
func TestLocalServerChildHelper(t *testing.T) {
	spec := os.Getenv(localServerChildEnv)
	if spec == "" {
		t.Skip("helper for the local server registry tests; runs in a child process")
	}
	kind, model, ok := strings.Cut(spec, "|")
	if !ok {
		t.Fatalf("bad child spec %q", spec)
	}
	switch kind {
	case "register":
		if err := oaicaRegisterLocalServer(model, "http://127.0.0.1:0", "sk-"+model); err != nil {
			t.Fatalf("register %q: %v", model, err)
		}
	case "drop":
		oaicaUnregisterLocalServer(model)
	default:
		t.Fatalf("unknown child kind %q", kind)
	}
}

func localServerRegistryPath(t *testing.T, home string) string {
	t.Helper()
	return filepath.Join(home, ".oaica", "local_servers.json")
}

func startLocalServerChild(t *testing.T, kind, model string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestLocalServerChildHelper")
	cmd.Env = append(os.Environ(), localServerChildEnv+"="+kind+"|"+model)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s child for %q: %v", kind, model, err)
	}
	return cmd, &out
}

func readRegistryEntries(t *testing.T, path string) []oaicaLocalServerEntry {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no registry was written: %v", err)
	}
	var entries []oaicaLocalServerEntry
	if err := json.Unmarshal(b, &entries); err != nil {
		t.Fatalf("the registry is not parseable: %v\n%s", err, b)
	}
	return entries
}

// The mechanism. A held lock must stop the writer: if the child finishes while
// another process holds it, this writer does not participate in the lock at
// all, and every concurrent start is a race.
func TestAWriterWaitsForTheRegistryLock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := localServerRegistryPath(t, home)

	held := make(chan struct{})
	released := make(chan struct{})
	go func() {
		_ = fileutil.WithFileLock(path, func() error {
			close(held)
			<-released
			return nil
		})
	}()
	<-held

	cmd, out := startLocalServerChild(t, "register", "locked-model")
	early := make(chan error, 1)
	go func() { early <- cmd.Wait() }()

	select {
	case err := <-early:
		t.Fatalf("the registry writer finished (%v) while another process held its lock — every concurrent `oaica serve` start is a load-mutate-write race, and the loser's entry is silently gone\nchild output:\n%s", err, out.String())
	case <-time.After(600 * time.Millisecond):
	}

	close(released)
	if err := <-early; err != nil {
		t.Fatalf("child failed once the lock was free: %v\n%s", err, out.String())
	}
	entries := readRegistryEntries(t, path)
	if len(entries) != 1 || entries[0].Model != "locked-model" {
		t.Errorf("registry holds %+v, want the one entry the child wrote", entries)
	}
}

// The teardown path is the same load-mutate-save over the same file, so it
// takes the same lock: a server exiting while another is registering must not
// write the snapshot it read before that entry existed.
func TestATeardownWaitsForTheRegistryLock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := localServerRegistryPath(t, home)

	// Two live servers, one of them about to exit.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	seed := `[{"model":"leaving","origin":"http://127.0.0.1:1"},{"model":"staying","origin":"http://127.0.0.1:2"}]`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	held := make(chan struct{})
	released := make(chan struct{})
	go func() {
		_ = fileutil.WithFileLock(path, func() error {
			close(held)
			<-released
			return nil
		})
	}()
	<-held

	cmd, out := startLocalServerChild(t, "drop", "leaving")
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		t.Fatalf("the teardown finished (%v) while another process held the registry lock — it writes the snapshot it read before that process's entry existed\nchild output:\n%s", err, out.String())
	case <-time.After(600 * time.Millisecond):
	}

	close(released)
	if err := <-done; err != nil {
		t.Fatalf("child failed once the lock was free: %v\n%s", err, out.String())
	}
	entries := readRegistryEntries(t, path)
	if len(entries) != 1 || entries[0].Model != "staying" {
		t.Errorf("registry holds %+v, want only the server that is still running", entries)
	}
}

// The outcome. Ten servers starting together is ten entries, not however many
// happened to be written last.
func TestConcurrentServerStartsAllRegister(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	models := make([]string, 0, 10)
	for i := range 10 {
		models = append(models, fmt.Sprintf("local-model-%02d", i))
	}

	type child struct {
		cmd *exec.Cmd
		out *bytes.Buffer
	}
	children := make([]child, 0, len(models))
	for _, m := range models {
		cmd, out := startLocalServerChild(t, "register", m)
		children = append(children, child{cmd, out})
	}
	for i, c := range children {
		if err := c.cmd.Wait(); err != nil {
			t.Fatalf("child %d failed: %v\n%s", i, err, c.out.String())
		}
	}

	seen := map[string]bool{}
	for _, e := range readRegistryEntries(t, localServerRegistryPath(t, home)) {
		seen[strings.TrimSpace(e.Model)] = true
	}
	for _, m := range models {
		if !seen[m] {
			t.Errorf("server %q registered itself at startup and is missing from the registry (%d of %d present) — `oaica launch`'s picker has no row for a server that is up and listening", m, len(seen), len(models))
		}
	}
}
