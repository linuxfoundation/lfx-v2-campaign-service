// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// TestListRecentProjectPlatformCampaigns_Live runs the HubSpot email monitor's scope query
// against a real database: newest first, bounded by the limit, scoped to the project and
// platform, blind to rows with no upstream id, and INCLUDING soft-deleted rows.
func TestListRecentProjectPlatformCampaigns_Live(t *testing.T) {
	pool := creativeAssetTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	briefID, projectID := insertCreativeAssetTestBrief(ctx, t, pool, "approved")
	otherBrief, otherProject := insertCreativeAssetTestBrief(ctx, t, pool, "approved")
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	insert := func(brief, project, platform, platformID, status string, slot int, created time.Time) {
		t.Helper()
		var pid any = platformID
		if platformID == "" {
			pid = nil
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO campaigns (project_id, brief_id, platform, variant, slot_version, platform_campaign_id,
			                       campaign_name, status, created_at)
			VALUES ($1, $2, $3, $8, $4, $5, 'n', $6, $7)`,
			project, brief, platform, slot, pid, status, created, fmt.Sprintf("v%d", slot)); err != nil {
			t.Fatalf("insert campaign: %v", err)
		}
	}
	insert(briefID, projectID, "hubspot", "101", "created", 1, base)
	insert(briefID, projectID, "hubspot", "102", "created", 2, base.Add(time.Hour))
	insert(briefID, projectID, "hubspot", "103", "created", 3, base.Add(2*time.Hour))
	insert(briefID, projectID, "hubspot", "104", "deleted", 4, base.Add(3*time.Hour))
	insert(briefID, projectID, "hubspot", "", "pending", 5, base.Add(4*time.Hour))
	insert(briefID, projectID, "meta-ads", "999", "created", 1, base.Add(5*time.Hour))
	insert(otherBrief, otherProject, "hubspot", "888", "created", 1, base.Add(6*time.Hour))

	repo := NewCampaignRepo(pool)
	got, err := repo.ListRecentProjectPlatformCampaigns(ctx, projectID, model.ProviderHubSpot, 10)
	if err != nil {
		t.Fatalf("ListRecentProjectPlatformCampaigns: %v", err)
	}
	var ids []string
	for _, c := range got {
		ids = append(ids, c.PlatformCampaignID)
	}
	// 104 is soft-deleted and still included — a local delete does not stop the HubSpot email —
	// carrying its status so the caller can mark it.
	if len(ids) != 4 || ids[0] != "104" || ids[1] != "103" || ids[2] != "102" || ids[3] != "101" {
		t.Fatalf("ids = %v, want [104 103 102 101]: newest first, own project and platform, dispatched (deleted included)", ids)
	}
	if got[0].Status != model.CampaignStatusDeleted || got[1].Status == model.CampaignStatusDeleted {
		t.Errorf("statuses = %q, %q; want the deleted row marked", got[0].Status, got[1].Status)
	}
	bounded, err := repo.ListRecentProjectPlatformCampaigns(ctx, projectID, model.ProviderHubSpot, 2)
	if err != nil || len(bounded) != 2 || bounded[0].PlatformCampaignID != "104" {
		t.Fatalf("limit 2 = %v, %v; want the two newest", bounded, err)
	}
	if _, err := repo.ListRecentProjectPlatformCampaigns(ctx, projectID, model.ProviderHubSpot, 0); err == nil {
		t.Error("a non-positive limit was read as no limit")
	}
	empty, err := repo.ListRecentProjectPlatformCampaigns(ctx, "no-such-project", model.ProviderHubSpot, 10)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Errorf("unknown project = %#v, %v; want an empty, non-nil slice", empty, err)
	}
}
