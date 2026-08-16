// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
)

// The repo's own statements, verbatim. Exercising the SQL rather than the repo method is
// deliberate: the bug these tests exist for was a BIND-ARGUMENT mismatch — a `$3`
// placeholder with two arguments — which no Go type check can see and which lives entirely
// in the statement. Copying the text here means a drift between the two is itself a
// failure worth having, and it lets these run against the migrated schema without
// constructing the unexported pool wrapper.
const (
	liveClaimSQL = `INSERT INTO campaigns
		(project_id, brief_id, job_id, platform, variant, campaign_name, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, '', 'pending', $6, $6)
		ON CONFLICT (brief_id, platform, variant) WHERE status <> 'deleted' DO NOTHING`
	liveReleaseSQL = `DELETE FROM campaigns WHERE brief_id=$1 AND platform=$2 AND variant=$3 AND status='pending'`
	liveCountSQL   = `SELECT count(*) FROM campaigns WHERE brief_id=$1 AND platform=$2 AND variant=$3 AND status <> 'deleted'`
)

// The claim/release round trip, against real Postgres.
//
// This exists because a bind-argument bug survived every unit test: DeleteDispatchClaim's
// query gained a `variant=$3` placeholder while Exec still passed two arguments, so EVERY
// claim release would have failed at runtime and stranded a 'pending' row — permanently
// blocking that slot, since nothing reaps pending campaigns. A query string is not type
// checked, so only a live database can catch it. Both review bots flagged it on #130.
func TestLiveClaimAndReleaseRoundTrip(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	briefID := insertBrief(ctx, t, pool)

	if _, err := pool.Exec(ctx, liveClaimSQL, "tlf", briefID, "00000000-0000-4000-8000-0000000000a1", "google-ads", "demand-gen", nil); err != nil {
		t.Fatalf("claim: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, liveCountSQL, briefID, "google-ads", "demand-gen").Scan(&n); err != nil || n != 1 {
		t.Fatalf("claim did not land in the demand-gen slot: n=%d err=%v", n, err)
	}

	// The statement whose argument list was wrong. A `$3` placeholder with two bound
	// arguments fails here and nowhere else.
	if _, err := pool.Exec(ctx, liveReleaseSQL, briefID, "google-ads", "demand-gen"); err != nil {
		t.Fatalf("release: %v — a failing release strands the pending row and blocks the slot forever", err)
	}
	if err := pool.QueryRow(ctx, liveCountSQL, briefID, "google-ads", "demand-gen").Scan(&n); err != nil || n != 0 {
		t.Fatalf("the pending row survived the release: n=%d err=%v", n, err)
	}
}

// The release must be SCOPED to its variant. Releasing a failed Demand Gen claim while a
// Search campaign is mid-dispatch on the same brief must not delete the Search claim: they
// are different campaigns, and a stray delete would let a second dispatcher win a claim
// already in flight and create a duplicate paid campaign.
func TestLiveReleaseDoesNotTouchAnotherVariantsClaim(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	briefID := insertBrief(ctx, t, pool)

	for _, v := range []string{"default", "demand-gen"} {
		if _, err := pool.Exec(ctx, liveClaimSQL, "tlf", briefID, "00000000-0000-4000-8000-0000000000a1", "google-ads", v, nil); err != nil {
			t.Fatalf("claim %q: %v — one brief must hold a claim per variant", v, err)
		}
	}

	if _, err := pool.Exec(ctx, liveReleaseSQL, briefID, "google-ads", "demand-gen"); err != nil {
		t.Fatalf("release: %v", err)
	}

	var n int
	if err := pool.QueryRow(ctx, liveCountSQL, briefID, "google-ads", "default").Scan(&n); err != nil || n != 1 {
		t.Errorf("the default-variant claim was destroyed by another variant's release (n=%d err=%v) — a dispatch in flight would lose its claim and could be double-created", n, err)
	}
	if err := pool.QueryRow(ctx, liveCountSQL, briefID, "google-ads", "demand-gen").Scan(&n); err != nil || n != 0 {
		t.Errorf("the demand-gen claim should have been released: n=%d err=%v", n, err)
	}
}
