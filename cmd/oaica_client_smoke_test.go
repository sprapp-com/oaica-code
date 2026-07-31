package cmd

import "testing"

// Manual smoke test against the LIVE api.sprapp.com router — not part of the
// normal `go test` suite (network-dependent), run explicitly:
//   go test ./cmd/ -run TestOaicaClientLiveSmoke -v
func TestOaicaClientLiveSmoke(t *testing.T) {
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
