// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// reconcileTestPool mirrors dbtest.Pool, minimally, for THIS file only. It cannot import
// the dbtest package: dbtest.go itself imports this package (postgres) to build its live-DB
// fixtures, so an in-package postgres test file that imported dbtest back would be a genuine
// import cycle (confirmed via `go vet`, which rejects it outright). reconcileAmbiguousAudienceCommit
// is unexported, so the external dbtest_test package cannot call it either — this file is the
// only place that CAN unit-test it against a real database.
var (
	reconcilePoolOnce sync.Once
	reconcilePool     *Pool
	reconcilePoolErr  error
)

func reconcileTestPool(t *testing.T) *Pool {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("TEST_DATABASE_URL is empty on a CI runner: this live-database test would skip entirely")
		}
		t.Skip("TEST_DATABASE_URL is not set; skipping the live-database test")
	}

	reconcilePoolOnce.Do(func() {
		if err := Migrate(dsn); err != nil {
			reconcilePoolErr = err
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		reconcilePool, reconcilePoolErr = NewPool(ctx, dsn)
	})
	if reconcilePoolErr != nil {
		t.Fatalf("live-database harness: %v", reconcilePoolErr)
	}
	return reconcilePool
}

// reconcileUniqueID returns a value no other row in this schema shares, following the same
// name-prefix + crypto/rand-suffix shape as dbtest.UniqueID (see its doc comment for why
// both halves matter — the schema is shared across runs and never dropped).
func reconcileUniqueID(t *testing.T, suffix string) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("cannot generate a unique id: %v", err)
	}
	return "reconcile-" + suffix + "-" + hex.EncodeToString(b[:])
}

func insertReconcileTestBrief(ctx context.Context, t *testing.T, pool *Pool) (briefID, projectID string) {
	t.Helper()
	projectID = reconcileUniqueID(t, "brief")
	err := pool.QueryRow(ctx, `
		INSERT INTO campaign_briefs (project_id, program_type, event_slug, status)
		VALUES ($1, 'events', $1, 'approved')
		RETURNING id`, projectID).Scan(&briefID)
	if err != nil {
		t.Fatalf("insert approved parent brief: %v", err)
	}
	return briefID, projectID
}

// TestReconcileAmbiguousAudienceCommit_MovesABuildingRowToFailed covers the case a Commit
// error does not prove a rollback: the row genuinely exists (as CreateAudienceForApprovedBrief's
// commit-error branch cannot itself tell), and reconcile must flip it to 'failed' so the
// (brief_id, platform) build lease (migration 000018) is released instead of held forever.
func TestReconcileAmbiguousAudienceCommit_MovesABuildingRowToFailed(t *testing.T) {
	pool := reconcileTestPool(t)
	ctx := context.Background()

	briefID, projectID := insertReconcileTestBrief(ctx, t, pool)

	var row model.CampaignAudience
	row.ProjectID = projectID
	row.BriefID = briefID
	row.Platform = model.ProviderHubSpot
	err := pool.QueryRow(ctx, `
		INSERT INTO campaign_audiences (project_id, brief_id, platform, status)
		VALUES ($1, $2, $3, 'building')
		RETURNING id, version`, projectID, briefID, string(model.ProviderHubSpot)).
		Scan(&row.ID, &row.Version)
	if err != nil {
		t.Fatalf("insert building audience row: %v", err)
	}

	reconcileAmbiguousAudienceCommit(ctx, pool, &row)

	var status string
	var version int64
	if err := pool.QueryRow(ctx, `SELECT status, version FROM campaign_audiences WHERE id = $1`, row.ID).
		Scan(&status, &version); err != nil {
		t.Fatalf("read back reconciled row: %v", err)
	}
	if status != "failed" {
		t.Errorf("status = %q, want %q — the build lease is still held", status, "failed")
	}
	if version != row.Version+1 {
		t.Errorf("version = %d, want %d — reconcile must bump version like every other update", version, row.Version+1)
	}

	// The lease is what this whole fix is FOR: with the row moved to 'failed', a fresh
	// build for the same (brief, platform) must be free to take it again.
	var newID string
	err = pool.QueryRow(ctx, `
		INSERT INTO campaign_audiences (project_id, brief_id, platform, status)
		VALUES ($1, $2, $3, 'building')
		RETURNING id`, projectID, briefID, string(model.ProviderHubSpot)).Scan(&newID)
	if err != nil {
		t.Errorf("lease still held after reconcile: rebuild insert failed: %v", err)
	}
}

// TestReconcileAmbiguousAudienceCommit_NoOpWhenTheCommitReallyRolledBack covers the OTHER
// branch: the Commit error meant what it said, no row exists at that id, and reconcile must
// not error or panic — it has nothing to reconcile.
func TestReconcileAmbiguousAudienceCommit_NoOpWhenTheCommitReallyRolledBack(t *testing.T) {
	pool := reconcileTestPool(t)
	ctx := context.Background()

	row := &model.CampaignAudience{
		// A plausible id/brief_id — matching the shape the real caller always has, from the
		// INSERT's RETURNING clause — that was never actually committed.
		ID:        "00000000-0000-0000-0000-000000000000",
		BriefID:   "00000000-0000-0000-0000-000000000001",
		ProjectID: reconcileUniqueID(t, "never-committed"),
		Version:   1,
	}

	// Must not panic and must not block past its own bounded timeout; a zero-row UPDATE
	// is the expected, harmless outcome.
	reconcileAmbiguousAudienceCommit(ctx, pool, row)
}

