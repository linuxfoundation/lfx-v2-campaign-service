// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
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
