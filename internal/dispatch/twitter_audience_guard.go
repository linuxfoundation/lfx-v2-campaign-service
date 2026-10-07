// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
)

// twitterAudienceGuard bounds what X audience reads cost an ad account, process-wide.
//
// WHY. Each read creates up to six asynchronous stats jobs on the account, and X allows 100
// concurrent jobs per ACCOUNT. The account is shared: every foundation on the LF system account,
// and the X account monitor (which needs job slots of its own), draw on the same 100. A read that
// times out leaves its jobs running until X expires them, so a user refreshing a slow panel could
// otherwise pile up abandoned jobs until the account monitor — for every foundation on the
// account — cannot create any. Each read also holds the client's 1-write/sec pacer for a few
// seconds. So, keyed by account:
//
//   - identical concurrent reads (same account, window and scope) share ONE set of jobs
//     (singleflight); a joiner whose context ends first gets its own context error, and a joiner
//     whose LEADER's context ended re-leads rather than inheriting that error (read);
//   - at most twitterAudienceAccountConcurrency reads run per account at once; a waiter gives up
//     when its context ends (503 at the service) rather than queueing jobs it cannot collect;
//   - a SUCCESSFUL result is reused for twitterAudienceCacheTTL, while the window it was computed
//     for still names the same instants on the account's calendar (so "today" is never served
//     across the account's midnight), so refreshes create no jobs. Failures are never cached.
//
// Keys include the sorted, de-duplicated scope, so one project's result can only be reused for a
// request over exactly the same campaigns on the same account — the cache never widens a scope.
type twitterAudienceGuard struct {
	mu       sync.Mutex
	cache    map[string]twitterAudienceCached
	inflight map[string]*twitterAudienceCall
	slots    map[string]chan struct{}
	// onJoin, when set, is called (outside mu) each time a caller joins an in-flight read. Test
	// hook only.
	onJoin func()
}

const (
	twitterAudienceCacheTTL           = 5 * time.Minute
	twitterAudienceCacheMax           = 256
	twitterAudienceAccountConcurrency = 1
)

type twitterAudienceCached struct {
	ai       *twitter.AudienceInsights
	storedAt time.Time
}

type twitterAudienceCall struct {
	done chan struct{}
	ai   *twitter.AudienceInsights
	err  error
}

func newTwitterAudienceGuard() *twitterAudienceGuard {
	return &twitterAudienceGuard{
		cache:    map[string]twitterAudienceCached{},
		inflight: map[string]*twitterAudienceCall{},
		slots:    map[string]chan struct{}{},
	}
}

// twitterAudienceKey is account, window and the sorted, de-duplicated scope.
func twitterAudienceKey(accountID string, window twitter.MetricsWindow, campaignIDs []string) string {
	ids := append([]string(nil), campaignIDs...)
	sort.Strings(ids)
	uniq := ids[:0]
	for i, id := range ids {
		if i == 0 || id != ids[i-1] {
			uniq = append(uniq, id)
		}
	}
	return accountID + "\x00" + string(window) + "\x00" + strings.Join(uniq, ",")
}

// fresh reports whether a cached result may still be served at now: inside the TTL, and its
// window still resolving to the same instants on the account's calendar.
func (c twitterAudienceCached) fresh(now time.Time) bool {
	if c.ai == nil || c.ai.Location == nil || now.Sub(c.storedAt) >= twitterAudienceCacheTTL || now.Before(c.storedAt) {
		return false
	}
	start, end, err := twitter.AudienceWindowBounds(c.ai.Window, now, c.ai.Location)
	return err == nil && start.Equal(c.ai.WindowStart) && end.Equal(c.ai.WindowEnd)
}

// twitterAudienceMaxReLeads bounds how many times one caller takes over as leader after the
// read it joined ended with its LEADER's context (see read). Two re-leads is enough for an
// ordinary run of disconnecting clients and guarantees termination however many arrive.
const twitterAudienceMaxReLeads = 2

// leaderContextError tags a shared result that failed only because the LEADING caller's own
// context ended (client disconnect, the leader's deadline) — a fact about that one caller, not
// about X. Joiners re-lead on it; every other failure (an upstream 5xx, an X-side timeout while
// the leader's context was still live, a malformed file) is shared as is, never retried.
type leaderContextError struct{ err error }

