// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
)

// guardResult is a cacheable successful read for the guard tests.
func guardResult() *twitter.AudienceInsights {
	start := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	return &twitter.AudienceInsights{Window: twitter.WindowToday, Location: time.UTC, WindowStart: start, WindowEnd: start.Add(24 * time.Hour)}
}

func guardNow() time.Time { return time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC) }

// newJoinWatchedGuard returns a guard whose joins are signalled on the returned channel.
func newJoinWatchedGuard() (*twitterAudienceGuard, chan struct{}) {
	g := newTwitterAudienceGuard()
	joined := make(chan struct{}, 8)
	g.onJoin = func() { joined <- struct{}{} }
	return g, joined
}

type guardOutcome struct {
	ai  *twitter.AudienceInsights
	err error
}

// The leader's client goes away while its fetch is running: a joiner with a live context takes
// over as leader and succeeds, instead of inheriting the leader's context.Canceled.
func TestTwitterAudienceGuard_JoinerReLeadsWhenLeaderCancelledDuringFetch(t *testing.T) {
	g, joined := newJoinWatchedGuard()
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	started := make(chan struct{})
	leader := make(chan guardOutcome, 1)
	go func() {
		ai, err := g.read(leaderCtx, "acc1", "k", 0, guardNow, nil, func(ctx context.Context) (*twitter.AudienceInsights, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		})
		leader <- guardOutcome{ai, err}
	}()
	waitFor(t, started, "the leader's fetch")

	var joinerFetches atomic.Int32
	joiner := make(chan guardOutcome, 1)
	go func() {
		ai, err := g.read(context.Background(), "acc1", "k", 0, guardNow, nil, func(context.Context) (*twitter.AudienceInsights, error) {
			joinerFetches.Add(1)
			return guardResult(), nil
		})
		joiner <- guardOutcome{ai, err}
	}()
	waitFor(t, joined, "the joiner to join")
	cancelLeader()

	got := <-joiner
	if got.err != nil || got.ai == nil {
		t.Fatalf("joiner: %+v, want the re-led read's result", got)
	}
	if joinerFetches.Load() != 1 {
		t.Errorf("joiner fetched %d time(s), want 1 (it re-led)", joinerFetches.Load())
	}
	l := <-leader
	var lce *leaderContextError
	if !errors.Is(l.err, context.Canceled) || errors.As(l.err, &lce) {
		t.Errorf("leader: err = %v, want its own untagged context.Canceled", l.err)
	}
}

// The same, while the leader is still waiting for the account's slot: its cancellation must not
// fail the joiner either.
func TestTwitterAudienceGuard_JoinerReLeadsWhenLeaderCancelledWaitingForSlot(t *testing.T) {
	g, joined := newJoinWatchedGuard()
	// Hold the account's only slot with a read over another scope.
	release := make(chan struct{})
	holding := make(chan struct{})
	holder := make(chan error, 1)
	go func() {
		_, err := g.read(context.Background(), "acc1", "other", 0, guardNow, nil, func(context.Context) (*twitter.AudienceInsights, error) {
			close(holding)
			<-release
			return guardResult(), nil
		})
		holder <- err
	}()
	waitFor(t, holding, "the slot holder")

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leader := make(chan error, 1)
	go func() {
		_, err := g.read(leaderCtx, "acc1", "k", 0, guardNow, nil, func(context.Context) (*twitter.AudienceInsights, error) {
			return nil, errors.New("the cancelled leader must never fetch")
		})
		leader <- err
	}()
	waitInflight(t, g, "k")
	joiner := make(chan guardOutcome, 1)
	go func() {
		ai, err := g.read(context.Background(), "acc1", "k", 0, guardNow, nil, func(context.Context) (*twitter.AudienceInsights, error) {
			return guardResult(), nil
		})
		joiner <- guardOutcome{ai, err}
	}()
	waitFor(t, joined, "the joiner to join the waiting leader")
	cancelLeader()
	if err := <-leader; !errors.Is(err, context.Canceled) {
		t.Errorf("leader: err = %v, want context.Canceled", err)
	}
	close(release)
	if got := <-joiner; got.err != nil || got.ai == nil {
		t.Errorf("joiner: %+v, want a result after re-leading", got)
	}
	if err := <-holder; err != nil {
		t.Errorf("holder: %v", err)
	}
}

