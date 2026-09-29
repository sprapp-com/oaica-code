package cmd

// F128-L2-2 (2026-09-29 audit, round 128): install.sh does not take an https download from a plain-http
// redirect. The download helpers are cut out of the script by their markers, as scripts/tests do.

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRound128InstallShRefusesAnHTTPSDowngradeRedirect(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl")
	}
	src, err := os.ReadFile("../scripts/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	i, j := strings.Index(s, "# --- download helpers (begin) ---"), strings.Index(s, "# --- download helpers (end) ---")
	if i < 0 || j < i {
		t.Fatal("download helper markers not found in install.sh")
	}
	block := s[i:j]

	served := 0
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served++
		w.Write([]byte("EVIL-BYTES"))
	}))
	defer plain.Close()
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+r.URL.Path, http.StatusFound)
	}))
	defer tlsSrv.Close()
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsSrv.Certificate().Raw}), 0o600)

	script := `status(){ :; }; error(){ echo "ERROR: $*"; exit 1; }; warning(){ :; }; available(){ command -v "$1" >/dev/null; }
TEMP_DIR="` + dir + `"
` + block + `
fetch_archive "` + tlsSrv.URL + `" a.tgz "` + filepath.Join(dir, "a.tgz") + `"
`
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), "CURL_CA_BUNDLE="+ca, "SSL_CERT_FILE="+ca)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Errorf("install.sh fetched an https URL through a plain-http redirect and succeeded:\n%s", out)
	}
	if served != 0 {
		t.Errorf("the plain-http hop was contacted %d times", served)
	}
}
