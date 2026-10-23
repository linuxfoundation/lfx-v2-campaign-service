// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
)

// TestLiveClaimThenUpsertStampsPlatformBoundAt drives the orchestrator's real sequence —
// ClaimCampaignDispatch, then UpsertCampaign — and reads platform_bound_at (000048) back.
//
// The claim INSERTs a row with no upstream id, so it must carry no stamp: its created_at is the
// claim time, which is exactly what the deleted-campaign scope must NOT order ownership by. The
// upsert that binds the id stamps it, after the claim; the owning job's re-upsert of the SAME id
// keeps that first stamp, or a routine re-persist would move its binding later than a takeover
// that really came after it (lfx-self-serve-ops#897).
func TestLiveClaimThenUpsertStampsPlatformBoundAt(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})

	briefID := insertBrief(ctx, t, pool)
	projectID := dbtest.UniqueID(t, "project")
	jobID := uuid.NewString()

	claimed, _, err := repo.ClaimCampaignDispatch(ctx, projectID, briefID,
		model.ProviderGoogleAds, "", model.FirstSlotVersion, jobID, nil)
	if err != nil {
		t.Fatalf("ClaimCampaignDispatch: %v", err)
	}
	if !claimed {
		t.Fatal("precondition failed: the claim was not won on a fresh brief")
	}
	var (
		claimRowID string
		createdAt  time.Time
		boundAt    *time.Time
	)
	if qerr := pool.QueryRow(ctx,
		`SELECT id::text, created_at, platform_bound_at FROM campaigns WHERE brief_id = $1`, briefID).
		Scan(&claimRowID, &createdAt, &boundAt); qerr != nil {
		t.Fatalf("read the claim row: %v", qerr)
	}
	if boundAt != nil {
		t.Fatalf("claim row platform_bound_at = %v, want NULL: a claim binds no upstream id", boundAt)
	}

	upstream := dbtest.UniqueID(t, "upstream")
	upsert := func() *time.Time {
		t.Helper()
		got, uerr := repo.UpsertCampaign(ctx, &model.Campaign{
			ProjectID: projectID, BriefID: briefID,
			JobID:              &jobID,
			Platform:           model.ProviderGoogleAds,
			CampaignName:       "bound-at",
			Status:             "created",
			PlatformCampaignID: upstream,
		}, nil)
		if uerr != nil {
			t.Fatalf("UpsertCampaign: %v", uerr)
		}
		if got.ID != claimRowID {
			t.Fatalf("upsert landed on row %s, want the claim row %s", got.ID, claimRowID)
		}
		var stamp *time.Time
		if qerr := pool.QueryRow(ctx,
			`SELECT platform_bound_at FROM campaigns WHERE id = $1`, got.ID).Scan(&stamp); qerr != nil {
			t.Fatalf("read platform_bound_at: %v", qerr)
		}
		return stamp
	}

	first := upsert()
	if first == nil || !first.After(createdAt) {
		t.Fatalf("after finalizing the claim, platform_bound_at = %v; want a stamp after the claim's created_at %v",
			first, createdAt)
	}
	again := upsert()
	if again == nil || !again.Equal(*first) {
		t.Fatalf("re-upserting the same id moved platform_bound_at from %v to %v; the first binding must stand",
			first, again)
	}
}
