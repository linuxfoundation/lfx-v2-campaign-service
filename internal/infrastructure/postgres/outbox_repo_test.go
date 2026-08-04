// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/indexer"
	"github.com/stretchr/testify/assert"
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
