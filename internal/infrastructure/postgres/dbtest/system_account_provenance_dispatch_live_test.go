// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
)

// TestLiveClaimThenUpsertPersistsProvenance drives the sequence the orchestrator ACTUALLY
// runs — ClaimCampaignDispatch first, UpsertCampaign second — and then reads
// ran_on_system_account back off disk.
//
// Every other live test in this file's neighbourhood seeds its row with UpsertCampaign
// itself, so the upsert's INSERT arm runs and stamps the column. That is not the production
// path. Orchestrator.dispatchPlatform reaches an upsert only after ClaimCampaignDispatch
// returned claimed=true, and that claim has ALREADY INSERTED a 'pending' row on the same
// (brief_id, platform, variant) slot. By the time UpsertCampaign runs the row exists, so the
// upsert takes its DO UPDATE arm — and the column is deliberately absent from that arm's SET
// list. Without the conditional update this test pins, the dispatcher computes the flag and
// the database discards it, leaving NULL on every normal dispatch: the exact three-state
// collapse ("unknown") that the column exists to prevent, and one no unit test can see
// because the fake repositories have no conflict arm at all.
//
// The claim is what makes this test different from TestLiveUpsertCampaignPersistsProvenance.
// Remove the claim and this becomes that test, and passes against broken code.
func TestLiveClaimThenUpsertPersistsProvenance(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})

	yes, no := true, false
	for _, tc := range []struct {
		name string
		flag *bool
		want *bool
		why  string
	}{
		{
			name: "dispatched on the LF system account",
			flag: &yes,
			want: &yes,
			why: "this is the case the column was added for: LF-funded spend must be " +
				"attributable, and a normal dispatch is the ONLY way such a row is ever created",
		},
		{
			name: "dispatched on the project's own connection",
			flag: &no,
			want: &no,
			why: "false is a positive claim that the project paid, and it must survive the " +
				"claim/upsert pair just as true does",
		},
		{
			name: "dispatcher recorded no provenance",
			flag: nil,
			want: nil,
			why: "a dispatcher that could not determine the account leaves NULL; the row is " +
				"honestly unknown rather than falsely attributed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			briefID := insertBrief(ctx, t, pool)
			projectID := dbtest.UniqueID(t, "project")
			jobID := uuid.NewString()

			// Step 1 — the claim. This INSERTs the 'pending' placeholder that makes the
			// upsert below take its conflict arm.
			claimed, _, err := repo.ClaimCampaignDispatch(ctx, projectID, briefID,
				model.ProviderGoogleAds, "", jobID, nil)
			if err != nil {
				t.Fatalf("ClaimCampaignDispatch: %v", err)
			}
			if !claimed {
				t.Fatal("precondition failed: the claim was not won on a fresh brief, so this " +
					"test is not exercising the claim-then-upsert path it exists for")
			}

			// Step 2 — the upsert the dispatcher performs on success, carrying the flag the
			// credential resolver computed.
			got, err := repo.UpsertCampaign(ctx, &model.Campaign{
				ProjectID: projectID, BriefID: briefID,
				JobID:              &jobID,
				Platform:           model.ProviderGoogleAds,
				CampaignName:       dbtest.UniqueID(t, "campaign"),
				Status:             "created",
				PlatformCampaignID: dbtest.UniqueID(t, "upstream"),
				RanOnSystemAccount: tc.flag,
			}, nil)
			if err != nil {
				t.Fatalf("UpsertCampaign after the claim: %v", err)
			}

			assertProvenance(t, got.RanOnSystemAccount, tc.want,
				"after claim+upsert, UpsertCampaign returned", tc.why)

			var stored *bool
			if qerr := pool.QueryRow(ctx,
				`SELECT ran_on_system_account FROM campaigns WHERE id = $1`, got.ID).Scan(&stored); qerr != nil {
				t.Fatalf("read the stored row: %v", qerr)
			}
			assertProvenance(t, stored, tc.want,
				"after claim+upsert, the stored ran_on_system_account holds", tc.why)
		})
	}
}

