package launch

import "testing"

func TestPlanSetGetRemove(t *testing.T) {
	withTempOaicaHome(t)

	if err := PlanSet("oaica-full", TierPlanProfile{Model: "kat-awq", SonnetModel: "kat-awq-7b", Description: "Full plan"}); err != nil {
		t.Fatalf("PlanSet: %v", err)
	}
	prof, err := PlanGet("oaica-full")
	if err != nil {
		t.Fatalf("PlanGet: %v", err)
	}
	if prof.Model != "kat-awq" || prof.SonnetModel != "kat-awq-7b" {
		t.Fatalf("unexpected profile: %+v", prof)
	}

	names, err := PlanSortedNames()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "oaica-full" {
		t.Fatalf("PlanSortedNames = %v", names)
	}

	existed, err := PlanRemove("oaica-full")
	if err != nil {
		t.Fatal(err)
	}
	if !existed {
		t.Fatal("PlanRemove reported not-existed for a plan that was just set")
	}
	if _, err := PlanGet("oaica-full"); err == nil {
		t.Fatal("PlanGet succeeded after removal")
	}
}

func TestPlanSet_RequiresModel(t *testing.T) {
	withTempOaicaHome(t)
	if err := PlanSet("x", TierPlanProfile{}); err == nil {
		t.Fatal("expected error for empty --model")
	}
	if err := PlanSet("", TierPlanProfile{Model: "x"}); err == nil {
		t.Fatal("expected error for empty plan name")
	}
}

func TestExtractPlanFlag(t *testing.T) {
	cases := []struct {
		in       []string
		wantPlan string
		wantRest []string
	}{
		{[]string{"--plan", "oaica-full", "--other"}, "oaica-full", []string{"--other"}},
		{[]string{"--plan=oaica-full"}, "oaica-full", nil},
		{[]string{"--other"}, "", []string{"--other"}},
	}
	for _, c := range cases {
		plan, rest := extractPlanFlag(c.in)
		if plan != c.wantPlan {
			t.Errorf("extractPlanFlag(%v) plan = %q, want %q", c.in, plan, c.wantPlan)
		}
		if len(rest) != len(c.wantRest) {
			t.Errorf("extractPlanFlag(%v) rest = %v, want %v", c.in, rest, c.wantRest)
		}
	}
}

func TestResolvePlanModels(t *testing.T) {
	withTempOaicaHome(t)
	if err := PlanSet("oaica-full", TierPlanProfile{Model: "kat-awq", SonnetModel: "kat-awq-7b"}); err != nil {
		t.Fatal(err)
	}

	// No --model/--sonnet-model given: plan fills both.
	m, s, err := resolvePlanModels("oaica-full", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if m != "kat-awq" || s != "kat-awq-7b" {
		t.Fatalf("resolvePlanModels = (%q, %q)", m, s)
	}

	// Explicit --model overrides the plan's model; --sonnet-model still fills from plan.
	m, s, err = resolvePlanModels("oaica-full", "explicit-model", "")
	if err != nil {
		t.Fatal(err)
	}
	if m != "explicit-model" || s != "kat-awq-7b" {
		t.Fatalf("resolvePlanModels with explicit model = (%q, %q)", m, s)
	}

	// No plan name: passthrough unchanged.
	m, s, err = resolvePlanModels("", "some-model", "some-sonnet")
	if err != nil {
		t.Fatal(err)
	}
	if m != "some-model" || s != "some-sonnet" {
		t.Fatalf("resolvePlanModels no-plan passthrough = (%q, %q)", m, s)
	}

	// Unknown plan name: error.
	if _, _, err := resolvePlanModels("does-not-exist", "", ""); err == nil {
		t.Fatal("expected error for unknown plan")
	}
}
