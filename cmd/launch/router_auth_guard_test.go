package launch

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/ollama/ollama/api"
)

func stubCloudFetch(t *testing.T, entries []oaicaModelEntry, err error) {
	t.Helper()
	old := oaicaFetchCloudModelEntries
	ollamaCloudEntriesFn = nil
	oaicaFetchCloudModelEntries = func() ([]oaicaModelEntry, error) { return entries, err }
	t.Cleanup(func() { oaicaFetchCloudModelEntries = old })
}

// deadClient is an Ollama API client pointed at a port nothing listens on,
// so client.Show fails with a transport error (not a 404).
func deadClient(t *testing.T) *api.Client {
	t.Helper()
	u, _ := url.Parse("http://127.0.0.1:1")
	return api.NewClient(u, http.DefaultClient)
}

// A fresh install with no OAICA_API_KEY: the router answers 401, the model
// is not local, there are no user remotes. The error must name the fix
// (set OAICA_API_KEY / oaica signin) instead of falling through to an
// Ollama registry pull.
func TestShowOrPull_RouterAuthErrorNamesTheFix(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[]}`)
	stubBareIndex(t, map[string][]string{})
	stubCloudFetch(t, nil, &oaicaRouterError{Status: 401, Host: "https://api.oaica.com", Body: `{"error":{"code":"invalid_api_key"}}`})

	err := showOrPullWithPolicy(context.Background(), deadClient(t), "kat-awq", missingModelFail, false)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"OAICA_API_KEY", "oaica signin", "rejected the API key", "kat-awq"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q should mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "pull") {
		t.Fatalf("auth failure must not be reported as a pull problem: %q", err)
	}
}

// Router unreachable (network down, self-hosted box offline) is NOT an auth
// failure: the flow must fall open to local resolution exactly as before.
func TestShowOrPull_RouterUnreachableFailsOpen(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[]}`)
	stubBareIndex(t, map[string][]string{})
	stubCloudFetch(t, nil, errors.New("couldn't reach https://api.oaica.com: dial tcp: i/o timeout"))

	err := showOrPullWithPolicy(context.Background(), deadClient(t), "kat-awq", missingModelFail, false)
	if err == nil {
		t.Fatal("expected an error from the dead local client")
	}
	if strings.Contains(err.Error(), "rejected the API key") {
		t.Fatalf("unreachable router must not be reported as an auth failure: %q", err)
	}
}

func TestIsOaicaRouterAuthErr(t *testing.T) {
	cases := map[error]bool{
		&oaicaRouterError{Status: 401}: true,
		&oaicaRouterError{Status: 403}: true,
		&oaicaRouterError{Status: 503}: false,
		errors.New("dial tcp"):         false,
		nil:                            false,
	}
	for err, want := range cases {
		if got := isOaicaRouterAuthErr(err); got != want {
			t.Errorf("isOaicaRouterAuthErr(%v) = %v, want %v", err, got, want)
		}
	}
}
