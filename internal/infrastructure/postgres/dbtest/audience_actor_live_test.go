// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
)

// insertApprovedBrief creates the APPROVED parent CreateAudienceForApprovedBrief requires and
// returns its id and version. insertBrief leaves the row at its default status, which that
// method rejects with ErrStaleApproval before it ever reaches the INSERT — a test built on it
// would pass without exercising the statement under test at all.
func insertApprovedBrief(ctx context.Context, t *testing.T, pool *pgxpool.Pool) (string, string, int64) {
	t.Helper()

	id := dbtest.UniqueID(t, "brief")
	var (
		briefID   string
		projectID string
		version   int64
	)
	err := pool.QueryRow(ctx, `
		INSERT INTO campaign_briefs (project_id, program_type, event_slug, status)
		VALUES ($1, 'events', $2, 'approved')
		RETURNING id, project_id, version`, id, id).Scan(&briefID, &projectID, &version)
	if err != nil {
		t.Fatalf("insert approved parent brief: %v", err)
	}
	return briefID, projectID, version
}

// TestCreateAudienceForApprovedBrief_EmptyRawJSONIsNotAFailedInsert pins what nullJSON is
// actually load-bearing for on this path, which is not what it looks like.
//
// The tempting claim is that binding a json.RawMessage directly makes pgx marshal a nil slice
// to the JSONB literal `null` instead of SQL NULL. It does not: pgx v5 tests nil-ness before
// its JSON codec runs, so the nil case — marshalActor returning nil for an unauthenticated
// build — binds as SQL NULL either way. A test written against that claim passes with the
// wrapper REMOVED, which is how the claim was falsified.
//
// The real difference is the EMPTY-but-non-nil value. `json.RawMessage{}` is not nil, reaches
// the codec, is sent as zero bytes, and PostgreSQL rejects it: SQLSTATE 22P02, invalid input
// syntax for type json. So the failure mode is a FAILED INSERT, not a wrong row — louder than
// the one originally alleged, and invisible to the SQL-text tests in audience_repo_test.go,
// which assert placeholder positions and cannot see the bound Go value at all.
//
// No caller passes an empty non-nil value today, so this pins a guard rather than a live bug.
// It is still worth pinning: CreateAudience and CreateAudienceForApprovedBrief now differ in
// nothing but this wrapper, and an unexplained difference between two near-identical inserts
// is the kind that gets normalised away by whoever touches them next.
//
// Revert check: drop the nullJSON wrappers and the empty-value subtest fails with the 22P02
// insert error. The nil subtest passes either way, deliberately — it records that the nil
// case is safe on its own, so nobody re-derives the wrong reason for the wrapper.
func TestCreateAudienceForApprovedBrief_EmptyRawJSONIsNotAFailedInsert(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewAudienceRepo(&postgres.Pool{Pool: pool})

	cases := map[string]json.RawMessage{
		// What audience_build.go produces for an unauthenticated build: marshalActor
		// returns a nil RawMessage, and SuppressionListIDs is never set.
		"nil":   nil,
		"empty": {},
	}
	for name, v := range cases {
		t.Run(name, func(t *testing.T) {
			briefID, projectID, version := insertApprovedBrief(ctx, t, pool)
			created, err := repo.CreateAudienceForApprovedBrief(ctx, &model.CampaignAudience{
				ProjectID:          projectID,
				BriefID:            briefID,
				Platform:           model.ProviderHubSpot,
				CreatedBy:          v,
				SuppressionListIDs: v,
			}, version)
			if err != nil {
				t.Fatalf("CreateAudienceForApprovedBrief(%s): %v — an absent actor must store "+
					"as SQL NULL, not abort the build. The row is the audience the caller is "+
					"waiting on; failing the insert loses the whole build, not just its "+
					"attribution.", name, err)
			}

			// Three-way, not a boolean: `IS NULL` alone cannot tell SQL NULL from the JSONB
			// literal `null`, and keeping them apart is the point of the column.
			for _, col := range []string{"created_by", "suppression_list_ids"} {
				var got string
				q := `SELECT CASE
					WHEN ` + col + ` IS NULL THEN 'sql-null'
					WHEN ` + col + ` = 'null'::jsonb THEN 'jsonb-null'
					ELSE 'value' END
					FROM campaign_audiences WHERE id = $1`
				if qerr := pool.QueryRow(ctx, q, created.ID).Scan(&got); qerr != nil {
					t.Fatalf("classify %s: %v", col, qerr)
				}
				if got != "sql-null" {
					t.Errorf("%s stored as %q, want \"sql-null\": an unattributed build records "+
						"ABSENCE. The JSONB literal `null` would assert the actor was captured "+
						"and is nothing, which nothing downstream can distinguish from a corrupt "+
						"write — and this trail is the only record of who spent the money, "+
						"because builds run through shared system accounts.", col, got)
				}
			}
		})
	}
}
