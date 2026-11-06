// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// Reddit publishes ONE rate limit for the reporting operation (POST /ad_accounts/{id}/reports):
// the `ads-reporting` policy, 60 requests per 60 seconds per ad account. Every caller of that
// operation in this service — the campaign metrics read, the account monitor, the audience read —
// spends from it, so a budget that only one of them respects is not a budget. This file is the
// ledger: EVERY HTTP attempt at the reporting operation (each page of a paginated report, and each
// 429 retry, is its own attempt) takes one slot from the ad account's ledger before it is sent
// (requestWithThrottleRetryCounted). The slot is taken as late as possible — after the token
// refresh and the request build, immediately before the send — so an attempt that fails before it
// could be sent (a failed refresh, a cancelled context) spends none.
//
// Sizing (per ad account, ACROSS EVERY REPLICA, over a sliding 60-second window):
//
//   - reportBudgetTotal (45) of Reddit's 60 for ALL report attempts. The 15 left over are headroom
//     for what this ledger cannot see: Reddit Ads Manager and other tools on the same account.
//     cmd/verify-metrics is NOT inside that headroom: an operator CLI on a read-only database
//     session, it keeps an unshared process-local ledger of its own (NewLocalReportBudgets) at
//     the full 45, so a run while the service is busy can overdraw the account — run it off-peak.
//   - reportBudgetInsights (24) of those 45 for the project-scoped insight reads (the audience and
//     keyword performance reads, which share readBreakdownReports; marked with
//     withInsightsReports). So an insight read can never take more than 24 of the 45, and the
//     metrics read and account monitor always keep at least 21.
//
// An attempt that finds no slot waits for the oldest one to expire — but only if that happens
// before its context's deadline. If it cannot, it fails at once with ErrReportBudgetExhausted rather
// than sleeping into a timeout, and nothing is sent: an insight read then fails whole (503), never
// partially.
//
// SHARED ACROSS REPLICAS (#380). The service's ledger is NewSharedReportBudgets, which records
// slots in Postgres (domain.RedditReportLedger, postgres.RedditReportLedger): one table row per
// slot, counted and inserted under a per-account transaction advisory lock, on the database's
// clock. So the account-wide total is 45 however many replicas run. It FAILS CLOSED: until the
// container binds the ledger, and whenever Postgres cannot be asked, an attempt is refused with
// ErrReportBudgetUnavailable and nothing is sent (the same unclassified 503 as an exhausted
// budget) — never sent unbudgeted. It is a sliding window of slots rather than a refilling token
// bucket on purpose: a bucket of 45 refilled at 45 a minute admits a full bucket plus a minute's
// refill — up to 90 — inside one 60-second window, past Reddit's 60. Neither shortcut considered
// before it works: a lease like the X stats jobs' makes non-owner pods refuse, which is
// intolerable on the user-facing monitor endpoint, and dividing this 45 by the replica count
// leaves less than the 24 a single audience read already needs.
//
// The process-local ledger (NewReportBudgets, NewLocalReportBudgets) remains for tests and the
// read-only operator CLI; it is exact for one process only.
const (
	reportBudgetWindow   = 60 * time.Second
	reportBudgetTotal    = 45
	reportBudgetInsights = 24
)

// ErrReportBudgetExhausted is returned, before any request is sent, when a reporting attempt
// cannot get a slot in the ad account's reporting budget before its context's deadline. Fixed
// text: no account id.
var ErrReportBudgetExhausted = errors.New("reddit-ads: the ad account's reporting request budget is exhausted for this deadline")

// ErrReportBudgetUnavailable is returned, before any request is sent, when the shared reporting
// ledger could not be asked for a slot: none is bound yet, or Postgres failed or did not answer in
// time. The attempt is refused rather than sent unbudgeted (fail closed), and maps to the same
// unclassified 503 as ErrReportBudgetExhausted. Fixed text: no account id; the cause stays wrapped
// for the log.
var ErrReportBudgetUnavailable = errors.New("reddit-ads: the shared reporting request budget is unavailable")

// reportLedgerReleaseTimeout bounds handing a shared slot back. The release runs on a context
// detached from the request's cancellation: it follows a failure, and the request may be ending.
const reportLedgerReleaseTimeout = 5 * time.Second

// reportLedgerMinWait floors the wait between two asks of the shared ledger on a full budget.
const reportLedgerMinWait = 10 * time.Millisecond

// reportLedgerMaxJitter caps the random delay added to a shared-ledger wait.
const reportLedgerMaxJitter = 50 * time.Millisecond

