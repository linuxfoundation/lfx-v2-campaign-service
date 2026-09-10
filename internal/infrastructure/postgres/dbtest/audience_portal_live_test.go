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

// TestUpdateAudience_PortalRoundTripsThroughTheDatabase pins the one thing no other test on this
// column can see: that the value BuildAudience puts on the row is the value that comes back out.
//
// Every other test of built_in_portal_id runs against something that cannot fail this way. The
// build tests use fakeAudienceRepo, which stores a Go struct; TestScanAudience_PortalNullIsNotRecorded
// exercises the read; TestAudienceCols_ColumnOrderMatchesScanAudience compares SQL TEXT. None of
// them binds a parameter to a live PostgreSQL.
//
// That gap matters here more than for a typical column, because updateAudienceQuery does not bind
// in positional order -- built_in_portal_id is `$10` in a SET list whose earlier columns are $1..$5,
// with the WHERE clause holding $6..$9. It is written that way for a reason (appending the
// placeholder avoids renumbering every argument after it) and the reason is exactly what makes it
// fragile: a placeholder pointing at the wrong argument still compiles, still passes the SQL-text
// tests, and still passes every fake-backed build test. It surfaces as a build that succeeds with
// an in-memory portal while the persisted row holds someone else's value or NULL -- and then every
// dispatch of that audience is refused with ErrCampaignProvenanceUnknown, which reads like a
// missing lookup rather than a mis-bound query.
//
// Both directions are asserted. A non-empty portal must survive the round trip, and the empty case
// must persist as NULL rather than an empty string, because the dispatch guard distinguishes "no
// portal recorded" from a portal that does not match and an empty string would satisfy neither
// reading cleanly.
func TestUpdateAudience_PortalRoundTripsThroughTheDatabase(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewAudienceRepo(&postgres.Pool{Pool: pool})

	for name, portal := range map[string]string{
		"a resolved portal is persisted":    "8112310",
		"an unresolved portal stays absent": "",
	} {
		t.Run(name, func(t *testing.T) {
			briefID, projectID := insertApprovedBrief(ctx, t, pool)
			created, _, err := repo.CreateAudienceForApprovedBrief(ctx, &model.CampaignAudience{
				ProjectID: projectID,
				BriefID:   briefID,
				Platform:  model.ProviderHubSpot,
			})
			if err != nil {
				t.Fatalf("CreateAudienceForApprovedBrief: %v", err)
			}

			// What BuildAudience does: stamp the portal on the claimed row, then update.
			created.BuiltInPortalID = portal
			created.PlatformMasterListID = "30967"
			created.Status = model.AudienceBuilt
			updated, uerr := repo.UpdateAudience(ctx, created, created.Version)
			if uerr != nil {
				t.Fatalf("UpdateAudience: %v", uerr)
			}

			// RETURNING, so this is the row the database actually holds -- not the struct we sent.
			if updated.BuiltInPortalID != portal {
				t.Errorf("UpdateAudience returned portal %q, want %q: the write bound the wrong argument, "+
					"so a build succeeds in memory and every later dispatch of this row is refused",
					updated.BuiltInPortalID, portal)
			}
			// The neighbouring column, because a mis-numbered placeholder shows up as a SWAP: the
			// portal reading correctly while something else silently took its value would pass the
			// assertion above on its own.
			if updated.PlatformMasterListID != "30967" {
				t.Errorf("master list id = %q, want 30967: a neighbouring column was overwritten, "+
					"which is what an off-by-one placeholder looks like", updated.PlatformMasterListID)
			}

			// Read it back independently of RETURNING, and check NULL vs empty string at the
			// column itself -- scanAudience maps both to "", so the Go value cannot tell them apart.
			var isNull bool
			if qerr := pool.QueryRow(ctx,
				`SELECT built_in_portal_id IS NULL FROM campaign_audiences WHERE id=$1`,
				created.ID).Scan(&isNull); qerr != nil {
				t.Fatalf("read back: %v", qerr)
			}
			if wantNull := portal == ""; isNull != wantNull {
				t.Errorf("built_in_portal_id IS NULL = %v, want %v: an unresolved portal must be "+
					"absent rather than an empty string, so the dispatch guard's \"no portal recorded\" "+
					"arm stays distinguishable", isNull, wantNull)
			}
		})
	}
}
