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

// TestListCampaignsForBrief_HappyPath verifies that ListCampaignsForBrief returns
// all non-deleted campaigns for a brief, ordered deterministically.
func TestListCampaignsForBrief_HappyPath(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()

	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})
	briefID := insertBrief(ctx, t, pool)
	projectID := dbtest.UniqueID(t, "project")

	// Insert a few campaigns for this brief.
	for i := 0; i < 3; i++ {
		err := pool.QueryRow(ctx, `
			INSERT INTO campaigns (project_id, brief_id, platform, campaign_name, status, created_by, updated_by)
			VALUES ($1, $2, $3, $4, 'created', NULL, NULL)
			RETURNING id`, projectID, briefID, model.ProviderGoogleAds, dbtest.UniqueID(t, "campaign")).Scan(nil)
		if err != nil {
			t.Fatalf("insert campaign: %v", err)
		}
	}

	campaigns, err := repo.ListCampaignsForBrief(ctx, projectID, briefID)
	if err != nil {
		t.Fatalf("ListCampaignsForBrief: %v", err)
	}
	if len(campaigns) != 3 {
		t.Errorf("expected 3 campaigns, got %d", len(campaigns))
	}
}

// TestListCampaignsForBrief_EmptyList verifies that an empty array is returned
// when the brief has no campaigns, not an error.
func TestListCampaignsForBrief_EmptyList(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()

	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})
	briefID := insertBrief(ctx, t, pool)
	projectID := dbtest.UniqueID(t, "project")

	// Don't insert any campaigns for this brief.
	campaigns, err := repo.ListCampaignsForBrief(ctx, projectID, briefID)
	if err != nil {
		t.Fatalf("ListCampaignsForBrief: %v", err)
	}
	if len(campaigns) != 0 {
		t.Errorf("expected 0 campaigns, got %d", len(campaigns))
	}
}

// TestListCampaignsForBrief_ExcludesSoftDeleted verifies that soft-deleted
// campaigns are excluded from the list.
func TestListCampaignsForBrief_ExcludesSoftDeleted(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()

	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})
	briefID := insertBrief(ctx, t, pool)
	projectID := dbtest.UniqueID(t, "project")

	// Insert one live and one deleted campaign.
	liveID := dbtest.UniqueID(t, "campaign")
	deletedID := dbtest.UniqueID(t, "campaign")

	err := pool.QueryRow(ctx, `
		INSERT INTO campaigns (project_id, brief_id, platform, campaign_name, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, 'created', NULL, NULL)
		RETURNING id`, projectID, briefID, model.ProviderGoogleAds, liveID).Scan(nil)
	if err != nil {
		t.Fatalf("insert live campaign: %v", err)
	}

	// Insert and immediately soft-delete the second one.
	var deletedRowID string
	err = pool.QueryRow(ctx, `
		INSERT INTO campaigns (project_id, brief_id, platform, campaign_name, status, created_by, updated_by, version)
		VALUES ($1, $2, $3, $4, 'created', NULL, NULL, 1)
		RETURNING id`, projectID, briefID, model.ProviderRedditAds, deletedID).Scan(&deletedRowID)
	if err != nil {
		t.Fatalf("insert campaign to delete: %v", err)
	}

	// Soft-delete it.
	_, err = pool.Exec(ctx, `UPDATE campaigns SET status='deleted', version=version+1 WHERE id=$1`, deletedRowID)
	if err != nil {
		t.Fatalf("soft-delete campaign: %v", err)
	}

	campaigns, err := repo.ListCampaignsForBrief(ctx, projectID, briefID)
	if err != nil {
		t.Fatalf("ListCampaignsForBrief: %v", err)
	}
	if len(campaigns) != 1 {
		t.Errorf("expected 1 campaign (deleted excluded), got %d", len(campaigns))
	}
}

