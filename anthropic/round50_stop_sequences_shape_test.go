package anthropic

// round50_stop_sequences_shape_test.go — round 50's finding on the stop list:
// the elements were never checked.
//
// `stop_sequences` is an array of strings, and the metered gateway leg refuses a
// body that says otherwise. The local and proxy legs decoded it into a plain
// []string, where Go's json decoder turns a JSON null element into a no-op with
// NO error — so `[null]` decoded to exactly `[""]`, the turn was served, and the
// backend was handed an empty stop string: a stop sequence that matches at every
// position. One body, two verdicts (400 through the meter, a served turn here)
// and a hazard on the side that accepted it.
//
// The whole field being null stays accepted: that is a client stating nothing,
// which the gateway accepts too.

import (
	"encoding/json"
	"testing"
)

// round50DecodeStops decodes the stop list out of a request body the way every
// entry point does — through the JSON decoder, not by building the struct.
func round50DecodeStops(t *testing.T, field string) (MessagesRequest, error) {
	t.Helper()
	var req MessagesRequest
	body := `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]`
	if field != "" {
		body += `,"stop_sequences":` + field
	}
	body += `}`
	return req, json.Unmarshal([]byte(body), &req)
}

// TestAStopSequenceThatIsNotAStringIsRefused is the finding. Every element must
// be a JSON string, and a body that is not refused is handed the client's own
// strings with nothing invented.
func TestAStopSequenceThatIsNotAStringIsRefused(t *testing.T) {
	for _, c := range []struct {
		field   string
		refused bool
	}{
		{`["STOP"]`, false},
		{`["a","b"]`, false},
		{`[]`, false},
		{`null`, false},  // the field states nothing at all
		{`[null]`, true}, // decodes to [""] without the element check
		{`["a",null]`, true},
		{`[1]`, true},
		{`["a",1]`, true},
		{`[true]`, true},
		{`[["a"]]`, true},
		{`[{"a":1}]`, true},
		{`"STOP"`, true}, // not an array
		{`5`, true},
	} {
		t.Run(c.field, func(t *testing.T) {
			_, err := round50DecodeStops(t, c.field)
			if c.refused {
				if err == nil {
					t.Fatalf("stop_sequences:%s was accepted; the gateway leg refuses it, and an accepted null element becomes an empty stop string the backend matches at every position", c.field)
				}
				return
			}
			if err != nil {
				t.Fatalf("stop_sequences:%s was refused: %v", c.field, err)
			}
		})
	}
}

// TestAStopListIsHandedOnAsAPlainStringSlice keeps the type change from eating
// the option: the readers of options["stop"] type-switch on []string, so a named
// slice type would fall through every arm and lose the stop list silently.
func TestAStopListIsHandedOnAsAPlainStringSlice(t *testing.T) {
	req, err := round50DecodeStops(t, `["STOP","END"]`)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err := FromMessagesRequest(req)
	if err != nil {
		t.Fatalf("conversion: %v", err)
	}
	got, ok := out.Options["stop"].([]string)
	if !ok {
		t.Fatalf("options[\"stop\"] is %T, want []string — every reader of this option switches on that type", out.Options["stop"])
	}
	if len(got) != 2 || got[0] != "STOP" || got[1] != "END" {
		t.Errorf("options[\"stop\"] is %v, want the client's own list", got)
	}

	// A body that states nothing must leave the option unset, not set to empty.
	req, err = round50DecodeStops(t, `null`)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err = FromMessagesRequest(req)
	if err != nil {
		t.Fatalf("conversion: %v", err)
	}
	if v, present := out.Options["stop"]; present {
		t.Errorf("a body stating no stop sequences was handed stop=%v", v)
	}
}
