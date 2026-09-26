package launch

// shard_flag_tagged_id_integrity_test.go — `--shard` split its argument at the
// FIRST colon, so a leg whose id itself carries one never got its weight
// (2026-09-27 audit, round 22).
//
// The flag documents "the same picker vocabulary as --sonnet-model", and that
// vocabulary includes Ollama's tag form ("llama3.2:3b"), the `<name>:cloud`
// catalogue row and the `<name>:local` row. `--shard gpt-oss:cloud:3` cut at
// the first colon gave model "gpt-oss" and weight "cloud:3", Atoi failed, and
// the entry was dropped with no message — so the user's weighted split silently
// degraded to plain failover. The weight is the segment AFTER the last colon.

import "testing"

func TestShardFlagsAcceptTaggedModelIDs(t *testing.T) {
	shards, rest := extractShardFlags([]string{
		"--shard", "gpt-oss:cloud:3",
		"--shard", "llama3.2:3b:2",
		"--shard=kat:local:1",
		"run",
	})

	want := map[string]int{"gpt-oss:cloud": 3, "llama3.2:3b": 2, "kat:local": 1}
	for model, weight := range want {
		if shards[model] != weight {
			t.Errorf("shards[%q] = %d, want %d — the id keeps its colons and the weight is the last segment (got %v)", model, shards[model], weight, shards)
		}
	}
	if len(shards) != len(want) {
		t.Errorf("shards = %v, want exactly %v", shards, want)
	}
	if len(rest) != 1 || rest[0] != "run" {
		t.Errorf("rest = %v, want [run]", rest)
	}
}

// TestShardFlagsStillDropAMalformedEntry is the control: the documented silent
// drop for an entry that carries no usable weight is unchanged.
func TestShardFlagsStillDropAMalformedEntry(t *testing.T) {
	for _, spec := range []string{"bad-no-colon", "zero-weight:0", "negative:-1", "not-an-int:abc", "trailing:"} {
		shards, _ := extractShardFlags([]string{"--shard", spec})
		if len(shards) != 0 {
			t.Errorf("--shard %q produced %v, want it dropped as the flag documents", spec, shards)
		}
	}
}