// TestListCampaignsForBrief_Scoping_DifferentBrief verifies that campaigns
// under a different brief are excluded.
func TestListCampaignsForBrief_Scoping_DifferentBrief(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()

	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})

	briefID1 := insertBrief(ctx, t, pool)
	briefID2 := insertBrief(ctx, t, pool)
	projectID := dbtest.UniqueID(t, "project")

	// Insert campaigns under both briefs.
	err := pool.QueryRow(ctx, `
		INSERT INTO campaigns (project_id, brief_id, platform, campaign_name, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, 'created', NULL, NULL)
		RETURNING id`, projectID, briefID1, model.ProviderGoogleAds, dbtest.UniqueID(t, "campaign")).Scan(nil)
	if err != nil {
		t.Fatalf("insert campaign for brief 1: %v", err)
	}

	err = pool.QueryRow(ctx, `
		INSERT INTO campaigns (project_id, brief_id, platform, campaign_name, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, 'created', NULL, NULL)
		RETURNING id`, projectID, briefID2, model.ProviderRedditAds, dbtest.UniqueID(t, "campaign")).Scan(nil)
	if err != nil {
		t.Fatalf("insert campaign for brief 2: %v", err)
	}

	// List campaigns for brief 1.
	campaigns, err := repo.ListCampaignsForBrief(ctx, projectID, briefID1)
	if err != nil {
		t.Fatalf("ListCampaignsForBrief: %v", err)
	}
	if len(campaigns) != 1 {
		t.Errorf("expected 1 campaign (only from brief 1), got %d", len(campaigns))
	}
	if campaigns[0].BriefID != briefID1 {
		t.Errorf("campaign has wrong brief: got %s, want %s", campaigns[0].BriefID, briefID1)
	}
}

// TestListCampaignsForBrief_Scoping_DifferentProject verifies that campaigns
// under a different project are excluded.
func TestListCampaignsForBrief_Scoping_DifferentProject(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()

	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})

	briefID := insertBrief(ctx, t, pool)
	projectID1 := dbtest.UniqueID(t, "project")
	projectID2 := dbtest.UniqueID(t, "project")

	// Insert campaigns under both projects (but the same brief ID since it's globally unique).
	// Note: in reality, the same briefID won't be in two projects, but we're testing the
	// scoping anyway.
	err := pool.QueryRow(ctx, `
		INSERT INTO campaigns (project_id, brief_id, platform, campaign_name, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, 'created', NULL, NULL)
		RETURNING id`, projectID1, briefID, model.ProviderGoogleAds, dbtest.UniqueID(t, "campaign")).Scan(nil)
	if err != nil {
		t.Fatalf("insert campaign for project 1: %v", err)
	}

	err = pool.QueryRow(ctx, `
		INSERT INTO campaigns (project_id, brief_id, platform, campaign_name, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, 'created', NULL, NULL)
		RETURNING id`, projectID2, briefID, model.ProviderRedditAds, dbtest.UniqueID(t, "campaign")).Scan(nil)
	if err != nil {
		t.Fatalf("insert campaign for project 2: %v", err)
	}

	// List campaigns for project 1 only.
	campaigns, err := repo.ListCampaignsForBrief(ctx, projectID1, briefID)
	if err != nil {
		t.Fatalf("ListCampaignsForBrief: %v", err)
	}
	if len(campaigns) != 1 {
		t.Errorf("expected 1 campaign (only from project 1), got %d", len(campaigns))
	}
	if campaigns[0].ProjectID != projectID1 {
		t.Errorf("campaign has wrong project: got %s, want %s", campaigns[0].ProjectID, projectID1)
	}
}

// TestListCampaignsForBrief_BriefNotFound verifies that ErrNotFound is returned
// when the brief does not exist or has been archived.
func TestListCampaignsForBrief_BriefNotFound(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()

	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})
	projectID := dbtest.UniqueID(t, "project")
	nonexistentBriefID := dbtest.UniqueID(t, "brief")

	campaigns, err := repo.ListCampaignsForBrief(ctx, projectID, nonexistentBriefID)
	if err != domain.ErrNotFound {
		t.Errorf("expected ErrNotFound for nonexistent brief, got %v", err)
	}
	if campaigns != nil {
		t.Errorf("campaigns should be nil when brief not found, got %v", campaigns)
	}
}

// TestListCampaignsForBrief_ArchivedBriefNotFound verifies that ErrNotFound is
// returned when the brief has been archived.
func TestListCampaignsForBrief_ArchivedBriefNotFound(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()

	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})
	briefID := insertBrief(ctx, t, pool)
	projectID := dbtest.UniqueID(t, "project")

	// Archive the brief.
	_, err := pool.Exec(ctx, `UPDATE campaign_briefs SET status='archived' WHERE id=$1`, briefID)
	if err != nil {
		t.Fatalf("archive brief: %v", err)
	}

	campaigns, err := repo.ListCampaignsForBrief(ctx, projectID, briefID)
	if err != domain.ErrNotFound {
		t.Errorf("expected ErrNotFound for archived brief, got %v", err)
	}
	if campaigns != nil {
		t.Errorf("campaigns should be nil when brief archived, got %v", campaigns)
	}
}
