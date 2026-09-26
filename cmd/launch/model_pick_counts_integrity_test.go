package launch

// model_pick_counts_integrity_test.go — ~/.oaica/model_picks.json is the one
// store under oaica's own directory that was still read-modify-written with no
// lock, and the one whose read path could turn a file it cannot parse into a
// file holding a single entry (2026-09-26 audit, eleventh round).
//
// Two failures in the same twenty lines:
//
//   - recordModelPick loaded the counts, incremented one name and wrote the
//     whole map. The picker records a pick on every `oaica launch`, so two
//     launches at once both read the same snapshot and the rename that landed
//     last kept one model's pick — the "frequently used" section the picker
//     pins at the top was then ordered from a history that quietly loses
//     entries.
//   - json.Unmarshal into map[string]int fails on any shape this version does
//     not recognise (a newer file, a hand-edit, a truncated write). The load
//     returned nil, the record started from an empty map, and the write
//     published that — deleting every other model's history, with no notice and
//     no copy of the bytes. Nothing bounded the keys either: the map grew for
//     the life of the installation.
//
// The lock is the one the rest of the stores take (fileutil.WithFileLock,
// auth_store.go:152); the parse failure is handled the way the auth store
// handles it — the bytes are moved aside, the user is told, and the counts
// start clean.

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
)

// modelPickChildEnv marks the re-executed copy of this test binary. The value
// is "<home>|<model name>|<rendezvous file>".
const modelPickChildEnv = "LAUNCH_MODEL_PICK_TEST_CHILD"

// TestModelPickChildHelper is not a test of its own: the parent re-executes the
// test binary with modelPickChildEnv set and this records one pick.
//
// HOME is installed HERE, after TestMain, because TestMain owns HOME (it
// repoints it at a shared scratch directory before any test runs) — the child's
// own environment is the only point in a test binary that outranks that.
func TestModelPickChildHelper(t *testing.T) {
	spec := os.Getenv(modelPickChildEnv)
	if spec == "" {
		t.Skip("helper for the model-pick concurrency test; runs in a child process")
	}
	parts := strings.SplitN(spec, "|", 3)
	if len(parts) != 3 {
		t.Fatalf("bad child spec %q", spec)
	}
	home, name, release := parts[0], parts[1], parts[2]
	os.Setenv("HOME", home)

	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(release); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the parent never released the writers")
		}
		time.Sleep(time.Millisecond)
	}
	recordModelPick(name)
}

// TestModelPickCountsSurviveConcurrentLaunches records one model pick per child
// process, all released together: every launch's pick must still be counted
// afterwards.
func TestModelPickCountsSurviveConcurrentLaunches(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	ids := []string{"mp-aa", "mp-bb", "mp-cc", "mp-dd", "mp-ee"}

	release := filepath.Join(t.TempDir(), "release")
	cmds := make([]*exec.Cmd, 0, len(ids))
	outs := make([]*bytes.Buffer, 0, len(ids))
	for _, id := range ids {
		cmd := exec.Command(os.Args[0], "-test.run=TestModelPickChildHelper")
		cmd.Env = append(os.Environ(), modelPickChildEnv+"="+home+"|"+id+"|"+release)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Start(); err != nil {
			t.Fatalf("start child %s: %v", id, err)
		}
		cmds = append(cmds, cmd)
		outs = append(outs, &out)
	}
	if err := os.WriteFile(release, []byte("go"), 0o600); err != nil {
		t.Fatalf("release the writers: %v", err)
	}
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("child %d failed: %v\n%s", i, err, outs[i].String())
		}
	}

	counts := loadModelPickCounts()
	for _, id := range ids {
		if counts[id] == 0 {
			t.Errorf("model %q is not counted after its launch recorded the pick (%d of %d names in the file) — two launches that overlap publish snapshots of the same file and the one that renames last decides which picks survive", id, len(counts), len(ids))
		}
	}
}

// TestUnusableModelPickCountsAreKeptNotReplaced covers the worse half: a file
// this version cannot read must not come back as a one-entry map. The bytes are
// the user's, so they are moved aside under a name that says what they are, the
// user is told, and the new file holds the pick that was just made.
func TestUnusableModelPickCountsAreKeptNotReplaced(t *testing.T) {
	cases := map[string]string{
		"a newer or foreign shape":      `{"version":2,"counts":{"deepseek":9,"openrouter":4}}`,
		"a truncated write":             `{"deepseek":`,
		"a value this type cannot hold": `{"deepseek":"lots"}`,
	}
	for label, seed := range cases {
		t.Run(label, func(t *testing.T) {
			home := t.TempDir()
			setLaunchTestHome(t, home)
			path, err := modelPickHistoryPath()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
				t.Fatal(err)
			}

			warn := captureStderr(t, func() { recordModelPick("fresh-pick") })

			// The picks that were in the file are still on disk, byte for byte,
			// under the name that says what they are.
			kept, err := filepath.Glob(path + ".unreadable-*")
			if err != nil {
				t.Fatal(err)
			}
			if len(kept) != 1 {
				t.Fatalf("found %d files kept aside from %s, want exactly 1 — the counts that were in it have been replaced with a map holding only the pick just made, and the file is gone", len(kept), path)
			}
			if b, err := os.ReadFile(kept[0]); err != nil || string(b) != seed {
				t.Errorf("%s does not hold the original bytes (%v): %q", kept[0], err, b)
			}
			if !strings.Contains(warn, kept[0]) {
				t.Errorf("the quarantine was silent: warning output was %q, want it to name %s — a file moved out from under a user without saying so is the same class of bug as replacing it", warn, kept[0])
			}

			// And the launch that just happened is recorded.
			var counts map[string]int
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("no counts file was written after the quarantine: %v", err)
			}
			if err := json.Unmarshal(b, &counts); err != nil {
				t.Fatalf("the new counts file is not parseable: %v\n%s", err, b)
			}
			if counts["fresh-pick"] != 1 {
				t.Errorf("fresh-pick = %d in the new file, want 1", counts["fresh-pick"])
			}
		})
	}
}

// TestModelPickCountsAreBounded pins that there IS a ceiling, not which one:
// the picker reads the top 5 by count, so any bound in the thousands is
// generous, while this file had nothing pruning it and grew for the life of the
// installation. The seed is far above every plausible ceiling, so the assertion
// below is behaviour — a pruned file cannot grow on every record — rather than
// a restatement of the constant.
func TestModelPickCountsAreBounded(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	path, err := modelPickHistoryPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	seed := map[string]int{"keep-me": 9876}
	for i := 0; i < 20000; i++ {
		seed[fmt.Sprintf("noise-%05d", i)] = 1
	}
	b, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}

	recordModelPick("fresh-pick")

	counts := loadModelPickCounts()
	if len(counts) > len(seed)/2 {
		t.Errorf("the counts file holds %d names after a pick, from %d seeded — nothing bounds it, so every model name ever launched stays for the life of the installation", len(counts), len(seed))
	}
	if counts["keep-me"] != 9876 {
		t.Errorf("keep-me = %d, want 9876 — pruning must keep the most-picked models", counts["keep-me"])
	}
	if counts["fresh-pick"] != 1 {
		t.Errorf("fresh-pick = %d, want 1 — the pick being recorded must never be the one pruned away, or a model can never accumulate a count", counts["fresh-pick"])
	}
}
