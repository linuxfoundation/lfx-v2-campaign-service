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
		ID:      "00000000-0000-0000-0000-000000000000",
		Version: 1,
	}

	// Must not panic and must not block past its own bounded timeout; a zero-row UPDATE
	// is the expected, harmless outcome.
	reconcileAmbiguousAudienceCommit(ctx, pool, row)
}
