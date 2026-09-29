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
}

type remoteEntry struct {
	res     remoteResult
	expires time.Time
}

type remoteFlight struct {
	done chan struct{}
	res  remoteResult
}

type remoteCache struct {
	mu       sync.Mutex
	m        map[string]remoteEntry
	inflight map[string]*remoteFlight
	sem      chan struct{}
}

const (
	remoteValidTTL   = 60 * time.Second // a cancellation or refund reaches the gateway within a minute
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
func (c *remoteCache) check(ctx context.Context, idSource string, fn func(context.Context) (remoteResult, bool)) remoteResult {
	sum := sha256.Sum256([]byte(idSource))
	id := hex.EncodeToString(sum[:])
	now := time.Now()
	c.mu.Lock()
	c.init()
	if e, ok := c.m[id]; ok && now.Before(e.expires) {
		c.mu.Unlock()
		return e.res
	}
	if f, ok := c.inflight[id]; ok {
		c.mu.Unlock()
		select {
		case <-f.done:
			return f.res
		case <-ctx.Done():
			return remoteResult{}
		}
	}
	f := &remoteFlight{done: make(chan struct{})}
	c.inflight[id] = f
	c.mu.Unlock()

	// The lookup runs on its own goroutine and its own clock: it is SHARED by every waiter of this key, so no one
	// caller's context may end it. Each caller — the first included — stops WAITING on its own context, and the
	// lookup finishes for the others (2026-09-30 audit, round 134, F134-L3-2; the request-bound rule of round 133
	// still holds for every caller's wait).
	go func() {
		var res remoteResult
		definite := false
		// At most remoteMaxCalls in flight. The lookup WAITS for a slot (bounded by remoteSlotWait) instead of being
		// refused at once: refusing let four unauthenticated junk keys lock every paying subscriber out, and five
		// subscribers re-validating together refused the fifth (F134-L3-1).
		slot := time.NewTimer(remoteSlotWait)
		select {
		case c.sem <- struct{}{}:
			slot.Stop()
			callCtx, cancel := context.WithTimeout(context.Background(), remoteTimeout)
			res, definite = fn(callCtx)
			cancel()
			<-c.sem
		case <-slot.C:
		}
		c.mu.Lock()
		if definite {
			if len(c.m) >= remoteMaxEntries {
				c.evictLocked(now)
			}
			ttl := remoteInvalidTTL
			if res.ok {
				ttl = remoteValidTTL
			}
			c.m[id] = remoteEntry{res: res, expires: time.Now().Add(ttl)}
		}
		delete(c.inflight, id)
		f.res = res
		close(f.done)
		c.mu.Unlock()
	}()

	select {
	case <-f.done:
		return f.res
	case <-ctx.Done():
		return remoteResult{}
	}
}

// evictLocked frees room one entry at a time: expired first, then refused answers, then any — a flood of junk
// keys cannot push a paying customer's cached answer out (they are refused answers).
func (c *remoteCache) evictLocked(now time.Time) {
	for k, e := range c.m {
		if !now.Before(e.expires) {
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
	for k := range c.m {
		delete(c.m, k)
		if len(c.m) < remoteMaxEntries*3/4 {
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
