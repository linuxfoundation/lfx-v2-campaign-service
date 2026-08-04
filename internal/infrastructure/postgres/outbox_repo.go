// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
// The claim is a SHORT transaction that stamps a lease and commits. Publishing then happens with
// NO transaction and NO pool connection held — see DrainPendingIndexMessages for why that
// restructuring was required.
//
// Three properties have to hold together, and the naive "oldest N pending" query provides none
// of them:
//
//  1. EXCLUSIVITY. FOR UPDATE SKIP LOCKED makes the claim itself atomic between concurrent
//     pods, but its row locks end when this short transaction commits — long before the publish.
//     The leased_until stamp is what carries exclusivity across the publish: a row is claimable
//     only when its lease is absent or EXPIRED, so a second pod cannot take a row this pod is
//     still publishing. An expiry (rather than a permanent flag) is what makes a crashed pod
//     recoverable: its rows become claimable again instead of wedging their resource forever.
//
//  2. PER-RESOURCE ORDER. Exclusivity alone is not enough: SKIP LOCKED will happily skip an
//     older locked row for object X and hand this pod a NEWER row for the same X, publishing
//     the update before the create. The NOT EXISTS predicate is what prevents that — a row is
//     claimable only when no older pending row exists for the same (object_type, object_id).
//     One message per resource per pass; the next pass takes the successor. A failed delivery
//     therefore blocks only its OWN resource, which is the correct behaviour: publishing past
//     it would reorder that resource's history.
//
//     The predecessor subquery deliberately does NOT filter on the lease. An older row that is
//     currently LEASED by another pod is in-flight, not absent, so it must still block its
//     successor — otherwise this pod would publish the update while the peer is still publishing
//     the create, which is exactly the reordering this predicate exists to prevent. "Older
//     pending row exists" is the correct test regardless of who holds it.
//
//  3. NO STARVATION BY POISON MESSAGES. Blocking one resource is only acceptable if it stays
//     confined to that resource. `attempts` was recorded but never affected ELIGIBILITY, so a
//     row that can never be delivered was re-selected on EVERY pass — and once enough of them
//     accumulate as the oldest resource heads they consume the whole batch, starving every
//     newer write in the table. The last_attempt_at predicate applies exponential backoff, so a
//     persistently failing row yields its slot. Measured on 50 poison heads plus one new write:
//     without backoff the batch was 50 poison rows and the new write was never indexed; with
//     backoff the batch was the new write alone.
//
// The backoff uses clock_timestamp(), NOT now(). now() is TRANSACTION-START time (the same trap
// that makes created_at unusable for ordering, below), and the drain holds ONE transaction open
// across the entire pass. Stamping a failure with now() would record when the PASS began rather
// than when the attempt failed, and comparing against now() would test a clock frozen at pass
// start — both understating the wait by up to the pass duration, in the direction that reopens
// the starvation this predicate exists to close.
//
// Ordering is by id, NOT created_at. created_at defaults to now(), which in PostgreSQL is
// TRANSACTION-START time — a transaction that began earlier but committed its write later gets
// an EARLIER created_at, so sorting by it can invert the committed order. id comes from a
// BIGSERIAL assigned at INSERT, which does not have that inversion.
// The lease is stamped in the SAME statement that selects the rows, so the claim is atomic: a
// concurrent pod either sees the lease or is excluded by SKIP LOCKED, never neither. Splitting
// the SELECT and the UPDATE would leave a window where two pods both read an unleased row.
const drainClaimQuery = `WITH claimable AS (
	SELECT o.id
	FROM index_outbox o
	WHERE o.published_at IS NULL
	  AND (o.leased_until IS NULL OR o.leased_until < clock_timestamp())
	  AND (
	      o.last_attempt_at IS NULL
	      OR o.last_attempt_at < clock_timestamp() - LEAST(
	             POWER(2, LEAST(o.attempts, ` + maxBackoffShiftSQL + `))::int,
	             ` + maxBackoffSecondsSQL + `
	         ) * INTERVAL '1 second'
	  )
	  AND NOT EXISTS (
	      SELECT 1 FROM index_outbox p
	      WHERE p.published_at IS NULL
	        AND p.object_type = o.object_type
	        AND p.object_id = o.object_id
	        AND p.id < o.id
	  )
	ORDER BY o.id ASC
	LIMIT $1
	FOR UPDATE SKIP LOCKED
)
UPDATE index_outbox o
SET leased_until = clock_timestamp() + ($2::text)::interval,
    leased_by    = $3
FROM claimable c
WHERE o.id = c.id
RETURNING o.id, o.object_type, o.object_id, o.payload, o.attempts, o.leased_until`

