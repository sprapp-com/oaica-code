package launch

// shard_weight_bound_integrity_test.go — `--shard model:weight` reached the
// hash ring unbounded, and the ring was built per request (2026-09-26 audit,
// tenth round).
//
// weightedPick built Weight*weightedRingVpointsPerUnit points per candidate on
// EVERY request (the comment claimed the ring was "rebuilt only when the
// healthy set changes" — there is no cache). The weight comes straight from
// the flag, which rejected only `<= 0`, so `--shard box/big:1000000` allocated
// and sorted ~1e9 points — ~149 GiB — before picking one, per request, on the
// request path of a launched session.
//
// The weight is a relative share: every ratio a user wants is small. It is now
// bounded at the flag, out-of-range values are skipped exactly like the other
// malformed ones (a non-numeric weight, a weight <= 0), and the ring itself is
// apportioned to a fixed budget so the memory one request can allocate never
// depends on a number in a flag.

import (
	"testing"
)

// A weight no ratio needs is skipped, like every other malformed value, rather
// than silently applied at whatever cost it implies.
func TestAnAbsurdShardWeightIsNotApplied(t *testing.T) {
	for _, spec := range []string{"box/big:1000000", "box/big:10001"} {
		shards, _ := extractShardFlags([]string{"--shard", spec})
		if len(shards) != 0 {
			t.Errorf("--shard %s was applied with weight %d — Weight*%d ring points are allocated and sorted on every request, for a share no ratio expresses",
				spec, shards["box/big"], weightedRingVpointsPerUnit)
		}
	}
	// The weights a plan actually uses still work.
	shards, rest := extractShardFlags([]string{"--shard", " box/glm-4.6 : 3 ", "--shard", "zai/glm-4.5-air:2", "run"})
	if shards["box/glm-4.6"] != 3 || shards["zai/glm-4.5-air"] != 2 {
		t.Errorf("ordinary weights were dropped: %v (rest=%v)", shards, rest)
	}
	if shards["box/glm-4.6"] > maxShardWeight {
		t.Errorf("a weight at the documented maximum was rejected")
	}
}

// The ring itself is bounded whatever weights reach it, and the heavy leg
// keeps the bulk of it.
func TestTheRingIsBoundedWhateverTheWeights(t *testing.T) {
	light := proxyRoute{BaseURL: "https://a.example/v1", UpstreamModel: "a", Weight: 1}
	heavy := proxyRoute{BaseURL: "https://b.example/v1", UpstreamModel: "b", Weight: 1000}

	ring := weightedRing([]proxyRoute{light, heavy})
	if len(ring) > maxRingPoints {
		t.Fatalf("a %d:%d weight split produced a ring of %d points, want at most %d — this is built and sorted on the request path, so its size is the memory one request can force",
			heavy.Weight, light.Weight, len(ring), maxRingPoints)
	}
	count := map[string]int{}
	for _, p := range ring {
		count[p.route.BaseURL]++
	}
	if count[light.BaseURL] < 1 || count[heavy.BaseURL] < 1 {
		t.Fatalf("a leg was dropped from the ring entirely: %v", count)
	}
	if count[heavy.BaseURL] <= count[light.BaseURL] {
		t.Errorf("the ring does not follow the weights: %d points for the weight-%d leg vs %d for the weight-%d leg",
			count[heavy.BaseURL], heavy.Weight, count[light.BaseURL], light.Weight)
	}
}

// Control: an ordinary split keeps its full granularity, so the bound cannot
// be satisfied by shrinking every ring to one point per leg.
func TestAnOrdinarySplitKeepsItsGranularity(t *testing.T) {
	a := proxyRoute{BaseURL: "https://a.example/v1", UpstreamModel: "a", Weight: 3}
	b := proxyRoute{BaseURL: "https://b.example/v1", UpstreamModel: "b", Weight: 1}

	ring := weightedRing([]proxyRoute{a, b})
	if len(ring) != 4*weightedRingVpointsPerUnit {
		t.Errorf("a 3:1 split produced %d points, want %d — weightedPick's split accuracy depends on the point count (see weightedRingVpointsPerUnit)",
			len(ring), 4*weightedRingVpointsPerUnit)
	}
	count := map[string]int{}
	for _, p := range ring {
		count[p.route.BaseURL]++
	}
	if count[a.BaseURL] != 3*weightedRingVpointsPerUnit || count[b.BaseURL] != weightedRingVpointsPerUnit {
		t.Errorf("the 3:1 split came out %d:%d", count[a.BaseURL], count[b.BaseURL])
	}
}
