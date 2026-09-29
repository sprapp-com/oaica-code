package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRound115ShutdownDropsMeterReports(t *testing.T) {
	var ingested atomic.Int32
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		time.Sleep(300 * time.Millisecond) // an ordinary cross-region round trip under load
		ingested.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer hub.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	}))
	defer up.Close()
	ledger := filepath.Join(t.TempDir(), "ledger.jsonl")
	g := &gateway{}
	if err := g.apply(gwConfig{UpstreamAddr: up.URL, ListenAddr: ":0", LedgerPath: ledger, MeterHubAddr: hub.URL, Region: "r",
		APIKeys: []gwKey{{SHA256: keyHash("sk"), Label: "k"}},
		Models:  []gwModel{{ID: "kat-awq"}}}); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	var lmu sync.Mutex
	log.SetOutput(writerFunc(func(p []byte) (int, error) { lmu.Lock(); defer lmu.Unlock(); return logs.Write(p) }))
	defer log.SetOutput(os.Stderr)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := &http.Server{Handler: mux(g)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveGateway(ctx, g, srv, ln, 5*time.Second) }()
	base := "http://" + ln.Addr().String()
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("POST", base+"/v1/chat/completions", strings.NewReader(`{"model":"kat-awq","messages":[{"role":"user","content":"go"}]}`))
			req.Header.Set("Authorization", "Bearer sk")
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	cancel() // SIGTERM while 5 requests are in flight
	<-done   // main() returns here and the process exits
	wg.Wait()
	b, _ := os.ReadFile(ledger)
	rows := strings.Count(string(b), "\n")
	atExit := ingested.Load()
	lmu.Lock()
	said := logs.String()
	lmu.Unlock()
	fmt.Printf("ledger rows=%d, meterhub ingested at process exit=%d\n", rows, atExit)
	fmt.Printf("log lines mentioning meterhub/report at exit: %q\n", grepLines(said, "report"))
	time.Sleep(3 * time.Second)
	fmt.Printf("(had the process lived 3s more: ingested=%d)\n", ingested.Load())
	if int(atExit) < rows && !strings.Contains(said, "report") {
		t.Errorf("%d billed rows never reach meterhub and nothing says so", rows-int(atExit))
	}
}

func grepLines(s, sub string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return out
}
