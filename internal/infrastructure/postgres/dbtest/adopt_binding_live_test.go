// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
)

// sqlstateUniqueViolation is what a duplicate binding must raise. The repo maps it onto
// domain.ErrConflict; here we assert the RAW code, because the point is that the database
// refuses the row at all, not how a Go layer dresses the refusal up.
const sqlstateUniqueViolation = "23505"

// insertBriefIn creates a brief in a CALLER-CHOSEN project.
//
// insertBrief (schema_live_test.go) derives the project from the test name, which is
// exactly wrong here: the guard under test is keyed WITHOUT project, so the assertions
// below have to place two briefs in one project and two briefs in different projects and
// get the same refusal from both. event_slug still has to differ — campaign_briefs
// carries its own partial-unique (project_id, event_slug) — so it is the only thing this
// helper varies.
func insertBriefIn(ctx context.Context, t *testing.T, pool *pgxpool.Pool, project, slug string) string {
	t.Helper()

	var briefID string
	err := pool.QueryRow(ctx, `
		INSERT INTO campaign_briefs (project_id, program_type, event_slug)
		VALUES ($1, 'events', $2)
		RETURNING id`, project, slug).Scan(&briefID)
	if err != nil {
		t.Fatalf("insert parent brief: %v", err)
	}
	return briefID
}

// insertBinding inserts one campaign row carrying an upstream campaign id. platformID is a
// *string so the NULL case — a dispatch claim that exists before the platform has minted
// anything — can be expressed, since that is one of the rows the index must NOT cover.
func insertBinding(ctx context.Context, t *testing.T, pool *pgxpool.Pool, project, briefID string, platformID *string, status string) error {
	t.Helper()
	return insertBindingOn(ctx, t, pool, model.ProviderGoogleAds, project, briefID, platformID, status)
}

