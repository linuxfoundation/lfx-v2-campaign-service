// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/indexer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIndexObjectTypesMatchTheIndexer pins the duplicated object-type constants.
//
// This package must not import the indexer (the dependency runs the other way), so the values
// are copied. A copy that drifts routes every outbox row to the wrong subject — silently, since
// nothing else compares them.
func TestIndexObjectTypesMatchTheIndexer(t *testing.T) {
	assert.Equal(t, indexer.ObjectTypeBrief, indexObjectTypeBrief)
	assert.Equal(t, indexer.ObjectTypeCampaign, indexObjectTypeCampaign)
}

// TestDrainClaimsOneRowPerResourceInOrder pins the two properties that make the drain correct
// with multiple replicas. Both were verified against a live PostgreSQL 16 before being written
// down here; this test guards them from silent edits.
//
//  1. EXCLUSIVITY (FOR UPDATE SKIP LOCKED): each pod gets a disjoint set and holds the locks
//     through the publish AND the retire, so two pods can never publish the same row.
//
//  2. PER-RESOURCE ORDER (the NOT EXISTS predecessor check): exclusivity alone is NOT enough.
//     SKIP LOCKED will skip an older locked row for object X and hand a second pod the NEWER row
//     for the same X — publishing an update before its create. Gating on "no older pending row
//     for this object" is what stops that. Confirmed empirically: with pod A holding b1's create,
//     a concurrent pod claimed ZERO rows rather than b1's update.
//
// Ordering is by id, NOT created_at: created_at defaults to now(), which is TRANSACTION-START
// time in PostgreSQL, so a transaction that began earlier but wrote later gets an earlier
// created_at and sorting by it can invert the committed order.
//
// Asserted against the SQL text because this package has no live-database harness in CI; the
// alternative is no regression coverage at all for the properties that make the drain correct.
func TestDrainClaimsOneRowPerResourceInOrder(t *testing.T) {
	q := drainClaimQuery

	assert.Contains(t, q, "FOR UPDATE SKIP LOCKED",
		"an unclaimed read lets every replica drain the same rows")
	assert.Contains(t, q, "published_at IS NULL",
		"the drain must only consider unretired rows")

	// The predecessor check, keyed on the SAME resource and a LOWER id.
	assert.Contains(t, q, "NOT EXISTS",
		"without a predecessor check, SKIP LOCKED hands a second pod a newer row for a locked object")
	assert.Contains(t, q, "p.object_type = o.object_type")
	assert.Contains(t, q, "p.object_id = o.object_id")
	assert.Contains(t, q, "p.id < o.id",
		"the predecessor check must compare ids, the commit-ordered key")

	// Order by id, never created_at — see the doc comment.
	orderIdx := strings.Index(q, "ORDER BY")
	limitIdx := strings.Index(q, "LIMIT")
	if orderIdx < 0 || limitIdx < orderIdx {
		t.Fatalf("expected ORDER BY before LIMIT in the drain query, got:\n%s", q)
	}
	order := q[orderIdx:limitIdx]
	assert.Contains(t, order, "o.id", "id is the commit-ordered key")
	assert.NotContains(t, order, "created_at",
		"created_at is transaction-START time; sorting by it can invert the committed order")
}

