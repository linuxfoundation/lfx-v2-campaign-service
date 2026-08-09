// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
)

// TestLiveCampaignActorColumnsExistAndAreNullable checks the half of migration 000016 that
// source text cannot reach.
//
// campaign_repo_test.go's TestMigration000016_AddsCampaignActorColumns asserts over the
// migration FILE — that it says ALTER TABLE campaigns and names both columns as JSONB. That
// is worth having, but it is an assertion about a string. It cannot tell you the migration
// RAN, that it applied to the deployed schema, or that a later migration did not drop or
// retype a column out from under the repository. This asks the catalog.
//
// Nullability is asserted, not assumed. Every campaign row predating 000016 has no actor to
// backfill, and attributedActor legitimately returns nil for a machine-to-machine caller, so
// a NOT NULL here would reject writes the service performs on purpose — and would do it at
// runtime, on the first unattributed dispatch, not at migration time.
func TestLiveCampaignActorColumnsExistAndAreNullable(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()

	for _, col := range []string{"created_by", "updated_by"} {
		var dataType, isNullable string
		err := pool.QueryRow(ctx, `
			SELECT data_type, is_nullable
			FROM information_schema.columns
			WHERE table_name = 'campaigns' AND column_name = $1`, col).Scan(&dataType, &isNullable)
		if err != nil {
			t.Fatalf("campaigns.%s not present in the migrated schema (%v) — migration 000016 "+
				"did not apply, and every actor read/write in campaign_repo.go fails at runtime", col, err)
		}
		if dataType != "jsonb" {
			t.Errorf("campaigns.%s data_type = %q, want jsonb — marshalActor writes a JSONB "+
				"document; another type round-trips through []byte but loses the ability to "+
				"query into the actor", col, dataType)
		}
		if isNullable != "YES" {
			t.Errorf("campaigns.%s is NOT NULL — rows predating 000016 have no actor to backfill, "+
				"and an unattributed (machine-to-machine) dispatch legitimately writes nil, so "+
				"this rejects writes the service makes on purpose", col)
		}
	}

	// And a nil actor really does insert. The catalog says the column permits NULL; this
	// says the table as a whole does — no trigger, no CHECK, no later migration has since
	// made an unattributed campaign unwritable.
	briefID := insertBrief(ctx, t, pool)
	var createdBy, updatedBy *string
	err := pool.QueryRow(ctx, `
		INSERT INTO campaigns (project_id, brief_id, platform, campaign_name, status, created_by, updated_by)
		VALUES ($1, $2, 'google-ads', $3, 'draft', NULL, NULL)
		RETURNING created_by::text, updated_by::text`,
		dbtest.UniqueID(t, "project"), briefID, dbtest.UniqueID(t, "unattributed")).
		Scan(&createdBy, &updatedBy)
	if err != nil {
		t.Fatalf("insert a campaign with no actor: %v — an unattributed dispatch cannot commit", err)
	}
	if createdBy != nil || updatedBy != nil {
		t.Errorf("actors read back as (%v, %v), want (nil, nil): NULL must mean \"not recorded\" "+
			"and must not be defaulted into a value naming a principal who never acted",
			createdBy, updatedBy)
	}
}
