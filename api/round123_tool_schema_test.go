package api

// F123-L1-4 (2026-09-29 audit, round 123): a tool schema nested absurdly deep is refused, not decoded
// in time quadratic in its depth; a deep but real one still decodes.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func nestedSchema(depth int) string {
	return strings.Repeat(`{"type":"object","properties":{"a":`, depth) + `{"type":"string"}` + strings.Repeat(`}}`, depth)
}

func TestRound123DeepToolSchemaIsRefusedQuickly(t *testing.T) {
	var p ToolFunctionParameters
	start := time.Now()
	err := json.Unmarshal([]byte(nestedSchema(3000)), &p)
	if err == nil {
		t.Fatal("a 6000-level schema was accepted")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("refusing it took %s", d)
	}
	var ok ToolFunctionParameters
	if err := json.Unmarshal([]byte(nestedSchema(40)), &ok); err != nil {
		t.Errorf("an 80-level schema was refused: %v", err)
	}
}

// F124-L1-1 (2026-09-29 audit, round 124): the cost is bytes x depth, so a schema that is deep AND
// large is refused, while a large flat one and a deep small one still decode.
func TestRound124DeepAndLargeToolSchemaIsRefused(t *testing.T) {
	big := strings.Repeat("d", 1<<20)
	deepBig := strings.Repeat(`{"type":"object","properties":{"a":`, 100) + `{"type":"string","description":"` + big + `"}` + strings.Repeat(`}}`, 100)
	var p ToolFunctionParameters
	start := time.Now()
	if err := json.Unmarshal([]byte(deepBig), &p); err == nil {
		t.Error("a 200-JSON-level schema with a 1 MiB leaf was accepted")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("refusing it took %s", d)
	}
	flatBig := `{"type":"object","properties":{"a":{"type":"string","description":"` + big + `"}}}`
	var ok ToolFunctionParameters
	if err := json.Unmarshal([]byte(flatBig), &ok); err != nil {
		t.Errorf("a large flat schema was refused: %v", err)
	}
	var ok2 ToolFunctionParameters
	if err := json.Unmarshal([]byte(nestedSchema(60)), &ok2); err != nil {
		t.Errorf("a 60-level small schema was refused: %v", err)
	}
}