// Backoff bounds, inlined into drainClaimQuery. A failed row waits 2^attempts seconds before it
// is eligible again, capped so a long-failing message still retries roughly hourly rather than
// receding forever — the outage it is waiting out may end at any time.
//
// The shift is capped SEPARATELY from the seconds because POWER(2, n) overflows int well before
// the seconds cap would bite: attempts is unbounded, so 2^60 must never be evaluated.
const (
	maxBackoffShiftSQL   = "12"   // 2^12 = 4096s, past the seconds cap below
	maxBackoffSecondsSQL = "3600" // retry a persistently failing row at most hourly
)

// defaultOutboxBatch bounds one relay pass. Small: the relay runs frequently, and a large batch
// would hold a slow publish open while newer rows queue behind it.
const defaultOutboxBatch = 50

// leaseDuration is how long a claim stays exclusive.
//
// It must COMFORTABLY EXCEED the worst-case publish of a whole batch, or a lease expires while
// this pod is still working and a peer republishes rows behind it. The relay bounds a pass at
// its relayPassTimeout, mirrored below, so 60s leaves the same margin again. The number is
// duplicated rather than imported because this package must not depend on indexer (the
// dependency runs the other way, as TestIndexObjectTypesMatchTheIndexer records for the object
// types); TestClaimStampsALeaseThatOutlastsAPass pins the relationship on this side.
//
// The cost of a longer lease is only recovery latency after a pod CRASHES: its rows sit until
// the lease expires. 60s against a 15s relay interval is a bounded, acceptable delay, and it is
// strictly better than the alternative failure — two pods publishing one resource out of order.
const leaseDuration = 60 * time.Second

// relayPassBudget mirrors the indexer's relayPassTimeout — the longest a single drain pass can
// run, and therefore the longest a claimed row can be mid-publish. Duplicated for the reason
// given above; if the relay's bound grows past this, leaseDuration must grow with it.
const relayPassBudget = 30 * time.Second

// settleTimeout bounds the short transaction that records one publish outcome. It runs on a
// context detached from the pass, so it needs its own bound: without one a wedged database at
// shutdown could hang the relay past the budget Relay.Stop enforces.
const settleTimeout = 5 * time.Second

// leaseOwner identifies this process in leased_by. HOSTNAME is the pod name under Kubernetes,
// which is what an operator needs to find the replica holding a stuck resource. The PID suffix
// keeps two processes on one host (tests, local runs) distinct.
//
// Correctness does not depend on this being unique — the lease guard also requires an unexpired
// leased_until, and rows are claimed under SKIP LOCKED — but a collision would make the
// diagnostic misleading, which is the whole reason the column exists.
func leaseOwner() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%s/%d", host, os.Getpid())
}

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
	// PHASE 1 — claim. A SHORT transaction: stamp the lease, commit, release the connection.
	claimed, err := r.claimBatch(ctx, limit)
	if err != nil {
		return 0, err
	}
	if len(claimed) == 0 {
		return 0, nil
	}

	// PHASE 2 — publish, with NO transaction and NO pool connection held.
	//
	// deliver is a NATS request/reply that waits on the indexer, and the pass budget allows up to
	// relayPassTimeout (30s) across a batch. Doing this inside the claim transaction pinned a
	// pool connection for that whole time: with a small pool a slow indexer blocked every
	// brief/campaign write and the readiness probe, converting the broker degradation this table
	// exists to ISOLATE into a service outage. It also defeated the bounded Relay.Stop, because
	// the pool.Close() that follows waits on a checked-out connection.
	for _, c := range claimed {
		// Stop early on shutdown rather than pushing through a cancelled context. Unretired rows
		// keep their lease, which expires on its own, so the next process picks them up.
		if ctx.Err() != nil {
			break
		}
		derr := deliver(ctx, c.msg)

		// PHASE 3 — retire, in its own short transaction, guarded on still holding the lease.
		// Uses a context detached from ctx: on shutdown the publish above may have SUCCEEDED,
		// and a cancelled ctx would skip the retire and republish a delivered message next pass.
		if rerr := r.settle(ctx, c, derr); rerr != nil {
			return published, rerr
		}
		if derr == nil {
			published++
		}
	}
	return published, nil
}

// claimedRow pairs a claimed message with the lease token that authorises retiring it.
type claimedRow struct {
	msg   *model.OutboxMessage
	owner string
}

