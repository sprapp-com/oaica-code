package cmd

// oaica_pull_existing_file_integrity_test.go — `oaica pull` decided a model was
// already installed from its BYTE LENGTH alone, and skipped the download that
// the manifest's sha256 exists to gate (2026-09-26 audit).
//
// The download path checks the digest (and the HuggingFace path checks it too),
// but the "already downloaded" shortcut above it compared only os.Stat().Size
// against manifest.SizeBytes. A file of the right size and the wrong content —
// a truncated-then-padded file, a corrupted one, a same-size blob someone else
// put there — was reported as "already downloaded, skipping" forever: the
// command succeeds, the model is broken, and re-running the pull never fixes
// it because the shortcut is what runs first. The digest the router offers is
// the whole reason a manifest is trusted; a length is not a substitute for it.

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func pullTestSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// A model file already sitting at the destination with the manifest's size but
// not its content must be re-downloaded, not skipped.
func TestPullReplacesAnExistingFileWhoseDigestDoesNotMatch(t *testing.T) {
	invPullSetup(t)

	good := []byte("the real weights, 26 bytes.")
	sum := pullTestSHA256(good)

	router := newPullRouter(t, map[string]any{
		"model": "corrupt", "size_bytes": len(good), "sha256": sum, "pull_url": "/v1/pull/corrupt", "source": "file",
	}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(good)
	})
	t.Setenv("OAICA_HOST", router.URL)

	// Same size, different bytes — what a truncated-and-padded download, a
	// corrupted disk, or any other writer leaves behind.
	dest, err := oaicaModelPath("corrupt")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		t.Fatal(err)
	}
	wrong := make([]byte, len(good))
	copy(wrong, "NOT the real weights.....x")
	if err := os.WriteFile(dest, wrong, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := oaicaPullModel("corrupt"); err != nil {
		t.Fatalf("the pull failed outright instead of replacing the bad file: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(good) {
		t.Errorf("the pull reported success while leaving the previous file in place: the destination holds %q, not the manifest's bytes — a same-size file with the wrong content is installed as the model, and every later pull skips it again", got)
	}
}

// The control: a correct file is still skipped, so the pull does not re-fetch
// every model it already has.
func TestPullStillSkipsAFileThatMatchesItsDigest(t *testing.T) {
	invPullSetup(t)

	good := []byte("the real weights, 26 bytes.")
	router := newPullRouter(t, map[string]any{
		"model": "whole", "size_bytes": len(good), "sha256": pullTestSHA256(good), "pull_url": "/v1/pull/whole", "source": "file",
	}, func(w http.ResponseWriter, r *http.Request) {
		t.Error("the byte stream was fetched even though the installed file matches the manifest's digest")
	})
	t.Setenv("OAICA_HOST", router.URL)

	dest, err := oaicaModelPath("whole")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, good, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := oaicaPullModel("whole"); err != nil {
		t.Fatal(err)
	}
}