// A joiner whose OWN context ends gets its own error; the leader carries on.
func TestTwitterAudienceGuard_JoinerOwnCancellationIsItsOwnError(t *testing.T) {
	g, joined := newJoinWatchedGuard()
	release := make(chan struct{})
	started := make(chan struct{})
	leader := make(chan error, 1)
	go func() {
		_, err := g.read(context.Background(), "acc1", "k", 0, guardNow, nil, func(context.Context) (*twitter.AudienceInsights, error) {
			close(started)
			<-release
			return guardResult(), nil
		})
		leader <- err
	}()
	waitFor(t, started, "the leader's fetch")
	joinerCtx, cancelJoiner := context.WithCancel(context.Background())
	joiner := make(chan error, 1)
	go func() {
		_, err := g.read(joinerCtx, "acc1", "k", 0, guardNow, nil, func(context.Context) (*twitter.AudienceInsights, error) {
			return nil, errors.New("the joiner must not fetch")
		})
		joiner <- err
	}()
	waitFor(t, joined, "the joiner to join")
	cancelJoiner()
	if err := <-joiner; !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "waiting for an identical read") {
		t.Errorf("joiner: err = %v, want its own cancellation", err)
	}
	close(release)
	if err := <-leader; err != nil {
		t.Errorf("leader: %v", err)
	}
}

// An upstream failure with the leader's context live is shared with every joiner, not retried.
func TestTwitterAudienceGuard_UpstreamFailureIsSharedNotRetried(t *testing.T) {
	g, joined := newJoinWatchedGuard()
	upstream := errors.New("x ads api GET stats/jobs failed (503)")
	release := make(chan struct{})
	started := make(chan struct{})
	var fetches atomic.Int32
	fetch := func(context.Context) (*twitter.AudienceInsights, error) {
		if fetches.Add(1) == 1 {
			close(started)
		}
		<-release
		return nil, upstream
	}
	leader := make(chan error, 1)
	go func() {
		_, err := g.read(context.Background(), "acc1", "k", 0, guardNow, nil, fetch)
		leader <- err
	}()
	waitFor(t, started, "the leader's fetch")
	joiner := make(chan error, 1)
	go func() {
		_, err := g.read(context.Background(), "acc1", "k", 0, guardNow, nil, fetch)
		joiner <- err
	}()
	waitFor(t, joined, "the joiner to join")
	close(release)
	if err := <-leader; !errors.Is(err, upstream) {
		t.Errorf("leader: %v", err)
	}
	if err := <-joiner; !errors.Is(err, upstream) {
		t.Errorf("joiner: err = %v, want the shared upstream failure", err)
	}
	if n := fetches.Load(); n != 1 {
		t.Errorf("%d fetches, want 1: an upstream failure is not retried", n)
	}
}

// waitInflight waits until key has a leader registered, so the next caller for it JOINS rather
// than leading.
func waitInflight(t *testing.T, g *twitterAudienceGuard, key string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		g.mu.Lock()
		_, ok := g.inflight[key]
		g.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no in-flight read for %q", key)
		}
		time.Sleep(time.Millisecond)
	}
}

// guardClock is a settable clock safe to read from the guard's goroutines.
type guardClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *guardClock) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *guardClock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

func dayResult(day time.Time) *twitter.AudienceInsights {
	return &twitter.AudienceInsights{Window: twitter.WindowToday, Location: time.UTC, WindowStart: day, WindowEnd: day.Add(24 * time.Hour)}
}

