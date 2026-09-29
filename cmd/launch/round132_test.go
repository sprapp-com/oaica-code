package launch

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// F132-L2-3 (2026-09-29 audit, round 132): a peer that sends headers with a Content-Length and never the body is
// answered 401 within the body deadline, not held for ever.
func TestRound132UnauthenticatedStalledBodyIsAnswered(t *testing.T) {
	old := normalizingProxyBodyTimeout
	normalizingProxyBodyTimeout = 500 * time.Millisecond
	defer func() { normalizingProxyBodyTimeout = old }()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	go func() { _ = RunNormalizingProxyOnKeyed("127.0.0.1", port, 1, "K", "") }()
	waitForListener(t, "127.0.0.1", port)
	c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "POST /v1/messages HTTP/1.1\r\nHost: 127.0.0.1\r\nContent-Length: 1\r\n\r\n")
	c.SetReadDeadline(time.Now().Add(4 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("no answer within the deadline: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status %d, want 401", resp.StatusCode)
	}
}

// F133-L1-1: a server failure is not a verdict on the key — it gets the offline grace, not a "revoked" lockout.
func TestRound133ServerFailuresGetTheOfflineGraceNotALockout(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"500": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
			w.Write([]byte(`{"error":"internal_error"}`))
		},
		"429": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(429)
			w.Write([]byte(`{"error":"too many requests"}`))
		},
		"404": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(404)
			w.Write([]byte(`{"error":"not_found"}`))
		},
		"null":      func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`null`)) },
		"emptyjson": func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) },
	} {
		t.Run(name, func(t *testing.T) {
			setLaunchTestHome(t, t.TempDir())
			stubLicenseServer(t, h)
			if err := saveLicenseFile(licenseFile{Key: "K", InstanceID: "I", ValidatedAt: time.Now().Add(-licenseRevalidateTTL - time.Hour)}); err != nil {
				t.Fatal(err)
			}
			if err := requireLicenseLive(nil, nil); err != nil {
				t.Errorf("a %s from the licence server locked out a licence inside its grace: %v", name, err)
			}
		})
	}
	// and a server error must not open a licence that is past its grace, whatever its body says
	setLaunchTestHome(t, t.TempDir())
	stubLicenseServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		w.Write([]byte(`{"valid":true,"error":"forbidden"}`))
	})
	saveLicenseFile(licenseFile{Key: "K", InstanceID: "I", ValidatedAt: time.Now().Add(-licenseOfflineGrace - time.Hour)})
	if err := requireLicenseLive(nil, nil); err == nil {
		t.Error("a 403 with valid:true opened a licence past its grace")
	}
}

// F133-L1-2: the key is in the request body; a redirect must not carry it to another host.
func TestRound133LicenceClientDoesNotFollowRedirects(t *testing.T) {
	var leaked atomic.Bool
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Store(true)
		w.Write([]byte(`{"valid":true,"activated":true}`))
	}))
	defer foreign.Close()
	stubLicenseServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, foreign.URL+"/x", http.StatusTemporaryRedirect)
	})
	if _, err := callLicenseAPI("/validate", url.Values{"license_key": {"oaica-lic-secret"}}); err == nil {
		t.Error("a redirect was accepted as an answer")
	}
	if leaked.Load() {
		t.Error("the licence key was re-sent to the redirect target")
	}
}

// F134-L1-1: re-running `oaica activate` on an already activated machine does not spend another seat.
func TestRound134ReactivatingTheSameMachineSpendsNoSeat(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	var activations atomic.Int32
	stubLicenseServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/activate":
			activations.Add(1)
			w.Write([]byte(`{"activated":true,"valid":true,"instance":{"id":"inst-1","name":"host"},"meta":{"product":"oaica-code"}}`))
		case "/validate":
			w.Write([]byte(`{"valid":true,"activated":false,"meta":{"product":"oaica-code"}}`))
		}
	})
	key := "oaica-lic-" + strings.Repeat("a", 32)
	for i := 0; i < 4; i++ {
		cmd := ActivateCmd()
		cmd.SetArgs([]string{key})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	if n := activations.Load(); n != 1 {
		t.Errorf("4 runs of `oaica activate` on one machine spent %d seats, want 1", n)
	}
}

// F134-L1-2: a rate-limited activation says so, instead of claiming the server is unreachable.
func TestRound134ActivationRefusalKeepsTheServersWords(t *testing.T) {
	stubLicenseServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		w.Write([]byte(`{"error":"too many requests"}`))
	})
	_, err := activateLicenseLive("oaica-lic-"+strings.Repeat("b", 32), "")
	if err == nil || !strings.Contains(err.Error(), "too many requests") || strings.Contains(err.Error(), "could not reach") {
		t.Errorf("err = %v; want the server's own words, not 'could not reach'", err)
	}
}

// F135-L1-1: a validate that gives no verdict must not fall through to /activate and strand the held seat.
func TestRound135ValidateFlapSpendsNoSeat(t *testing.T) {
	for _, status := range []int{429, 500, 503} {
		setLaunchTestHome(t, t.TempDir())
		var activations atomic.Int32
		stubLicenseServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/activate":
				activations.Add(1)
				w.Write([]byte(`{"activated":true,"valid":true,"instance":{"id":"inst-2","name":"h"},"meta":{"product":"oaica-code"}}`))
			case "/validate":
				w.WriteHeader(status)
				w.Write([]byte(`{"error":"flap"}`))
			}
		})
		key := "oaica-lic-" + strings.Repeat("d", 32)
		saveLicenseFile(licenseFile{Key: key, InstanceID: "inst-1", InstanceName: "h", ValidatedAt: time.Now()})
		cmd := ActivateCmd()
		cmd.SetArgs([]string{key})
		if err := cmd.Execute(); err == nil {
			t.Errorf("validate %d: activate reported success without a verdict", status)
		}
		if n := activations.Load(); n != 0 {
			t.Errorf("validate %d spent %d seat(s)", status, n)
		}
		if f, _ := loadLicenseFile(); f.InstanceID != "inst-1" {
			t.Errorf("validate %d replaced the stored instance with %q", status, f.InstanceID)
		}
	}
	// control: a definite valid:false (revoked instance) DOES re-activate
	setLaunchTestHome(t, t.TempDir())
	var activations atomic.Int32
	stubLicenseServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/activate":
			activations.Add(1)
			w.Write([]byte(`{"activated":true,"valid":true,"instance":{"id":"inst-9","name":"h"},"meta":{"product":"oaica-code"}}`))
		case "/validate":
			w.Write([]byte(`{"valid":false,"activated":false,"error":"this machine is not activated for that key"}`))
		}
	})
	key := "oaica-lic-" + strings.Repeat("e", 32)
	saveLicenseFile(licenseFile{Key: key, InstanceID: "inst-1", InstanceName: "h", ValidatedAt: time.Now()})
	cmd := ActivateCmd()
	cmd.SetArgs([]string{key})
	if err := cmd.Execute(); err != nil || activations.Load() != 1 {
		t.Errorf("a definite refusal must re-activate once: err=%v activations=%d", err, activations.Load())
	}
}
