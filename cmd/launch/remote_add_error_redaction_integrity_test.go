package launch

// remote_add_error_redaction_integrity_test.go — `oaica remote add` printed the
// credential it was refusing (2026-09-26 audit, ninth round, auditor B).
//
// validateRemoteBaseURL exists so a --base-url that cannot work is refused at
// the point the user typed it, and six of its branches quote the value back.
// The success line two lines below deliberately redacts
// (cmd.go's `launch.RedactBaseURL(r.EndpointBase())`), and every other site
// that prints a base URL does the same (models.go, oaica_models.go,
// tier_routing.go, doctor) — this one did not, so a base URL carrying its key as
// userinfo (`https://sk-live-…@api.example.com`) put that key in the terminal,
// the CI log and the shell's scrollback, in the exact case the user has just
// mistyped the line and is most likely to paste the output somewhere.

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
)

const addEchoKey = "sk-live-ERRORECHO-0123456789"

// addEchoInputs are base URLs that trip each validation branch, with the key in
// the userinfo so a leak is unambiguous.
func addEchoInputs() []string {
	return []string{
		"https://" + addEchoKey + "@api.example.com/v1 with space",
		"https://" + addEchoKey + "@api.example.com/v1\nnext-line",
		"api.example.com/v1?key=" + addEchoKey,
		"ftp://" + addEchoKey + "@api.example.com",
		"https://" + addEchoKey + "@",
		"https://" + addEchoKey + "@api.example.com:99999999/v1",
	}
}

// Every validation branch must name the problem without naming the credential.
func TestRemoteAddValidationErrorsDoNotEchoTheKey(t *testing.T) {
	withTempRemotesFile(t)

	for _, in := range addEchoInputs() {
		_, err := RemoteAdd(RemoteAddOptions{Name: "box", BaseURL: in})
		if err == nil {
			t.Errorf("--base-url %q was accepted; this input was chosen to trip a validation branch", redactBaseURL(in))
			continue
		}
		if strings.Contains(err.Error(), addEchoKey) {
			t.Errorf("`oaica remote add --base-url %s` printed the credential in its refusal:\n%s\n\nthe success line below it redacts, and so does every other place this value is printed",
				redactBaseURL(in), err)
		}
		// The rest of the URL is the diagnostic and must survive: a message that
		// dropped the host would leave the user with nothing to fix.
		if !strings.Contains(err.Error(), "api.example.com") {
			t.Errorf("--base-url %q: the refusal lost the host — a redacted message that says nothing about which URL is wrong is not a usable error\n%s", redactBaseURL(in), err)
		}
	}
}

// The URL that redacts cleanly while a key hides somewhere else: url.Parse's
// own error text re-states the value, so the branches that wrap `err` from
// url.Parse have to be redacted too.
func TestRemoteAddParseErrorDoesNotEchoTheKey(t *testing.T) {
	withTempRemotesFile(t)

	// A control character makes url.Parse fail, and its message quotes the URL.
	in := "https://" + addEchoKey + "@api.example.com/\x7f"
	_, err := RemoteAdd(RemoteAddOptions{Name: "box", BaseURL: in})
	if err == nil {
		t.Fatalf("a URL with a DEL in it was accepted")
	}
	if strings.Contains(err.Error(), addEchoKey) {
		t.Errorf("the message that wraps url.Parse's error carries the credential:\n%s", err)
	}
}

// And the boundary, as the CLI actually produces it: main.go prints whatever
// RunE returns with cobra.CheckErr, and SilenceErrors is set, so nothing
// downstream redacts. Whatever a future validation branch prints, this is the
// contract the terminal sees — driven through the real command in the cmd
// package, where that boundary lives.
func TestRedactErrorKeepsTheDiagnostic(t *testing.T) {
	withTempRemotesFile(t)

	in := "https://" + addEchoKey + "@api.example.com/v1 with space"
	_, err := RemoteAdd(RemoteAddOptions{Name: "box", BaseURL: in})
	if err == nil {
		t.Fatalf("premise: the input did not fail validation")
	}
	printed := fmt.Sprintf("%v", RedactError(err))
	if strings.Contains(printed, addEchoKey) {
		t.Errorf("the redaction helper left the credential in an error the CLI prints:\n%s", printed)
	}
	if !strings.Contains(printed, "api.example.com") {
		t.Errorf("the redaction erased the diagnostic along with the key:\n%s", printed)
	}
	if _, parseErr := url.Parse(in); parseErr != nil {
		t.Logf("note: %v", parseErr)
	}
}
