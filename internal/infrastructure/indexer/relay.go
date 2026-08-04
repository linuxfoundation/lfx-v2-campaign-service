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
type OutboxReader interface {
	PendingIndexMessages(ctx context.Context, limit int) ([]*model.OutboxMessage, error)
	MarkIndexMessagePublished(ctx context.Context, id int64) error
	RecordIndexMessageFailure(ctx context.Context, id int64, cause string) error
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
func (r *Relay) drain(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, relayPassTimeout)
	defer cancel()

	// Without a service credential every message would be REJECTED by the indexer for an empty
	// authorization header — but NATS would accept the publish, so drain would retire the row
	// as delivered and the recovery this table exists for would be permanently lost. Skip the
	// pass instead: rows stay pending until the token is configured.
	if strings.TrimSpace(r.authorization) == "" {
		r.warnNoToken()
		return
	}

	msgs, err := r.outbox.PendingIndexMessages(ctx, 0)
	if err != nil {
		// Do not log a cancellation during shutdown as a failure.
		if parent.Err() == nil {
			slog.ErrorContext(ctx, "index relay could not read the outbox", "error", err)
		}
		return
	}
	if len(msgs) == 0 {
		return
	}

	var published int
	for _, m := range msgs {
		// Stop early on shutdown rather than pushing through a cancelled context: the rows
		// stay pending and the next process takes them.
		if ctx.Err() != nil {
			break
		}
		payload, aerr := r.stamp(m.Payload)
		if aerr != nil {
			// A row we cannot parse can never be published; record it so it is visible rather
			// than retried silently forever.
			if rerr := r.outbox.RecordIndexMessageFailure(ctx, m.ID, aerr.Error()); rerr != nil {
				slog.ErrorContext(ctx, "index relay could not record an unparseable outbox row",
					"outbox_id", m.ID, "error", rerr)
			}
			continue
		}
		if perr := r.pub.PublishRaw(ctx, Subject(m.ObjectType), m.ObjectID, payload); perr != nil {
			if rerr := r.outbox.RecordIndexMessageFailure(ctx, m.ID, perr.Error()); rerr != nil {
				slog.ErrorContext(ctx, "index relay could not record a publish failure",
					"outbox_id", m.ID, "error", rerr)
			}
			continue
		}
		if merr := r.outbox.MarkIndexMessagePublished(ctx, m.ID); merr != nil {
			// The message WAS published but the row is still pending, so the next pass will
			// republish it. That is safe — the indexer overwrites by object id, so a duplicate
			// is a no-op — and is the right trade against dropping it.
			slog.ErrorContext(ctx, "index relay published a message but could not retire it (it will republish)",
				"outbox_id", m.ID, "error", merr)
			continue
		}
		published++
	}
	if published > 0 {
		slog.InfoContext(ctx, "index relay published pending messages",
			"count", published, "batch", len(msgs))
	}
}
