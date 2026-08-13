// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// The safety boundary for LFXV2-2665, asserted against a real database because it is a SQL
// predicate and nothing else — a fake repository would only prove that Go code calls Go code.
//
// The reap deletes claims that provably never reached the provider. What makes that safe is not
// the age: 'pending' marks BOTH a claim in flight and an ambiguous outcome the orchestrator
// retains precisely because a paid campaign MAY already exist upstream. Deleting the second kind
// would authorize a duplicate paid create.
//
// The discriminator is that the claim INSERT writes neither platform_campaign_id nor result,
// and every provider-touching path populates at least one. So this test's whole job is to prove
// the predicate keeps that distinction: the untouched row goes, the two orphan shapes stay.
func TestReapUnreachedDispatchClaims_LeavesAnythingThatMayExistUpstream(t *testing.T) {
	pool := reconcileTestPool(t)
	ctx := context.Background()
	repo := NewCampaignRepo(pool)

	briefID, projectID := insertReconcileTestBrief(ctx, t, pool)

	// Three claims, all 'pending' and all old enough to be reaped by age alone. They differ
	// ONLY in the two columns the predicate reads.
	insert := func(platform model.Provider, campaignID string, result any) string {
		t.Helper()
		var id string
		err := pool.QueryRow(ctx, `
			INSERT INTO campaigns (project_id, brief_id, job_id, platform, campaign_name, status,
			                       platform_campaign_id, result, created_at)
			VALUES ($1, $2, gen_random_uuid(), $3, '', 'pending', $4, $5, now() - interval '1 hour')
			RETURNING id`, projectID, briefID, string(platform), campaignID, result).Scan(&id)
		if err != nil {
			t.Fatalf("insert %s claim: %v", platform, err)
		}
		return id
	}

	// 1. Never reached the provider: exactly what claimCampaignDispatchQuery writes, then a
	//    crash. Nothing exists upstream. SAFE to delete.
	unreached := insert(model.ProviderGoogleAds, "", nil)
	// 2. Created upstream and recorded the id. Deleting this would let a retry create a SECOND
	//    paid campaign.
	created := insert(model.ProviderMetaAds, "123456789", nil)
	// 3. The ambiguous-create / group-orphan shape: id deliberately empty, but a reconcile blob
	//    was persisted. The provider MAY hold a campaign. Also unsafe.
	ambiguous := insert(model.ProviderRedditAds, "", []byte(`{"reconcile":"ambiguous create"}`))

	n, err := repo.ReapUnreachedDispatchClaims(ctx, 100)
	if err != nil {
		t.Fatalf("ReapUnreachedDispatchClaims: %v", err)
	}
	if n != 1 {
		t.Errorf("reaped %d rows, want exactly 1 — only the claim that never reached the provider", n)
	}

	exists := func(id string) bool {
		t.Helper()
		var found bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM campaigns WHERE id = $1)`, id).Scan(&found); err != nil {
			t.Fatalf("existence check: %v", err)
		}
		return found
	}

	if exists(unreached) {
		t.Error("the unreached claim survived, so its (brief, platform) pair stays blocked forever — the defect this exists to fix")
	}
	// These two are the money assertions. A predicate that deletes either one lets a retry
	// create a duplicate PAID campaign for a campaign that already exists upstream.
	if !exists(created) {
		t.Error("a claim carrying a platform_campaign_id was reaped; a retry can now duplicate a real paid campaign")
	}
	if !exists(ambiguous) {
		t.Error("a claim carrying a reconcile result was reaped; the provider may already hold that campaign")
	}
}

// A claim young enough to still be in flight must survive, or the reap races a running worker:
// it would delete the claim out from under a dispatcher mid-provider-call, and the next caller
// would win the freed unique index and create a second campaign.
func TestReapUnreachedDispatchClaims_LeavesClaimsYoungerThanTheReportAge(t *testing.T) {
	pool := reconcileTestPool(t)
	ctx := context.Background()
	repo := NewCampaignRepo(pool)

	briefID, projectID := insertReconcileTestBrief(ctx, t, pool)

	var id string
	err := pool.QueryRow(ctx, `
		INSERT INTO campaigns (project_id, brief_id, job_id, platform, campaign_name, status, created_at)
		VALUES ($1, $2, gen_random_uuid(), $3, '', 'pending', now())
		RETURNING id`, projectID, briefID, string(model.ProviderGoogleAds)).Scan(&id)
	if err != nil {
		t.Fatalf("insert young claim: %v", err)
	}

	n, err := repo.ReapUnreachedDispatchClaims(ctx, 100)
	if err != nil {
		t.Fatalf("ReapUnreachedDispatchClaims: %v", err)
	}
	if n != 0 {
		t.Errorf("reaped %d rows; a claim younger than stuckClaimReportAge may still be in flight", n)
	}

	var found bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM campaigns WHERE id = $1)`, id).Scan(&found); err != nil {
		t.Fatalf("existence check: %v", err)
	}
	if !found {
		t.Error("an in-flight claim was reaped out from under a running dispatcher")
	}
}
