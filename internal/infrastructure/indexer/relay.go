// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package indexer

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// OutboxReader is the outbox slice the relay needs. An interface so the relay is testable
// without a database.
//
// ONE method, not a read/mark/record triple. The claim, the publish and the retire have to
// happen under the same row locks — a relay that read rows, published, then marked them in
// separate calls let every replica load the SAME batch, so a slow pod could publish an earlier
// `updated` after a faster one had already published the later `deleted`. Handing the publish
// down as a callback is what keeps the lock held across it.
type OutboxReader interface {
	// DrainPendingIndexMessages claims a batch, calls deliver for each message, and retires
	// only what deliver confirms. A deliver error leaves that row pending for a later pass.
	DrainPendingIndexMessages(ctx context.Context, limit int, deliver func(context.Context, *model.OutboxMessage) error) (int, error)
	// PrunePublishedIndexMessages deletes PUBLISHED rows past the retention window. Pending
	// rows are undelivered work and are never eligible, however old.
	PrunePublishedIndexMessages(ctx context.Context, olderThan time.Duration, limit int) (int64, error)
}

// RawPublisher publishes an already-marshalled payload to a subject. The relay must NOT
// re-marshal: the payload was fixed when the row was written, and re-deriving it would let a
// later contract change alter the meaning of a message enqueued under the old one.
type RawPublisher interface {
	PublishRaw(ctx context.Context, subject, objectID string, payload []byte) error
}

// relayInterval is how often the relay drains the outbox. Frequent enough that a dropped
// publish is repaired in seconds rather than at the next write, cheap enough that the partial
// index makes an empty pass nearly free.
const relayInterval = 15 * time.Second

// relayPassTimeout bounds one drain so a slow broker or database cannot wedge the goroutine.
const relayPassTimeout = 30 * time.Second

// Relay drains the index outbox, publishing rows that the direct publish did not deliver.
//
// This is what makes indexing RECOVERABLE rather than best-effort: the outbox row co-commits
// with its resource, so a process that dies between commit and publish leaves the row behind
// and the relay picks it up. Without it a dropped message is simply lost — terminal writes like
// archiving a brief have no "next write" to repair the index.
type Relay struct {
	outbox OutboxReader
	pub    RawPublisher
	// authorization is the SERVICE credential injected at publish time. Outbox rows
	// deliberately store no token: the table is JSONB retained for audit with no pruning, so a
	// per-request JWT written there would persist as a live credential indefinitely.
	authorization string

	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
	// warnOnce keeps the missing-credential warning to a single line rather than one per pass.
	warnOnce sync.Once
}

// NewRelay constructs a Relay. authorization is the service credential stamped onto every
// replayed message; the indexer REQUIRES a non-empty authorization header and drops messages
// without one.
func NewRelay(outbox OutboxReader, pub RawPublisher, authorization string) *Relay {
	return &Relay{outbox: outbox, pub: pub, authorization: authorization}
}

// Start begins draining in the background. Safe to call once; Stop ends it.
func (r *Relay) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.done = make(chan struct{})
	go func() {
		defer close(r.done)
		ticker := time.NewTicker(relayInterval)
		defer ticker.Stop()
		// Drain once at startup: the most likely reason rows are pending is that THIS pod's
		// predecessor died between a commit and its publish.
		r.drain(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.drain(ctx)
			}
		}
	}()
}

// Stop ends the relay and waits for the in-flight pass, bounded by wait.
//
// Bounded because this runs during shutdown: a slow broker must not hold the pod past its
// termination budget. An abandoned pass is safe — unpublished rows stay pending and the next
// process drains them, which is the entire point of the table.
func (r *Relay) Stop(wait time.Duration) {
	r.once.Do(func() {
		if r.cancel == nil {
			return
		}
		r.cancel()
		select {
		case <-r.done:
		case <-time.After(wait):
			slog.Warn("index relay did not stop promptly; abandoning the pass (rows stay pending)")
		}
	})
}

// warnNoToken logs the missing-credential condition ONCE. Every pass would otherwise log it, and
// a message repeated every 15s trains operators to ignore it.
func (r *Relay) warnNoToken() {
	r.warnOnce.Do(func() {
		slog.Warn("index relay is idle: no service credential configured (set INDEXER_SERVICE_TOKEN). " +
			"Outbox rows stay PENDING and will be published once it is set — nothing is lost.")
	})
}

