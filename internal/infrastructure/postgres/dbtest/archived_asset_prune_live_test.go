// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
)

// ageBrief sets a brief's status and its updated_at to `age` ago. For an archived brief
// updated_at is the archival time (archiveBriefQuery sets it, and nothing updates an archived
// brief afterwards), so this is exactly what archiving it `age` ago would have produced.
func ageBrief(ctx context.Context, t *testing.T, pool *pgxpool.Pool, briefID, status string, age time.Duration) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE campaign_briefs SET status = $2, updated_at = now() - $3::interval WHERE id = $1`,
		briefID, status, age.String()); err != nil {
		t.Fatalf("age brief %s: %v", briefID, err)
	}
}

// drainArchivedAssetPrune runs batches of two until one comes back short, as one sweeper pass
// does, and returns how many batches ran.
//
// The prune is cluster-global, not scoped to a test's rows, so the tests in this file must stay
// NON-parallel: a concurrent test's long-archived assets would be deleted too and change the
// batch counts asserted here.
func drainArchivedAssetPrune(ctx context.Context, t *testing.T, repo *postgres.CreativeAssetRepo) int {
	t.Helper()
	for i := range 1000 {
		n, ran, err := repo.PruneArchivedBriefAssets(ctx, domain.ArchivedBriefAssetRetention, 2)
		if err != nil {
			t.Fatalf("PruneArchivedBriefAssets: %v", err)
		}
		if !ran {
			t.Fatal("the prune lock was held although no other pruner is running")
		}
		if n < 2 {
			return i + 1
		}
	}
	t.Fatal("the prune never drained")
	return 0
}

// TestLivePruneArchivedBriefAssetsDeletesOnlyPastTheWindow (#171): assets of a brief archived 91
// days ago are deleted, in batches (three assets, batch of two); a brief archived 89 days ago and
// an ACTIVE brief untouched for 200 days keep theirs. Age alone must never make an active brief's
// assets eligible, and archival alone must not either until the window has passed.
func TestLivePruneArchivedBriefAssetsDeletesOnlyPastTheWindow(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewCreativeAssetRepo(&postgres.Pool{Pool: pool})

	oldBrief, oldProject := insertApprovedBrief(ctx, t, pool)
	recentBrief, recentProject := insertApprovedBrief(ctx, t, pool)
	activeBrief, activeProject := insertApprovedBrief(ctx, t, pool)

	var expired []string
	for _, sum := range []string{"p171a1", "p171a2", "p171a3"} {
		expired = append(expired, insertCreativeAsset(ctx, t, repo, oldProject, oldBrief, 1200, 628, sum).ID)
	}
	recent := insertCreativeAsset(ctx, t, repo, recentProject, recentBrief, 1200, 628, "p171b1").ID
	active := insertCreativeAsset(ctx, t, repo, activeProject, activeBrief, 1200, 628, "p171c1").ID

	ageBrief(ctx, t, pool, oldBrief, "archived", 91*24*time.Hour)
	ageBrief(ctx, t, pool, recentBrief, "archived", 89*24*time.Hour)
	ageBrief(ctx, t, pool, activeBrief, "approved", 200*24*time.Hour)

	if batches := drainArchivedAssetPrune(ctx, t, repo); batches < 2 {
		t.Errorf("the prune drained in %d batch(es); three assets at a limit of two need at least two", batches)
	}

	for _, id := range expired {
		if assetExists(ctx, t, pool, id) {
			t.Errorf("asset %s of a brief archived 91 days ago survived the prune", id)
		}
	}
	if !assetExists(ctx, t, pool, recent) {
		t.Error("an asset of a brief archived 89 days ago was deleted inside the 90-day window")
	}
	if !assetExists(ctx, t, pool, active) {
		t.Error("an asset of an ACTIVE brief was deleted: only archival starts the window")
	}

	// Idempotent: a second pass finds nothing and fails nothing.
	n, ran, err := repo.PruneArchivedBriefAssets(ctx, domain.ArchivedBriefAssetRetention, 0)
	if err != nil || !ran || n != 0 {
		t.Errorf("second pass = (%d, %v, %v), want (0, true, nil)", n, ran, err)
	}
}

// TestLivePruneArchivedBriefAssetsSkipsWhileAnotherReplicaPrunes: with the prune lock held by
// another session's transaction (another replica mid-batch), a batch neither waits nor deletes;
// it reports ran=false. Once that transaction ends, the lock is free again (transaction-scoped).
func TestLivePruneArchivedBriefAssetsSkipsWhileAnotherReplicaPrunes(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewCreativeAssetRepo(&postgres.Pool{Pool: pool})

	brief, project := insertApprovedBrief(ctx, t, pool)
	asset := insertCreativeAsset(ctx, t, repo, project, brief, 1200, 628, "p171d1").ID
	ageBrief(ctx, t, pool, brief, "archived", 120*24*time.Hour)

	other, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the other replica's transaction: %v", err)
	}
	// The same lock PruneArchivedBriefAssets takes: class "CAPR", key 0.
	if _, err := other.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2)`, int32(0x43415052), int32(0)); err != nil {
		_ = other.Rollback(ctx)
		t.Fatalf("take the prune lock: %v", err)
	}

	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	n, ran, err := repo.PruneArchivedBriefAssets(cctx, 0, 0)
	cancel()
	if err != nil {
		_ = other.Rollback(ctx)
		t.Fatalf("PruneArchivedBriefAssets while the lock is held: %v (it must skip, not wait)", err)
	}
	if ran || n != 0 {
		t.Errorf("batch with the lock held = (%d, ran=%v), want (0, ran=false)", n, ran)
	}
	if !assetExists(ctx, t, pool, asset) {
		t.Error("a batch deleted although another replica held the prune lock")
	}

	_ = other.Rollback(ctx)
	drainArchivedAssetPrune(ctx, t, repo)
	if assetExists(ctx, t, pool, asset) {
		t.Error("the asset survived once the lock was released")
	}
}