// jitterReportWait adds up to a tenth of d (at most reportLedgerMaxJitter) to d. Never less than
// d: waking before the slot frees would only be refused again.
func jitterReportWait(d time.Duration) time.Duration {
	j := min(d/10, reportLedgerMaxJitter)
	if j <= 0 {
		return d
	}
	return d + rand.N(j) //nolint:gosec // spreading wake-ups, not a secret
}

// reportPathRE matches the reporting operation's path (query string already split off), capturing
// the ad account id the budget is keyed by.
var reportPathRE = regexp.MustCompile(`^/ad_accounts/([A-Za-z0-9_]+)/reports$`)

type insightsReportsKey struct{}

// withInsightsReports marks ctx's report attempts as an insight read's, so they also draw on the
// insight sub-budget.
func withInsightsReports(ctx context.Context) context.Context {
	return context.WithValue(ctx, insightsReportsKey{}, true)
}

func isInsightsReports(ctx context.Context) bool {
	v, _ := ctx.Value(insightsReportsKey{}).(bool)
	return v
}

// ReportBudgets is the per-ad-account ledger of reporting attempts. The zero value is not usable.
// NewSharedReportBudgets is the service's: its slots live in a domain.RedditReportLedger every
// replica shares. NewReportBudgets and NewLocalReportBudgets keep them in this process's memory.
// A Client spends from the ledger WithReportBudgets gives it, or from defaultReportBudgets — an
// unbound shared ledger, so a client nobody gave a ledger refuses every report (fail closed).
type ReportBudgets struct {
	window          time.Duration
	total, insights int
	now             func() time.Time
	// sleep waits d or until ctx ends. Swapped in tests.
	sleep func(ctx context.Context, d time.Duration) error
	// onReserve, when set, is called after each slot is taken. Tests only: it lets a test prove a
	// slot was reserved and then released, not merely that the ledger ended empty.
	onReserve func(accountID string, insights bool)

	// shared marks a ledger built by NewSharedReportBudgets: slots are taken from the bound
	// domain.RedditReportLedger (ledger), never from byAccount, and with none bound every attempt
	// is refused (ErrReportBudgetUnavailable).
	shared bool
	ledger atomic.Pointer[boundReportLedger]

	mu        sync.Mutex
	byAccount map[string]*accountReportLedger
	// lastSweep is when sweepLocked last pruned EVERY account and dropped the empty ledgers.
	lastSweep time.Time
}

// accountReportLedger holds the send times of an account's attempts inside the window: every
// attempt in all, and the insight reads' attempts in insights (a subset).
type accountReportLedger struct {
	all, insights []time.Time
}

// boundReportLedger boxes the bound shared ledger for the atomic pointer.
type boundReportLedger struct{ l domain.RedditReportLedger }

// defaultReportBudgets is what a Client with no WithReportBudgets spends from: an UNBOUND shared
// ledger, which refuses every report. The service's clients are all built by
// dispatch.RedditDispatcher, which gives them its own shared ledger once the container binds
// Postgres to it.
var defaultReportBudgets = NewSharedReportBudgets()

// NewSharedReportBudgets builds the service's ledger at the production sizing (45 of Reddit's 60
// per account per minute, 24 of them for insight reads), its slots kept in the domain
// ledger BindLedger binds — shared by every replica. Until one is bound every attempt is refused
// with ErrReportBudgetUnavailable.
func NewSharedReportBudgets() *ReportBudgets {
	b := newReportBudgets(reportBudgetWindow, reportBudgetTotal, reportBudgetInsights, time.Now)
	b.shared = true
	return b
}

// BindLedger binds the shared ledger this budget records its slots in (a nil l is ignored).
// Only meaningful on a NewSharedReportBudgets budget; safe to call while clients spend from it.
func (b *ReportBudgets) BindLedger(l domain.RedditReportLedger) {
	if b != nil && l != nil && b.shared {
		b.ledger.Store(&boundReportLedger{l: l})
	}
}

// LedgerBound reports whether a shared ledger is bound (always false for a process-local one).
func (b *ReportBudgets) LedgerBound() bool {
	return b != nil && b.shared && b.ledger.Load() != nil
}

// NewReportBudgets builds a process-local ledger. The service never spends from one (its
// clients use the shared ledger); it exists so tests can give a client a ledger of their own
// (WithReportBudgets).
func NewReportBudgets(window time.Duration, total, insights int) *ReportBudgets {
	return newReportBudgets(window, total, insights, time.Now)
}

// NewLocalReportBudgets is a process-local ledger at the production sizing, for a process that
// cannot write the shared one: cmd/verify-metrics, whose database session is read-only. Its
// attempts are NOT counted by the shared ledger, and it is a full 45 (an audience read alone needs
// 24, more than the 15 of headroom), so it can overdraw the account while the service is also
// reading it: such a process is for off-peak operator use.
func NewLocalReportBudgets() *ReportBudgets {
	return newReportBudgets(reportBudgetWindow, reportBudgetTotal, reportBudgetInsights, time.Now)
}

