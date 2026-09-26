package fileutil

// lock_integrity_test.go — the lost update, reproduced across real processes
// (2026-09-26 audit, fourth round).
//
// An atomic rename makes each WRITE torn-free, so this defect survived every
// previous audit: eleven concurrent `oaica auth login <provider>` each read
// the same auth.json, each added their own credential, and eleven "Logged in"
// lines left ONE key on disk. The same shape loses remotes, plans, aliases
// and manifest entries.
//
// The test spawns real child processes rather than goroutines: an in-process
// test would pass against a mutex, and the bug is specifically between
// processes.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// childEnv marks the re-executed copy of this test binary.
const childEnv = "FILEUTIL_LOCK_TEST_CHILD"

// TestFileLockChildHelper is not a test of its own: the parent re-executes
// the test binary with childEnv set, and this function performs the
// load-mutate-write the store helpers do.
func TestFileLockChildHelper(t *testing.T) {
	spec := os.Getenv(childEnv)
	if spec == "" {
		t.Skip("helper for TestFileLockSerializesConcurrentWriters; runs in a child process")
	}
	parts := strings.SplitN(spec, "|", 3)
	if len(parts) != 3 {
		t.Fatalf("bad child spec %q", spec)
	}
	path, id, holdFor := parts[0], parts[1], parts[2]
	hold, _ := time.ParseDuration(holdFor)

	err := WithFileLock(path, func() error {
		// Read.
		body, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		// Mutate. The sleep is what makes the race real: without holding the
		// lock across it, another process reads the pre-mutation snapshot.
		time.Sleep(hold)
		body = append(body, []byte(id+"\n")...)
		// Write.
		return WriteFileAtomic(path, body, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFileLockSerializesConcurrentWriters(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "auth.json")

	const n = 8
	children := make([]*exec.Cmd, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("provider-%d", i)
		cmd := exec.Command(os.Args[0], "-test.run=TestFileLockChildHelper", "-test.v")
		cmd.Env = append(os.Environ(), childEnv+"="+store+"|"+id+"|40ms")
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Start(); err != nil {
			t.Fatalf("start child %d: %v", i, err)
		}
		children = append(children, cmd)
	}
	for i, cmd := range children {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("child %d failed: %v", i, err)
		}
	}

	body, err := os.ReadFile(store)
	if err != nil {
		t.Fatalf("no store was written: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	seen := map[string]bool{}
	for _, l := range lines {
		if l != "" {
			seen[l] = true
		}
	}
	if len(seen) != n {
		t.Errorf("%d of %d concurrent writers survived (%v of %s) — every process reported success, so a provisioning script configures eight providers and leaves the ones that lost the race unconfigured", len(seen), n, sortedKeys(seen), store)
	}
	for i := 0; i < n; i++ {
		if id := "provider-" + strconv.Itoa(i); !seen[id] {
			t.Errorf("%s is missing from the store after its writer reported success", id)
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
