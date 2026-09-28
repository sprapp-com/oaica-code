package launch

import "testing"

func TestTranslateModelID_AnthropicDotsBecomeDashes(t *testing.T) {
	if got := translateModelID("anthropic", "claude-haiku-4.5"); got != "claude-haiku-4-5" {
		t.Fatalf("got %q, want claude-haiku-4-5", got)
	}
	// Lossless: no native Anthropic slug contains a dot.
	if got := translateModelID("anthropic", "claude-opus-4-5"); got != "claude-opus-4-5" {
		t.Fatalf("dashed id must be untouched, got %q", got)
	}
}

// The half of the rule that a careless implementation gets wrong: OpenAI ids
// legitimately keep their dots.
func TestTranslateModelID_OpenAIDotsUntouched(t *testing.T) {
	if got := translateModelID("openai", "gpt-5.4"); got != "gpt-5.4" {
		t.Fatalf("got %q, want gpt-5.4 unchanged", got)
	}
}

func TestTranslateModelID_AggregatorAnthropicPrefix(t *testing.T) {
	vendor, rest, ok := splitVendorPrefix("anthropic/claude-haiku-4.5")
	if !ok || vendor != "anthropic" || rest != "claude-haiku-4.5" {
		t.Fatalf("split = %q %q %v", vendor, rest, ok)
	}
	if got := aggregatorWire(vendor, "openai"); got != "anthropic" {
		t.Fatalf("wire = %q", got)
	}
	if got := translateModelID("anthropic", rest); got != "claude-haiku-4-5" {
		t.Fatalf("aggregator anthropic id = %q", got)
	}
}

// OpenRouter-shaped ids keep their vendor/model form; the prefix selects the
// wire but is not stripped from anything else.
func TestTranslateModelID_OpenRouterFormsPassThrough(t *testing.T) {
	vendor, rest, ok := splitVendorPrefix("openai/gpt-5.4")
	if !ok || vendor != "openai" {
		t.Fatalf("split = %q %q %v", vendor, rest, ok)
	}
	if got := aggregatorWire(vendor, "openai"); got != "openai" {
		t.Fatalf("wire = %q", got)
	}
	if got := translateModelID("openai", rest); got != "gpt-5.4" {
		t.Fatalf("id = %q", got)
	}
}

// An unknown prefix is passed through, never dropped and never guessed at.
func TestTranslateModelID_UnknownPrefixPassthrough(t *testing.T) {
	if vendor, _, ok := splitVendorPrefix("meta-llama/llama-4"); ok {
		t.Fatalf("meta-llama must not be treated as a wire-selecting vendor, got %q", vendor)
	}
	if got := translateModelID("openai", "meta-llama/llama-4"); got != "meta-llama/llama-4" {
		t.Fatalf("unknown prefix id = %q", got)
	}
}

func TestTranslateModelID_NoSlashIsNotAnAggregator(t *testing.T) {
	if _, _, ok := splitVendorPrefix("gpt-5.4"); ok {
		t.Fatal("an id with no slash has no vendor prefix")
	}
}
