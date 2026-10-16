// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
)

// TestLiveCampaignBiddingStrategyRoundTrips pins migration 000047 (LFXV2-2665) against the real
// schema and repository, through the dispatch's own claim-then-upsert order: the dispatch upsert writes the create's bidding_strategy and starting
// max_cpc_bid; a later upsert on the same slot fills them only while empty (the stored value wins,
// so a bid the lever set is never overwritten); ReplaceCampaign round-trips the strategy; and the
// CHECK refuses a value outside the vocabulary.
func TestLiveCampaignBiddingStrategyRoundTrips(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})
	briefID := insertBrief(ctx, t, pool)
	projectID := dbtest.UniqueID(t, "project")
	jobID := uuid.NewString()

	// Every upsert finalises this job's claim, as Orchestrator.dispatchPlatform does: an
	// upsert on a slot another live row holds is a lost claim (LFXV2-2023).
	claimed, _, err := repo.ClaimCampaignDispatch(ctx, projectID, briefID,
		model.ProviderMetaAds, "", model.FirstSlotVersion, jobID, nil)
	if err != nil {
		t.Fatalf("ClaimCampaignDispatch: %v", err)
	}
	if !claimed {
		t.Fatal("precondition failed: the claim was not won on a fresh brief")
	}

	manual, bid := model.BiddingManual, 1.25
	created, err := repo.UpsertCampaign(ctx, &model.Campaign{
		ProjectID: projectID, BriefID: briefID, JobID: &jobID, Platform: model.ProviderMetaAds,
		CampaignName: dbtest.UniqueID(t, "campaign"), Status: "created",
		BiddingStrategy: &manual, MaxCPCBid: &bid,
	}, nil)
	if err != nil {
		t.Fatalf("UpsertCampaign: %v", err)
	}
	if created.BiddingStrategy == nil || *created.BiddingStrategy != model.BiddingManual || created.MaxCPCBid == nil || *created.MaxCPCBid != bid {
		t.Fatalf("upserted bidding = %v / %v, want manual / 1.25", created.BiddingStrategy, created.MaxCPCBid)
	}

	// A later upsert on the same slot carrying other values (or none) must not replace them.
	automatic, other := model.BiddingAutomatic, 9.0
	again, err := repo.UpsertCampaign(ctx, &model.Campaign{
		ProjectID: projectID, BriefID: briefID, JobID: &jobID, Platform: model.ProviderMetaAds,
		CampaignName: created.CampaignName, Status: "created",
		BiddingStrategy: &automatic, MaxCPCBid: &other,
	}, nil)
	if err != nil {
		t.Fatalf("second UpsertCampaign: %v", err)
	}
	if *again.BiddingStrategy != model.BiddingManual || *again.MaxCPCBid != bid {
		t.Fatalf("a second upsert changed bidding to %v / %v; the stored values must win", *again.BiddingStrategy, *again.MaxCPCBid)
	}
	cleared, err := repo.UpsertCampaign(ctx, &model.Campaign{
		ProjectID: projectID, BriefID: briefID, JobID: &jobID, Platform: model.ProviderMetaAds,
		CampaignName: created.CampaignName, Status: "created",
	}, nil)
	if err != nil {
		t.Fatalf("third UpsertCampaign: %v", err)
	}
	if cleared.BiddingStrategy == nil || cleared.MaxCPCBid == nil {
		t.Fatal("an upsert carrying no bidding erased the recorded values")
	}

	// ReplaceCampaign round-trips the strategy it loaded.
	cleared.CampaignName += "-renamed"
	replaced, err := repo.ReplaceCampaign(ctx, cleared, cleared.Version, domain.CampaignLockToken{}, nil)
	if err != nil {
		t.Fatalf("ReplaceCampaign: %v", err)
	}
	if replaced.BiddingStrategy == nil || *replaced.BiddingStrategy != model.BiddingManual {
		t.Fatalf("an unrelated replace changed bidding_strategy to %v", replaced.BiddingStrategy)
	}

	if _, err := pool.Exec(ctx, `UPDATE campaigns SET bidding_strategy = 'target' WHERE id = $1`, created.ID); err == nil {
		t.Error("bidding_strategy 'target' was stored; the CHECK must refuse values outside automatic/manual")
	}
}
