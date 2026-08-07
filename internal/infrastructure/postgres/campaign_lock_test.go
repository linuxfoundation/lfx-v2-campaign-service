// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// resetCooldownState restores the package-level cooldown vars to a fresh state, since they are
// shared across every test in this package (including StopCooldownsForShutdown's sync.Once).
func resetCooldownState(t *testing.T) {
	t.Helper()
	cooldownMu.Lock()
	cooldownWG = sync.WaitGroup{}
	cooldownShutdown = make(chan struct{})
	cooldownOnce = sync.Once{}
	cooldownStopped = false
	cooldownReleaseBoundNanos.Store(0)
	cooldownMu.Unlock()
}

// TestReleaseCampaignLockAfterCooldown_NoStragglerRaceWithShutdown pins the fix for the
// cooldownWG happens-before violation: sync.WaitGroup requires that any Add(1) which starts
// when the counter is zero happen before the matching Wait call. Without the cooldownMu-gated
// cooldownStopped check, a straggler ReleaseCampaignLockAfterCooldown call racing
// StopCooldownsForShutdown could call cooldownWG.Add(1) after StopCooldownsForShutdown's Wait
// had already observed a zero counter and returned — a contract violation that "go test -race"
// (and the runtime's own WaitGroup misuse detector) can catch. Every straggler in this test
// uses the zero CampaignLockToken, so ReleaseCampaignLock no-ops without needing a real
// Postgres connection — this test is only exercising the WaitGroup synchronization.
func TestReleaseCampaignLockAfterCooldown_NoStragglerRaceWithShutdown(t *testing.T) {
	resetCooldownState(t)

	r := &CampaignRepo{}

	const stragglers = 50
	var ready sync.WaitGroup
	start := make(chan struct{})
	var done sync.WaitGroup
	ready.Add(stragglers)
	done.Add(stragglers)
	for i := 0; i < stragglers; i++ {
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			// Races StopCooldownsForShutdown below on purpose.
			r.ReleaseCampaignLockAfterCooldown(domain.CampaignLockToken{}, time.Hour)
		}()
	}

	// Wait for every goroutine to be scheduled and parked on start, then release them at the
	// same moment StopCooldownsForShutdown begins, maximizing the chance of hitting the race
	// window the fix closes.
	ready.Wait()
	close(start)
	StopCooldownsForShutdown(2 * time.Second)
	done.Wait()

	cooldownMu.Lock()
	stopped := cooldownStopped
	cooldownMu.Unlock()
	if !stopped {
		t.Fatal("expected cooldownStopped to be true after StopCooldownsForShutdown")
	}
}

// TestReleaseCampaignLockAfterCooldown_StragglerAfterStopReleasesSynchronously pins the
// straggler path itself: once cooldownStopped is true, ReleaseCampaignLockAfterCooldown must
// not spawn a tracked goroutine at all — it must call ReleaseCampaignLock synchronously. This
// is what makes the no-Add-after-Wait guarantee hold without leaking an untracked goroutine.
func TestReleaseCampaignLockAfterCooldown_StragglerAfterStopReleasesSynchronously(t *testing.T) {
	resetCooldownState(t)

	r := &CampaignRepo{}
	StopCooldownsForShutdown(time.Second)

	before := runningCooldownGoroutines()
	r.ReleaseCampaignLockAfterCooldown(domain.CampaignLockToken{}, time.Hour)
	after := runningCooldownGoroutines()

	if after != before {
		t.Fatalf("straggler after shutdown spawned a tracked cooldown goroutine: before=%d after=%d", before, after)
	}
}

// runningCooldownGoroutines snapshots the cooldownWG counter indirectly: it is 0 once every
// tracked goroutine has called Done, which is only observable by racing a Wait — instead this
// helper simply asserts Wait returns immediately, proving no goroutine is currently tracked.
func runningCooldownGoroutines() int {
	done := make(chan struct{})
	go func() {
		cooldownWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return 0
	case <-time.After(200 * time.Millisecond):
		return 1
	}
}

// TestStopCooldownsForShutdown_PublishesReleaseBoundBeforeWaking pins the hand-off that makes
// cooldownStopTimeout bound the CONNECTION and not merely the wait. A cooldown release cut
// short by shutdown must unlock under the same budget Close is waiting on; if it fell back to
// lockReleaseTimeout, Close would return while the connection stayed checked out and
// pgxpool.Close would block on it for the difference — outside ContainerCloseTimeout.
//
// The publish must also happen BEFORE the channel close that wakes the goroutines, or a
// goroutine that wakes first reads a zero and falls back to the default. This test asserts the
// bound is already visible to anything the close wakes.
func TestStopCooldownsForShutdown_PublishesReleaseBoundBeforeWaking(t *testing.T) {
	resetCooldownState(t)

	if got := shutdownReleaseBound(); got != lockReleaseTimeout {
		t.Fatalf("before shutdown, bound = %v, want the %v fallback", got, lockReleaseTimeout)
	}

	const budget = 250 * time.Millisecond
	// Observed from a goroutine parked on cooldownShutdown, i.e. exactly what a cut-short
	// release sees at wake time — not merely what is readable after Stop returns.
	observed := make(chan time.Duration, 1)
	go func() {
		<-cooldownShutdown
		observed <- shutdownReleaseBound()
	}()

	StopCooldownsForShutdown(budget)

	select {
	case got := <-observed:
		if got != budget {
			t.Errorf("a cooldown woken by shutdown would unlock with a %v budget, want %v (Close's own wait)", got, budget)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine parked on cooldownShutdown was never woken")
	}
}

// observeReleaseBounds installs the release-budget observer for the duration of a test and
// returns a func that drains what was recorded.
func observeReleaseBounds(t *testing.T) func() []time.Duration {
	t.Helper()
	var mu sync.Mutex
	var seen []time.Duration
	obs := func(d time.Duration) {
		mu.Lock()
		seen = append(seen, d)
		mu.Unlock()
	}
	releaseBoundObserver.Store(&obs)
	t.Cleanup(func() { releaseBoundObserver.Store(nil) })
	return func() []time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return append([]time.Duration(nil), seen...)
	}
}