// TestLiveProvenanceIsWrittenOnceThenFrozen pins the two halves of the invariant that the
// conditional update must hold SIMULTANEOUSLY, and that a naive fix breaks one of.
//
// The finding this file's sibling test exposes is that the column was never written on the
// dispatch path. The obvious repair — put ran_on_system_account in the DO UPDATE SET list —
// fixes that and destroys the reason it was omitted: the flag is a historical fact about
// money already spent, and a project that connects its own ad account next month must not be
// able to rewrite who paid for a campaign the LF funded. So the update has to be conditional
// on the stored value being NULL, and these sub-tests are the two directions of that:
//
//   - a stored FALSE must not be upgradable to TRUE (the case a COALESCE would miss, since
//     false is not NULL), and
//   - a stored TRUE must not be erased or flipped by any later write.
func TestLiveProvenanceIsWrittenOnceThenFrozen(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})

	yes, no := true, false
	for _, tc := range []struct {
		name    string
		first   *bool
		second  *bool
		wantEnd *bool
		why     string
	}{
		{
			name:    "stored false is not upgradable to true",
			first:   &no,
			second:  &yes,
			wantEnd: &no,
			why: "the campaign ran on the project's own account; a later write carrying true " +
				"would move already-spent project money onto the LF's books",
		},
		{
			name:    "stored true is not downgradable to false",
			first:   &yes,
			second:  &no,
			wantEnd: &yes,
			why: "the project connected its own account after the fact; that does not change " +
				"who paid for a campaign the LF already funded",
		},
		{
			name:    "stored true is not erased by a write carrying nothing",
			first:   &yes,
			second:  nil,
			wantEnd: &yes,
			why: "a DB-only status change carries no provenance, and must leave the recorded " +
				"fact alone rather than degrading it to \"unknown\"",
		},
		{
			name:    "stored false is not erased by a write carrying nothing",
			first:   &no,
			second:  nil,
			wantEnd: &no,
			why: "false is knowledge, not an absence; a later unattributed write must not " +
				"turn it back into \"unknown\"",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			briefID := insertBrief(ctx, t, pool)
			projectID := dbtest.UniqueID(t, "project")
			jobID := uuid.NewString()
			name := dbtest.UniqueID(t, "campaign")
			upstream := dbtest.UniqueID(t, "upstream")

			claimed, _, err := repo.ClaimCampaignDispatch(ctx, projectID, briefID,
				model.ProviderGoogleAds, "", jobID, nil)
			if err != nil {
				t.Fatalf("ClaimCampaignDispatch: %v", err)
			}
			if !claimed {
				t.Fatal("precondition failed: the claim was not won on a fresh brief")
			}

			base := func(flag *bool, status string) *model.Campaign {
				return &model.Campaign{
					ProjectID: projectID, BriefID: briefID,
					JobID:              &jobID,
					Platform:           model.ProviderGoogleAds,
					CampaignName:       name,
					Status:             status,
					PlatformCampaignID: upstream,
					RanOnSystemAccount: flag,
				}
			}

			created, err := repo.UpsertCampaign(ctx, base(tc.first, "created"), nil)
			if err != nil {
				t.Fatalf("first upsert (finalising the claim): %v", err)
			}
			if created.RanOnSystemAccount == nil || *created.RanOnSystemAccount != *tc.first {
				t.Fatalf("precondition failed: the first upsert did not record %v, so this "+
					"sub-test cannot detect a later overwrite", *tc.first)
			}

			updated, err := repo.UpsertCampaign(ctx, base(tc.second, "active"), nil)
			if err != nil {
				t.Fatalf("second upsert: %v", err)
			}
			if updated.ID != created.ID {
				t.Fatalf("the second upsert INSERTed a new row (%s vs %s) instead of taking "+
					"the conflict arm — this sub-test is not exercising the update path",
					updated.ID, created.ID)
			}
			if updated.Version <= created.Version {
				t.Fatalf("version did not advance (%d -> %d): the update arm did not run",
					created.Version, updated.Version)
			}

			var stored *bool
			if qerr := pool.QueryRow(ctx,
				`SELECT ran_on_system_account FROM campaigns WHERE id = $1`, created.ID).Scan(&stored); qerr != nil {
				t.Fatalf("read the stored row: %v", qerr)
			}
			assertProvenance(t, stored, tc.wantEnd,
				"after a second upsert, the stored ran_on_system_account holds", tc.why)
		})
	}
}
