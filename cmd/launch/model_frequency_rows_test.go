package launch

import "testing"

// The picker renders its own "Frequently used" section from this flag (see
// cmd/tui/selector.go's render split), so the flag has to be set here — a row
// the picker is going to show under that header must carry it, and a row that
// was never picked must not.
//
// The membership rule is model_frequency.go's, not this flag's: the top five by
// pick count, so a model picked once is in when the machine has not picked five
// others. (The design spec asked for an n >= 2 floor; the repo's recorded
// decision is the top-5 rule, pinned by
// TestBuildModelList_PinsFrequentlyPickedModelsAboveRecommendations, and this
// test follows it.)
func TestBuildModelList_MarksFrequentlyPickedRows(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	for i := 0; i < 3; i++ {
		recordModelPick("hot-model")
	}
	recordModelPick("cold-model")

	existing := []modelInfo{
		{Name: "hot-model:latest"},
		{Name: "cold-model:latest"},
		{Name: "never-picked:latest"},
	}
	items, _, _, _ := buildModelListWithRecommendations(existing, nil, nil, "")

	byName := map[string]ModelItem{}
	for _, it := range items {
		byName[it.Name] = it
	}
	if !byName["hot-model"].Frequent {
		t.Errorf("hot-model.Frequent = false, want true: the picker shows it under the Frequently used header")
	}
	if !byName["cold-model"].Frequent {
		t.Errorf("cold-model.Frequent = false, want true: it is in the top five by pick count")
	}
	if byName["never-picked"].Frequent {
		t.Errorf("never-picked.Frequent = true, want false: a model this machine never ran must not lead the picker")
	}
}

// The flag must survive the conversion into picker rows — a model marked here
// and dropped there is a section that never renders.
func TestSelectionItems_CarryTheFrequentFlag(t *testing.T) {
	items := []ModelItem{{Name: "hot-model", Frequent: true}, {Name: "cold-model"}}
	out := ApplyAccountStateToSelectionItems(items, AccountState{})
	if !out[0].Frequent || out[1].Frequent {
		t.Fatalf("SelectionItem.Frequent = %v/%v, want true/false", out[0].Frequent, out[1].Frequent)
	}
}