func (e *leaderContextError) Error() string { return e.err.Error() }
func (e *leaderContextError) Unwrap() error { return e.err }

// read serves key from the cache, joins an identical in-flight read, or runs fetch under the
// account's concurrency slot. The returned result is shared and must be treated as read-only.
//
// A joiner shares the leader's outcome, with ONE exception: when the leader failed because ITS
// context ended (leaderContextError) while the joiner's own context is live, the joiner loops and
// starts the read again — as the new leader, or by joining whoever did. Without that, one client
// disconnecting mid-read turned every concurrent identical read into a 503 carrying that client's
// context.Canceled. At most twitterAudienceMaxReLeads times; a joiner whose own context ends gets
// its own error.
func (g *twitterAudienceGuard) read(ctx context.Context, accountID, key string, now func() time.Time,
	fetch func(context.Context) (*twitter.AudienceInsights, error)) (*twitter.AudienceInsights, error) {
	for reLeads := 0; ; reLeads++ {
		ai, joined, err := g.readOnce(ctx, accountID, key, now, fetch)
		var lce *leaderContextError
		if joined && errors.As(err, &lce) && ctx.Err() == nil && reLeads < twitterAudienceMaxReLeads {
			continue
		}
		return ai, err
	}
}

// readOnce is one pass of read. joined reports whether the outcome is another caller's.
func (g *twitterAudienceGuard) readOnce(ctx context.Context, accountID, key string, now func() time.Time,
	fetch func(context.Context) (*twitter.AudienceInsights, error)) (ai *twitter.AudienceInsights, joined bool, err error) {
	g.mu.Lock()
	if c, ok := g.cache[key]; ok {
		if c.fresh(now()) {
			g.mu.Unlock()
			return c.ai, false, nil
		}
		delete(g.cache, key)
	}
	if call, ok := g.inflight[key]; ok {
		g.mu.Unlock()
		if g.onJoin != nil {
			g.onJoin()
		}
		select {
		case <-call.done:
			return call.ai, true, call.err
		case <-ctx.Done():
			return nil, false, fmt.Errorf("x audience read: waiting for an identical read in flight: %w", ctx.Err())
		}
	}
	call := &twitterAudienceCall{done: make(chan struct{})}
	g.inflight[key] = call
	slot, ok := g.slots[accountID]
	if !ok {
		slot = make(chan struct{}, twitterAudienceAccountConcurrency)
		g.slots[accountID] = slot
	}
	g.mu.Unlock()

	defer func() {
		// Tag a failure caused by THIS caller's context, so joiners can tell it from X's.
		if call.err != nil && ctx.Err() != nil {
			call.err = &leaderContextError{err: call.err}
		}
		g.mu.Lock()
		delete(g.inflight, key)
		if call.err == nil && call.ai != nil {
			g.store(key, call.ai, now())
		}
		g.mu.Unlock()
		close(call.done)
	}()

	select {
	case slot <- struct{}{}:
	case <-ctx.Done():
		call.err = fmt.Errorf("x audience read: another audience read on this x ads account is still running: %w", ctx.Err())
		return nil, false, call.err
	}
	defer func() { <-slot }()
	call.ai, call.err = fetch(ctx)
	// The leader's own error is returned untagged (the tag is added in the deferred close, after
	// these values are taken), so a caller never sees leaderContextError for its own context.
	return call.ai, false, call.err
}

// store caches ai under key, first dropping expired entries and, at the size bound, the oldest.
// Caller holds mu.
func (g *twitterAudienceGuard) store(key string, ai *twitter.AudienceInsights, now time.Time) {
	for k, c := range g.cache {
		if now.Sub(c.storedAt) >= twitterAudienceCacheTTL {
			delete(g.cache, k)
		}
	}
	for len(g.cache) >= twitterAudienceCacheMax {
		oldest, oldestAt := "", time.Time{}
		for k, c := range g.cache {
			if oldest == "" || c.storedAt.Before(oldestAt) {
				oldest, oldestAt = k, c.storedAt
			}
		}
		delete(g.cache, oldest)
	}
	g.cache[key] = twitterAudienceCached{ai: ai, storedAt: now}
}
