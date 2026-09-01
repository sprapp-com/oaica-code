package launch

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// stubInstallerPath is the fake "downloaded installer" path the test stub
// of fetchInstallerScriptFn returns.
const stubInstallerPath = "/tmp/oaica-test-installer.sh"

// stubFetchInstallerScript replaces the network fetch with a fake path for
// the duration of the test. The returned func restores the original.
func stubFetchInstallerScript(t *testing.T) func() {
	t.Helper()
	old := fetchInstallerScriptFn
	fetchInstallerScriptFn = func(string) (string, error) { return stubInstallerPath, nil }
	return func() { fetchInstallerScriptFn = old }
}

func TestFetchInstallerScriptServesDownloadedBytes(t *testing.T) {
	body := "#!/bin/sh\necho hi\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer srv.Close()

	path, err := fetchInstallerScript(srv.URL)
	if err != nil {
		t.Fatalf("fetchInstallerScript: %v", err)
	}
	defer os.Remove(path)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != body {
		t.Fatalf("downloaded bytes = %q, want %q", b, body)
	}
}

func TestFetchInstallerScriptRejectsWrongPin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("#!/bin/sh\n"))
	}))
	defer srv.Close()

	sum := sha256.Sum256([]byte("not the script"))
	env := installerSHAEnv(srv.URL)
	t.Setenv(env, hex.EncodeToString(sum[:]))
	if _, err := fetchInstallerScript(srv.URL); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("expected SHA-256 refusal, got %v", err)
	}
}

func TestInstallerSHAEnv(t *testing.T) {
	got := installerSHAEnv("https://claude.ai/install.sh")
	if got != "OAICA_INSTALL_SHA256_CLAUDE_AI_INSTALL_SH" {
		t.Fatalf("installerSHAEnv = %q", got)
	}
}
