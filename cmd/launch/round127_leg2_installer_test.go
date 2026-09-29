package launch

// F127-L2-2 (2026-09-29 audit, round 127): the installer download does not follow a redirect from https to http.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRound127InstallerRefusesAnHTTPSDowngradeRedirect(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("echo pwned\n"))
	}))
	defer plain.Close()
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/install.sh", http.StatusFound)
	}))
	defer tlsSrv.Close()
	old := http.DefaultTransport
	http.DefaultTransport = tlsSrv.Client().Transport
	defer func() { http.DefaultTransport = old }()
	if path, err := fetchInstallerScript(tlsSrv.URL + "/install.sh"); err == nil {
		t.Errorf("an https installer URL was fulfilled from plain http: %s", path)
	}
}
