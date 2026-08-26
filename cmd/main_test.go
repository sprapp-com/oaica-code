package cmd

import (
	"os"
	"testing"
)

// TestMain isolates the cmd test suite from the real OAICA router and from
// provider keys in the developer's shell. Each test stands up its own fake
// OLLAMA_HOST; the router must answer "no models" unless a test overrides
// oaicaListModelsDetailed itself. Without this, 8 RunHandler tests reached
// the live router and failed on any machine without a reachable OAICA_HOST.
func TestMain(m *testing.M) {
	os.Setenv("OAICA_HOST", "")
	os.Setenv("OAICA_API_KEY", "")
	os.Setenv("Z_AI_API_KEY", "")
	os.Setenv("OPENROUTER_API_KEY", "")
	oaicaListModelsDetailed = func() ([]oaicaModelListEntry, error) { return nil, nil }
	os.Exit(m.Run())
}