// A "today" read that joins a read started before the account's midnight must not get
// yesterday's buckets: the joined result's window no longer names its instants, so it re-leads.
func TestTwitterAudienceGuard_JoinedResultAcrossMidnightReLeads(t *testing.T) {
	g, joined := newJoinWatchedGuard()
	clock := &guardClock{t: time.Date(2026, 10, 5, 23, 59, 0, 0, time.UTC)}
	oct5, oct6 := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	release := make(chan struct{})
	started := make(chan struct{})
	leader := make(chan guardOutcome, 1)
	go func() {
		ai, err := g.read(context.Background(), "acc1", "k", 0, clock.now, nil, func(context.Context) (*twitter.AudienceInsights, error) {
			close(started)
			<-release
			return dayResult(oct5), nil
		})
		leader <- guardOutcome{ai, err}
	}()
	waitFor(t, started, "the leader's fetch")
	var fetches atomic.Int32
	joiner := make(chan guardOutcome, 1)
	go func() {
		ai, err := g.read(context.Background(), "acc1", "k", 0, clock.now, nil, func(context.Context) (*twitter.AudienceInsights, error) {
			fetches.Add(1)
			return dayResult(oct6), nil
		})
		joiner <- guardOutcome{ai, err}
	}()
	waitFor(t, joined, "the joiner to join")
	clock.set(time.Date(2026, 10, 6, 0, 1, 0, 0, time.UTC))
	close(release)
	if l := <-leader; l.err != nil || !l.ai.WindowStart.Equal(oct5) {
		t.Errorf("leader: %+v", l)
	}
	got := <-joiner
	if got.err != nil || got.ai == nil || !got.ai.WindowStart.Equal(oct6) {
		t.Fatalf("joiner: %+v, want today's (Oct 6) window", got)
	}
	if fetches.Load() != 1 {
		t.Errorf("joiner fetched %d time(s), want 1 (it re-led)", fetches.Load())
	}
}

// abandonedFetch fails leaving n jobs (ids prefix+i) running on X.
func abandonedFetch(prefix string, n int, calls *atomic.Int32) func(context.Context) (*twitter.AudienceInsights, error) {
	return func(context.Context) (*twitter.AudienceInsights, error) {
		calls.Add(1)
		ids := make([]string, n)
		for i := range ids {
			ids[i] = prefix + strconv.Itoa(i)
		}
		return nil, &twitter.AudienceJobsAbandonedError{JobIDs: ids, Err: twitter.ErrAudienceJobsUnfinished}
	}
}

// Jobs failed reads leave running count against the account: a read that would take the count
// past the budget is refused before it creates anything, unless X reports enough of them finished
// or their hold has passed.
func TestTwitterAudienceGuard_AbandonedJobsBudget(t *testing.T) {
	clock := &guardClock{t: guardNow()}
	var fetches, statusReads atomic.Int32
	stillRunning := func(running []string, err error) twitterAudienceRunningFn {
		return func(_ context.Context, ids []string) ([]string, error) {
			statusReads.Add(1)
			return running, err
		}
	}
	g := newTwitterAudienceGuard()
	// Two failed six-job reads: 12 outstanding, exactly the budget.
	for i, prefix := range []string{"1", "2"} {
		if _, err := g.read(context.Background(), "acc1", "k"+prefix, 6, clock.now, stillRunning(nil, errors.New("unused")), abandonedFetch(prefix, 6, &fetches)); !errors.Is(err, twitter.ErrAudienceJobsUnfinished) {
			t.Fatalf("read %d: %v", i, err)
		}
	}
	if statusReads.Load() != 0 {
		t.Errorf("X was asked about jobs while the account was within budget")
	}
	ok := func(context.Context) (*twitter.AudienceInsights, error) { fetches.Add(1); return guardResult(), nil }

	// X cannot answer: every abandoned job keeps counting, the read is refused before fetching.
	before := fetches.Load()
	_, err := g.read(context.Background(), "acc1", "k3", 6, clock.now, stillRunning(nil, errors.New("x down")), ok)
	if err == nil || !strings.Contains(err.Error(), "may still be running") || fetches.Load() != before {
		t.Fatalf("over budget, X unreachable: err = %v, fetches %d → %d", err, before, fetches.Load())
	}
	// X still lists all twelve as running: refused.
	all := []string{"10", "11", "12", "13", "14", "15", "20", "21", "22", "23", "24", "25"}
	if _, err := g.read(context.Background(), "acc1", "k3", 6, clock.now, stillRunning(all, nil), ok); err == nil || fetches.Load() != before {
		t.Fatalf("over budget, all running: err = %v", err)
	}
	// X reports six finished: the read fits and runs.
	if _, err := g.read(context.Background(), "acc1", "k3", 6, clock.now, stillRunning(all[:6], nil), ok); err != nil || fetches.Load() != before+1 {
		t.Fatalf("after six finished: err = %v, fetches %d", err, fetches.Load())
	}
	// Past the hold, the remaining abandoned jobs stop counting without asking X.
	statusReads.Store(0)
	g2 := newTwitterAudienceGuard()
	for _, prefix := range []string{"1", "2"} {
		_, _ = g2.read(context.Background(), "acc2", "k"+prefix, 6, clock.now, nil, abandonedFetch(prefix, 6, &fetches))
	}
	clock.set(clock.now().Add(twitterAudienceAbandonedJobHold + time.Second))
	if _, err := g2.read(context.Background(), "acc2", "k3", 6, clock.now, stillRunning(nil, errors.New("must not be asked")), ok); err != nil {
		t.Errorf("after the hold: %v", err)
	}
	if statusReads.Load() != 0 {
		t.Error("X was asked about jobs whose hold had passed")
	}
}

