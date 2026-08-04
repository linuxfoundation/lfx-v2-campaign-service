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

// drainClaimQuery claims a batch of unpublished rows for THIS pod.
//
// FOR UPDATE SKIP LOCKED is the load-bearing clause: it gives each replica a disjoint batch and
// holds the locks for the whole pass, so no two pods can publish the same object concurrently.
// ORDER BY created_at, id is total — created_at alone can tie at now() resolution, and an
// ambiguous order between an update and a delete is the interleaving this table prevents.
const drainClaimQuery = `SELECT id, object_type, object_id, payload, attempts
	FROM index_outbox
	WHERE published_at IS NULL
	ORDER BY created_at ASC, id ASC
	LIMIT $1
	FOR UPDATE SKIP LOCKED`

// defaultOutboxBatch bounds one relay pass. Small: the relay runs frequently, and a large batch
// would hold a slow publish open while newer rows queue behind it.
const defaultOutboxBatch = 50

// DrainPendingIndexMessages claims a batch of unpublished messages and publishes each one
// through deliver, retiring only what deliver confirms.
//
// The claim is what makes this correct with MULTIPLE REPLICAS. An unclaimed read let every pod
// load the SAME batch, so a slow pod could publish an earlier `updated` after a faster one had
// already published the later `deleted` for that brief — resurrecting an archived document and
// reopening the very race the outbox exists to close. Rolling deploys make this routine, not
// exotic: two pods overlap by design.
//
// SELECT ... FOR UPDATE SKIP LOCKED gives each pod a DISJOINT batch, and the row locks are held
// for the whole pass — through the publish and the retire — so no other replica can process
// those rows until this transaction ends. SKIP LOCKED (rather than plain FOR UPDATE) means a
// second pod moves on to unclaimed work instead of blocking behind this one.
//
// deliver returning an error leaves the row PENDING, with the failure recorded, for a later
// pass. A pass that dies mid-flight rolls back and the rows simply return to the pool.
func (r *OutboxRepo) DrainPendingIndexMessages(
	ctx context.Context,
	limit int,
	deliver func(context.Context, *model.OutboxMessage) error,
) (published int, err error) {
	if limit <= 0 {
		limit = defaultOutboxBatch
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("drain index outbox: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// ORDER BY created_at, id: these are replayed writes for the same resources, so replaying
	// them out of order could reinstate a stale document. id breaks ties, since two rows can
	// share a created_at at now() resolution and ordering must be total to be deterministic.
	rows, qerr := tx.Query(ctx, drainClaimQuery, limit)
	if qerr != nil {
		return 0, fmt.Errorf("claim pending index messages: %w", qerr)
	}
	var claimed []*model.OutboxMessage
	for rows.Next() {
		var (
			m   model.OutboxMessage
			raw []byte
		)
		if serr := rows.Scan(&m.ID, &m.ObjectType, &m.ObjectID, &raw, &m.Attempts); serr != nil {
			rows.Close()
			return 0, fmt.Errorf("scan pending index message: %w", serr)
		}
		m.Payload = json.RawMessage(raw)
		claimed = append(claimed, &m)
	}
	rows.Close()
	if rerr := rows.Err(); rerr != nil {
		return 0, fmt.Errorf("iterate pending index messages: %w", rerr)
	}
	if len(claimed) == 0 {
		return 0, nil
	}

	for _, m := range claimed {
		// Stop early on shutdown rather than pushing through a cancelled context. The rollback
		// releases the claim and the rows go back to the pool for the next process.
		if ctx.Err() != nil {
			break
		}
		if derr := deliver(ctx, m); derr != nil {
			if rerr := recordFailureTx(ctx, tx, m.ID, derr.Error()); rerr != nil {
				return published, fmt.Errorf("record index message %d failure: %w", m.ID, rerr)
			}
			continue
		}
		if merr := markPublishedTx(ctx, tx, m.ID); merr != nil {
			return published, fmt.Errorf("mark index message %d published: %w", m.ID, merr)
		}
		published++
	}

	if cerr := tx.Commit(ctx); cerr != nil {
		// The commit failed, so NOTHING was retired — including messages that were genuinely
		// published. They stay pending and republish next pass, which is safe: the indexer
		// overwrites by object id, so a duplicate is a no-op.
		return 0, fmt.Errorf("drain index outbox: commit: %w", cerr)
	}
	return published, nil
}

func markPublishedTx(ctx context.Context, tx pgx.Tx, id int64) error {
	_, err := tx.Exec(ctx, `UPDATE index_outbox SET published_at = now() WHERE id = $1 AND published_at IS NULL`, id)
	return err
}

func recordFailureTx(ctx context.Context, tx pgx.Tx, id int64, cause string) error {
	_, err := tx.Exec(ctx,
		`UPDATE index_outbox SET attempts = attempts + 1, last_error = $2 WHERE id = $1 AND published_at IS NULL`,
		id, truncateErr(cause))
	return err
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
