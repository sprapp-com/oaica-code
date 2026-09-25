package launch

// remote_sweep_cache_key_test.go — the key of a remote's swept /v1/models
// cache must identify the ENDPOINT it was swept from (round-5 audit,
// 2026-09-26).
//
// ~/.oaica/cache/models/remote-<...>.json holds one remote's /v1/models answer
// for remoteModelsCacheTTL (10 min). It used to be keyed on the remote's NAME
// with every character outside [A-Za-z0-9-_.] rewritten to "_", validated by a
// TTL and nothing else — two defects from one key:
//
//   - `oaica remote add` is documented as "add or replace", so repointing a
//     box at a new host (moved, or a mirror replacing a dead origin) kept
//     serving the OLD host's rows for the rest of the window, while
//     resolveRemoteEndpoint resolves the row against the CURRENT base_url:
//     selecting "<remote>/<old-model>" sent an id the new host has never heard
//     of to the new host.
//   - the sanitizer is lossy, so two legal remote names ("my box", "my_box")
//     collided on ONE file and each was served the other's model list.
//
// The picker cache's own contract is the opposite — a remotes.json edit voids
// it — and that part works; the control below shows it, isolating the sweep
// cache behind it.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// sweepTestModelHost is one fake remote box: it answers /v1/models with a fixed
// id list and counts how many times it was asked.
func sweepTestModelHost(t *testing.T, ids ...string) (*httptest.Server, *int) {
	t.Helper()
	reqs := new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		*reqs++
		parts := ""
		for i, id := range ids {
			if i > 0 {
				parts += ","
			}
			parts += fmt.Sprintf(`{"id":%q}`, id)
		}
		fmt.Fprintf(w, `{"data":[%s]}`, parts)
	}))
	t.Cleanup(srv.Close)
	return srv, reqs
}

// rowsWithPrefix lists the model ids shown under one "<remote>/" prefix, in
// row order.
func rowsWithPrefix(models []LaunchModel, prefix string) []string {
	out := []string{}
	for _, m := range models {
		if len(m.Name) > len(prefix) && m.Name[:len(prefix)] == prefix {
			out = append(out, m.Name[len(prefix):])
		}
	}
	return out
}

func TestRepointedRemoteResweepsItsNewHost(t *testing.T) {
	withTempOaicaHome(t)
	t.Setenv("OAICA_HOST", "http://127.0.0.1:1") // pinned: no :local / ollama-cloud merge
	t.Setenv("OAICA_API_KEY", "")
	stubCloudFetch(t, []oaicaModelEntry{{ID: "router-row"}}, nil)

	srvA, reqsA := sweepTestModelHost(t, "a-model")
	srvB, reqsB := sweepTestModelHost(t, "b-model")

	// `oaica remote add box <url>` — the box on the LAN.
	writeRemotes(t, fmt.Sprintf(`{"remotes":[{"name":"box","base_url":%q}]}`, srvA.URL+"/v1"))

	first, err := newModelInventory(nil).Load(context.Background())
	if err != nil {
		t.Fatalf("first Load: %v", err)
	}
	if !invHas(first, "box/a-model") {
		t.Fatalf("premise changed: the sweep did not produce box/a-model: %v (A asked %d time(s))", invNames(first), *reqsA)
	}

	// The user repoints the SAME remote name at a different host.
	writeRemotes(t, fmt.Sprintf(`{"remotes":[{"name":"box","base_url":%q}]}`, srvB.URL+"/v1"))

	// CONTROL: the edit really is a configuration change as far as the picker
	// cache is concerned — remotes.json is fingerprinted by content, so the
	// cache written a moment ago no longer validates. That isolates the
	// per-remote sweep cache sitting behind it.
	if _, _, ok := loadPickerCache(); ok {
		t.Fatalf("premise changed: the picker cache still validates after remotes.json was rewritten, so this test " +
			"would not isolate the sweep cache")
	}

	*reqsA, *reqsB = 0, 0
	second, err := newModelInventory(nil).Load(context.Background())
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}

	if !invHas(second, "box/b-model") || invHas(second, "box/a-model") {
		ep, _ := resolveRemoteEndpoint("box/a-model")
		t.Errorf("remote \"box\" now points at host B (%s), which serves b-model only, but the picker shows %v "+
			"(host B was asked %d time(s), cached list %s). The sweep cache is keyed by the remote's NAME alone, so "+
			"the base_url change is invisible to it for the rest of its TTL; selecting the stale row routes to %s "+
			"with upstream model %q, which that host does not serve",
			srvB.URL+"/v1", invNames(second), *reqsB, invPickerCachePath(t), ep.BaseURL, ep.UpstreamModel)
	}
}

// Two remote names the CLI accepts must not share one cache file. RemoteAdd
// rejects only an empty name and "/", so "my box" and "my_box" are both
// reachable without hand-editing remotes.json.
func TestDistinctRemoteNamesDoNotShareASweepCacheFile(t *testing.T) {
	withTempOaicaHome(t)
	t.Setenv("OAICA_HOST", "http://127.0.0.1:1")
	t.Setenv("OAICA_API_KEY", "")
	stubCloudFetch(t, []oaicaModelEntry{{ID: "router-row"}}, nil)

	srvA, _ := sweepTestModelHost(t, "only-on-a")
	srvB, _ := sweepTestModelHost(t, "only-on-b")
	writeRemotes(t, fmt.Sprintf(
		`{"remotes":[{"name":"a b","base_url":%q},{"name":"a_b","base_url":%q}]}`,
		srvA.URL+"/v1", srvB.URL+"/v1"))

	first, err := newModelInventory(nil).Load(context.Background())
	if err != nil {
		t.Fatalf("first Load: %v", err)
	}
	if !invHas(first, "a b/only-on-a") || !invHas(first, "a_b/only-on-b") {
		t.Fatalf("premise changed: the live sweep should report each host's own list: %v", invNames(first))
	}

	// The key itself: two endpoints, two files.
	pA, err := remoteModelsCachePath("a b", srvA.URL+"/v1")
	if err != nil {
		t.Fatal(err)
	}
	pB, err := remoteModelsCachePath("a_b", srvB.URL+"/v1")
	if err != nil {
		t.Fatal(err)
	}
	if pA == pB {
		t.Errorf("remotes %q and %q share one sweep-cache file (%q): the sanitizer maps every character outside "+
			"[A-Za-z0-9-_.] to \"_\", so whichever remote is swept first populates the file and the other is served "+
			"from it — one host's model list appears under the other's prefix, and picking such a row sends that "+
			"model id to the wrong backend", "a b", "a_b", pA)
	}

	// Nothing changed in the configuration: same HOME, same remotes.json, same
	// hosts. A forced load bypasses the picker cache and reads the per-remote
	// sweep caches underneath it.
	second, err := newModelInventory(nil).Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	gotA, gotB := rowsWithPrefix(second, "a b/"), rowsWithPrefix(second, "a_b/")
	if fmt.Sprint(gotA) != fmt.Sprint([]string{"only-on-a"}) || fmt.Sprint(gotB) != fmt.Sprint([]string{"only-on-b"}) {
		t.Errorf("from the sweep caches: %q -> %v ; %q -> %v, want [only-on-a] and [only-on-b] — each remote's "+
			"cached list must come from its own endpoint", "a b", gotA, "a_b", gotB)
	}
}
