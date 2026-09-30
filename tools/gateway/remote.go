package main

// remote.go — the two bridges from this gateway to the oaica-saas Stripe side: a licence key for licensed
// weights (pull_license_validate_url) and a subscriber's API key (api_key_validate_url). Both are "ask the saas,
// remember the answer briefly", so they share one cache with the properties the audit asked for (round 133,
// F133-L3-3): concurrent lookups of the same key make ONE call, outbound calls are capped, only a DEFINITE answer
// is cached (a 429/5xx/timeout is refused but never remembered), the cache evicts entry by entry, and the call
// is bound to the request's context.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// The exact shapes oaica-saas mints (license.ts / keys.ts): anything else is refused without a network call.
var (
	licenceKeyShape = regexp.MustCompile(`^oaica-lic-[0-9a-f]{32}$`)
	apiKeyShape     = regexp.MustCompile(`^oaica-sk-[0-9a-f]{48}$`)
)

type remoteResult struct {
	ok    bool
	label string
	// unavailable: no clear answer came back (timeout, 429/5xx, no slot). Not a verdict on the key.
	unavailable bool
}

type remoteEntry struct {
	res     remoteResult
	expires time.Time
	// staleUntil: a VALID answer keeps serving past `expires` until this time while a refresh runs in the
	// background or the saas is unreachable (round 136, F136-L3-1/2).
	staleUntil time.Time
}

type remoteFlight struct {
	done      chan struct{}
	abandoned chan struct{} // closed when the last waiter has left
	waiters   int
	gone      bool
	res       remoteResult
}

type remoteCache struct {
	mu       sync.Mutex
	m        map[string]remoteEntry
	inflight map[string]*remoteFlight
	sem      chan struct{}
}

const (
	remoteValidTTL   = 60 * time.Second // a cancellation or refund reaches the gateway within a minute
	remoteStaleTTL   = 15 * time.Minute // how long a once-valid answer may keep serving when the saas cannot be asked
	remoteInvalidTTL = 30 * time.Second
	remoteMaxEntries = 4096
	remoteMaxCalls   = 4
)

var remoteTimeout = 4 * time.Second

// remoteSlotWait: how long a lookup waits for one of the remoteMaxCalls slots before it is refused (not cached).
var remoteSlotWait = 3 * time.Second

func (c *remoteCache) init() {
	if c.m == nil {
		c.m = make(map[string]remoteEntry)
		c.inflight = make(map[string]*remoteFlight)
		c.sem = make(chan struct{}, remoteMaxCalls)
	}
}

// check returns the cached answer for id, or runs fn once for every concurrent caller of the same id. fn returns
// (result, definite): only a definite answer is remembered.
//
// A VALID answer that has expired is still served (up to remoteStaleTTL) while a refresh runs in the background,
// and when the refresh gets no clear answer: a subscriber must not wait seconds behind a flood of junk keys or get
// a 401 because the saas is down for a minute (2026-09-30 audit, round 136, F136-L3-1/2). A definite "invalid"
// answer replaces it at once, so a cancellation still lands within a refresh.
func (c *remoteCache) check(ctx context.Context, idSource string, fn func(context.Context) (remoteResult, bool)) remoteResult {
	sum := sha256.Sum256([]byte(idSource))
	id := hex.EncodeToString(sum[:])
	now := time.Now()
	c.mu.Lock()
	c.init()
	e, have := c.m[id]
	if have && now.Before(e.expires) {
		c.mu.Unlock()
		return e.res
	}
	if have && e.res.ok && now.Before(e.staleUntil) {
		// NOT subject to the in-flight cap: a refresh exists only for an entry already cached as valid, which an
		// attacker cannot create, and skipping it under a flood of junk lookups left a revoked key served for the
		// whole stale window (2026-09-30 audit, round 137, F137-A-1).
		if f, ok := c.inflight[id]; !ok || f.gone {
			f := &remoteFlight{done: make(chan struct{}), abandoned: make(chan struct{})}
			c.inflight[id] = f
			go c.run(id, f, fn)
		}
		c.mu.Unlock()
		return e.res
	}
	if f, ok := c.inflight[id]; ok && !f.gone {
		f.waiters++
		c.mu.Unlock()
		return c.wait(ctx, f)
	}
	if len(c.inflight) >= remoteMaxEntries { // bound the goroutines an unauthenticated flood can hold
		c.mu.Unlock()
		return remoteResult{unavailable: true}
	}
	f := &remoteFlight{done: make(chan struct{}), abandoned: make(chan struct{}), waiters: 1}
	c.inflight[id] = f
	c.mu.Unlock()
	// The lookup runs on its own goroutine and its own clock: it is SHARED by every waiter of this key, so no one
	// caller's context may end it (round 134, F134-L3-2). But a lookup NOBODY is waiting for must not hold a slot
	// either: the last waiter to leave abandons the flight (round 135, F135-L3-1).
	go c.run(id, f, fn)
	return c.wait(ctx, f)
}