// TestReleaseCampaignLockAfterCooldown_EveryShutdownPathUsesTheShutdownBound pins the budget
// used by BOTH ways a cooldown release can run during shutdown, because both are reachable and
// only one of them looks like the shutdown path.
//
//  1. The straggler branch: ReleaseCampaignLockAfterCooldown called after cooldownStopped is
//     already true, which releases synchronously on the caller's goroutine.
//  2. The cooldown-elapsed branch: if the cooldown timer fires at the same instant shutdown
//     closes cooldownShutdown, BOTH select cases are ready and Go picks one at random — so the
//     ordinary-looking case can be the one that runs while Close is waiting.
//
// Either using lockReleaseTimeout (5s) keeps a pool connection checked out long after
// StopCooldownsForShutdown's 250ms wait returns, and pgxpool.Close blocks on it outside
// ContainerCloseTimeout. Reverting either branch to ReleaseCampaignLock fails this test with
// the offending budget named.
func TestReleaseCampaignLockAfterCooldown_EveryShutdownPathUsesTheShutdownBound(t *testing.T) {
	const budget = 250 * time.Millisecond

	t.Run("straggler after stop", func(t *testing.T) {
		resetCooldownState(t)
		recorded := observeReleaseBounds(t)
		r := &CampaignRepo{}
		StopCooldownsForShutdown(budget)

		r.ReleaseCampaignLockAfterCooldown(domain.CampaignLockToken{}, time.Hour)

		got := recorded()
		if len(got) != 1 {
			t.Fatalf("expected exactly one release, got %d: %v", len(got), got)
		}
		if got[0] != budget {
			t.Errorf("straggler released with a %v budget, want %v (Close's own wait); a 5s unlock here outlives pgxpool.Close's wait", got[0], budget)
		}
	})

	t.Run("cooldown elapsed during shutdown", func(t *testing.T) {
		resetCooldownState(t)
		recorded := observeReleaseBounds(t)
		r := &CampaignRepo{}

		// Publish the shutdown budget WITHOUT closing cooldownShutdown, then use a cooldown
		// of 0 so time.After fires immediately: cooldownShutdown is never ready, so the
		// select can only take the elapsed branch. That isolates the branch under test —
		// racing the two ready cases would leave which one ran up to Go's random choice.
		cooldownReleaseBoundNanos.Store(int64(budget))
		r.ReleaseCampaignLockAfterCooldown(domain.CampaignLockToken{}, 0)
		deadline := time.Now().Add(2 * time.Second)
		for len(recorded()) == 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}

		got := recorded()
		if len(got) != 1 {
			t.Fatalf("expected exactly one release, got %d: %v", len(got), got)
		}
		if got[0] != budget {
			t.Errorf("cooldown-elapsed release used a %v budget, want %v; this branch is reachable during shutdown because both select cases can be ready at once", got[0], budget)
		}
	})
}

// TestReleaseCampaignLock_OrdinaryPathKeepsTheGenerousBound is the other half of the pair: the
// shutdown bound must not leak into normal operation. Before shutdown publishes anything,
// shutdownReleaseBound falls back to lockReleaseTimeout, so a cooldown that elapses on a
// healthy process still gets the generous unlock budget — the alternative to a slow-but-
// successful unlock is destroying the connection.
func TestReleaseCampaignLock_OrdinaryPathKeepsTheGenerousBound(t *testing.T) {
	resetCooldownState(t)
	recorded := observeReleaseBounds(t)
	r := &CampaignRepo{}

	if err := r.ReleaseCampaignLock(context.Background(), domain.CampaignLockToken{}); err != nil {
		t.Fatalf("release: %v", err)
	}
	r.ReleaseCampaignLockAfterCooldown(domain.CampaignLockToken{}, 0)
	// Drain the cooldown goroutine without publishing a shutdown budget.
	deadline := time.Now().Add(2 * time.Second)
	for len(recorded()) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	got := recorded()
	if len(got) != 2 {
		t.Fatalf("expected 2 releases, got %d: %v", len(got), got)
	}
	for i, d := range got {
		if d != lockReleaseTimeout {
			t.Errorf("release %d used %v, want the ordinary %v — shutdown's tighter budget must not apply before shutdown", i, d, lockReleaseTimeout)
		}
	}
}