func newReportBudgets(window time.Duration, total, insights int, now func() time.Time) *ReportBudgets {
	return &ReportBudgets{window: window, total: total, insights: insights, now: now, sleep: sleepCtx, byAccount: map[string]*accountReportLedger{}}
}

// acquire takes one slot for an attempt on accountID — from the account's total and, for an
// insight read, also from its insight sub-budget — waiting for one to free up only when that
// happens before ctx's deadline.
//
// A done ctx takes no slot: ctx.Err() is checked before every take — so before the first, and
// again after every wait — because the caller would not send the attempt, and a slot spent on an
// attempt that is never sent is quota the account's other readers lose for a full window.
func (b *ReportBudgets) acquire(ctx context.Context, accountID string, insights bool) error {
	_, err := b.reserve(ctx, accountID, insights)
	return err
}

// reserve is acquire that also returns a release func for the slot it took. A caller that
// proves the attempt was NOT sent after taking the slot — a pre-send dial failure: DNS, connection
// refused, no route — calls release to hand the slot back; without it an outage would burn the
// account's budget on requests that never reached Reddit. release is idempotent and is a no-op once
// the slot has aged out of the window. It must NOT be called for an attempt that may have been
// sent: Reddit counts those.
func (b *ReportBudgets) reserve(ctx context.Context, accountID string, insights bool) (release func(), err error) {
	if b.total <= 0 || (insights && b.insights <= 0) {
		return nil, ErrReportBudgetExhausted // a slot can never free up
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrReportBudgetExhausted, err)
		}
		rel, wait, ok, err := b.take(ctx, accountID, insights)
		if err != nil {
			return nil, err
		}
		if ok {
			if b.onReserve != nil {
				b.onReserve(accountID, insights)
			}
			var once sync.Once
			return func() { once.Do(rel) }, nil
		}
		if dl, has := ctx.Deadline(); has && b.now().Add(wait).After(dl) {
			return nil, ErrReportBudgetExhausted
		}
		if err := b.sleep(ctx, wait); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrReportBudgetExhausted, err)
		}
	}
}

// take makes one attempt at a slot: from the bound shared ledger, or from this process's memory.
// It returns the slot's release on success, or the wait until the earliest blocking slot expires.
// An error — the shared ledger unbound or unanswering — refuses the attempt (fail closed).
func (b *ReportBudgets) take(ctx context.Context, accountID string, insights bool) (release func(), wait time.Duration, ok bool, err error) {
	if !b.shared {
		at, wait, ok := b.tryTake(accountID, insights)
		if !ok {
			return nil, wait, false, nil
		}
		return func() { b.release(accountID, insights, at) }, 0, true, nil
	}
	bound := b.ledger.Load()
	if bound == nil {
		return nil, 0, false, fmt.Errorf("%w: no shared ledger is bound", ErrReportBudgetUnavailable)
	}
	res, err := bound.l.Take(ctx, accountID, insights, domain.RedditReportLimits{Window: b.window, Total: b.total, Insights: b.insights})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			// The caller's deadline or cancellation ended the ask: that is the budget not
			// answering in time, as for a wait that outlasts the deadline — not the ledger down.
			return nil, 0, false, fmt.Errorf("%w: %w", ErrReportBudgetExhausted, ctxErr)
		}
		// Logged here, once per refused attempt, so a Postgres outage that fails Reddit reads
		// closed reads as "the ledger is down" and not as generic Reddit errors.
		slog.WarnContext(ctx, "the shared Reddit reporting ledger could not be asked; refusing the report attempt (fail closed)", "error", err)
		return nil, 0, false, fmt.Errorf("%w: %w", ErrReportBudgetUnavailable, err)
	}
	if !res.Taken {
		// Floored, so a ledger that reports a full budget expiring "now" (clock rounding) costs
		// a short sleep before the next ask, not a hot loop against the database; and
		// jittered, so the readers one full budget refused do not all ask again in the same
		// instant the oldest slot expires.
		return nil, jitterReportWait(max(res.Wait, reportLedgerMinWait)), false, nil
	}
	id := res.SlotID
	return func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reportLedgerReleaseTimeout)
		defer cancel()
		if err := bound.l.Release(rctx, id); err != nil {
			// The slot stays recorded until it ages out: the budget over-counts by one for at
			// most a window, never under-counts.
			slog.WarnContext(ctx, "could not hand a Reddit reporting slot back to the shared ledger; it is held until it expires", "error", err)
		}
	}, 0, true, nil
}

