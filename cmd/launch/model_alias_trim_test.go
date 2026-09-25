package launch

// model_alias_trim_test.go — `oaica model alias set` stores the TRIMMED name
// (ModelAliasSet) and `alias rm` trims too, but the reader did not: a lookup
// with surrounding whitespace named nothing, so `oaica model alias show "  glm
//  "` reported "no alias named ..." for an alias that `alias set` and `alias
// rm` with the identical argument both accepted (2026-09-26 audit).

import (
	"strings"
	"testing"
)

func TestModelAliasGetTrimsTheNameItLooksUp(t *testing.T) {
	withTempOaicaHome(t)

	const target = "ollama/glm-5.3-flash:cloud"
	if err := ModelAliasSet("glm", target); err != nil {
		t.Fatalf("ModelAliasSet: %v", err)
	}

	got, err := ModelAliasGet("  glm  ")
	if err != nil {
		t.Fatalf("ModelAliasGet(\"  glm  \") failed on an alias `alias set glm` created: %v", err)
	}
	if got != target {
		t.Errorf("ModelAliasGet(\"  glm  \") = %q, want %q", got, target)
	}

	// And the same spelling must be what removal accepts — set, get and
	// remove have to agree on which name they name.
	existed, err := ModelAliasRemove("  glm  ")
	if err != nil {
		t.Fatalf("ModelAliasRemove: %v", err)
	}
	if !existed {
		t.Error("ModelAliasRemove(\"  glm  \") reported not-existed for the alias ModelAliasSet just created")
	}
	if _, err := ModelAliasGet("glm"); err == nil {
		t.Error("the alias survived its own removal")
	}

	// A whitespace-only name is the empty name, not a lookup that hits
	// whatever the file happens to hold.
	if _, err := ModelAliasGet("   "); err == nil {
		t.Error("a whitespace-only name resolved to an alias")
	} else if !strings.Contains(err.Error(), "\"\"") {
		t.Errorf("the error for a whitespace-only name should quote the empty name it became, got: %v", err)
	}
}