// stamp injects the service credential into a stored payload. The stored message deliberately
// carries no authorization (see Relay.authorization), and the indexer drops any message whose
// header is missing or empty.
func (r *Relay) stamp(payload []byte) ([]byte, error) {
	// Decode into a MAP, not a Transaction. objectType is unexported and never serialized (the
	// indexer derives it from the subject), so a Transaction round-trip silently loses it — and
	// anything later reading ObjectType() off the result would route to "lfx.index.". The relay
	// routes from the outbox ROW instead, and a map keeps every other field byte-identical
	// including any the struct does not model.
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		return nil, fmt.Errorf("indexer: unparseable outbox payload: %w", err)
	}
	headers := map[string]string{}
	if raw, ok := msg["headers"]; ok {
		// A malformed headers value is not fatal: replace it rather than dropping the message.
		_ = json.Unmarshal(raw, &headers)
	}
	headers[authorizationHeader] = r.authorization
	encoded, err := json.Marshal(headers)
	if err != nil {
		return nil, fmt.Errorf("indexer: encode outbox headers: %w", err)
	}
	msg["headers"] = encoded
	return json.Marshal(msg)
}

// drain publishes one batch of pending messages.
//
// The publish runs INSIDE the outbox's claim transaction (as the deliver callback), so the row
// locks are held across it and no other replica can process the same rows. That is what makes
// per-object ordering hold across replicas: the publisher's per-object lock is process-local and
// could never have provided it.
func (r *Relay) drain(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, relayPassTimeout)
	defer cancel()

	// Without a service credential every message would be REJECTED by the indexer for an empty
	// authorization header — but NATS would accept the publish, so the row would be retired as
	// delivered and the recovery this table exists for would be permanently lost. Skip the pass
	// instead: rows stay pending until the token is configured.
	if strings.TrimSpace(r.authorization) == "" {
		r.warnNoToken()
		return
	}

	// Loop while a pass makes progress. A pass claims at most ONE row per resource (that is
	// what keeps a resource's messages in order), so a brief with a queued create+update+delete
	// needs three passes. Waiting a full relayInterval between them would take 45s to drain a
	// backlog that is ready NOW — the recovery this table exists for should not be that slow.
	// Bounded by relayPassTimeout on the context and by a pass that publishes nothing.
	// maxPasses bounds the loop. A pass is only supposed to repeat while it drains a backlog,
	// but "published > 0" is progress as REPORTED by the outbox — a bug that published without
	// retiring would otherwise spin here forever, burning the broker on the same rows. The cap
	// turns that into a bounded pass and a resumed drain on the next tick.
	const maxPasses = 20
	total := 0
	for pass := 0; pass < maxPasses; pass++ {
		published, err := r.drainOnce(ctx)
		if err != nil {
			// Do not log a cancellation during shutdown as a failure.
			if parent.Err() == nil {
				slog.ErrorContext(ctx, "index relay could not drain the outbox", "error", err)
			}
			break
		}
		total += published
		// Nothing published means either an empty outbox or every remaining resource is blocked
		// behind a failed delivery. Either way another immediate pass would not help.
		if published == 0 {
			break
		}
		// Re-check the deadline: relayPassTimeout bounds the whole drain, not one pass.
		if ctx.Err() != nil {
			break
		}
	}
	if total > 0 {
		slog.InfoContext(ctx, "index relay published pending messages", "count", total)
	}
	r.prune(ctx, parent)
}

// prune trims published history so the outbox cannot grow without bound: every brief and
// campaign mutation writes a full JSONB payload, and nothing else ever deletes one.
//
// Runs after the drain, never before — delivery is the relay's job and must not queue behind
// housekeeping. A prune failure is logged and dropped: it costs disk, never correctness.
func (r *Relay) prune(ctx context.Context, parent context.Context) {
	if ctx.Err() != nil {
		return
	}
	deleted, err := r.outbox.PrunePublishedIndexMessages(ctx, 0, 0)
	if err != nil {
		if parent.Err() == nil {
			slog.ErrorContext(ctx, "index relay could not prune published outbox rows", "error", err)
		}
		return
	}
	if deleted > 0 {
		slog.InfoContext(ctx, "index relay pruned published outbox rows", "count", deleted)
	}
}

// drainOnce runs a single claim-and-publish pass.
func (r *Relay) drainOnce(ctx context.Context) (int, error) {
	return r.outbox.DrainPendingIndexMessages(ctx, 0, func(ctx context.Context, m *model.OutboxMessage) error {
		payload, aerr := r.stamp(m.Payload)
		if aerr != nil {
			// A row we cannot parse can never be published. Returning the error records it on
			// the row rather than retrying it silently forever.
			return aerr
		}
		return r.pub.PublishRaw(ctx, Subject(m.ObjectType), m.ObjectID, payload)
	})
}