// claimBatch runs the claim in one short transaction and commits before returning, so no pool
// connection is held across the publish that follows.
func (r *OutboxRepo) claimBatch(ctx context.Context, limit int) ([]claimedRow, error) {
	owner := leaseOwner()
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("drain index outbox: begin claim tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// See drainClaimQuery for the properties this claim holds at once: exclusivity via the
	// lease, per-resource ordering, and backoff so a poison row cannot starve the batch.
	rows, qerr := tx.Query(ctx, drainClaimQuery, limit, leaseDuration.String(), owner)
	if qerr != nil {
		return nil, fmt.Errorf("claim pending index messages: %w", qerr)
	}
	var claimed []claimedRow
	for rows.Next() {
		var (
			m     model.OutboxMessage
			raw   []byte
			until time.Time
		)
		if serr := rows.Scan(&m.ID, &m.ObjectType, &m.ObjectID, &raw, &m.Attempts, &until); serr != nil {
			rows.Close()
			return nil, fmt.Errorf("scan pending index message: %w", serr)
		}
		m.Payload = json.RawMessage(raw)
		claimed = append(claimed, claimedRow{msg: &m, owner: owner})
	}
	rows.Close()
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate pending index messages: %w", rerr)
	}
	if cerr := tx.Commit(ctx); cerr != nil {
		// Nothing was leased, so the rows stay claimable. Returning empty is correct.
		return nil, fmt.Errorf("drain index outbox: commit claim: %w", cerr)
	}
	return claimed, nil
}

// settle records the outcome of one publish in its own short transaction.
//
// The context is DETACHED from the pass context (keeping only its values) and given its own
// small budget. On shutdown the publish may have already succeeded, and settling under a
// cancelled context would leave a delivered message pending — republished next pass. That is
// safe but wasteful, and for a failure it would lose the attempt stamp that drives the backoff,
// letting a poison row spin at full rate.
func (r *OutboxRepo) settle(ctx context.Context, c claimedRow, deliverErr error) error {
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()

	tx, err := r.db.Begin(settleCtx)
	if err != nil {
		return fmt.Errorf("settle index message %d: begin tx: %w", c.msg.ID, err)
	}
	defer func() { _ = tx.Rollback(settleCtx) }()

	if deliverErr != nil {
		if rerr := recordFailureTx(settleCtx, tx, c.msg.ID, deliverErr.Error(), c.owner); rerr != nil {
			return fmt.Errorf("record index message %d failure: %w", c.msg.ID, rerr)
		}
	} else {
		// A false here means the lease expired mid-publish and another pod owns the row now.
		// Not an error: that pod republishes, and the indexer overwrites by object id.
		if _, merr := markPublishedTx(settleCtx, tx, c.msg.ID, c.owner); merr != nil {
			return fmt.Errorf("mark index message %d published: %w", c.msg.ID, merr)
		}
	}
	if cerr := tx.Commit(settleCtx); cerr != nil {
		// A genuinely published message stays PENDING and republishes next pass. Safe: the
		// indexer overwrites by object id, so a duplicate is a no-op.
		return fmt.Errorf("settle index message %d: commit: %w", c.msg.ID, cerr)
	}
	return nil
}

// markPublishedSQL retires a row, but ONLY while this pod still holds the lease it claimed.
//
// The lease can expire mid-publish (a long broker stall), at which point another pod may claim
// and publish the same row. Retiring unconditionally would then let this pod's late write race
// the peer's. The leased_by/leased_until guard makes the retire a no-op in that case: the row
// stays owned by whoever holds the live lease. The message may be delivered twice, which is
// safe — the indexer overwrites by object id — but it is never retired by a pod that no longer
// owns it.
//
// published_at uses clock_timestamp(), not now(): now() would stamp TRANSACTION-START time, and
// this runs in its own short transaction after a publish that may have taken seconds.
const markPublishedSQL = `UPDATE index_outbox
	SET published_at = clock_timestamp(), leased_until = NULL, leased_by = NULL
	WHERE id = $1
	  AND published_at IS NULL
	  AND leased_by = $2
	  AND leased_until > clock_timestamp()`

func markPublishedTx(ctx context.Context, tx pgx.Tx, id int64, leaseOwner string) (bool, error) {
	tag, err := tx.Exec(ctx, markPublishedSQL, id, leaseOwner)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// recordFailureSQL advances the attempt count and STAMPS the attempt time. The stamp is what
// makes the backoff in drainClaimQuery work: without it a permanently failing row keeps a NULL
// last_attempt_at, stays eligible on every pass, and starves the batch.
//
// It also RELEASES the lease. A failed row must become claimable again as soon as its backoff
// elapses; leaving the lease set would block it (and, via the predecessor rule, its whole
// resource) until the lease expired on its own, stacking an arbitrary lease wait on top of the
// backoff the row already serves.
//
// Guarded on leased_by like the retire: a pod whose lease already expired must not stamp an
// attempt onto a row another pod now owns, or it would corrupt that pod's backoff accounting.
const recordFailureSQL = `UPDATE index_outbox
	SET attempts = attempts + 1,
	    last_error = $2,
	    last_attempt_at = clock_timestamp(),
	    leased_until = NULL,
	    leased_by = NULL
	WHERE id = $1
	  AND published_at IS NULL
	  AND leased_by = $3
	  AND leased_until > clock_timestamp()`

func recordFailureTx(ctx context.Context, tx pgx.Tx, id int64, cause, leaseOwner string) error {
	_, err := tx.Exec(ctx,
		recordFailureSQL,
		id, truncateErr(cause), leaseOwner)
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
