package launch

// cline_two_store_pair_integrity_test.go — Cline.Edit's two documents were
// written under two locks taken one after the other, so the pair had no
// atomicity (2026-09-26 audit, round 16).
//
// Edit rewrites Cline's providers.json and globalState.json from one selection:
// providers.json carries the ollama provider's settings (base URL, model id)
// and globalState.json carries the active provider and model ids. Each store's
// own lock orders writers of THAT file (cline_store_race_integrity_test.go),
// but the providers lock was released before the legacy one was taken, so two
// overlapping `oaica launch cline` commands could still publish their two
// documents in an interleaved order — launch A's providers.json beside launch
// B's globalState.json, a pair describing neither launch, with both commands
// reporting success. Pi closes the same shape by holding both of its locks
// across both writes.
//
// What this pins is the observable consequence: while another writer holds the
// legacy store's lock, an Edit must not have published providers.json either.
// The un-fixed Edit publishes providers.json first — its own lock is free —
// and only then blocks on the legacy lock.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ollama/ollama/cmd/internal/fileutil"
)

func TestClineEditPublishesThePairTogetherOrNotAtAll(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)

	providersPath := filepath.Join(home, ".cline", "data", "settings", "providers.json")
	legacyPath := filepath.Join(home, ".cline", "data", "globalState.json")
	if err := os.MkdirAll(filepath.Dir(providersPath), 0o700); err != nil {
		t.Fatal(err)
	}

	// Both documents already describe one selection ("claude-old" via
	// anthropic). An Edit that publishes only half of its own pair is what this
	// test has to catch, so the seed is a consistent pair to start from.
	seedProviders := `{"version": 1, "lastUsedProvider": "anthropic", ` +
		`"providers": {"anthropic": {"settings": {"model": "claude-old"}}}}`
	seedLegacy := `{"actModeApiProvider": "anthropic", "actModeOllamaModelId": "", "keptKey": "kept"}`
	if err := os.WriteFile(providersPath, []byte(seedProviders), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte(seedLegacy), 0o600); err != nil {
		t.Fatal(err)
	}

	release := filepath.Join(t.TempDir(), "release")
	cmd, out := startAgentStoreChild(t, agentStoreClineEdit, home, "cline-pair-model", release)

	lockPath := foreignStoreLockPathFor(t, home, legacyPath)
	err := fileutil.WithFileLock(lockPath, func() error {
		if err := os.WriteFile(release, []byte("go"), 0o600); err != nil {
			return err
		}
		// A publish that is not held back by the pair's second lock happens
		// within milliseconds of the release; one that waits for both locks is
		// still waiting here. A second of polling makes that a behaviour rather
		// than a timing accident.
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			got, rerr := os.ReadFile(providersPath)
			if rerr != nil {
				return rerr
			}
			if !bytes.Equal(got, []byte(seedProviders)) {
				t.Errorf("Edit published providers.json while another writer held globalState.json's lock — the two documents of one launch are written under separate locks, so two overlapping launches interleave and leave a pair that describes neither:\n%s", got)
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
		t.Fatalf("the Cline.Edit writer failed: %v\n%s", err, out.String())
	}

	// And once the pair can be written, both halves are — the fix must not
	// deadlock or drop a document.
	providers, err := os.ReadFile(providersPath)
	if err != nil {
		t.Fatalf("providers.json is gone after the Edit: %v", err)
	}
	legacy, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatalf("globalState.json is gone after the Edit: %v", err)
	}
	for _, want := range []string{`"lastUsedProvider": "ollama"`, `"model": "cline-pair-model"`} {
		if !strings.Contains(string(providers), want) {
			t.Errorf("providers.json does not carry %s after the Edit:\n%s", want, providers)
		}
	}
	for _, want := range []string{`"actModeOllamaModelId": "cline-pair-model"`, `"keptKey": "kept"`} {
		if !strings.Contains(string(legacy), want) {
			t.Errorf("globalState.json does not carry %s after the Edit:\n%s", want, legacy)
		}
	}
}
