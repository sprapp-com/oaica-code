package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// F130-L2-1 (2026-09-29 audit, round 130): two concurrent pulls of one model each get their own temp file. With
// one shared `<dest>.partial`, the second pull truncated the first's bytes, stalled and died, and the first
// installed a file that was zero in front while its wire hash still matched.
func TestRound130ConcurrentPullsDoNotShareATempFile(t *testing.T) {
	invPullSetup(t)
	body := bytes.Repeat([]byte("0123456789abcdef"), 1<<14) // 256 KiB
	half := len(body) / 2
	var mu sync.Mutex
	n := 0
	bOpened, releaseA := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/manifest/") {
			_ = json.NewEncoder(w).Encode(map[string]any{"model": "race", "size_bytes": len(body), "pull_url": "/v1/pull/race", "source": "file"})
			return
		}
		mu.Lock()
		n++
		me := n
		mu.Unlock()
		w.Header().Set("Content-Length", "262144")
		if me == 1 { // A: first half, then wait until B has opened its file
			w.Write(body[:half])
			w.(http.Flusher).Flush()
			<-bOpened
			w.Write(body[half:])
			close(releaseA)
			return
		}
		// B: headers only, then dies once A is done.
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		time.Sleep(300 * time.Millisecond)
		close(bOpened)
		<-releaseA
	}))
	defer srv.Close()
	t.Setenv("OAICA_HOST", srv.URL)

	var dest string
	var aerr error
	done := make(chan struct{})
	go func() { dest, aerr = oaicaPullModel("race"); close(done) }()
	time.Sleep(300 * time.Millisecond) // A has its half on disk
	go func() { _, _ = oaicaPullModel("race") }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("pull A never finished")
	}
	if aerr != nil {
		t.Fatalf("pull A: %v", aerr)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("installed file differs from what was served (zero bytes: %d)", bytes.Count(got, []byte{0}))
	}
}
