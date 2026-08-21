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

// TestLiveRanOnSystemAccountColumnShape asks the CATALOG what migration 000027 actually
// produced, rather than what its SQL text says.
//
// The distinction is the whole reason this file exists: campaign_repo_test.go can assert the
// migration file mentions the column, but only a migrated database can say whether the column
// exists in the deployed schema, whether it is the type the repository binds a *bool to, and —
// the assertion that matters most here — whether it is NULLABLE with NO DEFAULT.
//
// Nullability and the absent default are not stylistic. They are the three-state contract the
// column is built on: NULL means "unknown, this row predates the column", false means "known to
// have run on the project's own account", true means "known to have run on the LF account". A
// NOT NULL or a DEFAULT false would collapse the first state into the second and silently
// assert, of every historical campaign and every future write that forgets the flag, that the
// project paid for it. That understates LF spend, and nothing downstream could detect it.
func TestLiveRanOnSystemAccountColumnShape(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()

	var dataType, isNullable string
	var columnDefault *string
	err := pool.QueryRow(ctx, `
		SELECT data_type, is_nullable, column_default
		FROM information_schema.columns
		WHERE table_name = 'campaigns' AND column_name = 'ran_on_system_account'`).
		Scan(&dataType, &isNullable, &columnDefault)
	if err != nil {
		t.Fatalf("campaigns.ran_on_system_account not present in the migrated schema (%v) — "+
			"migration 000027 did not apply, and every read/write of the column in "+
			"campaign_repo.go fails at runtime", err)
	}
	if dataType != "boolean" {
		t.Errorf("campaigns.ran_on_system_account data_type = %q, want boolean", dataType)
	}
	if isNullable != "YES" {
		t.Error("campaigns.ran_on_system_account is NOT NULL — rows predating 000027 have no " +
			"provenance to backfill, so this either rejects them or forces a fabricated value")
	}
	if columnDefault != nil {
		t.Errorf("campaigns.ran_on_system_account has DEFAULT %q — a default makes the ABSENCE "+
			"of a value into a positive claim about which account paid; the writer must state "+
			"the fact or leave it unknown", *columnDefault)
	}
}

// TestLiveUpsertCampaignPersistsProvenance is the test an in-memory fake cannot stand in for:
// it drives the real UpsertCampaign against a real migrated Postgres and reads the stored row
// back, so a migration that applies cleanly while the INSERT writes the wrong value (or writes
// nothing, leaving NULL) is caught here and nowhere else.
//
// All three states are exercised, because they are three different claims and only one of them
// is the "obvious" one.
func TestLiveUpsertCampaignPersistsProvenance(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})

	yes, no := true, false
	cases := []struct {
		name string
		flag *bool
		want *bool
		why  string
	}{
		{
			name: "system account",
			flag: &yes,
			want: &yes,
			why: "a campaign dispatched through the LF system fallback must persist true, or " +
				"LF-funded spend cannot be attributed back to the project that incurred it",
		},
		{
			name: "project's own connection",
			flag: &no,
			want: &no,
			why: "a campaign dispatched on the project's own connection must persist false — " +
				"not NULL, which would understate what we actually know about the row",
		},
		{
			name: "unknown",
			flag: nil,
			want: nil,
			why: "a nil flag must persist as SQL NULL rather than collapsing to false; false " +
				"is a positive claim that the project's own account paid",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			briefID := insertBrief(ctx, t, pool)
			projectID := dbtest.UniqueID(t, "project")

			// indexPayload is nil: the outbox co-commit has its own tests, and passing nil keeps
			// this test's diagnostic about provenance alone.
			got, err := repo.UpsertCampaign(ctx, &model.Campaign{
				ProjectID: projectID, BriefID: briefID,
				Platform:           model.ProviderGoogleAds,
				CampaignName:       dbtest.UniqueID(t, "campaign"),
				Status:             "created",
				PlatformCampaignID: dbtest.UniqueID(t, "upstream"),
				RanOnSystemAccount: tc.flag,
			}, nil)
			if err != nil {
				t.Fatalf("UpsertCampaign: %v", err)
			}

			// The RETURNING round-trip: what the repository hands back.
			assertProvenance(t, got.RanOnSystemAccount, tc.want, "UpsertCampaign returned", tc.why)

			// And what is actually ON DISK — the RETURNING clause and a subsequent read could in
			// principle disagree (a trigger, a rewritten default), and the stored value is what
			// every reporting query will see.
			var stored *bool
			if qerr := pool.QueryRow(ctx,
				`SELECT ran_on_system_account FROM campaigns WHERE id = $1`, got.ID).Scan(&stored); qerr != nil {
				t.Fatalf("read the stored row: %v", qerr)
			}
			assertProvenance(t, stored, tc.want, "the stored row holds", tc.why)
		})
	}
}

