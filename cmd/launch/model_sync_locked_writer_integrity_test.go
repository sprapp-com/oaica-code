package launch

// model_sync_locked_writer_integrity_test.go — `oaica model sync` bypassed the
// locked writer for models.json and lost a concurrent session's update while
// reporting its own as applied (2026-09-26 audit, sixth round).
//
// model_manifest.go declares updateModelManifest "the ONLY writer path for
// models.json", and model_detect.go's scan was moved onto it. ModelSync was
// not: it does loadModelManifest() → mutate → m.save() with no lock, so it
// publishes a snapshot it read before another session's writer committed. The
// other session's entry is then gone, and the sync still prints
// `added: <its own ids>`. It also called m.save() unconditionally, rewriting
// the file even when nothing changed — the thing the scan callback's "changed"
// return was added to stop.
//
// This test drives the race deterministically instead of hoping for it: a real
// updateModelManifest writer holds the lock with a barrier inside its mutate
// closure, so its snapshot is read from disk BEFORE ModelSync runs and written
// AFTER it.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestModelSyncSerializesWithTheOtherManifestWriters
//
// The holder acquires the lock, loads the manifest, and waits. ModelSync must
// not be able to complete in that window; when the holder finishes with its own
// addition, the sync's addition has to survive on top of it.
func TestModelSyncSerializesWithTheOtherManifestWriters(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OAICA_MODELS_FILE", filepath.Join(dir, "models.json"))
	if _, err := ModelAdd(ModelAddOptions{ID: "user-added", Engine: "vllm", ModelPath: "/m/user-added"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	src := catalogFileFor(t, "cat.json", `{"version":1,"models":{"m2":{"engine":"vllm","model_path":"/m/m2"}}}`)

	read := make(chan struct{})
	release := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- updateModelManifest(func(m *modelManifest) (bool, error) {
			close(read)
			<-release
			m.Put(ModelManifestEntry{ID: "holder-added", Engine: "vllm", ModelPath: "/m/holder-added"})
			return true, nil
		})
	}()
	<-read

	syncDone := make(chan error, 1)
	go func() {
		_, err := ModelSync(src, false)
		syncDone <- err
	}()

	completedEarly := false
	select {
	case err := <-syncDone:
		if err != nil {
			t.Fatalf("ModelSync: %v", err)
		}
		completedEarly = true
	case <-time.After(500 * time.Millisecond):
		// Blocked on the lock, which is the point.
	}
	close(release)
	if err := <-holderDone; err != nil {
		t.Fatalf("the concurrent writer failed: %v", err)
	}
	if !completedEarly {
		if err := <-syncDone; err != nil {
			t.Fatalf("ModelSync: %v", err)
		}
	}

	m, err := loadModelManifest()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if completedEarly {
		t.Logf("ModelSync completed while the manifest lock was held — it is not serialized with the other writers")
	}
	if _, ok := m.Get("holder-added"); !ok {
		t.Errorf("the concurrent writer's entry is missing after `model sync` reported success (ids: %v) — ModelSync publishes a manifest it read before that writer committed, so a sync running beside `model add`, `model scan` or another sync silently discards the other one's result", m.SortedIDs())
	}
	if _, ok := m.Get("m2"); !ok {
		t.Errorf("the synced model m2 is missing (ids: %v) — the run reported it as added and it is not in the manifest", m.SortedIDs())
	}
	if _, ok := m.Get("user-added"); !ok {
		t.Errorf("the pre-existing user entry was lost (ids: %v)", m.SortedIDs())
	}
}

// A run that changes nothing must not rewrite the file — the same "changed"
// discipline the scan's callback keeps. A rewrite is not harmless: it publishes
// a snapshot, which is how the other writer's update is lost even when the
// sync itself had nothing to say.
func TestModelSyncDoesNotRewriteAnUnchangedManifest(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OAICA_MODELS_FILE", filepath.Join(dir, "models.json"))
	path, err := modelManifestPath()
	if err != nil {
		t.Fatal(err)
	}
	// A manifest whose only synced entry already matches the catalog, so the
	// sync has nothing to add, update or prune.
	src := catalogFileFor(t, "cat.json", `{"version":1,"models":{"m1":{"engine":"vllm","model_path":"/m/m1"}}}`)
	if _, err := ModelSync(src, false); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)

	// Identical catalog again: no additions, no prunes.
	if _, err := ModelSync(src, false); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	body2, _ := os.ReadFile(path)
	if string(body) != string(body2) {
		t.Errorf("the manifest content changed on a no-op sync:\n%s\n---\n%s", body, body2)
	}
	if after.ModTime().After(before.ModTime()) {
		t.Errorf("the manifest was rewritten (%s -> %s) by a sync with nothing to add, update or prune — an unconditional save publishes a stale snapshot over whatever a concurrent writer committed, which is the lost update this file exists to prevent", before.ModTime(), after.ModTime())
	}
}
