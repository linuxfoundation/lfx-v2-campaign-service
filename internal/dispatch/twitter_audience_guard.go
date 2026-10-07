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
//
//   - at most twitterAudienceAccountConcurrency reads run per account at once; a waiter gives up
//     when its context ends (503 at the service) rather than queueing jobs it cannot collect;
//
//   - a SUCCESSFUL result is reused for twitterAudienceCacheTTL, while the window it was computed
//     for still names the same instants on the account's calendar (so "today" is never served
//     across the account's midnight), so refreshes create no jobs. Failures are never cached.
//
//   - jobs a FAILED read left running on X (twitter.AudienceJobsAbandonedError) are counted
//     against the account until X reports them finished or twitterAudienceAbandonedJobHold
//     passes, and a read whose own jobs would take the account's count past
//     twitterAudienceOutstandingJobBudget is refused (503) before it creates any — so refreshing
//     a slow panel cannot pile abandoned jobs toward X's 100-per-account limit. The budget (12:
//     two full reads) is deliberately far below 100, leaving the rest as headroom for the
//     account monitor, which needs up to 10 jobs per submission;
//
//   - a joined result is returned only if its window still names the joiner's account-local
//     instants (a "today" read just after the account's midnight never takes yesterday's
//     buckets from a read that started before it).
//
// Per-account state (the slot, the abandoned jobs) is dropped when the account is idle, so it
// does not grow with the number of accounts ever read.
//
// Keys include the sorted, de-duplicated scope, so one project's result can only be reused for a
// request over exactly the same campaigns on the same account — the cache never widens a scope.
type twitterAudienceGuard struct {
	mu       sync.Mutex
	cache    map[string]twitterAudienceCached
	inflight map[string]*twitterAudienceCall
	slots    map[string]*twitterAudienceSlot
	// abandoned is, per account, the stats jobs failed reads left running on X.
	abandoned map[string][]twitterAudienceAbandonedJob
	// onJoin, when set, is called (outside mu) each time a caller joins an in-flight read. Test
	// hook only.
	onJoin func()
}

const (
	twitterAudienceCacheTTL           = 5 * time.Minute
	twitterAudienceCacheMax           = 256
	twitterAudienceAccountConcurrency = 1
	// twitterAudienceOutstandingJobBudget is the most audience stats jobs one account may have
	// outstanding (abandoned plus the read about to run): two full six-job reads, a small share of
	// X's 100 concurrent jobs per account, which the account monitor shares.
	twitterAudienceOutstandingJobBudget = 12
	// twitterAudienceAbandonedJobHold is how long an abandoned job is counted when X cannot be
	// asked about it. X documents no job lifetime; this is the bound the account monitor already
	// applies to a stats job that never finishes (service.accountReportAbandonAfter, 60 minutes).
	twitterAudienceAbandonedJobHold = 60 * time.Minute
)

// twitterAudienceSlot is one account's concurrency slot and how many callers hold or await it.
type twitterAudienceSlot struct {
	ch    chan struct{}
	users int
}

type twitterAudienceAbandonedJob struct {
	id    string
	until time.Time
}

// twitterAudienceRunningFn asks X which of ids are still running (twitter.Client.RunningStatsJobs).
type twitterAudienceRunningFn func(ctx context.Context, ids []string) ([]string, error)

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
		cache:     map[string]twitterAudienceCached{},
		inflight:  map[string]*twitterAudienceCall{},
		slots:     map[string]*twitterAudienceSlot{},
		abandoned: map[string][]twitterAudienceAbandonedJob{},
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
// window still current (windowCurrent).
func (c twitterAudienceCached) fresh(now time.Time) bool {
	if now.Sub(c.storedAt) >= twitterAudienceCacheTTL || now.Before(c.storedAt) {
		return false
	}
	return windowCurrent(c.ai, now)
}

