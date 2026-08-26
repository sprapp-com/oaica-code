package cmd

import (
	"os"
	"testing"
)

// Manual smoke test against the LIVE api.oaica.com router — not part of the
// normal `go test` suite (network-dependent), run explicitly:
//
//	go test ./cmd/ -run TestOaicaClientLiveSmoke -v
func TestOaicaClientLiveSmoke(t *testing.T) {
	if os.Getenv("OAICA_LIVE_SMOKE") == "" {
		t.Skip("live network smoke test; set OAICA_LIVE_SMOKE=1 to run")
	}
	// Use the real fetch, not the TestMain stub.
	oaicaListModelsDetailed = oaicaListModelsDetailedLive
	names, err := oaicaListModels()
	if err != nil {
		t.Fatalf("oaicaListModels: %v", err)
	}
	t.Logf("models: %v", names)
	if len(names) == 0 {
		t.Fatal("expected at least one model")
	}

	ok, _, err := oaicaModelExists("kat-coder-i-compact")
	if err != nil {
		t.Fatalf("oaicaModelExists: %v", err)
	}
	if !ok {
		t.Fatal("expected kat-coder-i-compact to exist")
	}

	reply, err := oaicaChat("kat-coder-i-compact", []oaicaChatMessage{
		{Role: "user", Content: "Say hi in exactly 3 words."},
	})
	if err != nil {
		t.Fatalf("oaicaChat: %v", err)
	}
	t.Logf("reply: %q", reply)
	if reply == "" {
		t.Fatal("expected non-empty reply")
	}
}