// release removes one slot recorded at `at` from the account's ledger (and from its insight
// sub-ledger for an insight attempt). Slots recorded at the same instant are interchangeable, so
// removing any one of them is exact.
func (b *ReportBudgets) release(accountID string, insights bool, at time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	l := b.byAccount[accountID]
	if l == nil {
		return
	}
	l.all = removeOne(l.all, at)
	if insights {
		l.insights = removeOne(l.insights, at)
	}
	if len(l.all) == 0 && len(l.insights) == 0 {
		delete(b.byAccount, accountID)
	}
}

// sweepLocked bounds the map: an account's ledger is otherwise pruned only when that account
// takes a slot again, so an ad account that stops reading would keep its (expired) entry forever
// and account churn would grow the map without bound. At most once per window it prunes EVERY
// account and deletes the ledgers left empty. Cleanup is demand-driven, not timed: it runs on
// the next eligible `tryTake`, so while reporting traffic continues the map holds no account
// idle for more than two windows, and its size is bounded by the accounts read in that span,
// not by every account ever seen. If report requests stop altogether, expired ledgers stay until
// the next request triggers a sweep. O(accounts) per sweep, at most one sweep per window. b.mu
// must be held.
func (b *ReportBudgets) sweepLocked(now time.Time) {
	if now.Sub(b.lastSweep) < b.window {
		return
	}
	b.lastSweep = now
	cutoff := now.Add(-b.window)
	for id, l := range b.byAccount {
		l.all = pruneBefore(l.all, cutoff)
		l.insights = pruneBefore(l.insights, cutoff)
		if len(l.all) == 0 && len(l.insights) == 0 {
			delete(b.byAccount, id)
		}
	}
}

func removeOne(ts []time.Time, at time.Time) []time.Time {
	for i, t := range ts {
		if t.Equal(at) {
			return append(ts[:i:i], ts[i+1:]...)
		}
	}
	return ts
}

// tryTake records an attempt if both applicable limits have room (returning the time it recorded),
// or reports how long until the earliest blocking slot expires.
func (b *ReportBudgets) tryTake(accountID string, insights bool) (time.Time, time.Duration, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	b.sweepLocked(now)
	l := b.byAccount[accountID]
	if l == nil {
		l = &accountReportLedger{}
		b.byAccount[accountID] = l
	}
	cutoff := now.Add(-b.window)
	l.all = pruneBefore(l.all, cutoff)
	l.insights = pruneBefore(l.insights, cutoff)
	fullAll := len(l.all) >= b.total
	fullInsights := insights && len(l.insights) >= b.insights
	if fullAll || fullInsights {
		// Pruned entries are all inside the window, so each expiry is in the future.
		var wait time.Duration
		if fullAll {
			wait = l.all[0].Add(b.window).Sub(now)
		}
		if fullInsights {
			if w := l.insights[0].Add(b.window).Sub(now); w > wait {
				wait = w
			}
		}
		return time.Time{}, wait, false
	}
	l.all = append(l.all, now)
	if insights {
		l.insights = append(l.insights, now)
	}
	return now, 0, true
}

// used reports how many attempts the account has inside the window (all, insights). Tests only.
func (b *ReportBudgets) used(accountID string) (all, insights int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	l := b.byAccount[accountID]
	if l == nil {
		return 0, 0
	}
	cutoff := b.now().Add(-b.window)
	return len(pruneBefore(l.all, cutoff)), len(pruneBefore(l.insights, cutoff))
}

// accounts reports how many account ledgers the map holds. Tests only.
func (b *ReportBudgets) accounts() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.byAccount)
}

func pruneBefore(ts []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(ts) && !ts[i].After(cutoff) {
		i++
	}
	if i == 0 {
		return ts
	}
	return append(ts[:0:0], ts[i:]...)
}

// WithReportBudgets makes the client spend from b instead of defaultReportBudgets (an unbound
// shared ledger, which refuses every report). dispatch.RedditDispatcher passes its shared ledger
// this way; tests and cmd/verify-metrics pass a process-local one.
func WithReportBudgets(b *ReportBudgets) Option {
	return func(c *Client) { c.reportBudgets = b }
}

// acquireReportSlot takes a budget slot when rawPath is the reporting operation, returning the
// slot's release func (see reserve); any other path is unbudgeted and gets a no-op release.
func (c *Client) acquireReportSlot(ctx context.Context, rawPath string) (release func(), err error) {
	m := reportPathRE.FindStringSubmatch(rawPath)
	if m == nil {
		return func() {}, nil
	}
	b := c.reportBudgets
	if b == nil {
		b = defaultReportBudgets
	}
	return b.reserve(ctx, m[1], isInsightsReports(ctx))
}
