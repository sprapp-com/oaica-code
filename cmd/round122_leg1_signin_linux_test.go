package cmd

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func round122OpenPTY(t *testing.T) (*os.File, *os.File) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skip(err)
	}
	if err := unix.IoctlSetPointerInt(int(m.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(int(m.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	s, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	return m, s
}

func TestRound122SigninEchoesKey(t *testing.T) {
	const key = "sk-live-ECHOPROBE-9f8e7d"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"m"}]}`))
	}))
	defer srv.Close()
	t.Setenv("OAICA_HOST", srv.URL)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OAICA_API_KEY", "")
	m, s := round122OpenPTY(t)
	defer m.Close()
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = s, s
	defer func() { os.Stdin, os.Stdout = oldIn, oldOut }()

	done := make(chan error, 1)
	go func() { _, err := oaicaInteractiveSignin(); done <- err }()
	var mu sync.Mutex
	var out strings.Builder
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := m.Read(buf)
			mu.Lock()
			out.Write(buf[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	snap := func() string { mu.Lock(); defer mu.Unlock(); return out.String() }
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(snap(), "API key") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	tio, _ := unix.IoctlGetTermios(int(s.Fd()), unix.TCGETS)
	t.Logf("ECHO flag on the terminal while the key is read: %v", tio.Lflag&unix.ECHO != 0)
	m.Write([]byte(key + "\n"))
	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("signin did not return; terminal: " + snap())
	}
	time.Sleep(300 * time.Millisecond)
	t.Logf("signin err=%v; terminal showed: %q", err, snap())
	if strings.Contains(snap(), key) {
		t.Errorf("RED: the OAICA key was echoed to the terminal (auth login/remote prompts use term.ReadPassword)")
	}
}