// TestLivePruneArchivedBriefAssetsZeroArgumentsFallBack: a zero window must fall back to the 90-day
// retention, never "archived at all", and a zero limit to the batch bound, never LIMIT 0. With
// the lock free, (0, 0) deletes the asset of a brief archived 91 days ago (so the limit is not 0)
// and keeps the one archived 89 days ago (so the window is not 0).
func TestLivePruneArchivedBriefAssetsZeroArgumentsFallBack(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewCreativeAssetRepo(&postgres.Pool{Pool: pool})

	oldBrief, oldProject := insertApprovedBrief(ctx, t, pool)
	recentBrief, recentProject := insertApprovedBrief(ctx, t, pool)
	expired := insertCreativeAsset(ctx, t, repo, oldProject, oldBrief, 1200, 628, "p171e1").ID
	recent := insertCreativeAsset(ctx, t, repo, recentProject, recentBrief, 1200, 628, "p171e2").ID
	ageBrief(ctx, t, pool, oldBrief, "archived", 91*24*time.Hour)
	ageBrief(ctx, t, pool, recentBrief, "archived", 89*24*time.Hour)

	n, ran, err := repo.PruneArchivedBriefAssets(ctx, 0, 0)
	if err != nil || !ran {
		t.Fatalf("PruneArchivedBriefAssets(0, 0) = (%d, %v, %v)", n, ran, err)
	}
	if n < 1 || assetExists(ctx, t, pool, expired) {
		t.Errorf("a zero limit deleted %d rows and left the expired asset: it must fall back to the batch bound, not LIMIT 0", n)
	}
	if !assetExists(ctx, t, pool, recent) {
		t.Error("a zero window deleted an asset archived 89 days ago: it must fall back to 90 days, not 0")
	}
}
