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
