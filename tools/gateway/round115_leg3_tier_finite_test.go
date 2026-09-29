package main

// F115-L3-3 (2026-09-29 audit, round 115): a tier price must be a finite number, as the flat
// price is; "Inf" parsed, passed `> 0`, and made every served turn's ledger row unencodable.

import "testing"

func TestRound115TierPriceMustBeFinite(t *testing.T) {
	for _, p := range []string{"Inf", "+Inf", "NaN", "-Inf"} {
		if err := validatePricingTiers([]gwPricingTier{{Prompt: p}}); err == nil {
			t.Errorf("tier price %q was accepted", p)
		}
	}
	if err := validatePricingTiers([]gwPricingTier{{Prompt: "0.000001"}}); err != nil {
		t.Errorf("a finite tier price was refused: %v", err)
	}
}
