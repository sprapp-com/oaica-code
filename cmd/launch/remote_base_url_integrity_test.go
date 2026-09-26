package launch

// remote_base_url_integrity_test.go — `remote add --base-url` was accepted
// verbatim (2026-09-26 audit, fourth round).
//
// Every value was stored and the command printed its success line, so
// `--base-url garbage` produced a remote that fails at request time inside the
// proxy — oaica builds "<base>/chat/completions" and http.NewRequest rejects the
// scheme — with an error that names neither the remote nor the flag. A value
// with an embedded newline is printed by `oaica remote list` and the picker, so
// it forges an extra line of output. A value with no host ("not/absolute") is
// the same failure by a shorter route.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestRemoteAddRefusesABaseURLThatCannotWork(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	for i, bad := range []string{
		"garbage",
		"not/absolute",
		"api.example.com",           // host with no scheme
		"ftp://api.example.com",     // scheme oaica cannot request
		"file:///etc/passwd",        // ditto, and it reads a local file
		"https://",                  // scheme, no host
		"https://api.example.com x", // interior space
		"https://api.example.com\nevil\trow",
	} {
		name := fmt.Sprintf("rr-%d", i)
		if _, err := RemoteAdd(RemoteAddOptions{Name: name, BaseURL: bad}); err == nil {
			t.Errorf("RemoteAdd accepted --base-url %q — the value is stored verbatim, so the command reports success and the remote fails later at request time with an error naming neither the remote nor this flag (and a newline in it forges a row in every listing that prints it)", bad)
		}
	}

	// A refused add must not have reached the store.
	path := userRemotesPath()
	if path == "" {
		t.Fatal("no remotes path under the test HOME")
	}
	if b, err := os.ReadFile(path); err == nil {
		var f userRemotesFile
		if json.Unmarshal(b, &f) == nil && len(f.Remotes) != 0 {
			t.Errorf("a refused --base-url reached remotes.json: %s", b)
		}
	}
}

func TestRemoteAddStillAcceptsRealEndpoints(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	// The shapes a real user configures: the documented form, a LAN box on
	// http, a base that already carries its version path, and the userinfo form
	// oaica explicitly supports (splitRemoteUserinfo).
	good := []string{
		"https://api.example.com",
		"https://api.example.com/v1",
		"http://192.168.1.10:11434/v1",
		"https://sk-abc@api.example.com/v1",
		"https://generativelanguage.googleapis.com/v1beta/openai",
	}
	for i, u := range good {
		if _, err := RemoteAdd(RemoteAddOptions{Name: fmt.Sprintf("ok-%d", i), BaseURL: u}); err != nil {
			t.Errorf("RemoteAdd refused %q: %v", u, err)
		}
	}

	path := userRemotesPath()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("remotes.json was not written: %v", err)
	}
	var f userRemotesFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Remotes) != len(good) {
		t.Errorf("%d of %d remotes stored: %s", len(f.Remotes), len(good), b)
	}
	for _, r := range f.Remotes {
		if !strings.HasPrefix(r.BaseURL, "http://") && !strings.HasPrefix(r.BaseURL, "https://") {
			t.Errorf("stored remote %q has base_url %q", r.Name, r.BaseURL)
		}
	}
}
