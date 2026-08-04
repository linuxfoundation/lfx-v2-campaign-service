// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
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
	Publish(ctx context.Context, msg Transaction)
	Close()
}

// Noop is used when NATS is not configured. It keeps every call site unconditional — the
// alternative is a nil check at each publish point, which is exactly where one gets forgotten.
type Noop struct{}

// Publish does nothing.
func (Noop) Publish(context.Context, Transaction) {}

// Close does nothing.
func (Noop) Close() {}

// NATSPublisher publishes over NATS core (not JetStream), which is AT-MOST-ONCE: a dropped
// message is simply lost.
//
// Do NOT read that as self-healing. A resource whose document is dropped is repaired only if
// something writes it AGAIN, and several writes have no successor: archiving a brief is
// terminal, and a created-then-never-edited campaign may never be written again. For those the
// index can stay permanently stale or missing — and because the Query Service serves lists and
// history FROM the index, that is user-visible, not merely a cache miss.
//
// This is a known gap, not a claim of correctness. Closing it properly needs delivery to be
// recoverable independently of the write — a transactional outbox with a relay, or a periodic
// database-to-index reconciliation sweep. Neither belongs behind a Publish() call: bounding the
// flush (see Publish) reduces the window but cannot close the commit-to-publish gap, since the
// process can die between the two no matter how the publish is bounded.
//
// What core NATS DOES buy is that indexing can never fail a write, which is the property this
// service actually depends on (the database is the source of truth).
type NATSPublisher struct {
	conn *nats.Conn

	// perResource serializes publishes for the SAME object id.
	//
	// The indexer does no version comparison — it overwrites the current document with
	// whatever arrives last. Two concurrent writers can commit v2 then v3 and reach Publish in
	// the reverse order, leaving the index holding v2 permanently: a stale document that no
	// later write repairs, because both writers think they succeeded.
	//
	// Holding a per-object lock across marshal+publish+flush keeps same-resource messages in
	// the order their callers reached this point. Different resources never contend.
	//
	// NOTE this orders the PUBLISH, not the commit. Two writers that commit in one order and
	// call Publish in the other are still mis-ordered — closing that needs the outbox pattern
	// (tracked separately). This removes the far more common in-process reordering.
	perResource sync.Map // objectID -> *sync.Mutex
}

// resourceLock returns the mutex guarding one object id, creating it on first use.
func (p *NATSPublisher) resourceLock(objectID string) *sync.Mutex {
	m, _ := p.perResource.LoadOrStore(objectID, &sync.Mutex{})
	return m.(*sync.Mutex)
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
		// Redact the URL in BOTH halves. Redacting only our own prefix is not enough: %w
		// renders nats.go's error, and its URL-parse failures embed the ORIGINAL string —
		// so a malformed credential-bearing NATS_URL would print "nats://***@host" and the
		// raw "user:pass@host" in the same log line. The wrapped error is flattened to text
		// so the credential cannot survive anywhere in the chain.
		return Noop{}, fmt.Errorf("connect to nats at %s: %s", redactURL(url), scrubURL(err.Error(), url))
	}
	return &NATSPublisher{conn: conn}, nil
}

// Publish sends one index document. It never returns an error by design — see Publisher.
func (p *NATSPublisher) Publish(ctx context.Context, msg Transaction) {
	subject := Subject(msg.ObjectType())
	if id := msg.objectID(); id != "" {
		lock := p.resourceLock(id)
		lock.Lock()
		defer lock.Unlock()
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		// Near-impossible for this struct, but do not swallow it: a silent marshal failure
		// would make the resource permanently invisible to search with no signal.
		slog.ErrorContext(ctx, "failed to marshal index document (resource will not be indexed)",
			"subject", subject, "object_id", msg.objectID(), "error", err)
		return
	}
	if err := p.conn.Publish(subject, payload); err != nil {
		slog.WarnContext(ctx, "failed to publish index document (resource may not appear in search until its next write)",
			"subject", subject, "object_id", msg.objectID(), "error", err)
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
			"subject", subject, "object_id", msg.objectID(), "error", err)
	}
}

