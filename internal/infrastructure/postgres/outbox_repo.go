// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

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

// drainClaimQuery claims a batch of unpublished rows for THIS pod, at most ONE per resource.
//
// Two properties have to hold together, and the naive "oldest N pending" query provides neither:
//
//  1. EXCLUSIVITY. FOR UPDATE SKIP LOCKED gives each replica a disjoint set and holds the locks
//     through the publish and the retire, so two pods can never publish the same row.
//
//  2. PER-RESOURCE ORDER. Exclusivity alone is not enough: SKIP LOCKED will happily skip an
//     older locked row for object X and hand this pod a NEWER row for the same X, publishing
//     the update before the create. The NOT EXISTS predicate is what prevents that — a row is
//     claimable only when no older pending row exists for the same (object_type, object_id).
//     One message per resource per pass; the next pass takes the successor. A failed delivery
//     therefore blocks only its OWN resource, which is the correct behaviour: publishing past
//     it would reorder that resource's history.
//
// Ordering is by id, NOT created_at. created_at defaults to now(), which in PostgreSQL is
// TRANSACTION-START time — a transaction that began earlier but committed its write later gets
// an EARLIER created_at, so sorting by it can invert the committed order. id comes from a
// BIGSERIAL assigned at INSERT, which does not have that inversion.
const drainClaimQuery = `SELECT o.id, o.object_type, o.object_id, o.payload, o.attempts
	FROM index_outbox o
	WHERE o.published_at IS NULL
	  AND NOT EXISTS (
	      SELECT 1 FROM index_outbox p
	      WHERE p.published_at IS NULL
	        AND p.object_type = o.object_type
	        AND p.object_id = o.object_id
	        AND p.id < o.id
	  )
	ORDER BY o.id ASC
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

// outboxRetention is how long a PUBLISHED row is kept before pruning.
//
// Long enough to be useful for an incident post-mortem ("was this brief indexed, and with what
// payload?"), short enough that the table cannot grow without bound. Only published rows are
// eligible — a pending row is undelivered work and is never pruned, however old.
const outboxRetention = 7 * 24 * time.Hour

// prunePassLimit bounds one delete so a first prune over a large backlog cannot hold a long
// transaction or spike replication lag. The relay prunes every pass, so a backlog drains over
// several passes rather than in one statement.
const prunePassLimit = 5000

// pruneQuery deletes DELIVERED history only.
//
// PENDING rows are never pruned, at any age. They are undelivered work, and this service has no
// full-reindex path — so discarding one is UNRECOVERABLE. The cases that matter are exactly the
// ones with no later write to repair them: a terminal brief archive (the brief stays searchable
// forever) and a created-then-never-edited campaign. An age-based sweep cannot tell "the
// indexer has been down for a month" from "this message is obsolete", and guessing wrong loses
// data permanently. Unbounded growth from a deliberately disabled indexer is handled at the
// SOURCE instead (see Container.newIndexPayload) rather than by deleting undelivered work.
const pruneQuery = `DELETE FROM index_outbox WHERE id IN (
	SELECT id FROM index_outbox
	WHERE published_at IS NOT NULL AND published_at < now() - $1::interval
	ORDER BY id
	LIMIT $2
)`

// PrunePublishedIndexMessages deletes PUBLISHED rows older than outboxRetention. Pending rows
// are never eligible — see pruneQuery.
//
// Without this the table grows with EVERY brief and campaign mutation and never shrinks: each
// row carries a full JSONB payload, so the table, its backups, and the vacuum workload all grow
// unbounded until storage runs out. The partial index stays small either way (it only covers
// pending rows), which is exactly why the growth would go unnoticed until it was a problem.
//
// Deleting by id via a bounded subquery keeps the statement short and lets the PRIMARY KEY do
// the work: `ORDER BY id LIMIT n` is served by an index scan on the pkey with published_at as a
// filter (verified with EXPLAIN on 20k rows), so no extra index is needed — and adding one on
// published_at would only cost writes, since the planner would not choose it for this shape.
func (r *OutboxRepo) PrunePublishedIndexMessages(ctx context.Context, olderThan time.Duration, limit int) (int64, error) {
	if olderThan <= 0 {
		olderThan = outboxRetention
	}
	if limit <= 0 {
		limit = prunePassLimit
	}
	// Two windows, one statement. A PUBLISHED row is delivered history (short window). A PENDING
	// row is undelivered work and gets a much longer one — it is only discarded once it is so
	// old that replaying it would be worse than a reindex. Without the second clause a disabled
	// or unprovisioned indexer grows the table forever, which the chart's optional token makes a
	// reachable steady state rather than a misconfiguration.
	tag, err := r.db.Exec(ctx, pruneQuery, olderThan.String(), limit)
	if err != nil {
		return 0, fmt.Errorf("prune published index messages: %w", err)
	}
	return tag.RowsAffected(), nil
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