// TestReconcileAmbiguousAudienceCommit_ScopesByTenant covers the bugbot finding on PR #106:
// a row with the right id but the WRONG project_id/brief_id must not be reconciled. id alone
// is the primary key and would be sufficient for correctness today, but every other write to
// this table (UpdateAudience) scopes by (id, brief_id, project_id) — this pins that the
// reconcile path holds the same tenant-isolation invariant rather than silently exempting
// itself from it.
func TestReconcileAmbiguousAudienceCommit_ScopesByTenant(t *testing.T) {
	pool := reconcileTestPool(t)
	ctx := context.Background()

	briefID, projectID := insertReconcileTestBrief(ctx, t, pool)

	var row model.CampaignAudience
	err := pool.QueryRow(ctx, `
		INSERT INTO campaign_audiences (project_id, brief_id, platform, status)
		VALUES ($1, $2, $3, 'building')
		RETURNING id, version`, projectID, briefID, string(model.ProviderHubSpot)).
		Scan(&row.ID, &row.Version)
	if err != nil {
		t.Fatalf("insert building audience row: %v", err)
	}

	// Same id, wrong project — the (id, version) predicate the finding objected to would
	// still reconcile this; the tenant-scoped predicate must not.
	wrong := row
	wrong.BriefID = briefID
	wrong.ProjectID = reconcileUniqueID(t, "wrong-project")
	reconcileAmbiguousAudienceCommit(ctx, pool, &wrong)

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM campaign_audiences WHERE id = $1`, row.ID).
		Scan(&status); err != nil {
		t.Fatalf("read back row: %v", err)
	}
	if status != "building" {
		t.Errorf("status = %q, want %q — reconcile touched a row outside the caller's tenant scope", status, "building")
	}
}

// TestReconcileAmbiguousAudienceCommit_LeaveBuiltRowsUnchanged covers the fix for the Cursor
// Bugbot finding on PR #106: reconcileAmbiguousAudienceCommit must not downgrade a successfully
// built row to 'failed'. The reconcile path is called when tx.Commit returns an error that does
// not prove a rollback — the row MAY have been persisted with ANY status (building, built, or
// failed). Only 'building' rows hold the lease and need to be moved to 'failed'; 'built' rows
// are successful builds with valid master-list pointers and must not be corrupted.
func TestReconcileAmbiguousAudienceCommit_LeaveBuiltRowsUnchanged(t *testing.T) {
	pool := reconcileTestPool(t)
	ctx := context.Background()

	briefID, projectID := insertReconcileTestBrief(ctx, t, pool)

	// Insert a 'built' row with a master-list pointer, as if a build succeeded.
	var builtRow model.CampaignAudience
	masterListID := "master-list-" + reconcileUniqueID(t, "m")
	err := pool.QueryRow(ctx, `
		INSERT INTO campaign_audiences
			(project_id, brief_id, platform, platform_master_list_id, status)
		VALUES ($1, $2, $3, $4, 'built')
		RETURNING id, version`, projectID, briefID, string(model.ProviderHubSpot), masterListID).
		Scan(&builtRow.ID, &builtRow.Version)
	if err != nil {
		t.Fatalf("insert built audience row: %v", err)
	}
	builtRow.ProjectID = projectID
	builtRow.BriefID = briefID
	builtRow.Platform = model.ProviderHubSpot
	builtRow.PlatformMasterListID = masterListID

	// Call reconcile as if the INSERT had hit an ambiguous commit error. This should NOT
	// downgrade the 'built' row to 'failed'.
	reconcileAmbiguousAudienceCommit(ctx, pool, &builtRow)

	// Verify the row is still 'built' with its master-list pointer intact.
	var status, readMasterListID string
	var readVersion int64
	if err := pool.QueryRow(ctx,
		`SELECT status, platform_master_list_id, version FROM campaign_audiences WHERE id = $1`,
		builtRow.ID).Scan(&status, &readMasterListID, &readVersion); err != nil {
		t.Fatalf("read back reconciled row: %v", err)
	}
	if status != "built" {
		t.Errorf("status = %q, want %q — a built row must not be downgraded", status, "built")
	}
	if readMasterListID != masterListID {
		t.Errorf("platform_master_list_id = %q, want %q — master list pointer must be preserved", readMasterListID, masterListID)
	}
	if readVersion != builtRow.Version {
		t.Errorf("version = %d, want %d — reconcile must not bump version for a built row", readVersion, builtRow.Version)
	}

	// Importantly: the 'built' row does NOT hold the lease (the index is partial on
	// status='building'), so a fresh build for the same (brief, platform) must be able to
	// create a new 'building' row without hitting the uniqueness constraint.
	var newID string
	err = pool.QueryRow(ctx, `
		INSERT INTO campaign_audiences (project_id, brief_id, platform, status)
		VALUES ($1, $2, $3, 'building')
		RETURNING id`, projectID, briefID, string(model.ProviderHubSpot)).Scan(&newID)
	if err != nil {
		t.Errorf("fresh build should be able to take the lease: insert failed: %v", err)
	}
}