// TestPendingIndexPartialIndexSupportsTheClaim pins the migration's index against the query it
// exists to serve. The predecessor check probes (object_type, object_id, id) — a (created_at)
// index cannot serve that, so every pass would re-scan retained published history, which is the
// bulk of the table.
func TestPendingIndexPartialIndexSupportsTheClaim(t *testing.T) {
	sql, err := os.ReadFile(filepath.Join("migrations", "000008_index_outbox.up.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	got := string(sql)
	assert.Contains(t, got, "ON index_outbox (object_type, object_id, id)",
		"the partial index must key the predecessor check's probe columns")
	assert.Contains(t, got, "WHERE published_at IS NULL",
		"the index must stay PARTIAL so it does not grow with published history")
}

// TestPruneNeverTouchesPendingRows pins the most consequential boundary in the retention path.
//
// PENDING rows are undelivered work and this service has NO full-reindex path, so discarding one
// is unrecoverable. The cases that matter are precisely the ones with no later write to repair
// them: a terminal brief archive (the brief would stay searchable forever) and a
// created-then-never-edited campaign.
//
// An age-based sweep cannot distinguish "the indexer has been down for a month" from "this
// message is obsolete", and guessing wrong loses data permanently — so it must not guess. An
// earlier revision aged pending rows out after 30 days; that was wrong, and this test exists so
// it does not come back. Unbounded growth from a DELIBERATELY disabled indexer is handled at the
// source instead (the service enqueues nothing when NATS_URL is explicitly empty).
func TestPruneNeverTouchesPendingRows(t *testing.T) {
	assert.Contains(t, pruneQuery, "published_at IS NOT NULL",
		"only DELIVERED history is eligible for pruning")
	assert.NotContains(t, pruneQuery, "published_at IS NULL",
		"a pending row must never be selected for deletion, at any age")
	assert.NotContains(t, pruneQuery, "created_at",
		"aging by created_at would sweep undelivered work, which is unrecoverable here")
}

// TestDrainBacksOffFailedRows pins the anti-starvation guard.
//
// `attempts` was recorded but never affected ELIGIBILITY, so a row that can never be delivered
// (a poison message) was re-selected on every pass. Once enough of them accumulate as the oldest
// resource heads they consume the entire batch forever — at which point a failure no longer
// "blocks only its own resource", it starves every newer write in the table.
//
// Reproduced on a live PostgreSQL 16 with 50 poison heads plus one new valid write:
//
//	without backoff -> batch = 50 poison rows, new write NOT included (never indexed)
//	with backoff    -> batch = the new write alone
//
// The cap on the exponent is separate from the cap on seconds ON PURPOSE: attempts is unbounded,
// so POWER(2, attempts) would overflow int long before the seconds cap applied. Verified with
// attempts=999999, which stays eligible on an hourly retry rather than erroring.
func TestDrainBacksOffFailedRows(t *testing.T) {
	// A never-attempted row must always be eligible: backoff may only delay RETRIES.
	assert.Contains(t, drainClaimQuery, "o.last_attempt_at IS NULL",
		"a row that has never been attempted must never be held back")
	assert.Contains(t, drainClaimQuery, "o.last_attempt_at < clock_timestamp()",
		"a failed row must wait before it is eligible again, measured against WALL-CLOCK time: "+
			"now() is frozen at transaction start and the drain holds one tx across the whole pass")
	assert.Contains(t, drainClaimQuery, "POWER(2,",
		"the wait grows with attempts, so a persistently failing row yields its slot")

	// Both caps must be present, and the exponent cap must be the smaller of the two.
	assert.Contains(t, drainClaimQuery, "LEAST(o.attempts, "+maxBackoffShiftSQL+")",
		"the EXPONENT must be capped independently: attempts is unbounded and 2^n overflows int")
	assert.Contains(t, drainClaimQuery, maxBackoffSecondsSQL,
		"a long-failing row must still retry periodically; the outage may end at any time")

	shift, err := strconv.Atoi(maxBackoffShiftSQL)
	require.NoError(t, err)
	secs, err := strconv.Atoi(maxBackoffSecondsSQL)
	require.NoError(t, err)
	assert.Less(t, shift, 31, "2^shift must fit in an int32 to avoid overflow in POWER()")
	assert.Greater(t, 1<<shift, secs,
		"the exponent cap must exceed the seconds cap, or the seconds cap could never apply")
	assert.LessOrEqual(t, secs, 3600, "a failing row must retry at least hourly")
}

// TestRecordFailureStampsTheAttemptTime pins the other half of the backoff: the predicate is
// inert unless every failure stamps last_attempt_at. Without it a poison row keeps a NULL
// timestamp and stays permanently eligible — exactly the starvation the backoff prevents.
func TestRecordFailureStampsTheAttemptTime(t *testing.T) {
	assert.Contains(t, recordFailureSQL, "last_attempt_at = clock_timestamp()",
		"the stamp must be WALL-CLOCK: now() would record when the PASS began, not when the "+
			"attempt failed, understating every backoff by up to the pass duration")
	assert.NotContains(t, recordFailureSQL, "= now()",
		"verified on live PostgreSQL: now() does not advance within a transaction")
	assert.Contains(t, recordFailureSQL, "attempts = attempts + 1",
		"and must advance the attempt count that sizes the backoff")
}
