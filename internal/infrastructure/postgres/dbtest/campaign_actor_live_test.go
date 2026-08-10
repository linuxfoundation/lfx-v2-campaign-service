// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
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

// TestLiveReplaceCampaignActorWrite pins replaceCampaignQuery's `updated_by=COALESCE($9, updated_by)`
// against the real repository and the real migrated schema.
//
// Every other test of this assignment is an assertion about SQL source text or about a fake, and
// neither can see the two behaviours that matter, because both live in the COALESCE rather than in
// the Go around it: a supplied actor must REPLACE whoever last moved the row, and a nil actor must
// PRESERVE them. A plain `updated_by=$9` passes a source-shape check and every fake, and differs
// from the intended behaviour only on the second call below — which is the call that decides
// whether an unattributed machine-to-machine write erases the human whose action the row records.
//
// The prior mover is SEEDED, not left NULL, for the same reason the service-level actor tests seed
// one: with NULL there, COALESCE and a bare assignment both leave NULL behind on the nil-actor
// replace, and the test would pass against the defect it exists to catch.
func TestLiveReplaceCampaignActorWrite(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()

	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})
	briefID := insertBrief(ctx, t, pool)
	projectID := dbtest.UniqueID(t, "project")

	// created_by is seeded too, and asserted unchanged throughout: replaceCampaignQuery does not
	// name the column at all, so an edit that started writing it — the natural-looking symmetry —
	// would rewrite the row's origin on every ordinary update.
	first := &model.Actor{Name: "Katherine Johnson", Email: "kjohnson@example.org", Username: "kjohnson"}
	second := &model.Actor{Name: "Dorothy Vaughan", Email: "dvaughan@example.org", Username: "dvaughan"}
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal seed actor: %v", err)
	}

	var (
		id      string
		version int64
	)
	name := dbtest.UniqueID(t, "campaign")
	err = pool.QueryRow(ctx, `
		INSERT INTO campaigns (project_id, brief_id, platform, campaign_name, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, 'created', $5, $5)
		RETURNING id, version`,
		projectID, briefID, string(model.ProviderGoogleAds), name, string(firstJSON)).Scan(&id, &version)
	if err != nil {
		t.Fatalf("seed an attributed campaign: %v", err)
	}

	replace := func(actor *model.Actor, expected int64) *model.Campaign {
		t.Helper()
		// indexPayload is nil: the outbox co-commit is a different behaviour with its own tests,
		// and passing nil keeps this test's failure diagnostic about actors alone.
		got, rerr := repo.ReplaceCampaign(ctx, &model.Campaign{
			ID: id, ProjectID: projectID, BriefID: briefID,
			CampaignName: name, Status: "created", UpdatedBy: actor,
		}, expected, domain.CampaignLockToken{}, nil)
		if rerr != nil {
			t.Fatalf("ReplaceCampaign at version %d: %v", expected, rerr)
		}
		return got
	}

	// A supplied actor replaces the previous mover.
	updated := replace(second, version)
	if updated.UpdatedBy == nil || *updated.UpdatedBy != *second {
		t.Fatalf("after an attributed replace, updated_by = %+v, want %+v — the caller who "+
			"actually made this change is not recorded against it", updated.UpdatedBy, second)
	}
	if updated.CreatedBy == nil || *updated.CreatedBy != *first {
		t.Errorf("after an attributed replace, created_by = %+v, want %+v unchanged — a replace "+
			"must not rewrite who originally created the campaign", updated.CreatedBy, first)
	}

	// A nil actor preserves it. This is the assertion the COALESCE exists for.
	preserved := replace(nil, updated.Version)
	if preserved.UpdatedBy == nil || *preserved.UpdatedBy != *second {
		t.Errorf("after an UNattributed replace, updated_by = %+v, want %+v preserved — an "+
			"unattributed write (attributedActor returned nil, having logged it) is an ordinary "+
			"machine-to-machine update, not an instruction to forget the last human who acted",
			preserved.UpdatedBy, second)
	}
	if preserved.CreatedBy == nil || *preserved.CreatedBy != *first {
		t.Errorf("after an UNattributed replace, created_by = %+v, want %+v unchanged",
			preserved.CreatedBy, first)
	}
}