// PublishRaw publishes an already-marshalled payload. Used by the relay, which must NOT
// re-marshal: the payload was fixed when the outbox row was written, so re-deriving it would
// let a later contract change alter the meaning of a message enqueued under the old one.
//
// Unlike Publish it RETURNS an error — the relay needs to know whether to retire the row.
// It takes the same per-object lock, so a replayed message cannot interleave with a live one
// for the same resource.
func (p *NATSPublisher) PublishRaw(ctx context.Context, subject, objectID string, payload []byte) error {
	// Check the context FIRST. Publish only buffers, so a context that has already ended cannot
	// produce a confirmed delivery — reporting success would let the relay retire the outbox row
	// for a message that may never have reached the wire, reopening the loss window the outbox
	// exists to close. Checked here rather than beside the flush for two reasons: there is no
	// point taking a per-object lock we cannot use, and flushBudget only consults the DEADLINE,
	// so a context cancelled without one would otherwise report a full budget and flush anyway.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("indexer: context ended before publish: %w", err)
	}
	if p.conn == nil {
		return errNoConnection
	}
	// Lock on the OBJECT id, not the subject: locking the subject would serialize every
	// resource of that type against each other, which is both slower and unnecessary — only
	// same-resource ordering matters.
	if objectID != "" {
		lock := p.resourceLock(objectID)
		lock.Lock()
		defer lock.Unlock()
	}
	// REQUEST, not Publish. A flush only confirms the bytes reached the broker — it says nothing
	// about whether the indexer ACCEPTED them. The indexer replies "OK" on success and
	// "ERROR: ..." on any envelope/config/data rejection (verified in lfx-v2-indexer-service:
	// IndexingMessageHandler.HandleWithReply), and it subscribes to lfx.index.* WITH reply
	// support. Treating a flush as success therefore retired outbox rows for messages that were
	// rejected outright, which is the same silent-drop this table exists to prevent — only
	// harder to notice, because everything looks delivered.
	wait := flushBudget(ctx)
	if wait <= 0 {
		return errNoFlushBudget
	}
	reply, err := p.conn.Request(subject, payload, wait)
	if err != nil {
		// Includes nats.ErrNoResponders (no indexer running) and a timeout waiting for the ACK.
		// Both leave the row PENDING, which is correct: nothing confirmed the message landed.
		return fmt.Errorf("indexer: %s not acknowledged: %w", subject, err)
	}
	if body := strings.TrimSpace(string(reply.Data)); body != ackOK {
		// The indexer received it and REFUSED it. Retrying verbatim will fail identically, but
		// the row must not be retired as delivered — RecordIndexMessageFailure writes the
		// reason onto the row so a persistent rejection is visible rather than silent.
		return fmt.Errorf("indexer: %s rejected the message: %s", subject, truncateReply(body))
	}
	return nil
}

// ackOK is the indexer's success reply. Anything else — notably its "ERROR: ..." form — means
// the message was received and REJECTED.
const ackOK = "OK"

// maxReplyLen bounds how much of a rejection reply is echoed into an error, so a verbose
// upstream message cannot bloat the outbox row it gets recorded on.
const maxReplyLen = 200

func truncateReply(s string) string {
	if len(s) <= maxReplyLen {
		return s
	}
	return s[:maxReplyLen] + "…"
}

// PublishRaw on a Noop reports FAILURE. Nothing was sent, so reporting success would let the
// relay retire the row as delivered — and a pod started with indexing disabled or misconfigured
// would then silently drain every pending message, permanently defeating outbox recovery for
// messages that were never published at all.
//
// Leaving them pending is the correct outcome: a later process with a working publisher drains
// them. The relay records the failure rather than spinning silently.
func (Noop) PublishRaw(context.Context, string, string, []byte) error { return errNoConnection }

// errNoFlushBudget covers the rare case where the budget is exhausted but the context reports no
// error, so PublishRaw never reports success for an unflushed publish.
var errNoFlushBudget = errors.New("indexer: no flush budget remaining")

// errNoConnection is returned by PublishRaw when the publisher has no connection, so the relay
// leaves the row pending rather than retiring an undelivered message.
var errNoConnection = errors.New("indexer: no NATS connection")

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

// scrubURL removes every occurrence of a credential-bearing URL from arbitrary error text,
// replacing it with the redacted form. Used on wrapped dependency errors, whose wording is not
// ours to control and which are known to embed the input verbatim.
//
// It replaces the RAW url first, then the userinfo alone — a parse error may quote a normalised
// or partial form of the input, so matching the full string alone is not sufficient. A
// comma-separated server list is scrubbed entry by entry (see below).
func scrubURL(text, rawURL string) string {
	if rawURL == "" {
		return text
	}
	// NATS accepts a COMMA-SEPARATED server list, and nats.go's parse error quotes only the
	// offending entry. Scrubbing the list as one string matched nothing, so that entry's
	// credential reached the log verbatim — scrub each entry independently.
	if strings.Contains(rawURL, ",") {
		for _, entry := range strings.Split(rawURL, ",") {
			if entry = strings.TrimSpace(entry); entry != "" {
				text = scrubURL(text, entry)
			}
		}
		return text
	}
	safe := redactURL(rawURL)
	text = strings.ReplaceAll(text, rawURL, safe)
	// Also catch the bare userinfo, which survives when the error quotes a rewritten URL.
	if at := strings.LastIndexByte(rawURL, '@'); at >= 0 {
		if scheme := strings.Index(rawURL, "://"); scheme >= 0 && scheme+3 <= at {
			if creds := rawURL[scheme+3 : at]; creds != "" {
				text = strings.ReplaceAll(text, creds+"@", "***@")
			}
		}
	}
	return text
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
