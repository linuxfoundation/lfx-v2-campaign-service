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
// upstream id, and NOT one another project has since adopted — a live row elsewhere, or a later
// one even once deleted, makes the campaign that project's, and it must leave this project's
// monitor (lfx-self-serve-ops#897).
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
	adoptedThenDeleted, reclaimed := id("adopted-then-deleted"), id("reclaimed")

	// created_at is set per row, one second apart in insertion order, so "recorded later" is
	// the order the rows are inserted below rather than whatever now() each insert happens to see.
	base := time.Now().UTC().Add(-time.Hour)
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
			                       campaign_name, status, created_at)
			VALUES ($1, $2, 'meta-ads', $5, $3, $4, 'n', $6, $7)`,
			project, brief, slot, pid, fmt.Sprintf("v%d", slot), status, base.Add(time.Duration(slot)*time.Second)); err != nil {
			t.Fatalf("insert campaign: %v", err)
		}
	}
	insert(briefA, projectA, kept, "deleted")
	// Deleted here, then adopted by project B: B's live row takes it over.
	insert(briefA, projectA, adopted, "deleted")
	insert(briefB, projectB, adopted, "created")
	// Deleted here, adopted by B, then deleted by B too: B's later row still holds it, so it
	// must not come back into this project's monitor with the budget and spend B ran on it.
	insert(briefA, projectA, adoptedThenDeleted, "deleted")
	insert(briefB, projectB, adoptedThenDeleted, "deleted")
	// The reverse order: B's deleted row is OLDER, so this project is the latest owner and keeps it.
	insert(briefB, projectB, reclaimed, "deleted")
	insert(briefA, projectA, reclaimed, "deleted")
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
	want := []string{kept, ownLive, reclaimed}
	sort.Strings(want)
	if fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Fatalf("ids = %v, want %v: own deleted rows with an upstream id, none another project holds live or recorded later", ids, want)
	}

	empty, err := NewCampaignRepo(pool).ListProjectPlatformDeletedCampaignIDs(ctx, "no-such-project", model.ProviderMetaAds)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Errorf("unknown project = %#v, %v; want an empty, non-nil slice", empty, err)
	}
}
