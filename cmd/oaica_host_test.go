package cmd

import "testing"

// The default router must be the public gateway a customer's key works
// against. It was api.sprapp.com until 2026-08-26, which had been answering
// 503 "no inference box currently reachable" — every fresh install with no
// OAICA_HOST set got zero models and fell through to the Ollama registry
// ("pull model manifest: file does not exist").
func TestOaicaHost_DefaultIsPublicGateway(t *testing.T) {
	t.Setenv("OAICA_HOST", "")
	if got, want := oaicaHost(), "https://api.oaica.com"; got != want {
		t.Fatalf("oaicaHost() = %q, want %q", got, want)
	}
}

func TestOaicaHost_EnvOverrideTrimsSlash(t *testing.T) {
	t.Setenv("OAICA_HOST", " http://127.0.0.1:8081/ ")
	if got, want := oaicaHost(), "http://127.0.0.1:8081"; got != want {
		t.Fatalf("oaicaHost() = %q, want %q", got, want)
	}
}
