package anthropic

import (
	"encoding/json"
	"testing"
)

func TestRound119OutputFormatSpellings(t *testing.T) {
	schema := `{"type":"object","properties":{"a":{"type":"integer"}},"required":["a"]}`
	for _, tc := range []struct{ name, extra string }{
		{"output_config.format (GA)", `"output_config":{"format":{"type":"json_schema","schema":` + schema + `}}`},
		{"output_format (structured-outputs beta)", `"output_format":{"type":"json_schema","schema":` + schema + `}`},
	} {
		var req MessagesRequest
		if err := json.Unmarshal([]byte(`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],`+tc.extra+`}`), &req); err != nil {
			t.Fatal(err)
		}
		cr, err := FromMessagesRequest(req)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		t.Logf("%-42s -> ChatRequest.Format=%q", tc.name, string(cr.Format))
		if string(cr.Format) != schema {
			t.Errorf("%s: the schema did not reach the runner's Format: %q", tc.name, string(cr.Format))
		}
	}
}