func (c *remoteCache) run(id string, f *remoteFlight, fn func(context.Context) (remoteResult, bool)) {
	var res remoteResult
	definite := false
	// At most remoteMaxCalls in flight. The lookup WAITS for a slot (bounded by remoteSlotWait) instead of being
	// refused at once (round 134, F134-L3-1).
	slot := time.NewTimer(remoteSlotWait)
	select {
	case c.sem <- struct{}{}:
		slot.Stop()
		callCtx, cancel := context.WithTimeout(context.Background(), remoteTimeout)
		stop := make(chan struct{})
		go func() {
			select {
			case <-f.abandoned:
				cancel()
			case <-stop:
			}
		}()
		res, definite = fn(callCtx)
		close(stop)
		cancel()
		<-c.sem
	case <-f.abandoned:
		slot.Stop()
	case <-slot.C:
	}
	now := time.Now()
	c.mu.Lock()
	if definite {
		// Only an insert that GROWS the map needs room: a refresh replaces an existing entry (F138-A-2).
		if _, exists := c.m[id]; !exists && len(c.m) >= remoteMaxEntries {
			c.evictLocked(now)
		}
		ttl := remoteInvalidTTL
		ent := remoteEntry{res: res}
		if res.ok {
			ttl = remoteValidTTL
			ent.staleUntil = now.Add(remoteStaleTTL)
		}
		ent.expires = now.Add(ttl)
		c.m[id] = ent
	} else {
		res.unavailable = true
		// No clear answer: a once-valid key keeps serving (and is retried shortly) instead of turning into a 401.
		if e, ok := c.m[id]; ok && e.res.ok && now.Before(e.staleUntil) {
			e.expires = now.Add(10 * time.Second)
			c.m[id] = e
			res = e.res
		}
	}
	if c.inflight[id] == f {
		delete(c.inflight, id)
	}
	f.res = res
	close(f.done)
	c.mu.Unlock()
}

// wait blocks for the flight's answer or the caller's own context; the last caller to give up abandons the flight.
func (c *remoteCache) wait(ctx context.Context, f *remoteFlight) remoteResult {
	select {
	case <-f.done:
		return f.res
	case <-ctx.Done():
		c.mu.Lock()
		f.waiters--
		if f.waiters == 0 && !f.gone {
			f.gone = true
			close(f.abandoned)
		}
		c.mu.Unlock()
		return remoteResult{unavailable: true}
	}
}

// evictLocked frees room one entry at a time: expired first, then refused answers, then any — a flood of junk
// keys cannot push a paying customer's cached answer out (they are refused answers).
func (c *remoteCache) evictLocked(now time.Time) {
	for k, e := range c.m {
		if !now.Before(e.expires) && !(e.res.ok && now.Before(e.staleUntil)) {
			delete(c.m, k)
		}
	}
	if len(c.m) < remoteMaxEntries {
		return
	}
	for k, e := range c.m {
		if !e.res.ok {
			delete(c.m, k)
			if len(c.m) < remoteMaxEntries*3/4 {
				return
			}
		}
	}
	// Refused answers freed the room that was needed: valid subscribers are never touched while there is any
	// (a flood of junk keys dropped one about every 600 keys) (round 138, F138-A-1).
	if len(c.m) < remoteMaxEntries {
		return
	}
	// Last resort (the cache is full of valid entries): drop only a few, not a quarter of the subscribers
	// (round 137, F137-A-3).
	for k := range c.m {
		delete(c.m, k)
		if len(c.m) < remoteMaxEntries-64 {
			return
		}
	}
}

var remoteClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// postForm posts a form and decodes a bounded JSON answer. definite is false for anything that is not a clean
// 200 with a JSON body (timeout, 429, 5xx, a challenge page): the caller refuses but does not remember it.
func postForm(ctx context.Context, target, bearer string, form url.Values, out any) (definite bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := remoteClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return false
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(out) == nil
}

func callLicenseValidate(ctx context.Context, validateURL, key string) (remoteResult, bool) {
	var out struct {
		Valid bool `json:"valid"`
		Meta  struct {
			Product string `json:"product"`
		} `json:"meta"`
	}
	if !postForm(ctx, validateURL, "", url.Values{"license_key": {key}}, &out) {
		return remoteResult{}, false
	}
	return remoteResult{ok: out.Valid && out.Meta.Product == "oaica-code"}, true
}

// callAPIKeyValidate asks oaica-saas whether an `oaica-sk-…` key belongs to a live subscription, and under what
// gateway label (the label meterhub keys its subscription row on).
func callAPIKeyValidate(ctx context.Context, validateURL, token, key string) (remoteResult, bool) {
	var out struct {
		Valid bool   `json:"valid"`
		Label string `json:"label"`
	}
	if !postForm(ctx, validateURL, token, url.Values{"api_key": {key}}, &out) {
		return remoteResult{}, false
	}
	return remoteResult{ok: out.Valid && out.Label != "", label: out.Label}, true
}
