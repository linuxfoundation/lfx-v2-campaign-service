// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package indexer

import (
	"context"
	"log/slog"
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

	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

// NewRelay constructs a Relay.
func NewRelay(outbox OutboxReader, pub RawPublisher) *Relay {
	return &Relay{outbox: outbox, pub: pub}
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

// drain publishes one batch of pending messages.
func (r *Relay) drain(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, relayPassTimeout)
	defer cancel()

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
		if perr := r.pub.PublishRaw(ctx, Subject(m.ObjectType), m.ObjectID, m.Payload); perr != nil {
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