// Per-account state is dropped when the account is idle: reading many accounts leaves no slots.
func TestTwitterAudienceGuard_SlotsDoNotGrowWithAccounts(t *testing.T) {
	g := newTwitterAudienceGuard()
	for i := 0; i < 50; i++ {
		acc := "acc" + strconv.Itoa(i)
		if _, err := g.read(context.Background(), acc, acc+"k", 3, guardNow, nil, func(context.Context) (*twitter.AudienceInsights, error) {
			return guardResult(), nil
		}); err != nil {
			t.Fatalf("read %s: %v", acc, err)
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.slots) != 0 || len(g.abandoned) != 0 {
		t.Errorf("%d slots and %d abandoned-job entries left after idle accounts, want none", len(g.slots), len(g.abandoned))
	}
}

// Anonymous jobs (creates that may have committed with no id) count against the budget, are never
// sent to X for reconciliation, and are released only when their hold expires.
func TestTwitterAudienceGuard_AnonymousJobsOnlyExpire(t *testing.T) {
	clock := &guardClock{t: guardNow()}
	g := newTwitterAudienceGuard()
	failAnon := func(context.Context) (*twitter.AudienceInsights, error) {
		return nil, &twitter.AudienceJobsAbandonedError{Unknown: 1, Err: errors.New("create x stats job: 503")}
	}
	for i := 0; i < 12; i++ {
		if _, err := g.read(context.Background(), "acc1", "k"+strconv.Itoa(i), 1, clock.now, nil, failAnon); err == nil {
			t.Fatal("expected the failure")
		}
	}
	var asked atomic.Int32
	running := func(_ context.Context, ids []string) ([]string, error) {
		asked.Add(1)
		return nil, nil // X would say nothing is running — but it cannot be asked about these
	}
	ok := func(context.Context) (*twitter.AudienceInsights, error) { return guardResult(), nil }
	if _, err := g.read(context.Background(), "acc1", "next", 1, clock.now, running, ok); err == nil || !strings.Contains(err.Error(), "may still be running") {
		t.Fatalf("err = %v, want the budget refusal", err)
	}
	if asked.Load() != 0 {
		t.Errorf("X was asked %d time(s) about jobs with no id", asked.Load())
	}
	clock.set(clock.now().Add(twitterAudienceAbandonedJobHold))
	if _, err := g.read(context.Background(), "acc1", "next", 1, clock.now, running, ok); err != nil {
		t.Errorf("after the hold: %v", err)
	}
}

// Reconciling named jobs must not drop the anonymous ones beside them: X says the six named jobs
// finished, but six anonymous ones still count until their hold expires.
func TestTwitterAudienceGuard_ReconciliationKeepsAnonymousJobs(t *testing.T) {
	clock := &guardClock{t: guardNow()}
	g := newTwitterAudienceGuard()
	_, _ = g.read(context.Background(), "acc1", "a", 6, clock.now, nil, func(context.Context) (*twitter.AudienceInsights, error) {
		return nil, &twitter.AudienceJobsAbandonedError{JobIDs: []string{"1", "2", "3", "4", "5", "6"}, Unknown: 6, Err: errors.New("unfinished")}
	})
	allDone := func(context.Context, []string) ([]string, error) { return nil, nil }
	ok := func(context.Context) (*twitter.AudienceInsights, error) { return guardResult(), nil }
	if _, err := g.read(context.Background(), "acc1", "b", 7, clock.now, allDone, ok); err == nil {
		t.Error("6 anonymous jobs + 7 planned exceeds the budget of 12; the read must be refused")
	}
	if _, err := g.read(context.Background(), "acc1", "c", 6, clock.now, allDone, ok); err != nil {
		t.Errorf("6 anonymous + 6 planned fits: %v", err)
	}
}
