package launch

// round42_client_leg_integrity_test.go — round 42's two findings on the
// client-side proxy, C42-2 and C42-5.
//
// C42-2: the cache read-out is read through statedCacheHit, whose fallback to
// the sibling `prompt_cache_hit_tokens` is guarded on the VALUE of the details
// count. Testing `== 0` let an explicit `cached_tokens: -5` short-circuit the
// fallback and then be discarded, so a usage object the gateway leg reports as
// 900 cached tokens was reported by this leg as 0 — one object, two answers,
// decided by which leg served it.
//
// C42-5: the system hoist merges every system message into one leading string,
// which it cannot do for a message carrying anything but text without dropping
// what it carries — an image part, a tool call, a tool-result id. All three legs
// apply the same rule; this is the client leg's half of it.

import (
	"encoding/json"
	"testing"
)

// round42Usage decodes a usage object the way the wire delivers it, so the
// anonymous details struct does not have to be spelled out here.
func round42Usage(t *testing.T, body string) *openAIUsage {
	t.Helper()
	var u openAIUsage
	if err := json.Unmarshal([]byte(body), &u); err != nil {
		t.Fatalf("unmarshal usage: %v", err)
	}
	return &u
}

// TestANonPositiveDetailsCountDoesNotSuppressTheSiblingHit is C42-2. A count
// that is not positive is no statement, so it must fall through to the sibling
// spelling that does state the hit — and the sibling's own non-positive value
// is no statement either.
func TestANonPositiveDetailsCountDoesNotSuppressTheSiblingHit(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want int
	}{
		{"a negative details count beside a stated sibling hit",
			`{"prompt_tokens":900,"prompt_tokens_details":{"cached_tokens":-5},"prompt_cache_hit_tokens":900}`, 900},
		{"a zeroed details object beside a stated sibling hit",
			`{"prompt_tokens":900,"prompt_tokens_details":{"cached_tokens":0},"prompt_cache_hit_tokens":900}`, 900},
		{"a details count alone",
			`{"prompt_tokens":900,"prompt_tokens_details":{"cached_tokens":450}}`, 450},
		{"a negative sibling with no details object",
			`{"prompt_tokens":900,"prompt_cache_hit_tokens":-5}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := round42Usage(t, tc.in).statedCacheHit(); got != tc.want {
				t.Errorf("statedCacheHit() = %d, want %d for %s: the details count falls through to the sibling when it is not positive, and a non-positive count is no statement on either spelling (the gateway leg reports this same object as %d cached)", got, tc.want, tc.in, tc.want)
			}
		})
	}
}

// TestASystemMessageCarryingMoreThanTextIsNotMerged is C42-5 on this leg. A
// message that carries an image cannot be merged into the single leading
// system string, so the conversation is left exactly as the client sent it.
func TestASystemMessageCarryingMoreThanTextIsNotMerged(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  openAIMessage
	}{
		{"an image part", openAIMessage{Role: "system", Content: "late", Images: []openAIImageBlock{{DataURL: "data:image/png;base64,QUJDRA=="}}}},
		{"a tool call", openAIMessage{Role: "system", Content: "late", ToolCalls: []openAIToolCall{{ID: "c1"}}}},
		{"a tool-result id", openAIMessage{Role: "system", Content: "late", ToolCallID: "c1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := []openAIMessage{{Role: "user", Content: "go"}, tc.msg}

			got := normalizeSystemFirst(in)

			if len(got) != 2 {
				t.Fatalf("normalizeSystemFirst() = %v, want the two messages exactly where the client put them: a system message carrying %s cannot be merged into a leading string without dropping it", got, tc.name)
			}
			if got[0].Role != "user" || got[0].Content != "go" {
				t.Errorf("message 0 = %v, want the user turn untouched", got[0])
			}
			if got[1].Role != "system" {
				t.Errorf("message 1 = %v, want the system message in place", got[1])
			}
			if got[1].Content != tc.msg.Content {
				t.Errorf("the system message's text was rewritten from %q to %q", tc.msg.Content, got[1].Content)
			}
			if len(got[1].Images) != len(tc.msg.Images) || len(got[1].ToolCalls) != len(tc.msg.ToolCalls) || got[1].ToolCallID != tc.msg.ToolCallID {
				t.Errorf("the system message reached the upstream as %+v, carrying less than the client sent (%+v)", got[1], tc.msg)
			}
		})
	}
}

// The control: a late system message that carries only text IS hoisted, so the
// bail-out above is about what a message carries and not about rewriting at
// all.
func TestALateTextOnlySystemMessageIsHoisted(t *testing.T) {
	got := normalizeSystemFirst([]openAIMessage{
		{Role: "user", Content: "go"},
		{Role: "system", Content: "late"},
	})
	if len(got) != 2 || got[0].Role != "system" || got[0].Content != "late" || got[1].Role != "user" {
		t.Errorf("normalizeSystemFirst() = %+v, want the system message first and the user turn after it: a text-only system message after a user turn is hoisted, which is what a strict chat template requires", got)
	}
}

// And the identity: an already-ordered conversation is returned byte for byte,
// because merging its several leading system messages re-renders a prompt the
// client sent.
func TestAnOrderedConversationIsNotReRenderedByTheClientLeg(t *testing.T) {
	in := []openAIMessage{
		{Role: "system", Content: "first"},
		{Role: "system", Content: "second"},
		{Role: "user", Content: "go"},
	}
	got := normalizeSystemFirst(in)
	if len(got) != 3 {
		t.Fatalf("normalizeSystemFirst() = %+v, want the three messages untouched", got)
	}
	for i := range in {
		if got[i].Role != in[i].Role || got[i].Content != in[i].Content {
			t.Errorf("message %d = %+v, want %+v: the rewrite is a no-op when the conversation is already ordered", i, got[i], in[i])
		}
	}
}