// windowCurrent reports whether ai's window, asked again at now, still resolves to the instants
// ai covers on the account's calendar — false across the account's midnight.
func windowCurrent(ai *twitter.AudienceInsights, now time.Time) bool {
	if ai == nil || ai.Location == nil {
		return false
	}
	start, end, err := twitter.AudienceWindowBounds(ai.Window, now, ai.Location)
	return err == nil && start.Equal(ai.WindowStart) && end.Equal(ai.WindowEnd)
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
//
// A joined SUCCESS is also re-led when its window no longer names the joiner's instants
// (windowCurrent): a read that started before the account's midnight must not answer a "today"
// that arrived after it. Same bound.
//
// planned is how many stats jobs fetch will create; running asks X which jobs are still running.
// Both feed the outstanding-job budget (admitJobs).
func (g *twitterAudienceGuard) read(ctx context.Context, accountID, key string, planned int, now func() time.Time,
	running twitterAudienceRunningFn, fetch func(context.Context) (*twitter.AudienceInsights, error)) (*twitter.AudienceInsights, error) {
	for reLeads := 0; ; reLeads++ {
		ai, joined, err := g.readOnce(ctx, accountID, key, planned, now, running, fetch)
		if joined && ctx.Err() == nil && reLeads < twitterAudienceMaxReLeads {
			var lce *leaderContextError
			if errors.As(err, &lce) || (err == nil && !windowCurrent(ai, now())) {
				continue
			}
		}
		return ai, err
	}
}

// readOnce is one pass of read. joined reports whether the outcome is another caller's.
func (g *twitterAudienceGuard) readOnce(ctx context.Context, accountID, key string, planned int, now func() time.Time,
	running twitterAudienceRunningFn, fetch func(context.Context) (*twitter.AudienceInsights, error)) (ai *twitter.AudienceInsights, joined bool, err error) {
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
		slot = &twitterAudienceSlot{ch: make(chan struct{}, twitterAudienceAccountConcurrency)}
		g.slots[accountID] = slot
	}
	slot.users++
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
		// Drop the account's slot once nobody holds or awaits it, so per-account state does not
		// grow with every account ever read.
		if slot.users--; slot.users == 0 {
			delete(g.slots, accountID)
		}
		g.mu.Unlock()
		close(call.done)
	}()

	select {
	case slot.ch <- struct{}{}:
	case <-ctx.Done():
		call.err = fmt.Errorf("x audience read: another audience read on this x ads account is still running: %w", ctx.Err())
		return nil, false, call.err
	}
	defer func() { <-slot.ch }()
	if aerr := g.admitJobs(ctx, accountID, planned, now, running); aerr != nil {
		call.err = aerr
		return nil, false, call.err
	}
	call.ai, call.err = fetch(ctx)
	var ab *twitter.AudienceJobsAbandonedError
	if errors.As(call.err, &ab) {
		g.recordAbandoned(accountID, ab.JobIDs, now().Add(twitterAudienceAbandonedJobHold))
	}
	// The leader's own error is returned untagged (the tag is added in the deferred close, after
	// these values are taken), so a caller never sees leaderContextError for its own context.
	return call.ai, false, call.err
}

// admitJobs refuses a read whose planned jobs would take the account's outstanding audience jobs
// past twitterAudienceOutstandingJobBudget. Called by the slot holder only, so no other audience
// read on the account changes the count meanwhile. Expired entries are dropped first; if the read
// would still not fit, X is asked which abandoned jobs are still running (one status read) and the
// finished ones are released. When X cannot answer, every unexpired job keeps counting.
func (g *twitterAudienceGuard) admitJobs(ctx context.Context, accountID string, planned int, now func() time.Time, running twitterAudienceRunningFn) error {
	g.mu.Lock()
	t := now()
	kept := g.abandoned[accountID][:0]
	for _, j := range g.abandoned[accountID] {
		if t.Before(j.until) {
			kept = append(kept, j)
		}
	}
	g.setAbandoned(accountID, kept)
	ids := make([]string, 0, len(kept))
	for _, j := range kept {
		ids = append(ids, j.id)
	}
	g.mu.Unlock()
	if len(ids)+planned <= twitterAudienceOutstandingJobBudget {
		return nil
	}
	outstanding := len(ids)
	if running != nil {
		if still, err := running(ctx, ids); err == nil {
			keep := make(map[string]bool, len(still))
			for _, id := range still {
				keep[id] = true
			}
			g.mu.Lock()
			left := g.abandoned[accountID][:0]
			for _, j := range g.abandoned[accountID] {
				if keep[j.id] {
					left = append(left, j)
				}
			}
			g.setAbandoned(accountID, left)
			outstanding = len(left)
			g.mu.Unlock()
		}
	}
	if outstanding+planned > twitterAudienceOutstandingJobBudget {
		return fmt.Errorf("x audience read: %d stats jobs left by earlier failed reads may still be running on this x ads account; this read needs %d more and the account's audience budget is %d (the rest of x's 100 concurrent jobs is kept for the account monitor) — retry later",
			outstanding, planned, twitterAudienceOutstandingJobBudget)
	}
	return nil
}

// recordAbandoned counts ids against accountID until they are reported finished or until passes.
func (g *twitterAudienceGuard) recordAbandoned(accountID string, ids []string, until time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	list := g.abandoned[accountID]
	for _, id := range ids {
		list = append(list, twitterAudienceAbandonedJob{id: id, until: until})
	}
	g.setAbandoned(accountID, list)
}

// setAbandoned stores list, deleting the account's entry when it is empty. Caller holds mu.
func (g *twitterAudienceGuard) setAbandoned(accountID string, list []twitterAudienceAbandonedJob) {
	if len(list) == 0 {
		delete(g.abandoned, accountID)
		return
	}
	g.abandoned[accountID] = list
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
