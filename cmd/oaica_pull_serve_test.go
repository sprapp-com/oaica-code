package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ollama/ollama/cmd/launch"
)

// withTempOaicaHome points ~/.oaica-derived paths at a throwaway HOME so
// tests never touch a developer's real manifest — mirrors
// cmd/launch/model_manifest_test.go's helper of the same shape.
func withTempOaicaHome(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, ".oaica"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestAutoPopulateModelManifest_CreatesEntry(t *testing.T) {
	withTempOaicaHome(t)

	autoPopulateModelManifest("my-model", "/home/x/.oaica/models/my-model.gguf")

	e, err := launch.ModelShow("my-model")
	if err != nil {
		t.Fatalf("ModelShow after autoPopulateModelManifest: %v", err)
	}
	if e.Engine != launch.EngineLlamaCPP {
		t.Errorf("Engine = %q, want %q", e.Engine, launch.EngineLlamaCPP)
	}
	if e.ModelPath != "/home/x/.oaica/models/my-model.gguf" {
		t.Errorf("ModelPath = %q", e.ModelPath)
	}
	if e.Notes == "" {
		t.Error("expected a non-empty notes explaining the entry is auto-populated/partial")
	}
}

func TestAutoPopulateModelManifest_NeverOverwritesExisting(t *testing.T) {
	withTempOaicaHome(t)

	// User already hand-configured this model with real detail.
	if _, err := launch.ModelAdd(launch.ModelAddOptions{
		ID: "my-model", Engine: "vllm", ContextWindow: 262144, Quant: "awq-w4a16",
	}); err != nil {
		t.Fatal(err)
	}

	// A pull of the same id must not blow that away with the blanker
	// llama.cpp/auto-populated record.
	autoPopulateModelManifest("my-model", "/some/path.gguf")

	e, err := launch.ModelShow("my-model")
	if err != nil {
		t.Fatal(err)
	}
	if e.Engine != "vllm" || e.ContextWindow != 262144 || e.Quant != "awq-w4a16" {
		t.Fatalf("existing hand-configured entry was overwritten: %+v", e)
	}
}

func TestAutoPopulateModelManifest_NeverPanicsOnManifestError(t *testing.T) {
	// HOME unset/unresolvable: os.UserHomeDir() and modelManifestPath()
	// fail. autoPopulateModelManifest must degrade to a warning, not panic
	// or return an error that could fail the pull itself.
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "") // Windows equivalent, belt and suspenders
	autoPopulateModelManifest("my-model", "/some/path.gguf") // must not panic
}

// The gateway catalog serves PUBLIC, plaintext GGUFs with decrypt_key: null
// (a key for a plaintext blob would be meaningless). Pulling one must stream
// the HF blob straight to disk and verify size + sha256, rather than failing
// on the old "source=hf but no decrypt_key" guard.
func TestOaicaPullFromHF_PlaintextNoDecryptKey(t *testing.T) {
	withTempOaicaHome(t)
	payload := []byte("GGUF plaintext weights")
	hf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	t.Cleanup(hf.Close)

	sum := sha256.Sum256(payload)
	hexSum := hex.EncodeToString(sum[:])
	url := hf.URL + "/model.gguf"
	dest := filepath.Join(t.TempDir(), "plain.gguf")

	got, err := oaicaPullFromHF("plain", &oaicaManifest{
		Source: "hf", SizeBytes: int64(len(payload)), HFURL: &url, SHA256: &hexSum,
	}, dest)
	if err != nil {
		t.Fatalf("plaintext pull: %v", err)
	}
	if got != dest {
		t.Fatalf("path = %q, want %q", got, dest)
	}
	b, err := os.ReadFile(dest)
	if err != nil || string(b) != string(payload) {
		t.Fatalf("file contents = %q (err %v), want %q", string(b), err, string(payload))
	}
	if _, err := os.Stat(dest + ".partial"); !os.IsNotExist(err) {
		t.Fatal(".partial file should be gone after a successful pull")
	}
}

func TestOaicaPullFromHF_PlaintextSHAMismatchCleansUp(t *testing.T) {
	withTempOaicaHome(t)
	payload := []byte("GGUF plaintext weights")
	hf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	t.Cleanup(hf.Close)

	bad := "00000000000000000000000000000000000000000000000000000000deadbeef"
	url := hf.URL + "/model.gguf"
	dest := filepath.Join(t.TempDir(), "plain.gguf")

	if _, err := oaicaPullFromHF("plain", &oaicaManifest{
		Source: "hf", SizeBytes: int64(len(payload)), HFURL: &url, SHA256: &bad,
	}, dest); err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("err = %v, want a sha256 mismatch", err)
	}
	if _, err := os.Stat(dest + ".partial"); !os.IsNotExist(err) {
		t.Fatal("partial file must be removed on a failed pull")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("destination must not exist after a failed pull")
	}
}

func TestOaicaPullFromHF_PlaintextSizeMismatchCleansUp(t *testing.T) {
	withTempOaicaHome(t)
	hf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("short"))
	}))
	t.Cleanup(hf.Close)

	url := hf.URL + "/model.gguf"
	dest := filepath.Join(t.TempDir(), "plain.gguf")

	if _, err := oaicaPullFromHF("plain", &oaicaManifest{
		Source: "hf", SizeBytes: 9999, HFURL: &url,
	}, dest); err == nil || !strings.Contains(err.Error(), "pull incomplete") {
		t.Fatalf("err = %v, want a pull-incomplete error", err)
	}
	if _, err := os.Stat(dest + ".partial"); !os.IsNotExist(err) {
		t.Fatal("partial file must be removed on a failed pull")
	}
}
