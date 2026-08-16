// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
)

// The claim/release pair must be exercised through the REAL repository methods, not through
// a copy of their SQL.
//
// campaign_variant_live_test.go pins the same invariant with `liveClaimSQL`/`liveReleaseSQL`
// string constants driven by pool.Exec. That proves the SQL is correct but cannot prove the
// Go binding is: the defect that file memorializes was an ARG-COUNT MISMATCH in the real
// method's Exec call, which a copied-SQL test reproduces correctly by construction and
// therefore can never catch. If ClaimCampaignDispatch's query string and its argument list
// drift apart again, only a test that calls the method sees it.
//
// Raised by dealako in review of #130; the adopt path was already covered this way through
// repo.AdoptCampaign in adopt_variant_live_test.go.
func TestLiveClaimAndReleaseDriveTheRealRepoMethods(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})

	briefID, project := insertApprovedBrief(ctx, t, pool)
	const variant = "demand-gen"
	// job_id is a uuid column, so these must be well-formed UUIDs — dbtest.UniqueID
	// returns a slug and Postgres rejects it (SQLSTATE 22P02).
	const (
		jobID  = "00000000-0000-4000-8000-0000000000b1"
		jobID2 = "00000000-0000-4000-8000-0000000000b2"
		jobID3 = "00000000-0000-4000-8000-0000000000b3"
		jobID4 = "00000000-0000-4000-8000-0000000000b4"
	)

	// First claim wins and inserts the pending row.
	claimed, existing, err := repo.ClaimCampaignDispatch(ctx, project, briefID, model.ProviderGoogleAds, variant, jobID, nil)
	if err != nil {
		t.Fatalf("ClaimCampaignDispatch: %v", err)
	}
	if !claimed {
		t.Fatalf("first claim did not win; existing=%+v", existing)
	}

	// Second claim for the SAME slot must lose — this is the single-flight guarantee the
	// orchestrator relies on to avoid two concurrent upstream creates.
	claimedAgain, row, err := repo.ClaimCampaignDispatch(ctx, project, briefID, model.ProviderGoogleAds, variant, jobID2, nil)
	if err != nil {
		t.Fatalf("second ClaimCampaignDispatch: %v", err)
	}
	if claimedAgain {
		t.Error("two claims won the same (brief, platform, variant) slot — the single-flight guarantee is broken")
	}
	if row == nil {
		t.Error("a losing claim must return the existing row so the caller can reconcile")
	}

	// A DIFFERENT variant on the same (brief, platform) is a different slot and must win.
	// This is the whole point of the third column: Search and Demand Gen coexist.
	claimedOther, _, err := repo.ClaimCampaignDispatch(ctx, project, briefID, model.ProviderGoogleAds, model.VariantDefault, jobID3, nil)
	if err != nil {
		t.Fatalf("claim on the default slot: %v", err)
	}
	if !claimedOther {
		t.Error("a different variant on the same (brief, platform) was refused — the slot key is not variant-aware")
	}

	// Release, then re-claim: the slot must be free again.
	if err := repo.DeleteDispatchClaim(ctx, briefID, model.ProviderGoogleAds, variant); err != nil {
		t.Fatalf("DeleteDispatchClaim: %v", err)
	}
	reclaimed, _, err := repo.ClaimCampaignDispatch(ctx, project, briefID, model.ProviderGoogleAds, variant, jobID4, nil)
	if err != nil {
		t.Fatalf("re-claim after release: %v", err)
	}
	if !reclaimed {
		t.Error("the slot was not free after DeleteDispatchClaim — a released claim must be re-claimable, or a retry strands forever")
	}
}