// insertBindingOn is insertBinding with the provider spelled out. Only the microsoft-ads
// sub-test needs it: 000020's predicate is scoped to google-ads, so the provider is the one
// column that decides whether the index applies to a row at all, and asserting the scope
// means inserting rows OUTSIDE it.
func insertBindingOn(ctx context.Context, t *testing.T, pool *pgxpool.Pool, platform model.Provider, project, briefID string, platformID *string, status string) error {
	t.Helper()

	_, err := pool.Exec(ctx, `
		INSERT INTO campaigns (project_id, brief_id, platform, campaign_name, status, platform_campaign_id)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		project, briefID, platform, dbtest.UniqueID(t, "campaign"), status, platformID)
	return err
}

// TestLiveOneUpstreamCampaignBindsToOneBrief checks migration 000020 against the real
// migrated schema.
//
// Everywhere else this guarantee is exercised by a FAKE repository plus a regex over the
// repo's SQL text. Both assert what someone believed the index does. Neither can catch the
// failure that actually matters — a predicate or key definition that is subtly wrong, so
// the migration applies, every unit test passes, and two briefs quietly control one paid
// campaign. From then on each brief's toggle and metrics reader act on the same upstream
// campaign: one pauses what the other just enabled, and the rows are individually
// well-formed, so nothing in the service can detect it afterwards.
//
// Each sub-test mints its own platform_campaign_id, which is what keeps the sub-tests
// independent now that the index no longer includes project_id — a shared id would make
// one sub-test's leftover row the cause of the next one's refusal.
func TestLiveOneUpstreamCampaignBindsToOneBrief(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()

	t.Run("a second brief cannot bind the same upstream campaign", func(t *testing.T) {
		project := dbtest.UniqueID(t, "project")
		platformID := dbtest.UniqueID(t, "gaid")
		first := insertBriefIn(ctx, t, pool, project, dbtest.UniqueID(t, "slug"))
		second := insertBriefIn(ctx, t, pool, project, dbtest.UniqueID(t, "slug"))

		if err := insertBinding(ctx, t, pool, project, first, &platformID, testCampaignStatus); err != nil {
			t.Fatalf("first binding: %v", err)
		}

		err := insertBinding(ctx, t, pool, project, second, &platformID, testCampaignStatus)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Fatalf("err = %v, want a unique violation — two briefs in one project just "+
				"bound the SAME upstream campaign, which is the entire failure 000020 exists to stop", err)
		}
		if pgErr.Code != sqlstateUniqueViolation {
			t.Fatalf("SQLSTATE = %s (%s), want %s", pgErr.Code, pgErr.Message, sqlstateUniqueViolation)
		}
	})

	t.Run("soft deleting the first binding frees the upstream campaign", func(t *testing.T) {
		project := dbtest.UniqueID(t, "project")
		platformID := dbtest.UniqueID(t, "gaid")
		first := insertBriefIn(ctx, t, pool, project, dbtest.UniqueID(t, "slug"))
		second := insertBriefIn(ctx, t, pool, project, dbtest.UniqueID(t, "slug"))

		if err := insertBinding(ctx, t, pool, project, first, &platformID, testCampaignStatus); err != nil {
			t.Fatalf("first binding: %v", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE campaigns SET status = 'deleted' WHERE brief_id = $1`, first); err != nil {
			t.Fatalf("soft-delete: %v", err)
		}

		// Without `status <> 'deleted'` in the predicate this fails, and a campaign
		// deleted here could never be adopted again by anyone.
		if err := insertBinding(ctx, t, pool, project, second, &platformID, testCampaignStatus); err != nil {
			t.Fatalf("re-bind after soft delete: %v — a soft-deleted row is still holding "+
				"the upstream campaign, so deletion is permanent in a way nobody asked for", err)
		}
	})

	t.Run("unprovisioned rows do not collide with each other", func(t *testing.T) {
		project := dbtest.UniqueID(t, "project")
		first := insertBriefIn(ctx, t, pool, project, dbtest.UniqueID(t, "slug"))
		second := insertBriefIn(ctx, t, pool, project, dbtest.UniqueID(t, "slug"))

		// A dispatch claim inserts its row BEFORE the platform mints an id. Those rows
		// are not bindings of anything, and in PostgreSQL two NULLs are distinct under a
		// unique index anyway — but `platform_campaign_id IS NOT NULL` is in the
		// predicate so they are not indexed at all, and this pins that they stay
		// insertable however that detail is spelled.
		if err := insertBinding(ctx, t, pool, project, first, nil, testCampaignStatus); err != nil {
			t.Fatalf("first unprovisioned claim: %v", err)
		}
		if err := insertBinding(ctx, t, pool, project, second, nil, testCampaignStatus); err != nil {
			t.Fatalf("second unprovisioned claim: %v — the guard is catching rows that "+
				"have no upstream campaign, which would block every concurrent dispatch", err)
		}
	})

	// A DIFFERENT project must be refused too, and this is the case the obvious key gets
	// wrong. Scoping the index by project reads as the careful choice — a bare platform id
	// is unique only within the account that minted it — but it assumes each project has
	// its own account, and for the provider adoption actually supports that is false:
	// Google Ads is ONE shared customer across every foundation, with a connection row per
	// project pointing at it. Project-scoped, both rows insert cleanly and then toggle the
	// same live campaign against each other.
	//
	// This does not make adoption an ownership check — a project holding a connection to
	// the shared customer can already read and pause anything in it straight through
	// Google's API. It enforces the invariant that IS this service's: one upstream
	// campaign, one brief.
	t.Run("a different project may not bind the same id", func(t *testing.T) {
		platformID := dbtest.UniqueID(t, "gaid")
		projectA := dbtest.UniqueID(t, "projectA")
		projectB := dbtest.UniqueID(t, "projectB")
		briefA := insertBriefIn(ctx, t, pool, projectA, dbtest.UniqueID(t, "slug"))
		briefB := insertBriefIn(ctx, t, pool, projectB, dbtest.UniqueID(t, "slug"))

		if err := insertBinding(ctx, t, pool, projectA, briefA, &platformID, testCampaignStatus); err != nil {
			t.Fatalf("project A binding: %v", err)
		}

		err := insertBinding(ctx, t, pool, projectB, briefB, &platformID, testCampaignStatus)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Fatalf("err = %v, want a unique violation — two projects sharing one upstream "+
				"ad account just bound the SAME campaign, and on Google Ads every project "+
				"shares that account", err)
		}
		if pgErr.Code != sqlstateUniqueViolation {
			t.Fatalf("SQLSTATE = %s (%s), want %s", pgErr.Code, pgErr.Message, sqlstateUniqueViolation)
		}
	})

	// The mirror image of the sub-test above, and the reason 000020's predicate carries
	// `platform = 'google-ads'`. The argument for keying globally rests entirely on Google
	// Ads' shared customer id; it does not generalise. Microsoft campaign ids are
	// ACCOUNT-scoped, and this service supports separate per-project Microsoft connections,
	// so account B minting an id account A already used is not a collision — it is two
	// distinct campaigns in two distinct id spaces. Under an unscoped index the second
	// insert raises 23505 on a perfectly ordinary dispatch, and because only AdoptCampaign
	// classifies 23505 as adoption-specific, the operator gets a generic 409 and an
	// UNCONFIRMED partial rather than anything nameable.
	//
	// Remove the scope from the migration and this sub-test fails while its google-ads
	// sibling still passes, which is exactly the distinction worth pinning.
	t.Run("microsoft is not constrained", func(t *testing.T) {
		platformID := dbtest.UniqueID(t, "msid")
		projectA := dbtest.UniqueID(t, "projectA")
		projectB := dbtest.UniqueID(t, "projectB")
		briefA := insertBriefIn(ctx, t, pool, projectA, dbtest.UniqueID(t, "slug"))
		briefB := insertBriefIn(ctx, t, pool, projectB, dbtest.UniqueID(t, "slug"))

		if err := insertBindingOn(ctx, t, pool, model.ProviderMicrosoftAds, projectA, briefA, &platformID, testCampaignStatus); err != nil {
			t.Fatalf("account A binding: %v", err)
		}

		if err := insertBindingOn(ctx, t, pool, model.ProviderMicrosoftAds, projectB, briefB, &platformID, testCampaignStatus); err != nil {
			t.Fatalf("account B binding: %v — 000020 is covering microsoft-ads, where campaign "+
				"ids are account-scoped. Two separate Microsoft accounts minting the same id is "+
				"routine, so this rejects normal dispatches and leaves them UNCONFIRMED", err)
		}
	})
}
