// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

//go:build integration

// Package postgres integration tests exercise the real SQL against a live Postgres —
// the steal path (LFXV2-2665) depends on behaviors an in-memory fake cannot model:
// `RETURNING (xmax = 0)` insert-vs-update discrimination, `ON CONFLICT DO UPDATE ...
// WHERE <false>` returning zero rows, `make_interval(secs => $5)` float binding, and
// the row-lock that serializes concurrent steals.
//
//	Run with: go test -tags=integration ./internal/infrastructure/postgres/ \
//	  with CAMPAIGN_TEST_DATABASE_URL pointing at a throwaway Postgres. Skipped (not
//
// failed) when that env var is unset, so the default `go test ./...` never needs a DB.
package postgres

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

const testProvider = model.ProviderLinkedInAds

// newIntegrationRepo migrates a fresh schema and returns a repo + a seeded brief id
// (campaigns.brief_id FKs campaign_briefs). Skips when no test DB is configured.
func newIntegrationRepo(t *testing.T) (*CampaignRepo, string) {
	t.Helper()
	dsn := os.Getenv("CAMPAIGN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("CAMPAIGN_TEST_DATABASE_URL not set; skipping DB integration test")
	}
	if err := Migrate(dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	pool, err := NewPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(func() { pool.Close() })

	// Seed a project + approved brief so the (brief_id) FK is satisfied, and clear any
	// prior campaigns for a clean (brief_id, platform) slate.
	projectID := "11111111-1111-1111-1111-111111111111"
	briefID := "22222222-2222-2222-2222-222222222222"
	if _, err := pool.Exec(ctx, `DELETE FROM campaigns WHERE brief_id = $1`, briefID); err != nil {
		t.Fatalf("clean campaigns: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO campaign_briefs (id, project_id, program_type, event_slug, status, version)
		VALUES ($1, $2, 'events', 'kubecon-test', 'approved', 1)
		ON CONFLICT (id) DO NOTHING`, briefID, projectID); err != nil {
		t.Fatalf("seed brief: %v", err)
	}
	return NewCampaignRepo(pool), briefID
}

// setClaimedAt back-dates a pending claim's lease so it looks stale/fresh on demand.
func setClaimedAt(t *testing.T, r *CampaignRepo, briefID string, at time.Time) {
	t.Helper()
	if _, err := r.db.Exec(context.Background(),
		`UPDATE campaigns SET claimed_at = $1 WHERE brief_id = $2 AND platform = $3`,
		at, briefID, string(testProvider)); err != nil {
		t.Fatalf("set claimed_at: %v", err)
	}
}

func TestIntegration_Claim_FreshInsert(t *testing.T) {
	r, briefID := newIntegrationRepo(t)
	claimed, row, err := r.ClaimCampaignDispatch(context.Background(), "11111111-1111-1111-1111-111111111111", briefID, testProvider, "job-1", 0)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !claimed || row == nil || row.Status != model.CampaignStatusPending {
		t.Fatalf("fresh claim: claimed=%v row=%+v", claimed, row)
	}
}

func TestIntegration_Claim_NoStealWithoutReclaimAfter(t *testing.T) {
	r, briefID := newIntegrationRepo(t)
	// First job claims.
	if claimed, _, err := r.ClaimCampaignDispatch(context.Background(), "11111111-1111-1111-1111-111111111111", briefID, testProvider, "job-1", 0); err != nil || !claimed {
		t.Fatalf("first claim: claimed=%v err=%v", claimed, err)
	}
	setClaimedAt(t, r, briefID, time.Now().Add(-time.Hour)) // very stale
	// A second job with reclaimAfter==0 must NOT steal even a stale orphan.
	claimed, row, err := r.ClaimCampaignDispatch(context.Background(), "11111111-1111-1111-1111-111111111111", briefID, testProvider, "job-2", 0)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if claimed {
		t.Errorf("reclaimAfter==0 must not steal a stale orphan, but claimed=true")
	}
	if row == nil || row.PlatformCampaignID != "" {
		t.Errorf("orphan must be returned intact, got %+v", row)
	}
}

func TestIntegration_Claim_StealsStaleOrphan(t *testing.T) {
	r, briefID := newIntegrationRepo(t)
	if claimed, _, err := r.ClaimCampaignDispatch(context.Background(), "11111111-1111-1111-1111-111111111111", briefID, testProvider, "job-1", 0); err != nil || !claimed {
		t.Fatalf("first claim: %v", err)
	}
	setClaimedAt(t, r, briefID, time.Now().Add(-time.Hour)) // stale
	// A resumable job (reclaimAfter>0) steals the stale orphan.
	claimed, row, err := r.ClaimCampaignDispatch(context.Background(), "11111111-1111-1111-1111-111111111111", briefID, testProvider, "job-2", time.Minute)
	if err != nil {
		t.Fatalf("steal claim: %v", err)
	}
	if !claimed {
		t.Errorf("a stale orphan must be stolen with reclaimAfter>0, got claimed=false")
	}
	if row == nil || row.JobID == nil || *row.JobID != "job-2" {
		t.Errorf("stolen row must carry the new job id, got %+v", row)
	}
}

func TestIntegration_Claim_DoesNotStealFreshLease(t *testing.T) {
	r, briefID := newIntegrationRepo(t)
	if claimed, _, err := r.ClaimCampaignDispatch(context.Background(), "11111111-1111-1111-1111-111111111111", briefID, testProvider, "job-1", 0); err != nil || !claimed {
		t.Fatalf("first claim: %v", err)
	}
	// claimed_at is now (fresh). A reclaimAfter of 1h must NOT steal it.
	claimed, _, err := r.ClaimCampaignDispatch(context.Background(), "11111111-1111-1111-1111-111111111111", briefID, testProvider, "job-2", time.Hour)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed {
		t.Errorf("a fresh (recently-leased) claim must not be stolen")
	}
}

func TestIntegration_Claim_DoesNotStealCompletedCampaign(t *testing.T) {
	r, briefID := newIntegrationRepo(t)
	if claimed, _, err := r.ClaimCampaignDispatch(context.Background(), "11111111-1111-1111-1111-111111111111", briefID, testProvider, "job-1", 0); err != nil || !claimed {
		t.Fatalf("first claim: %v", err)
	}
	// Complete the campaign (non-empty id) and back-date the lease.
	if _, err := r.db.Exec(context.Background(),
		`UPDATE campaigns SET platform_campaign_id = 'pc-done', status = 'created' WHERE brief_id = $1 AND platform = $2`,
		briefID, string(testProvider)); err != nil {
		t.Fatalf("complete: %v", err)
	}
	setClaimedAt(t, r, briefID, time.Now().Add(-time.Hour))
	// Even stale, a completed campaign (non-empty id) must NEVER be stolen.
	claimed, row, err := r.ClaimCampaignDispatch(context.Background(), "11111111-1111-1111-1111-111111111111", briefID, testProvider, "job-2", time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed {
		t.Errorf("a completed campaign must never be stolen, got claimed=true")
	}
	if row == nil || row.PlatformCampaignID != "pc-done" {
		t.Errorf("completed campaign must be returned intact, got %+v", row)
	}
}

func TestIntegration_Claim_ConcurrentStealExactlyOneWinner(t *testing.T) {
	r, briefID := newIntegrationRepo(t)
	if claimed, _, err := r.ClaimCampaignDispatch(context.Background(), "11111111-1111-1111-1111-111111111111", briefID, testProvider, "job-1", 0); err != nil || !claimed {
		t.Fatalf("first claim: %v", err)
	}
	setClaimedAt(t, r, briefID, time.Now().Add(-time.Hour)) // stale orphan

	const n = 12
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		wins  int
		start = make(chan struct{})
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			claimed, _, err := r.ClaimCampaignDispatch(context.Background(), "11111111-1111-1111-1111-111111111111", briefID, testProvider, "job-steal", time.Minute)
			if err != nil {
				return
			}
			if claimed {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if wins != 1 {
		t.Errorf("concurrent steal of one stale orphan must have exactly ONE winner, got %d", wins)
	}
}
