// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
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

// TestDrainClaimsRowsExclusively pins the claim in the drain query.
//
// Without FOR UPDATE SKIP LOCKED every replica loads the SAME batch, so a slow pod can publish
// an earlier `updated` after a faster one already published the later `deleted` for that
// brief — resurrecting an archived document and reopening the race the outbox exists to close.
// Rolling deploys make overlapping pods routine, not exotic.
//
// SKIP LOCKED specifically (not a bare FOR UPDATE) is what keeps a second pod moving on to
// unclaimed work rather than blocking behind this one.
//
// Asserted against the SQL text because this package has no live-database harness; the
// alternative is no coverage at all for the property that makes multi-replica drain correct.
func TestDrainClaimsRowsExclusively(t *testing.T) {
	q := drainClaimQuery

	assert.Contains(t, q, "FOR UPDATE SKIP LOCKED",
		"an unclaimed read lets every replica drain the same rows")
	assert.Contains(t, q, "published_at IS NULL",
		"the drain must only consider unretired rows")
	// Ordering must be TOTAL. created_at alone is not: two rows can share a now() value, and an
	// ambiguous order between an update and a delete for the same object is exactly the
	// interleaving this table prevents.
	orderIdx := strings.Index(q, "ORDER BY")
	limitIdx := strings.Index(q, "LIMIT")
	if orderIdx < 0 || limitIdx < orderIdx {
		t.Fatalf("expected ORDER BY before LIMIT in the drain query, got:\n%s", q)
	}
	order := q[orderIdx:limitIdx]
	assert.Contains(t, order, "created_at", "replay order is what keeps a stale document from being reinstated")
	assert.Contains(t, order, "id", "created_at can tie at now() resolution; id makes the order total")
}
