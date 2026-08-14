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

// The adopt INSERT must BIND the campaign's variant, not hardcode 'default'.
//
// This is the test that was missing, and its absence is why the defect shipped. The caller
// side of the fix was complete — the Google adapter reads advertising_channel_type, maps it
// to a variant, and the service persists it onto model.Campaign — but adoptCampaignQuery
// still wrote the literal 'default' and never bound the field. Every layer above the
// repository looked correct, and the existing coverage could not see it: the repo's own
// tests assert on the query STRING, and the service tests use a fake repository that stores
// whatever struct it is handed. A fake cannot disagree with the SQL, which is exactly the
// disagreement here.
//
// The consequence is a duplicate paid campaign. An adopted Demand Gen campaign filed under
// 'default' leaves the 'demand-gen' slot empty, so the next Demand Gen dispatch for that
// brief finds a free slot and creates a SECOND campaign upstream — both live, both
// spending, and the rows are individually well-formed so nothing downstream can detect it.
func TestLiveAdoptStoresTheCampaignsRealVariant(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})

	// The package's existing helper: it mints its own project id and returns both.
	briefID, project := insertApprovedBrief(ctx, t, pool)

	upstreamID := dbtest.UniqueID(t, "up")
	adopted, err := repo.AdoptCampaign(ctx, &model.Campaign{
		ProjectID:          project,
		BriefID:            briefID,
		Platform:           model.ProviderGoogleAds,
		Variant:            "demand-gen",
		PlatformCampaignID: upstreamID,
		CampaignName:       dbtest.UniqueID(t, "campaign"),
		Status:             model.CampaignStatusCreated,
	}, 1, nil)
	if err != nil {
		t.Fatalf("AdoptCampaign: %v", err)
	}

	// Asserted against the DATABASE, not the returned struct: a repository that echoed its
	// input back would satisfy the struct while having written 'default' to the column, and
	// the column is what every later slot lookup reads.
	var stored string
	if err := pool.QueryRow(ctx,
		`SELECT variant FROM campaigns WHERE id = $1`, adopted.ID).Scan(&stored); err != nil {
		t.Fatalf("read back the adopted row: %v", err)
	}
	if stored != "demand-gen" {
		t.Errorf("stored variant = %q, want %q — an adopted Demand Gen campaign filed under the Search slot leaves 'demand-gen' free, and the next Demand Gen dispatch creates a SECOND paid campaign", stored, "demand-gen")
	}
}

// The other half, and the reason the bug was survivable-looking: adopting into 'demand-gen'
// must NOT be blocked by an existing default-slot campaign on the same brief, and must still
// be blocked by an existing demand-gen one. With the variant hardcoded, the first case was
// wrongly refused — both rows collided on 'default' — so the failure could read as "adoption
// is being conservative" rather than as a mis-slotting.
func TestLiveAdoptVariantSlotsAreIndependent(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})

	// The package's existing helper: it mints its own project id and returns both.
	briefID, project := insertApprovedBrief(ctx, t, pool)

	mk := func(variant string) error {
		_, err := repo.AdoptCampaign(ctx, &model.Campaign{
			ProjectID:          project,
			BriefID:            briefID,
			Platform:           model.ProviderGoogleAds,
			Variant:            variant,
			PlatformCampaignID: dbtest.UniqueID(t, "up"),
			CampaignName:       dbtest.UniqueID(t, "campaign"),
			Status:             model.CampaignStatusCreated,
		}, 1, nil)
		return err
	}

	if err := mk(model.VariantDefault); err != nil {
		t.Fatalf("adopting into the default slot: %v", err)
	}
	// A DIFFERENT campaign type on the same brief is a different slot, so it must succeed.
	if err := mk("demand-gen"); err != nil {
		t.Fatalf("adopting a demand-gen campaign onto a brief that already holds a default-slot one must succeed, got: %v", err)
	}
	// The same slot twice must still be refused — the whole point of the slot key.
	if err := mk("demand-gen"); err == nil {
		t.Error("adopting a SECOND demand-gen campaign onto the same brief must be refused; the slot key is what stops a duplicate paid campaign")
	}
}
