package launch

// model_remover_trim_test.go — a name is stored trimmed, so it has to be
// looked up trimmed (round-4 audit, 2026-09-26).
//
// ModelAdd stores strings.TrimSpace(ID) and ModelAliasSet stores the trimmed
// name; ModelRemove, ModelShow and ModelAliasRemove looked up the argument
// verbatim. The argument a user passes to `rm` is the one they passed to
// `add`, so a padded name failed with "no manifest entry" while the row
// survived — and the padded spelling is the one they had just used to create
// it. RemoteRemove and PlanRemove already trim (the pin at the bottom).

import "testing"

func TestRemoversFindTheNameTheWriterStored(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	if _, err := ModelAdd(ModelAddOptions{ID: "  tiny  ", Engine: "vllm"}); err != nil {
		t.Fatalf("model add: %v", err)
	}
	m, err := loadModelManifest()
	if err != nil {
		t.Fatal(err)
	}
	if got := m.SortedIDs(); len(got) != 1 || got[0] != "tiny" {
		t.Fatalf("premise changed: `model add \"  tiny  \"` stored %v, want [tiny]", got)
	}

	if _, err := ModelShow("  tiny  "); err != nil {
		t.Errorf("ModelShow(\"  tiny  \") = %v; the entry id is stored trimmed, so the same padded argument must find it", err)
	}
	existed, err := ModelRemove("  tiny  ")
	if err != nil {
		t.Fatalf("ModelRemove: %v", err)
	}
	if !existed {
		m2, _ := loadModelManifest()
		t.Errorf("`oaica model rm \"  tiny  \"` reported no such entry and left %v in the manifest, though the same "+
			"padded argument is what `model add` stored under \"tiny\" — the user cannot remove the name they just created",
			m2.SortedIDs())
	}

	if err := ModelAliasSet("  glm  ", "ollama/glm-5.3-flash:cloud"); err != nil {
		t.Fatalf("alias set: %v", err)
	}
	names, err := ModelAliasSortedNames()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "glm" {
		t.Fatalf("premise changed: `alias set \"  glm  \"` stored %v", names)
	}
	existed, err = ModelAliasRemove("  glm  ")
	if err != nil {
		t.Fatalf("ModelAliasRemove: %v", err)
	}
	if !existed {
		names2, _ := ModelAliasSortedNames()
		t.Errorf("`oaica model alias rm \"  glm  \"` reported no alias and left %v defined, though the same padded "+
			"argument is what created \"glm\"", names2)
	}
}

// The contract the finding was measured against: the remover for a remote and
// the remover for a plan both trim. If they ever stop, the same dead end
// reappears one command over.
func TestRemoteAndPlanRemoversStillTrim(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	if _, err := RemoteAdd(RemoteAddOptions{Name: "  box  ", BaseURL: "https://api.example.com"}); err != nil {
		t.Fatal(err)
	}
	if existed, err := RemoteRemove("  box  "); err != nil || !existed {
		t.Errorf("RemoteRemove(\"  box  \") = (%v, %v), want (true, nil)", existed, err)
	}
	if err := PlanSet("  p2  ", TierPlanProfile{Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if existed, err := PlanRemove("  p2  "); err != nil || !existed {
		t.Errorf("PlanRemove(\"  p2  \") = (%v, %v), want (true, nil)", existed, err)
	}
}
