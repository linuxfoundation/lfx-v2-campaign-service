// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"
)

// Reddit publishes ONE rate limit for the reporting operation (POST /ad_accounts/{id}/reports):
// the `ads-reporting` policy, 60 requests per 60 seconds. Every caller of that operation in this
// service — the campaign metrics read, the account monitor, the audience read — spends from it, so
// a budget that only one of them respects is not a budget. This file is the process-wide ledger:
// EVERY HTTP attempt at the reporting operation, made by any Client in the process (each page of a
// paginated report, and each 429 retry, is its own attempt), takes one slot from the ad account's
// ledger before it is sent (requestWithThrottleRetryCounted). The slot is taken as late as
// possible — after the token refresh and the request build, immediately before the send — so an
// attempt that fails before it could be sent (a failed refresh, a cancelled context) spends none.
//
// Sizing (per ad account, per process, over a sliding 60-second window):
//
//   - reportBudgetTotal (45) of Reddit's 60 for ALL report attempts. The 15 left over are headroom
//     for what this ledger cannot see: Reddit Ads Manager and other tools on the same account.
//   - reportBudgetInsights (24) of those 45 for the project-scoped insight reads (the audience read;
//     marked with withInsightsReports). So an insight read can never take more than 24 of the 45,
//     and the metrics read and account monitor always keep at least 21.
//
// An attempt that finds no slot waits for the oldest one to expire — but only if that happens
// before its context's deadline. If it cannot, it fails at once with ErrReportBudgetExhausted rather
// than sleeping into a timeout, and nothing is sent: an insight read then fails whole (503), never
// partially.
//
// PER PROCESS. With more than one replica each process keeps its own ledger, so the account-wide
// total is up to replicas × 45. The chart therefore refuses to render REDDIT_METRICS_ENABLED with
// more than one replica (templates/deployment.yaml, beside the TWITTER_METRICS_ENABLED guard); a
// multi-replica deployment needs a shared ledger (as the X stats jobs use a database lease) before
// that guard can be lifted.
const (
	reportBudgetWindow   = 60 * time.Second
	reportBudgetTotal    = 45
	reportBudgetInsights = 24
)

// ErrReportBudgetExhausted is returned, before any request is sent, when a reporting attempt
// cannot get a slot in the ad account's reporting budget before its context's deadline. Fixed
// text: no account id.
var ErrReportBudgetExhausted = errors.New("reddit-ads: the ad account's reporting request budget is exhausted for this deadline")

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

// ReportBudgets is the per-ad-account ledger of reporting attempts. The zero value is not usable;
// use NewReportBudgets. One process-wide instance (defaultReportBudgets) is shared by every Client
// unless a test injects its own (WithReportBudgets).
type ReportBudgets struct {
	window          time.Duration
	total, insights int
	now             func() time.Time
	// sleep waits d or until ctx ends. Swapped in tests.
	sleep func(ctx context.Context, d time.Duration) error

	mu        sync.Mutex
	byAccount map[string]*accountReportLedger
}

// accountReportLedger holds the send times of an account's attempts inside the window: every
// attempt in all, and the insight reads' attempts in insights (a subset).
type accountReportLedger struct {
	all, insights []time.Time
}

var defaultReportBudgets = newReportBudgets(reportBudgetWindow, reportBudgetTotal, reportBudgetInsights, time.Now)

// NewReportBudgets builds a separate ledger. Production code never calls it — every production
// Client spends from defaultReportBudgets — it exists so tests in other packages can give a client
// a ledger of their own (WithReportBudgets) instead of sharing the process-wide one.
func NewReportBudgets(window time.Duration, total, insights int) *ReportBudgets {
	return newReportBudgets(window, total, insights, time.Now)
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
	if b.total <= 0 || (insights && b.insights <= 0) {
		return ErrReportBudgetExhausted // a slot can never free up
	}
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%w: %w", ErrReportBudgetExhausted, err)
		}
		wait, ok := b.tryTake(accountID, insights)
		if ok {
			return nil
		}
		if dl, has := ctx.Deadline(); has && b.now().Add(wait).After(dl) {
			return ErrReportBudgetExhausted
		}
		if err := b.sleep(ctx, wait); err != nil {
			return fmt.Errorf("%w: %w", ErrReportBudgetExhausted, err)
		}
	}
}

// tryTake records an attempt if both applicable limits have room, or reports how long until the
// earliest blocking slot expires.
func (b *ReportBudgets) tryTake(accountID string, insights bool) (time.Duration, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
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
		return wait, false
	}
	l.all = append(l.all, now)
	if insights {
		l.insights = append(l.insights, now)
	}
	return 0, true
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

// WithReportBudgets makes the client spend from b instead of the process-wide ledger. Tests only,
// like WithBaseURL: production clients must share defaultReportBudgets, or the budget is not
// truthful.
func WithReportBudgets(b *ReportBudgets) Option {
	return func(c *Client) { c.reportBudgets = b }
}

// acquireReportSlot takes a budget slot when rawPath is the reporting operation; any other path is
// unbudgeted.
func (c *Client) acquireReportSlot(ctx context.Context, rawPath string) error {
	m := reportPathRE.FindStringSubmatch(rawPath)
	if m == nil {
		return nil
	}
	b := c.reportBudgets
	if b == nil {
		b = defaultReportBudgets
	}
	return b.acquire(ctx, m[1], isInsightsReports(ctx))
}
