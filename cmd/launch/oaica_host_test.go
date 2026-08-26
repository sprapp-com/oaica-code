package launch

import "testing"

// Same contract as cmd.oaicaHost(): the launch/picker flow must query the
// public gateway by default, and the two packages must never drift apart
// (they did — cmd and launch each carried their own copy of the old host).
func TestOaicaLaunchHost_DefaultIsPublicGateway(t *testing.T) {
	t.Setenv("OAICA_HOST", "")
	if got, want := oaicaLaunchHost(), "https://api.oaica.com"; got != want {
		t.Fatalf("oaicaLaunchHost() = %q, want %q", got, want)
	}
}

func TestOaicaLaunchHost_EnvOverrideTrimsSlash(t *testing.T) {
	t.Setenv("OAICA_HOST", "http://127.0.0.1:8081/")
	if got, want := oaicaLaunchHost(), "http://127.0.0.1:8081"; got != want {
		t.Fatalf("oaicaLaunchHost() = %q, want %q", got, want)
	}
}
