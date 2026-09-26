package config

// config_lock_integrity_test.go — the legacy ~/.ollama/config.json store was a
// whole-file load→mutate→write with no file lock (2026-09-26 audit, round 16).
//
// Every mutating entry point in this package goes through save(), and every
// sibling store in the tree takes fileutil.WithFileLock around its own
// load-mutate-save (~/.oaica/config.json in launch/user_config.go, the remotes
// file in launch/remote_cli.go, model_picks.json, the alias store). This one
// did not, so two writers that overlapped both loaded the same snapshot and the
// later rename won — a command that printed success left its integration, its
// last model, or its last selection absent, and the next launch silently
// forgot the configuration that had just been written.
//
// The stores are only ever written under a lock here, so these two tests are
// the concurrent repro of both shapes: many integrations at once, and two
// different fields at once.

import (
	"fmt"
	"sync"
	"testing"
)

// Sixteen concurrent writers of DISTINCT integrations must all survive.
func TestConcurrentIntegrationWritesAllSurvive(t *testing.T) {
	setTestHome(t, t.TempDir())

	const writers = 16
	var wg sync.WaitGroup
	errs := make([]error, writers)
	start := make(chan struct{})
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = SaveIntegration(fmt.Sprintf("app-%02d", i), []string{fmt.Sprintf("model-%02d", i)})
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}

	survived := 0
	for i := 0; i < writers; i++ {
		name := fmt.Sprintf("app-%02d", i)
		cfg, err := LoadIntegration(name)
		if err != nil {
			t.Errorf("%s was reported as saved by SaveIntegration but LoadIntegration returns %v — %d of %d writers survived, so concurrent writers overwrote each other's snapshot",
				name, err, survived, writers)
			continue
		}
		survived++
		if len(cfg.Models) != 1 || cfg.Models[0] != fmt.Sprintf("model-%02d", i) {
			t.Errorf("%s holds %v, want its own model", name, cfg.Models)
		}
	}
}

// Two writers of two DIFFERENT fields of the same document must not erase one
// another.
func TestConcurrentLastModelAndSelectionBothSurvive(t *testing.T) {
	setTestHome(t, t.TempDir())

	var wg sync.WaitGroup
	wg.Add(2)
	start := make(chan struct{})
	go func() { defer wg.Done(); <-start; _ = SetLastModel("kat-awq") }()
	go func() { defer wg.Done(); <-start; _ = SetLastSelection("sel-0") }()
	close(start)
	wg.Wait()

	if got := LastModel(); got != "kat-awq" {
		t.Errorf("last_model = %q after a successful SetLastModel(\"kat-awq\") — a concurrent write to the same file dropped it, so the picker reopens on the wrong model", got)
	}
	if got := LastSelection(); got != "sel-0" {
		t.Errorf("last_selection = %q after a successful SetLastSelection(\"sel-0\")", got)
	}
}
