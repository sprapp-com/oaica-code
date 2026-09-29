package launch

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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
