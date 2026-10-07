// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"strings"
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
		ai, err := g.read(leaderCtx, "acc1", "k", guardNow, func(ctx context.Context) (*twitter.AudienceInsights, error) {
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
		ai, err := g.read(context.Background(), "acc1", "k", guardNow, func(context.Context) (*twitter.AudienceInsights, error) {
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
		_, err := g.read(context.Background(), "acc1", "other", guardNow, func(context.Context) (*twitter.AudienceInsights, error) {
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
		_, err := g.read(leaderCtx, "acc1", "k", guardNow, func(context.Context) (*twitter.AudienceInsights, error) {
			return nil, errors.New("the cancelled leader must never fetch")
		})
		leader <- err
	}()
	waitInflight(t, g, "k")
	joiner := make(chan guardOutcome, 1)
	go func() {
		ai, err := g.read(context.Background(), "acc1", "k", guardNow, func(context.Context) (*twitter.AudienceInsights, error) {
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
		_, err := g.read(context.Background(), "acc1", "k", guardNow, func(context.Context) (*twitter.AudienceInsights, error) {
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
		_, err := g.read(joinerCtx, "acc1", "k", guardNow, func(context.Context) (*twitter.AudienceInsights, error) {
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
		_, err := g.read(context.Background(), "acc1", "k", guardNow, fetch)
		leader <- err
	}()
	waitFor(t, started, "the leader's fetch")
	joiner := make(chan error, 1)
	go func() {
		_, err := g.read(context.Background(), "acc1", "k", guardNow, fetch)
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
