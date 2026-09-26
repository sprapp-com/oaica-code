package launch

// qwen_installer_verified_download_integrity_test.go — finding #18: every other
// agent installer in this package downloads its upstream script through
// installer_dl.go, which hashes the bytes and refuses to execute them when a
// SHA-256 pin (built-in or OAICA_INSTALL_SHA256_<URL>) does not match. The
// qwen installer was the exception: it built
//
//	bash -c "set -o pipefail; curl -fsSL <url> | sed ... | bash"
//
// so the fetched script went straight into a shell with no integrity check at
// all — a compromised CDN, a DNS hijack or a MITM on the user's network
// executed attacker code with the user's privileges, and a truncated download
// could splice into a syntactically valid partial script. These tests pin the
// two properties that fix has to have: the bytes that run are the bytes the
// verified download handed back, and a refused download executes nothing.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const qwenTestVerifiedPath = "/tmp/oaica-qwen-verified-installer.sh"

// TestQwenInstallerRunsTheVerifiedDownload pins the argv to the file
// installer_dl.go verified: bash, one argument, that file — no curl, no pipe,
// no sed stage, and the fetch must have gone through the gate for the real
// qwen script URL.
func TestQwenInstallerRunsTheVerifiedDownload(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			var fetchedURL string
			oldFetch := fetchInstallerScriptFn
			fetchInstallerScriptFn = func(u string) (string, error) {
				fetchedURL = u
				return qwenTestVerifiedPath, nil
			}
			t.Cleanup(func() { fetchInstallerScriptFn = oldFetch })

			bin, args, err := qwenInstallerCommand(goos)
			if err != nil {
				t.Fatalf("qwenInstallerCommand(%s) error = %v", goos, err)
			}
			if fetchedURL == "" {
				t.Fatalf("qwenInstallerCommand(%s) never went through installer_dl.go's verified download; it built %q %s, which executes a remote script that was never hashed",
					goos, bin, strings.Join(args, " "))
			}
			if !strings.Contains(fetchedURL, "install-qwen") {
				t.Errorf("verified download fetched %q, want the qwen install script", fetchedURL)
			}
			if bin != "bash" {
				t.Errorf("bin = %q, want bash", bin)
			}
			if len(args) != 1 || args[0] != qwenTestVerifiedPath {
				t.Fatalf("installer argv = %v, want exactly the verified file %q", args, qwenTestVerifiedPath)
			}
			joined := strings.Join(append([]string{bin}, args...), " ")
			for _, banned := range []string{"|", "curl", "sed", "sh -s", "-c "} {
				if strings.Contains(joined, banned) {
					t.Errorf("installer command %q still contains %q: the script must be executed from the verified file, never piped or re-piped through a shell", joined, banned)
				}
			}
		})
	}
}

// TestQwenInstallerDoesNotExecuteWhenTheDownloadIsRefused is the behavioural
// half: when installer_dl.go refuses the bytes (a wrong or missing pin), the
// install must stop. The old argv ignored the gate entirely — it shelled out to
// curl | bash regardless — so the refusal changed nothing about what ran.
func TestQwenInstallerDoesNotExecuteWhenTheDownloadIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell fake binaries")
	}
	setLaunchTestHome(t, t.TempDir())

	tmp := t.TempDir()
	t.Setenv("PATH", tmp)

	oldGOOS := qwenGOOS
	qwenGOOS = "linux"
	t.Cleanup(func() { qwenGOOS = oldGOOS })

	// Fake curl and bash that record any invocation. Nothing may run: the
	// download was refused, so there is no verified file to execute.
	ran := filepath.Join(tmp, "ran.log")
	for _, name := range []string{"curl", "bash"} {
		script := fmt.Sprintf("#!/bin/sh\necho %s >> %q\nexit 0\n", name, ran)
		if err := os.WriteFile(filepath.Join(tmp, name), []byte(script), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}

	oldFetch := fetchInstallerScriptFn
	fetchInstallerScriptFn = func(string) (string, error) {
		return "", fmt.Errorf("installer qwen failed SHA-256 verification (got aa, want bb) — refusing to execute")
	}
	t.Cleanup(func() { fetchInstallerScriptFn = oldFetch })

	oldConfirm := DefaultConfirmPrompt
	DefaultConfirmPrompt = func(string, ConfirmOptions) (bool, error) { return true, nil }
	t.Cleanup(func() { DefaultConfirmPrompt = oldConfirm })

	_, err := ensureQwenInstalled()

	logged, _ := os.ReadFile(ran)
	if len(logged) > 0 {
		t.Fatalf("the installer executed %s even though the verified download refused the bytes — a refusal that does not stop execution is not a gate", strings.TrimSpace(string(logged)))
	}
	if err == nil || !strings.Contains(err.Error(), "refusing to execute") {
		t.Fatalf("ensureQwenInstalled with a refused download = %v, want the refusal", err)
	}
}

// TestQwenInstallerVerifiesTheRealGate drives installer_dl.go's actual
// checksum gate through the qwen entry point: a pin that does not match the
// bytes the server serves must stop the install before anything executes, and
// the matching pin must run exactly those bytes.
func TestQwenInstallerVerifiesTheRealGate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the unix install path runs the script with bash")
	}
	sentinel := filepath.Join(t.TempDir(), "executed")
	body := "#!/bin/sh\ntouch " + sentinel + "\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	oldURL := qwenInstallScriptURL
	qwenInstallScriptURL = srv.URL
	t.Cleanup(func() { qwenInstallScriptURL = oldURL })

	pinEnv := installerSHAEnv(srv.URL)
	t.Setenv(pinEnv, strings.Repeat("ab", 32))
	if _, _, err := qwenInstallerCommand("linux"); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("qwenInstallerCommand with a wrong pin = %v, want the SHA-256 refusal", err)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("bytes that failed SHA-256 verification were executed (%s)", sentinel)
	}

	// The same flow with the real pin: the downloaded file is what runs.
	sum := sha256.Sum256([]byte(body))
	t.Setenv(pinEnv, hex.EncodeToString(sum[:]))
	bin, args, err := qwenInstallerCommand("linux")
	if err != nil {
		t.Fatalf("qwenInstallerCommand with the correct pin error = %v", err)
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("running the verified installer: %v", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("the verified script did not run: %v", err)
	}
}
