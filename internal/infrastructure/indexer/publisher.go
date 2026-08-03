// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package indexer

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

// publishTimeout bounds a single publish+flush. Indexing is best-effort, so a slow broker must
// not hold a request goroutine: the write it follows has already committed.
const publishTimeout = 3 * time.Second

// connectTimeout bounds the initial dial at startup.
const connectTimeout = 5 * time.Second

// DrainTimeout bounds the shutdown drain. EXPORTED because the container must budget for it
// in ContainerCloseTimeout — Close really does spend this long draining, so a private constant
// would let the shutdown arithmetic silently understate the phase. The nats.go DEFAULT is 30s, which alone exceeds the
// service's entire graceful-shutdown budget (constants.DefaultShutdownTimeout, 25s) — a wedged
// broker would hold Container.Close past the budget and get the pod SIGKILLed mid-shutdown,
// defeating the very budget ContainerCloseTimeout exists to enforce. Indexing is best-effort,
// so a small fixed slice is the right trade: buffered publishes get a chance to flush, and an
// unreachable broker costs 2s of shutdown instead of 30.
const DrainTimeout = 2 * time.Second

// Publisher publishes index documents. Implementations MUST be non-fatal: the database is the
// source of truth, and a failed publish costs discoverability (the Query Service re-indexes on
// the next write), never correctness. It must therefore never fail the caller's operation.
type Publisher interface {
	Publish(ctx context.Context, body Body)
	Close()
}

// Noop is used when NATS is not configured. It keeps every call site unconditional — the
// alternative is a nil check at each publish point, which is exactly where one gets forgotten.
type Noop struct{}

// Publish does nothing.
func (Noop) Publish(context.Context, Body) {}

// Close does nothing.
func (Noop) Close() {}

// NATSPublisher publishes over NATS core (not JetStream): the Query Service re-indexes on every
// write, so an at-most-once delivery that drops a message self-heals on the next update, and
// persistence would add operational weight for no correctness gain.
type NATSPublisher struct {
	conn *nats.Conn
}

// NewNATSPublisher dials url and returns a Publisher.
//
// A dial failure is NOT fatal: it returns a Noop plus the error so the caller can log and boot
// anyway. Campaign dispatch — the thing this service exists to do — does not depend on the
// index, so refusing to start over an unreachable broker would convert a degraded search
// experience into a total outage.
func NewNATSPublisher(url string) (Publisher, error) {
	if url == "" {
		return Noop{}, nil
	}
	conn, err := nats.Connect(url,
		nats.Timeout(connectTimeout),
		nats.DrainTimeout(DrainTimeout),
		nats.MaxReconnects(-1), // reconnect forever; a broker restart must not permanently mute indexing
		nats.RetryOnFailedConnect(true),
	)
	if err != nil {
		return Noop{}, fmt.Errorf("connect to nats at %s: %w", redactURL(url), err)
	}
	return &NATSPublisher{conn: conn}, nil
}

// Publish sends one index document. It never returns an error by design — see Publisher.
func (p *NATSPublisher) Publish(ctx context.Context, body Body) {
	subject := Subject(body.ObjectType)
	payload, err := json.Marshal(body)
	if err != nil {
		// Near-impossible for this struct, but do not swallow it: a silent marshal failure
		// would make the resource permanently invisible to search with no signal.
		slog.ErrorContext(ctx, "failed to marshal index document (resource will not be indexed)",
			"subject", subject, "object_ref", body.ObjectRef, "error", err)
		return
	}
	if err := p.conn.Publish(subject, payload); err != nil {
		slog.WarnContext(ctx, "failed to publish index document (resource may not appear in search until its next write)",
			"subject", subject, "object_ref", body.ObjectRef, "error", err)
		return
	}
	// Flush with a bound so a wedged broker cannot hold the caller. Publish() alone only
	// buffers, so without this a message can be silently discarded on a later connection drop.
	//
	// The bound is the SMALLER of publishTimeout and whatever the caller's context still
	// allows. That matters during shutdown: the orchestrator's grace window is sized as
	// persistResultTimeout + jobFinalizeTimeout + 1s and does NOT budget for a flush between
	// them, so a flat 3s wait per publish could push a run past the grace period and race the
	// pool close. Honouring the caller's deadline keeps a best-effort convenience from
	// consuming a budget reserved for writes that actually matter.
	flushWait := flushBudget(ctx)
	if flushWait <= 0 {
		// No budget left: the publish is buffered and the connection drain on shutdown is
		// its remaining chance to land. Not an error — indexing is best-effort by contract.
		return
	}
	if err := p.conn.FlushTimeout(flushWait); err != nil {
		slog.WarnContext(ctx, "index document published but not flushed (delivery unconfirmed)",
			"subject", subject, "object_ref", body.ObjectRef, "error", err)
	}
}

// Close drains and closes the connection.
func (p *NATSPublisher) Close() {
	if p.conn == nil {
		return
	}
	// Drain rather than Close so buffered publishes are flushed on shutdown.
	if err := p.conn.Drain(); err != nil {
		slog.Warn("failed to drain nats connection", "error", err)
	}
}

// flushBudget returns how long Publish may wait for a flush: the SMALLER of publishTimeout
// and whatever the caller's context still allows (publishTimeout when it has no deadline).
// Extracted so the bound is directly testable — asserting it via wall-clock timing against a
// broker is unreliable, since an unreachable broker fails fast and would make such a test pass
// even with the bound removed.
func flushBudget(ctx context.Context) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return publishTimeout
	}
	if remaining := time.Until(deadline); remaining < publishTimeout {
		return remaining
	}
	return publishTimeout
}

// redactURL strips any credentials from a NATS URL before it reaches a log line. A NATS URL
// may carry user:pass (nats://user:pass@host:4222), and the dial error this is used in is
// logged at startup — so without this the broker password lands in the pod logs.
func redactURL(url string) string {
	at := strings.LastIndexByte(url, '@')
	if at < 0 {
		return url
	}
	if scheme := strings.Index(url, "://"); scheme >= 0 && scheme+3 <= at {
		return url[:scheme+3] + "***@" + url[at+1:]
	}
	return "***@" + url[at+1:]
}
