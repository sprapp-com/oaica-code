package launch

import "testing"

func TestApplyAccountStateToSelectionItems_PropagatesRemote(t *testing.T) {
	items := []ModelItem{
		{Name: "ollama/glm-5.3-flash", Remote: true},
		{Name: "my-local-model", Local: true},
		{Name: "router-model"},
	}
	out := ApplyAccountStateToSelectionItems(items, AccountState{Status: accountStateUnknown})
	if len(out) != 3 {
		t.Fatalf("got %d items, want 3", len(out))
	}
	if !out[0].Remote {
		t.Error("ollama/glm-5.3-flash: Remote should be true")
	}
	if out[0].Local {
		t.Error("ollama/glm-5.3-flash: Local should be false")
	}
	if !out[1].Local || out[1].Remote {
		t.Errorf("my-local-model: got Local=%v Remote=%v, want Local=true Remote=false", out[1].Local, out[1].Remote)
	}
	if out[2].Remote || out[2].Local {
		t.Errorf("router-model: got Local=%v Remote=%v, want both false", out[2].Local, out[2].Remote)
	}
}
