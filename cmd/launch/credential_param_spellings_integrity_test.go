package launch

// credential_param_spellings_integrity_test.go — the parameter-name vocabulary
// missed the ordinary ways a credential parameter is spelled, and a miss is
// symmetric: a name the redactor does not recognise is printed verbatim AND is
// invisible to the leak scan, so the doctor reported a clean bill of health on a
// URL it had just failed to hide (2026-09-26 audit, seventh round).
//
// The word list matched only whole, exactly-spelled words ("key", "secret",
// "creds") plus a fixed set of no-separator compounds ("apikey", "authkey").
// So `?secretkey=sk-live-…` — a key whose name is the word "secret" glued to
// the word "key", exactly like the "apikey" the list already knew — went out in
// the clear, and the same name was not collected by querySecrets.
//
// The fix keeps the exact-word match but makes the joined-name test a
// CORE test rather than a fixed-string one: strip a trailing qualifier that
// names a field ON a credential ("key id", "password hash", "token value") and
// compare the remainder against the credential vocabulary, so any prefix or
// suffix carrying one of those words is recognised wherever it is glued.

import (
	"strings"
	"testing"
)

// credentialSpellings are names that must be treated as credentials. Every one
// of these is a credential word with a qualifier glued on, which is how
// parameter names are actually written.
var credentialSpellings = []string{
	"secretkey", "accesskey", "appkey", "signkey", "xkey", "mykey",
	"publickey", "keyid", "apikeyid", "keyvalue", "keyhash",
	"clientcreds", "usercreds", "creds", "cred",
	"passphrase", "pass", "passwordhash", "passwordvalue",
	"secretid", "secretvalue", "sessionid", "tokenid", "cookieid", "authid",
	"jwtid", "authtoken", "sessiontoken", "accesstoken", "refreshtoken",
	"api_key", "apiKey", "x-api-key", "access-token", "client_secret",
	"Authorization", "auth_key", "privateKey", "secret_key",
}

// notCredentials are names that must NOT be treated as credentials. Over-
// redaction is the cheaper error, but the scan feeds a support report and a
// false positive there is a lie about the deployment.
var notCredentials = []string{
	"model", "temperature", "stream", "region", "version", "page", "q",
	"sort", "locale", "deployment", "limit", "format", "top_p", "n",
}

func TestCredentialParameterSpellingsAreRecognised(t *testing.T) {
	for _, name := range credentialSpellings {
		if !looksLikeCredentialParam(name) {
			t.Errorf("looksLikeCredentialParam(%q) = false — a parameter this spelled is printed verbatim by the redactor and skipped by the leak scan, so the doctor reports a clean deployment while the key travels in the clear", name)
		}
	}
}

func TestOrdinaryParametersAreNotTreatedAsCredentials(t *testing.T) {
	for _, name := range notCredentials {
		if looksLikeCredentialParam(name) {
			t.Errorf("looksLikeCredentialParam(%q) = true — an ordinary parameter is now redacted out of every report and flagged by the leak scan", name)
		}
	}
}

// End to end through the two consumers, because the predicate being right is
// only half of it: the redactor must actually hide the value, and the scan must
// actually collect it.
func TestUnrecognisedSpellingsAreHiddenAndCollected(t *testing.T) {
	for _, name := range credentialSpellings {
		url := "https://api.example.com/v1?" + name + "=sk-live-SECRETVALUE"
		got := redactCredentials(url)
		if strings.Contains(got, "SECRETVALUE") {
			t.Errorf("redactCredentials(%q) = %q — the value is still on the line", url, got)
		}
		if !strings.Contains(got, name) {
			t.Errorf("redactCredentials(%q) = %q — the parameter NAME must survive, it is the diagnostic", url, got)
		}
		secrets := querySecrets(url)
		if len(secrets) != 1 || secrets[0] != "sk-live-SECRETVALUE" {
			t.Errorf("querySecrets(%q) = %v — the leak scan does not see this parameter, so `oaica doctor` reports the URL as clean", url, secrets)
		}
	}
}

// The controls, through the same two consumers: an ordinary parameter is left
// alone by both.
func TestOrdinaryParametersSurviveBothConsumers(t *testing.T) {
	for _, name := range notCredentials {
		url := "https://api.example.com/v1?" + name + "=ordinaryvalue"
		if got := redactCredentials(url); got != url {
			t.Errorf("redactCredentials(%q) = %q, want it unchanged", url, got)
		}
		if secrets := querySecrets(url); len(secrets) != 0 {
			t.Errorf("querySecrets(%q) = %v, want none", url, secrets)
		}
	}
}
