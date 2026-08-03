// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package indexer

import (
	"context"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// TestDrainTimeoutFitsShutdownBudget pins the drain bound against the service's graceful
// shutdown budget. nats.go defaults DrainTimeout to 30s — alone MORE than the entire
// DefaultShutdownTimeout (25s) — so a wedged broker would hold Container.Close past the
// budget and get the pod SIGKILLed mid-shutdown. This asserts the relationship, not the
// literal, so raising either constant re-checks the invariant instead of silently breaking it.
func TestDrainTimeoutFitsShutdownBudget(t *testing.T) {
	if DrainTimeout >= constants.DefaultShutdownTimeout {
		t.Fatalf("DrainTimeout (%s) must be well under DefaultShutdownTimeout (%s): a wedged broker would overrun the shutdown budget",
			DrainTimeout, constants.DefaultShutdownTimeout)
	}
	// It must also leave room for the phases that actually matter (dispatch drain, pool
	// close), not merely fit. A quarter of the budget is a generous ceiling for a
	// best-effort convenience.
	if DrainTimeout > constants.DefaultShutdownTimeout/4 {
		t.Errorf("DrainTimeout (%s) takes more than a quarter of the shutdown budget (%s) for a best-effort concern",
			DrainTimeout, constants.DefaultShutdownTimeout)
	}
}

// TestFlushBudget_HonoursCallerDeadline pins the flush bound against the orchestrator's
// shutdown grace window. The grace is sized as persistResultTimeout + jobFinalizeTimeout + 1s
// and budgets NOTHING for a flush between them, so a flat publishTimeout wait per publish
// could push a dispatch past the grace period and race the pool close.
//
// This asserts the BOUND rather than wall-clock time around a real publish: an unreachable
// broker fails its flush immediately, so a timing-based test would pass even with the bound
// removed — vacuously green for the wrong reason.
func TestFlushBudget_HonoursCallerDeadline(t *testing.T) {
	t.Run("no deadline uses the full publish timeout", func(t *testing.T) {
		if got := flushBudget(context.Background()); got != publishTimeout {
			t.Fatalf("flushBudget = %s, want %s", got, publishTimeout)
		}
	})

	t.Run("a tighter caller deadline wins", func(t *testing.T) {
		const budget = 50 * time.Millisecond
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		defer cancel()
		got := flushBudget(ctx)
		if got > budget {
			t.Fatalf("flushBudget = %s, want <= %s: a flat %s wait can overrun the shutdown grace window", got, budget, publishTimeout)
		}
		if got <= 0 {
			t.Fatalf("flushBudget = %s, want a positive remainder", got)
		}
	})

	t.Run("a looser caller deadline does not extend the wait", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), publishTimeout*10)
		defer cancel()
		if got := flushBudget(ctx); got != publishTimeout {
			t.Fatalf("flushBudget = %s, want %s: a generous caller must not raise the ceiling", got, publishTimeout)
		}
	})

	t.Run("an expired deadline yields no budget", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), -time.Second)
		defer cancel()
		if got := flushBudget(ctx); got > 0 {
			t.Fatalf("flushBudget = %s, want <= 0 so Publish skips the flush entirely", got)
		}
	})
}
