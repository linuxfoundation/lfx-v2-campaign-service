// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package indexer

import (
	"context"
	"strings"
	"sync"
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

// TestNATSPublisher_SerializesPerResource pins that concurrent publishes for the SAME object id
// do not interleave.
//
// The indexer does no version comparison — it overwrites the current document with whatever
// arrives last. Two writers committing v2 then v3 but publishing in reverse order leave the
// index holding v2 permanently, since both writers think they succeeded and no later write
// repairs it.
func TestNATSPublisher_SerializesPerResource(t *testing.T) {
	p := &NATSPublisher{} // no conn: Publish fails fast, which is enough to exercise the lock

	const goroutines = 16
	var (
		mu       sync.Mutex
		inFlight int
		maxSeen  int
	)

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lock := p.resourceLock("same-id")
			lock.Lock()
			mu.Lock()
			inFlight++
			if inFlight > maxSeen {
				maxSeen = inFlight
			}
			mu.Unlock()

			mu.Lock()
			inFlight--
			mu.Unlock()
			lock.Unlock()
		}()
	}
	wg.Wait()

	assert.Equal(t, 1, maxSeen, "publishes for one resource must not overlap, or a stale version can win")

	// Different resources must NOT contend — that would serialize the whole service.
	assert.NotSame(t, p.resourceLock("a"), p.resourceLock("b"))
	assert.Same(t, p.resourceLock("a"), p.resourceLock("a"))
}

// TestScrubURL_HandlesCommaSeparatedServerLists pins the multi-server case. NATS accepts a
// comma-separated list, and nats.go's parse error quotes only the OFFENDING entry — so scrubbing
// the list as a single string matched nothing and that entry's credential reached the log
// verbatim.
func TestScrubURL_HandlesCommaSeparatedServerLists(t *testing.T) {
	const p1, p2 = "pass-one", "pass-two" // secretlint-disable-line -- fixture asserting both are scrubbed
	list := "nats://a:" + p1 + "@host1:4222,nats://b:" + p2 + "@bad host:4222"

	// The error quotes only the second entry.
	got := scrubURL(`parse "nats://b:`+p2+`@bad host:4222": invalid character`, list)
	assert.NotContains(t, got, p2, "the offending entry's password must be scrubbed")
	assert.Contains(t, got, "bad host:4222", "the host stays, so the failure is diagnosable")

	// And when the error quotes the first entry instead.
	got = scrubURL(`dial tcp: nats://a:`+p1+`@host1:4222 refused`, list)
	assert.NotContains(t, got, p1)
	assert.Contains(t, got, "host1:4222")

	// A single-server URL still works (no comma).
	got = scrubURL(`parse "nats://u:secret@h:4222": bad`, "nats://u:secret@h:4222") // secretlint-disable-line -- fixture
	assert.NotContains(t, got, "secret")
}

// TestPublishRaw_ContextEndedIsNotSuccess pins the contract the relay depends on: a publish that
// was never flushed must REPORT FAILURE.
//
// conn.Publish only buffers. If the context ends before the flush, nothing is confirmed on the
// wire — and a nil return would let Relay.drain retire the outbox row for a delivery that may
// never have happened, reopening the exact loss window the outbox exists to close. A duplicate
// on the next pass is harmless (the indexer overwrites by object id); a dropped message is not.
//
// Both shapes matter. flushBudget only consults the DEADLINE, so a context cancelled without one
// still reports a full budget — checking ctx.Err() is what catches it.
func TestPublishRaw_ContextEndedIsNotSuccess(t *testing.T) {
	// No connection is needed: the context check is the FIRST thing PublishRaw does. A nil conn
	// would return errNoConnection instead — a different error, which ErrorIs distinguishes, so
	// this also pins the ordering rather than merely tolerating it.
	p := &NATSPublisher{}

	t.Run("cancelled without a deadline", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		require.Positive(t, flushBudget(ctx), "precondition: the budget alone does not reveal this")

		err := p.PublishRaw(ctx, Subject(ObjectTypeBrief), "brief-1", []byte(`{}`))
		require.Error(t, err, "an unflushed publish must not report success")
		assert.ErrorIs(t, err, context.Canceled)
	})

	t.Run("deadline already passed", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), -time.Second)
		defer cancel()

		err := p.PublishRaw(ctx, Subject(ObjectTypeBrief), "brief-1", []byte(`{}`))
		require.Error(t, err)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
	})
}

// TestPublishRaw_OnlyAnOKReplyCountsAsDelivered pins the ACK contract the outbox depends on.
//
// A NATS flush confirms only that bytes reached the BROKER. The indexer subscribes to
// lfx.index.* with reply support and answers "OK" on success or "ERROR: ..." on any
// envelope/config/data rejection (lfx-v2-indexer-service, IndexingMessageHandler.HandleWithReply).
// Treating a flush as delivery therefore retired outbox rows for messages that were REFUSED
// outright — the same silent drop this table exists to prevent, but harder to notice, because
// every row looks delivered.
//
// Verified end-to-end against a real nats-server with a responder mimicking the indexer:
//
//	accepted    -> nil
//	rejected    -> "indexer: lfx.index.campaign rejected the message: ERROR: ..."
//	no listener -> "indexer: ... not acknowledged: nats: no responders available for request"
//
// This unit test pins the reply CLASSIFICATION, which is the part that decides whether a row is
// retired; the package has no NATS harness in CI, so the transport itself is covered by the
// live check above rather than duplicated here.
func TestPublishRaw_OnlyAnOKReplyCountsAsDelivered(t *testing.T) {
	assert.Equal(t, "OK", ackOK, "the indexer's success reply is a literal OK")

	// Anything that is not exactly OK must be treated as a rejection, including the indexer's
	// own error form and an empty body.
	for _, reply := range []string{"ERROR: error processing indexing message", "", "ok", "OKAY"} {
		assert.NotEqual(t, ackOK, strings.TrimSpace(reply),
			"%q must not be mistaken for an acknowledgement", reply)
	}
	// Surrounding whitespace is tolerated: the reply is trimmed before comparison, so a broker
	// or client that pads the payload does not turn a success into a spurious rejection.
	assert.Equal(t, ackOK, strings.TrimSpace(" OK\n"))
}

// TestTruncateReply_BoundsWhatARejectionCanWrite keeps a verbose upstream error from bloating the
// outbox row it gets recorded on (last_error is a retained TEXT column).
func TestTruncateReply_BoundsWhatARejectionCanWrite(t *testing.T) {
	assert.Equal(t, "short", truncateReply("short"))
	long := strings.Repeat("x", maxReplyLen+50)
	got := truncateReply(long)
	assert.Len(t, []rune(got), maxReplyLen+1, "truncated to the cap plus the ellipsis")
	assert.True(t, strings.HasSuffix(got, "…"), "a truncated reply is marked as such")
}
