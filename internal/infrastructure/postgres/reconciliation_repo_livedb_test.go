// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// This file exercises the reconciliation SQL against a REAL PostgreSQL database.
//
// Why a live database rather than a mock. The correctness of ReleaseDispatchClaimByID
// rests entirely on behaviour a mock cannot reproduce: FOR UPDATE row locking, READ
// COMMITTED snapshot semantics, and the fact that a concurrent re-claim bumps version
// and resets created_at. A fake repo asserting "the guard string appears in the query"
// would pass against a query that deletes live claims. The central test here
// (TestReleaseDispatchClaim_RefusesReclaimedLiveClaim) is the one that would catch
// that, and it only means anything against a real engine.
//
// It skips when RECON_TEST_DATABASE_URL is unset so CI (which has no database) stays
// green; run it locally with a throwaway cluster:
//
//	initdb -D /tmp/pgrecon -U postgres --auth=trust
//	pg_ctl -D /tmp/pgrecon -o "-p 55460" start
//	createdb -h 127.0.0.1 -p 55460 -U postgres campaign
//	RECON_TEST_DATABASE_URL='postgres://postgres@127.0.0.1:55460/campaign' go test ./internal/infrastructure/postgres/ -run LiveDB -v

const (
	testBriefID    = "11111111-1111-1111-1111-111111111111"
	testProjectID  = "cncf"
	testCampaignID = "22222222-2222-2222-2222-222222222222"
)

