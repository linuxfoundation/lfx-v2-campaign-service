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

// TestLiveListCampaignsForBriefOrdersAndExcludesDeleted checks the two properties
// ListCampaignsForBrief's callers depend on and that no unit test can establish.
//
// A fake can agree with a broken query. The brief-metrics handler renders these rows as a
// table an operator re-reads while a campaign runs, so the ORDER BY is a contract: without a
// total order Postgres may return the same rows in a different sequence between two reads and
// the table reshuffles under them. And the soft-delete exclusion is what makes a deleted
// campaign invisible, matching GetCampaign.
//
// Both are properties of the SQL against the real migrated schema, so both are checked here.
//
// LIMITATION, stated rather than glossed: the soft-delete assertion is revert-binding —
// dropping `status <> 'deleted'` fails this test — but the ORDER BY is NOT independently
// binding today. The planner serves this query from uq_campaigns_brief_platform_variant_live,
// which is keyed on (brief_id, platform, variant), so an index scan already returns rows in
// the asserted order and removing the clause changes nothing at these row counts (verified
// with EXPLAIN). The clause stays because that is an accident of the current plan: a bitmap
// heap scan or parallel seq scan on a larger table returns no order at all, and the consumer's
// table would start reshuffling between reads with nothing failing to say so.
func TestLiveListCampaignsForBriefOrdersAndExcludesDeleted(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})

	projectID := "cncf"
	// brief_id is UUID and carries a FOREIGN KEY to campaign_briefs(id), so the brief must
	// exist and its id must be a real UUID — dbtest.UniqueID returns a descriptive string,
	// which the column rejects outright (22P02).
	var briefID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO campaign_briefs (project_id, program_type, event_slug)
		VALUES ($1, 'events', $2) RETURNING id::text`,
		projectID, dbtest.UniqueID(t, "slug")).Scan(&briefID); err != nil {
		t.Fatalf("seed brief: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DELETE FROM campaign_briefs WHERE id=$1`, briefID); err != nil {
			t.Errorf("clean up seeded brief: %v", err)
		}
	})

	// Inserted in an order that does NOT match the expected output, so a query that returned
	// insertion order (or no order at all) is distinguishable from one that sorts.
	seed := []struct {
		platform model.Provider
		variant  string
		status   string
	}{
		{model.ProviderRedditAds, model.VariantDefault, "created"},
		{model.ProviderGoogleAds, "demand-gen", "created"},
		{model.ProviderMetaAds, model.VariantDefault, "deleted"}, // must not appear
		{model.ProviderGoogleAds, model.VariantDefault, "created"},
		{model.ProviderLinkedInAds, model.VariantDefault, "created"},
	}
	for _, s := range seed {
		if _, err := pool.Exec(ctx, `
			INSERT INTO campaigns (project_id, brief_id, platform, variant, campaign_name, status)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			projectID, briefID, string(s.platform), s.variant, "n/a", s.status); err != nil {
			t.Fatalf("seed campaign %s/%s: %v", s.platform, s.variant, err)
		}
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DELETE FROM campaigns WHERE brief_id=$1`, briefID); err != nil {
			t.Errorf("clean up seeded campaigns: %v", err)
		}
	})

	got, err := repo.ListCampaignsForBrief(ctx, projectID, briefID)
	if err != nil {
		t.Fatalf("ListCampaignsForBrief: %v", err)
	}

	// (platform, variant) ascending. google-ads/default sorts before google-ads/demand-gen?
	// No — 'd' < 'e' is false; "default" > "demand-gen" is false either way, so state the
	// expectation explicitly rather than reasoning about it in prose.
	type key struct{ platform, variant string }
	want := []key{
		{"google-ads", "default"},
		{"google-ads", "demand-gen"},
		{"linkedin-ads", "default"},
		{"reddit-ads", "default"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d campaigns, want %d — the soft-deleted meta row must be excluded", len(got), len(want))
	}
	for i, w := range want {
		if string(got[i].Platform) != w.platform || got[i].Variant != w.variant {
			t.Errorf("row %d = (%s, %s), want (%s, %s) — the ORDER BY is a contract the consumer's table depends on",
				i, got[i].Platform, got[i].Variant, w.platform, w.variant)
		}
	}
	for _, c := range got {
		if c.Platform == model.ProviderMetaAds {
			t.Error("a soft-deleted campaign was returned; reads must exclude it, matching GetCampaign")
		}
	}
}

// A brief with no campaigns reads as an EMPTY slice rather than an error: that is what every
// brief looks like before it is dispatched, and the brief-wide metrics read must be able to
// answer "nothing to measure yet" without failing.
func TestLiveListCampaignsForBriefEmptyBriefIsNotAnError(t *testing.T) {
	pool := dbtest.Pool(t)
	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})

	// A syntactically valid UUID that no brief uses: the query must return no rows rather
	// than fail. A non-UUID string would be rejected by the column type instead, which would
	// test the driver rather than the query.
	got, err := repo.ListCampaignsForBrief(context.Background(), "cncf", "00000000-0000-0000-0000-0000000000ff")
	if err != nil {
		t.Fatalf("an undispatched brief must not error: %v", err)
	}
	if got == nil {
		t.Error("returned a nil slice; the contract is a non-nil empty slice")
	}
	if len(got) != 0 {
		t.Errorf("got %d campaigns for a brief with none", len(got))
	}
}
