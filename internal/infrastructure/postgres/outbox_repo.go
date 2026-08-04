// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// OutboxRepo reads and retires index-outbox rows. Rows are WRITTEN by the repositories that own
// the resource, inside the same transaction as the resource itself — that co-commit is the whole
// point, so there is no standalone "enqueue" here for a caller to reach for by mistake.
type OutboxRepo struct {
	db *Pool
}

// NewOutboxRepo constructs an OutboxRepo.
func NewOutboxRepo(db *Pool) *OutboxRepo { return &OutboxRepo{db: db} }

// defaultOutboxBatch bounds one relay pass. Small: the relay runs frequently, and a large batch
// would hold a slow publish open while newer rows queue behind it.
const defaultOutboxBatch = 50

// PendingIndexMessages returns the oldest unpublished messages, oldest first.
//
// Ordering matters: these are replayed writes for the SAME resources the live publisher already
// serializes, so replaying them out of order could reinstate a stale document.
func (r *OutboxRepo) PendingIndexMessages(ctx context.Context, limit int) ([]*model.OutboxMessage, error) {
	if limit <= 0 {
		limit = defaultOutboxBatch
	}
	q := `SELECT id, object_type, object_id, payload, attempts
		FROM index_outbox
		WHERE published_at IS NULL
		ORDER BY created_at ASC
		LIMIT $1`
	rows, err := r.db.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending index messages: %w", err)
	}
	defer rows.Close()

	var out []*model.OutboxMessage
	for rows.Next() {
		var (
			m   model.OutboxMessage
			raw []byte
		)
		if serr := rows.Scan(&m.ID, &m.ObjectType, &m.ObjectID, &raw, &m.Attempts); serr != nil {
			return nil, fmt.Errorf("scan pending index message: %w", serr)
		}
		m.Payload = json.RawMessage(raw)
		out = append(out, &m)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate pending index messages: %w", rerr)
	}
	return out, nil
}

// MarkIndexMessagePublished retires a row after a confirmed publish.
func (r *OutboxRepo) MarkIndexMessagePublished(ctx context.Context, id int64) error {
	q := `UPDATE index_outbox SET published_at = now() WHERE id = $1 AND published_at IS NULL`
	if _, err := r.db.Exec(ctx, q, id); err != nil {
		return fmt.Errorf("mark index message %d published: %w", id, err)
	}
	return nil
}

// RecordIndexMessageFailure records a failed attempt WITHOUT retiring the row, so the next pass
// retries it. attempts/last_error make a persistently failing message visible rather than
// leaving it silently spinning.
func (r *OutboxRepo) RecordIndexMessageFailure(ctx context.Context, id int64, cause string) error {
	q := `UPDATE index_outbox
		SET attempts = attempts + 1, last_error = $2
		WHERE id = $1 AND published_at IS NULL`
	if _, err := r.db.Exec(ctx, q, id, truncateErr(cause)); err != nil {
		return fmt.Errorf("record index message %d failure: %w", id, err)
	}
	return nil
}

// maxOutboxErrLen bounds what a dependency's error text can write into the row.
const maxOutboxErrLen = 500

func truncateErr(s string) string {
	if len(s) <= maxOutboxErrLen {
		return s
	}
	return s[:maxOutboxErrLen]
}

// enqueueIndexMessage writes an outbox row inside an EXISTING transaction. Unexported and
// tx-scoped by signature: an outbox row that does not co-commit with its resource provides none
// of the guarantee the table exists for.
func enqueueIndexMessage(ctx context.Context, tx pgx.Tx, objectType, objectID string, payload []byte) error {
	q := `INSERT INTO index_outbox (object_type, object_id, payload) VALUES ($1,$2,$3)`
	if _, err := tx.Exec(ctx, q, objectType, objectID, payload); err != nil {
		return fmt.Errorf("enqueue index message for %s %s: %w", objectType, objectID, err)
	}
	return nil
}
