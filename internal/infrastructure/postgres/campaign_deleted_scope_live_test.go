// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// TestListProjectPlatformDeletedCampaignIDs_Live runs the account monitor's soft-deleted scope
// against a real database: only this project's deleted rows on this platform that have an
// upstream id, and NOT one another project has since adopted — a live row elsewhere makes the
// campaign that project's, and it must leave this project's monitor (lfx-self-serve-ops#897).
func TestListProjectPlatformDeletedCampaignIDs_Live(t *testing.T) {
	pool := creativeAssetTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	briefA, projectA := insertCreativeAssetTestBrief(ctx, t, pool, "approved")
	briefB, projectB := insertCreativeAssetTestBrief(ctx, t, pool, "approved")
	// Unique per run: the schema is shared across runs, and the meta binding index is global
	// over live rows, so a fixed id would collide with the previous run's live row.
	id := func(name string) string { return creativeAssetUniqueID(t, name) }
	kept, adopted, ownLive, live, bDeleted := id("kept"), id("adopted"), id("own-live"), id("live"), id("b-deleted")

	slot := 0
	insert := func(brief, project, platformID, status string) {
		t.Helper()
		slot++
		var pid any = platformID
		if platformID == "" {
			pid = nil
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO campaigns (project_id, brief_id, platform, variant, slot_version, platform_campaign_id,
			                       campaign_name, status)
			VALUES ($1, $2, 'meta-ads', $5, $3, $4, 'n', $6)`,
			project, brief, slot, pid, fmt.Sprintf("v%d", slot), status); err != nil {
			t.Fatalf("insert campaign: %v", err)
		}
	}
	insert(briefA, projectA, kept, "deleted")
	// Deleted here, then adopted by project B: B's live row takes it over.
	insert(briefA, projectA, adopted, "deleted")
	insert(briefB, projectB, adopted, "created")
	// Deleted and live in the SAME project: kept, it is this project's either way.
	insert(briefA, projectA, ownLive, "deleted")
	insert(briefA, projectA, ownLive, "created")
	insert(briefA, projectA, live, "created")
	insert(briefA, projectA, "", "deleted")
	insert(briefB, projectB, bDeleted, "deleted")

	got, err := NewCampaignRepo(pool).ListProjectPlatformDeletedCampaignIDs(ctx, projectA, model.ProviderMetaAds)
	if err != nil {
		t.Fatalf("ListProjectPlatformDeletedCampaignIDs: %v", err)
	}
	ids := make([]string, 0, len(got))
	for _, s := range got {
		ids = append(ids, s.PlatformCampaignID)
	}
	want := []string{kept, ownLive}
	sort.Strings(want)
	if len(ids) != 2 || ids[0] != want[0] || ids[1] != want[1] {
		t.Fatalf("ids = %v, want %v: own deleted rows with an upstream id, none another project holds live", ids, want)
	}

	empty, err := NewCampaignRepo(pool).ListProjectPlatformDeletedCampaignIDs(ctx, "no-such-project", model.ProviderMetaAds)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Errorf("unknown project = %#v, %v; want an empty, non-nil slice", empty, err)
	}
}
