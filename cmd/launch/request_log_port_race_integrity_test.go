package launch

// request_log_port_race_integrity_test.go — requestLogProxyPort was a plain
// int written at listener-bind time and read by every request goroutine
// (2026-09-26 audit, fourteenth round).
//
// The write runs in the goroutine that binds the listener (RunLocalLoggingProxy,
// RunAnthropicOpenAIProxyRoutes — both call setRequestLogProxyPort); the read
// runs inside appendRequestLog, which any per-connection goroutine reaches. A
// process that runs two proxies (a routing proxy and a logging proxy) has both
// writers live at once, and the second bind silently wins, so rows written by
// the first proxy carry the second proxy's port.
//
// The field is an atomic.Int64 now. The detector is the assertion: run this
// with -race, as `go test -race -run TestRequestLogProxyPortIsRaceFree
// ./cmd/launch/`. Reverting the field to a plain int makes the detector abort
// the test — a behavioural failure, not a compile error.

import (
	"net"
	"sync"
	"testing"
)

func TestRequestLogProxyPortIsRaceFree(t *testing.T) {
	// A real TCP listener, so setRequestLogProxyPort stores a port rather than
	// falling through the non-TCP branch.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	const iterations = 200
	var wg sync.WaitGroup

	// Writer: the bind-time path.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			setRequestLogProxyPort(ln)
		}
	}()

	// Readers: the request path. appendRequestLog reads the field before it
	// touches the filesystem, so the log itself is written to a temp home the
	// test already owns.
	home := t.TempDir()
	setTestHome(t, home)
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				appendRequestLog(requestLogEntry{Model: "race-model", StatusCode: 200})
			}
		}()
	}
	wg.Wait()
}