// liveDBPool opens the throwaway database, applies the migrations, and returns a pool.
func liveDBPool(t *testing.T) *Pool {
	t.Helper()
	dsn := os.Getenv("RECON_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("RECON_TEST_DATABASE_URL not set; skipping live-database reconciliation tests")
	}
	if err := Migrate(dsn); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	pool, err := NewPool(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedClaim resets the campaigns/briefs tables and inserts one campaign row with the
// given status/age, returning its version.
func seedClaim(t *testing.T, pool *Pool, status string, age time.Duration, platformCampaignID any, result any) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM campaigns`); err != nil {
		t.Fatalf("clear campaigns: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM campaign_audiences`); err != nil {
		t.Fatalf("clear audiences: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM campaign_briefs`); err != nil {
		t.Fatalf("clear briefs: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO campaign_briefs (id, project_id, program_type, event_slug, status)
		 VALUES ($1,$2,'events','kubecon-live-test','approved')`, testBriefID, testProjectID); err != nil {
		t.Fatalf("seed brief: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO campaigns (id, project_id, brief_id, platform, campaign_name, status,
		                        platform_campaign_id, result, created_at)
		 VALUES ($1,$2,$3,'google-ads','',$4,$5,$6, now() - make_interval(secs => $7))`,
		testCampaignID, testProjectID, testBriefID, status, platformCampaignID, result, age.Seconds()); err != nil {
		t.Fatalf("seed campaign: %v", err)
	}
	var v int64
	if err := pool.QueryRow(ctx, `SELECT version FROM campaigns WHERE id=$1`, testCampaignID).Scan(&v); err != nil {
		t.Fatalf("read seeded version: %v", err)
	}
	return v
}

func campaignRowCount(t *testing.T, pool *Pool) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM campaigns`).Scan(&n); err != nil {
		t.Fatalf("count campaigns: %v", err)
	}
	return n
}

// TestLiveDBReleaseDispatchClaim_ReleasesBareStrandedClaim is the happy path: an old
// bare claim with no evidence of an upstream create is released, freeing the pair.
func TestLiveDBReleaseDispatchClaim_ReleasesBareStrandedClaim(t *testing.T) {
	pool := liveDBPool(t)
	repo := NewReconciliationRepo(pool)
	v := seedClaim(t, pool, model.CampaignStatusPending, 30*time.Minute, nil, nil)

	item, err := repo.ReleaseDispatchClaimByID(context.Background(),
		testProjectID, testBriefID, testCampaignID, v, model.ClaimReleaseFloor)
	if err != nil {
		t.Fatalf("expected the stranded bare claim to be released, got %v", err)
	}
	if item.Kind != model.ReconcileStuckClaim {
		t.Errorf("kind = %q, want %q", item.Kind, model.ReconcileStuckClaim)
	}
	if n := campaignRowCount(t, pool); n != 0 {
		t.Errorf("claim row still present after release: %d rows", n)
	}
}

// TestLiveDBReleaseDispatchClaim_RefusesReclaimedLiveClaim is the test that matters
// most. It reproduces the exact TOCTOU the endpoint must survive: the operator reads
// the report (version v, 30 minutes old), and BEFORE they act a new dispatch re-claims
// the same pair — which bumps version and resets created_at, so a provider call is now
// in flight. Releasing here would free the pair and let a concurrent dispatch create a
// SECOND paid campaign.
//
// A status-only guarded DELETE deletes this row (verified directly against this same
// database). The version gate is what refuses it.
func TestLiveDBReleaseDispatchClaim_RefusesReclaimedLiveClaim(t *testing.T) {
	pool := liveDBPool(t)
	repo := NewReconciliationRepo(pool)
	observedVersion := seedClaim(t, pool, model.CampaignStatusPending, 30*time.Minute, nil, nil)

	// A fresh dispatch re-claims the stale pending row: version bumps, clock resets.
	if _, err := pool.Exec(context.Background(),
		`UPDATE campaigns SET created_at = now(), version = version + 1 WHERE id = $1`, testCampaignID); err != nil {
		t.Fatalf("simulate re-claim: %v", err)
	}

	_, err := repo.ReleaseDispatchClaimByID(context.Background(),
		testProjectID, testBriefID, testCampaignID, observedVersion, model.ClaimReleaseFloor)
	if !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("expected ErrPreconditionFailed for a re-claimed live claim, got %v", err)
	}
	if n := campaignRowCount(t, pool); n != 1 {
		t.Fatalf("a LIVE dispatch claim was deleted — a concurrent dispatch can now double-create a paid campaign (rows=%d)", n)
	}
}

// TestLiveDBReleaseDispatchClaim_RefusesYoungClaim covers the age floor, which is the
// backstop for the case the version gate cannot see: a claim DELETEd and re-INSERTed by
// a new dispatch restarts at version 1, which can coincidentally equal the version the
// operator observed. A fresh row is young, so the floor rejects it.
func TestLiveDBReleaseDispatchClaim_RefusesYoungClaim(t *testing.T) {
	pool := liveDBPool(t)
	repo := NewReconciliationRepo(pool)
	// Age 10s: a dispatch could genuinely still be in flight (providerCallTimeout 2m).
	v := seedClaim(t, pool, model.CampaignStatusPending, 10*time.Second, nil, nil)

	_, err := repo.ReleaseDispatchClaimByID(context.Background(),
		testProjectID, testBriefID, testCampaignID, v, model.ClaimReleaseFloor)
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected ErrConflict for a claim younger than the release floor, got %v", err)
	}
	if n := campaignRowCount(t, pool); n != 1 {
		t.Fatalf("a possibly-in-flight claim was deleted (rows=%d)", n)
	}
}

// TestLiveDBReleaseDispatchClaim_RefusesEvidenceOfUpstreamCreate covers the two shapes
// that prove something may exist on the ad platform. Releasing either would authorize a
// retry to create a duplicate paid campaign, so both must be refused even though the
// row is old and the version matches.
func TestLiveDBReleaseDispatchClaim_RefusesEvidenceOfUpstreamCreate(t *testing.T) {
	pool := liveDBPool(t)
	repo := NewReconciliationRepo(pool)

	cases := []struct {
		name     string
		status   string
		pcID     any
		result   any
		evidence string
	}{
		{"recorded upstream id", model.CampaignStatusPending, "cid-123", nil,
			"the upstream campaign id was recorded, so the campaign exists"},
		{"reconcile blob only", model.CampaignStatusPending, nil, []byte(`{"name":"kubecon"}`),
			"an ambiguous create persisted reconcile detail"},
		{"unconfirmed status", "unconfirmed", nil, nil,
			"the dispatcher classified the outcome UNCONFIRMED"},
		{"group_created status", "group_created", nil, nil,
			"a sub-resource was created upstream"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := seedClaim(t, pool, tc.status, 30*time.Minute, tc.pcID, tc.result)
			_, err := repo.ReleaseDispatchClaimByID(context.Background(),
				testProjectID, testBriefID, testCampaignID, v, model.ClaimReleaseFloor)
			if !errors.Is(err, domain.ErrConflict) {
				t.Fatalf("expected ErrConflict (%s), got %v", tc.evidence, err)
			}
			if n := campaignRowCount(t, pool); n != 1 {
				t.Fatalf("released a claim that had evidence of an upstream create (%s)", tc.evidence)
			}
		})
	}
}

// TestLiveDBReleaseDispatchClaim_TenantIsolation verifies the release cannot reach
// across projects: campaign ids are UUIDs, but the query is still project-scoped so a
// caller in another project gets ErrNotFound rather than deleting someone else's claim.
func TestLiveDBReleaseDispatchClaim_TenantIsolation(t *testing.T) {
	pool := liveDBPool(t)
	repo := NewReconciliationRepo(pool)
	v := seedClaim(t, pool, model.CampaignStatusPending, 30*time.Minute, nil, nil)

	_, err := repo.ReleaseDispatchClaimByID(context.Background(),
		"another-project", testBriefID, testCampaignID, v, model.ClaimReleaseFloor)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for a cross-project release, got %v", err)
	}
	if n := campaignRowCount(t, pool); n != 1 {
		t.Fatalf("a claim was deleted across a tenant boundary (rows=%d)", n)
	}
}

// TestLiveDBListReconciliationItems_ClassifiesAndExcludesHealthy verifies the inventory
// against real rows: it classifies bare claims as releasable, evidence-carrying rows as
// not, and excludes both settled campaigns and rows too young to be stuck.
func TestLiveDBListReconciliationItems_ClassifiesAndExcludesHealthy(t *testing.T) {
	pool := liveDBPool(t)
	repo := NewReconciliationRepo(pool)
	ctx := context.Background()
	seedClaim(t, pool, model.CampaignStatusPending, 30*time.Minute, nil, nil)

	// A settled campaign — must never appear.
	if _, err := pool.Exec(ctx,
		`INSERT INTO campaigns (project_id, brief_id, platform, campaign_name, status, platform_campaign_id, created_at)
		 VALUES ($1,$2,'reddit-ads','ok','created','cid-live', now() - interval '1 hour')`,
		testProjectID, testBriefID); err != nil {
		t.Fatalf("seed settled campaign: %v", err)
	}
	// An unconfirmed orphan — must appear, not releasable.
	if _, err := pool.Exec(ctx,
		`INSERT INTO campaigns (project_id, brief_id, platform, campaign_name, status, created_at)
		 VALUES ($1,$2,'meta-ads','','unconfirmed', now() - interval '20 minutes')`,
		testProjectID, testBriefID); err != nil {
		t.Fatalf("seed unconfirmed campaign: %v", err)
	}
	// A young pending claim — a healthy in-flight dispatch, must NOT appear.
	if _, err := pool.Exec(ctx,
		`INSERT INTO campaigns (project_id, brief_id, platform, campaign_name, status, created_at)
		 VALUES ($1,$2,'twitter-ads','','pending', now())`,
		testProjectID, testBriefID); err != nil {
		t.Fatalf("seed young claim: %v", err)
	}
	// A partially-built audience — must appear, not releasable.
	if _, err := pool.Exec(ctx,
		`INSERT INTO campaign_audiences (project_id, brief_id, platform, status, inclusion_summary, created_at)
		 VALUES ($1,$2,'hubspot','building','2 of 3 lists created', now() - interval '45 minutes')`,
		testProjectID, testBriefID); err != nil {
		t.Fatalf("seed partial audience: %v", err)
	}

	items, total, err := repo.ListReconciliationItems(ctx, testProjectID, 3*time.Minute, 100)
	if err != nil {
		t.Fatalf("list reconciliation items: %v", err)
	}

	byKind := map[model.ReconciliationKind][]model.ReconciliationItem{}
	for _, it := range items {
		byKind[it.Kind] = append(byKind[it.Kind], it)
	}
	if got := len(byKind[model.ReconcileStuckClaim]); got != 1 {
		t.Errorf("stuck_claim count = %d, want 1 (the young claim and settled campaign must be excluded)", got)
	}
	if got := len(byKind[model.ReconcileUnconfirmedCampaign]); got != 1 {
		t.Errorf("unconfirmed_campaign count = %d, want 1", got)
	}
	if got := len(byKind[model.ReconcilePartialAudience]); got != 1 {
		t.Errorf("partial_audience count = %d, want 1", got)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3", total)
	}

	if claims := byKind[model.ReconcileStuckClaim]; len(claims) == 1 && !claims[0].Resolvable {
		t.Error("a bare stranded claim must be reported resolvable")
	}
	for _, it := range append(byKind[model.ReconcileUnconfirmedCampaign], byKind[model.ReconcilePartialAudience]...) {
		if it.Resolvable {
			t.Errorf("%s must NOT be reported resolvable — the service cannot know what exists upstream", it.Kind)
		}
	}
}
