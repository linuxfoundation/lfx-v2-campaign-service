// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
)

// TestLiveReplaceCampaignRoundTripsMaxCPCBid pins migration 000038 against the real schema and
// repository: update-campaign-bid persists through ReplaceCampaign, so the column must exist,
// keep micro precision (NUMERIC(18,6), not budget_amount's two places), read back through
// scanCampaign, and survive a later replace that carries it unchanged — the path every other
// writer takes. A CHECK refusing a non-positive bid is asserted too.
func TestLiveReplaceCampaignRoundTripsMaxCPCBid(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})
	briefID := insertBrief(ctx, t, pool)
	projectID := dbtest.UniqueID(t, "project")
	name := dbtest.UniqueID(t, "campaign")

	var (
		id      string
		version int64
	)
	if err := pool.QueryRow(ctx, `
		INSERT INTO campaigns (project_id, brief_id, platform, campaign_name, status)
		VALUES ($1, $2, $3, $4, 'created')
		RETURNING id, version`,
		projectID, briefID, string(model.ProviderMicrosoftAds), name).Scan(&id, &version); err != nil {
		t.Fatalf("seed a campaign: %v", err)
	}

	loaded, err := repo.GetCampaign(ctx, projectID, briefID, id)
	if err != nil {
		t.Fatalf("GetCampaign: %v", err)
	}
	if loaded.MaxCPCBid != nil {
		t.Fatalf("a fresh row's max_cpc_bid = %v, want nil (\"never set through the bid endpoint\")", *loaded.MaxCPCBid)
	}

	bid := 0.123456
	loaded.MaxCPCBid = &bid
	updated, err := repo.ReplaceCampaign(ctx, loaded, version, domain.CampaignLockToken{}, nil)
	if err != nil {
		t.Fatalf("ReplaceCampaign with a bid: %v", err)
	}
	if updated.MaxCPCBid == nil || *updated.MaxCPCBid != bid {
		t.Fatalf("max_cpc_bid read back as %v, want %v at full micro precision", updated.MaxCPCBid, bid)
	}

	// Another writer replaces the row carrying the loaded value: the bid must survive.
	updated.CampaignName = name + "-renamed"
	again, err := repo.ReplaceCampaign(ctx, updated, updated.Version, domain.CampaignLockToken{}, nil)
	if err != nil {
		t.Fatalf("second ReplaceCampaign: %v", err)
	}
	if again.MaxCPCBid == nil || *again.MaxCPCBid != bid {
		t.Errorf("an unrelated replace changed max_cpc_bid to %v, want %v", again.MaxCPCBid, bid)
	}

	if _, err := pool.Exec(ctx, `UPDATE campaigns SET max_cpc_bid = 0 WHERE id = $1`, id); err == nil {
		t.Error("a zero max_cpc_bid was stored; the CHECK must refuse a non-positive bid")
	}
}
