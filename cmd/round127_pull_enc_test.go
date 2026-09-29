package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
)

// An encrypted HF model pulled twice: the second pull must see the installed model, as the plaintext arm does.
func TestRound127EncryptedPullIsSkippedWhenAlreadyInstalled(t *testing.T) {
	invPullSetup(t)
	restore := hfURLIsTrustedForTest()
	defer restore()
	key := make([]byte, 32)
	full := encryptChunkedForTest(t, key, [][]byte{[]byte("chunk-one-"), []byte("chunk-two-")})
	sum := sha256.Sum256(full) // the gateway's sha256: "SHA256 of the downloaded blob"
	var hits atomic.Int32
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write(full)
	}))
	defer blob.Close()
	router := newPullRouter(t, map[string]any{
		"model": "enc", "size_bytes": len(full), "source": "hf", "sha256": hex.EncodeToString(sum[:]),
		"hf_url": blob.URL + "/a.gguf", "decrypt_key": hex.EncodeToString(key),
	}, nil)
	t.Setenv("OAICA_HOST", router.URL)
	for i := 1; i <= 3; i++ {
		dest, err := oaicaPullModel("enc")
		b, _ := os.ReadFile(dest)
		t.Logf("pull %d: err=%v installed=%q blob downloads so far=%d", i, err, b, hits.Load())
	}
	if hits.Load() != 1 {
		t.Errorf("the encrypted model was downloaded %d times; the plaintext arm downloads once and skips after", hits.Load())
	}

	// Control: the plaintext HF arm.
	plain := []byte("plain-weights")
	ps := sha256.Sum256(plain)
	var phits atomic.Int32
	pblob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { phits.Add(1); w.Write(plain) }))
	defer pblob.Close()
	router2 := newPullRouter(t, map[string]any{
		"model": "pln", "size_bytes": len(plain), "source": "hf", "sha256": hex.EncodeToString(ps[:]), "hf_url": pblob.URL + "/p.gguf",
	}, nil)
	t.Setenv("OAICA_HOST", router2.URL)
	for i := 0; i < 3; i++ {
		oaicaPullModel("pln")
	}
	t.Logf("plaintext arm: %d downloads for 3 pulls", phits.Load())
	_ = fmt.Sprint
}

// The encrypted arm never checks the manifest's digest: swapped chunks (each authenticates alone) install.
func TestRound127EncryptedPullChecksTheManifestDigest(t *testing.T) {
	invPullSetup(t)
	restore := hfURLIsTrustedForTest()
	defer restore()
	key := make([]byte, 32)
	a := encryptChunkedForTest(t, key, [][]byte{[]byte("chunk-one-")})
	b := encryptChunkedForTest(t, key, [][]byte{[]byte("chunk-two-")})
	good := append(append([]byte{}, a...), b...)
	swapped := append(append([]byte{}, b...), a...)
	sum := sha256.Sum256(good)
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(swapped) }))
	defer blob.Close()
	router := newPullRouter(t, map[string]any{
		"model": "sw", "size_bytes": len(good), "source": "hf", "sha256": hex.EncodeToString(sum[:]),
		"hf_url": blob.URL + "/a.gguf", "decrypt_key": hex.EncodeToString(key),
	}, nil)
	t.Setenv("OAICA_HOST", router.URL)
	dest, err := oaicaPullModel("sw")
	got, _ := os.ReadFile(dest)
	t.Logf("err=%v installed=%q", err, got)
	if err == nil {
		t.Errorf("bytes whose digest is not the manifest's were installed on the encrypted arm")
	}
}