// TestLiveUpsertDoesNotRecomputeProvenanceOnUpdate is the LFXV2-3050 invariant that the
// column's meaning depends on, and the one most likely to be broken by a well-intentioned
// later edit: the flag records which account served the campaign AT CREATION, and a
// subsequent write must not revise it.
//
// The scenario is the real one. A campaign is created on the LF system account (true).
// Later the project connects its own ad account, and something updates the row — a status
// toggle, a re-persist of a dispatch result, any second upsert on the same slot. That update
// carries the CURRENT credential's answer (false, or nothing at all). If the conflict arm
// wrote it, the row would then claim the project paid for a campaign the LF actually funded:
// history rewritten by later configuration, with no trace that it changed.
//
// Both update shapes are exercised, because they fail differently. A bare assignment breaks
// on the false case; a COALESCE — the "safe" fix someone reaches for — still breaks on it,
// since false is not NULL. What holds both is the conflict arm's write-once guard, which
// assigns only while the STORED value IS NULL.
//
// Note what this test does NOT cover, and why it needs its sibling in
// system_account_provenance_dispatch_live_test.go: it seeds the row with UpsertCampaign
// itself, so the INSERT arm stamps the provenance before the update under test runs. The
// production path seeds the row with ClaimCampaignDispatch instead, which stamps nothing —
// so this test passed even while the conflict arm discarded the flag on every real dispatch.
// A test that both creates and updates through the same INSERT arm can only see the freezing
// half of the invariant, never the writing half.
func TestLiveUpsertDoesNotRecomputeProvenanceOnUpdate(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})

	yes, no := true, false
	for _, tc := range []struct {
		name   string
		update *bool
		why    string
	}{
		{
			name:   "later write says the project's own account",
			update: &no,
			why: "the project connected its own ad account after the campaign was created; " +
				"that does not change who paid for the campaign already created",
		},
		{
			name:   "later write knows nothing",
			update: nil,
			why: "an update that carries no provenance (a DB-only status change) must leave " +
				"the recorded fact alone rather than erasing it to \"unknown\"",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			briefID := insertBrief(ctx, t, pool)
			projectID := dbtest.UniqueID(t, "project")
			name := dbtest.UniqueID(t, "campaign")
			upstream := dbtest.UniqueID(t, "upstream")

			base := func(flag *bool, status string) *model.Campaign {
				return &model.Campaign{
					ProjectID: projectID, BriefID: briefID,
					Platform:           model.ProviderGoogleAds,
					CampaignName:       name,
					Status:             status,
					PlatformCampaignID: upstream,
					RanOnSystemAccount: flag,
				}
			}

			// Created on the LF system account.
			created, err := repo.UpsertCampaign(ctx, base(&yes, "created"), nil)
			if err != nil {
				t.Fatalf("seed the system-account campaign: %v", err)
			}
			if created.RanOnSystemAccount == nil || !*created.RanOnSystemAccount {
				t.Fatal("precondition failed: the seed campaign did not record the system " +
					"account, so this test cannot detect a later overwrite")
			}

			// A second upsert on the SAME slot: the conflict arm.
			updated, err := repo.UpsertCampaign(ctx, base(tc.update, "active"), nil)
			if err != nil {
				t.Fatalf("second upsert: %v", err)
			}
			if updated.ID != created.ID {
				t.Fatalf("the second upsert INSERTed a new row (%s vs %s) instead of taking the "+
					"conflict arm — this test is not exercising the update path it exists for",
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
			if stored == nil {
				t.Fatalf("ran_on_system_account was ERASED to NULL by a later update — %s", tc.why)
			}
			if !*stored {
				t.Fatalf("ran_on_system_account was rewritten to false by a later update: the "+
					"row now claims the project paid for a campaign created on the LF system "+
					"account. %s", tc.why)
			}
		})
	}
}

// assertProvenance compares two nullable provenance values, distinguishing all three states in
// the diagnostic — "nil" and "false" are different claims and a bare %v would blur them.
func assertProvenance(t *testing.T, got, want *bool, where, why string) {
	t.Helper()
	if want == nil {
		if got != nil {
			t.Errorf("%s %v, want NULL (unknown): %s", where, *got, why)
		}
		return
	}
	if got == nil {
		t.Errorf("%s NULL (unknown), want %v: %s", where, *want, why)
		return
	}
	if *got != *want {
		t.Errorf("%s %v, want %v: %s", where, *got, *want, why)
	}
}
