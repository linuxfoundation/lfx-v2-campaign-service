// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package indexer

import (
	"context"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// TestNewNATSPublisher_DialErrorHidesCredentials pins that a dial failure cannot put the broker
// password in a log line.
//
// Redacting only our own "%s" prefix is NOT enough — the wrapped nats.go error embeds the
// ORIGINAL url, so the same line printed "nats://***@host" alongside the raw
// "user:sup3rsecret@host". This asserts on the WHOLE error text for exactly that reason.
func TestNewNATSPublisher_DialErrorHidesCredentials(t *testing.T) {
	const password = "sup3r-s3cret" // secretlint-disable-line -- fixture asserting the password is scrubbed
	// A malformed host forces a parse error, which is the case that quotes the input back.
	raw := "nats://svcuser:" + password + "@bad host:4222"

	p, err := NewNATSPublisher(raw)
	require.Error(t, err, "a malformed url must fail the dial")
	require.NotNil(t, p, "a failed dial still returns a usable Noop")

	assert.NotContains(t, err.Error(), password,
		"the broker password must not survive anywhere in the error chain")
	assert.NotContains(t, err.Error(), "svcuser",
		"the broker username is part of the credential")
	// The host must survive — it is what makes the failure diagnosable.
	assert.Contains(t, err.Error(), "bad host:4222")
}

// TestScrubURL covers the shapes a dependency error can quote back.
func TestScrubURL(t *testing.T) {
	raw := "nats://u:p@host:4222" // secretlint-disable-line -- fixture
	cases := map[string]string{
		`parse "nats://u:p@host:4222": bad`: `parse "nats://***@host:4222": bad`,
		`dial failed for u:p@host:4222`:     `dial failed for ***@host:4222`,
		`nothing sensitive here`:            `nothing sensitive here`,
	}
	for in, want := range cases {
		assert.Equal(t, want, scrubURL(in, raw), "input %q", in)
	}
	// A blank url must not turn the text into nonsense.
	assert.Equal(t, "some error", scrubURL("some error", ""))
}
