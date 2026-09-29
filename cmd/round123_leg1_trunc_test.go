package cmd

import (
	"strings"
	"testing"
)

func TestRound123DiagnosisRedactsBeforeItCuts(t *testing.T) {
	key := "sk-oaica-0123456789abcdefghijklmnopqrstuv"
	t.Setenv("OAICA_API_KEY", key)
	t.Setenv("HOME", t.TempDir())
	body := `{"error":{"message":"` + strings.Repeat("x", 232) + ` invalid api key ` + key + `"}}`
	got := oaicaDiagnosisBody([]byte(body))
	t.Logf("tail: %q", got[len(got)-60:])
	if strings.Contains(got, key[:20]) {
		t.Fatalf("key prefix leaked: %q", got[strings.Index(got, "sk-"):])
	}
}
