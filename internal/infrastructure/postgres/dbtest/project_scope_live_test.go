// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
)

// TestLiveListProjectPlatformCampaignIDsIsTenantScoped exercises the query that is the
// authorization boundary for the keyword and audience reads.
//
// campaign_repo_test.go asserts SQL SUBSTRINGS on this query. That can prove the clauses are
// present in the text and nothing more: it cannot prove the two bind arguments land on the
// columns they name, that the exclusions actually remove rows, or that a nullable `result`
// scans without error. Those are properties of the SQL executed against the real migrated
// schema, and each one decides whether one project's GAQL read can be scoped with ANOTHER
// project's upstream campaign ids — on a customer shared across every foundation.
//
// The seed is built so a query that dropped any single clause returns a DIFFERENT id set:
//   - same provider, second project      -> project_id must exclude it
//   - same project, different provider   -> platform must exclude it
//   - same project+provider, deleted     -> status <> 'deleted' must exclude it
//   - same project+provider, empty id    -> the empty-string guard on platform_campaign_id
//     must exclude it
//   - same project+provider, NULL id     -> the IS NOT NULL guard must exclude it
//   - two live rows, one with NULL result and one with provenance JSON -> both returned,
//     and the nullable scan must not error
func TestLiveListProjectPlatformCampaignIDsIsTenantScoped(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})

	projectID := dbtest.UniqueID(t, "proj-under-test")
	otherProject := dbtest.UniqueID(t, "proj-other")

	newBrief := func(project string) string {
		var id string
		if err := pool.QueryRow(ctx, `
			INSERT INTO campaign_briefs (project_id, program_type, event_slug)
			VALUES ($1, 'events', $2) RETURNING id::text`,
			project, dbtest.UniqueID(t, "slug")).Scan(&id); err != nil {
			t.Fatalf("seed brief for %s: %v", project, err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `DELETE FROM campaigns WHERE brief_id=$1`, id)
			if _, err := pool.Exec(context.Background(), `DELETE FROM campaign_briefs WHERE id=$1`, id); err != nil {
				t.Errorf("clean up brief %s: %v", id, err)
			}
		})
		return id
	}

	briefUnderTest := newBrief(projectID)
	briefOther := newBrief(otherProject)

	// Platform ids are MINTED PER RUN, not hardcoded. uq_campaigns_platform_campaign_live is
	// UNIQUE on (platform, platform_campaign_id) for live google-ads rows and does NOT include
	// project_id, so a fixed id is globally unique across the whole database: one row left
	// behind by an earlier run (the campaign cleanup below tolerates failure) makes the next
	// seed fail with 23505 BEFORE any tenant-scope assertion runs. Sibling live tests mint
	// theirs for the same reason.
	//
	// numericID keeps them digits-only — platform_campaign_id is an upstream numeric handle and
	// the scope predicate this test's data feeds requires the canonical spelling — and the
	// `n` prefix digit keeps the ORDER BY ASC order equal to the declaration order, so the
	// positional assertions below still mean what they say.
	idFor := func(n int) string { return numericID(t, n) }
	idProvenance := idFor(1)
	idNullResult := idFor(2)
	idDeleted := idFor(3)
	idOtherProvider := idFor(4)
	idOtherProject := idFor(5)

	// platformCampaignID is a *string so the NULL case is representable and distinct from "".
	seed := []struct {
		name       string
		briefID    string
		project    string
		platform   model.Provider
		variant    string
		status     string
		platformID *string
		result     []byte
	}{
		{"live google row with provenance", briefUnderTest, projectID, model.ProviderGoogleAds, model.VariantDefault, "created", ptr(idProvenance), []byte(`{"customer_id":"9876543210"}`)},
		{"live google row with NULL result", briefUnderTest, projectID, model.ProviderGoogleAds, "demand-gen", "created", ptr(idNullResult), nil},
		{"deleted google row", briefUnderTest, projectID, model.ProviderGoogleAds, "deleted-variant", "deleted", ptr(idDeleted), nil},
		{"empty platform id", briefUnderTest, projectID, model.ProviderGoogleAds, "empty-variant", "created", ptr(""), nil},
		{"null platform id", briefUnderTest, projectID, model.ProviderGoogleAds, "null-variant", "created", nil, nil},
		{"same project, other provider", briefUnderTest, projectID, model.ProviderLinkedInAds, model.VariantDefault, "created", ptr(idOtherProvider), nil},
		{"other project, same provider", briefOther, otherProject, model.ProviderGoogleAds, model.VariantDefault, "created", ptr(idOtherProject), []byte(`{"customer_id":"9876543210"}`)},
	}
	for _, s := range seed {
		if _, err := pool.Exec(ctx, `
			INSERT INTO campaigns (project_id, brief_id, platform, variant, campaign_name, status, platform_campaign_id, result)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			s.project, s.briefID, string(s.platform), s.variant, "n/a", s.status, s.platformID, s.result); err != nil {
			t.Fatalf("seed %q: %v", s.name, err)
		}
	}

	got, err := repo.ListProjectPlatformCampaignIDs(ctx, projectID, model.ProviderGoogleAds)
	if err != nil {
		t.Fatalf("ListProjectPlatformCampaignIDs: %v", err)
	}

	// Assert the exact ID SET, not a count: a count of two is also what you get from the
	// wrong two rows, which is precisely the cross-tenant failure this boundary prevents.
	wantIDs := []string{idProvenance, idNullResult}
	if len(got) != len(wantIDs) {
		t.Fatalf("got %d scope rows %v, want exactly %v — every other seeded row is excluded by "+
			"one clause of the tenant boundary", len(got), idsOf(got), wantIDs)
	}
	for i, want := range wantIDs {
		if got[i].PlatformCampaignID != want {
			t.Errorf("scope row %d = %q, want %q (ORDER BY platform_campaign_id ASC); got set %v",
				i, got[i].PlatformCampaignID, want, idsOf(got))
		}
	}
	for _, g := range got {
		if g.PlatformCampaignID == idOtherProject {
			t.Error("another project's campaign id entered this project's GAQL scope — the " +
				"project_id predicate is the only thing preventing a cross-tenant keyword read")
		}
		if g.PlatformCampaignID == idOtherProvider {
			t.Error("a LinkedIn campaign id entered a google-ads scope; the platform predicate did not bind")
		}
		if g.PlatformCampaignID == idDeleted {
			t.Error("a soft-deleted campaign entered the scope")
		}
		if g.PlatformCampaignID == "" {
			t.Error("an empty platform_campaign_id entered the scope; in an IN list it matches " +
				"nothing or widens the predicate depending on rendering")
		}
	}

	// Provenance must survive the scan on BOTH the JSON and the NULL row: the caller matches
	// it against the connection's current customer, and a nullable column that failed to scan
	// (or silently became non-empty) would either error the read or, worse, make an unknown
	// provenance look like a recorded one.
	byID := map[string]model.ProjectCampaignScope{}
	for _, g := range got {
		byID[g.PlatformCampaignID] = g
	}
	withProv, ok := byID[idProvenance]
	if !ok {
		t.Fatalf("the provenance-bearing row is missing from %v", idsOf(got))
	}
	var blob struct {
		CustomerID string `json:"customer_id"`
	}
	if err := json.Unmarshal(withProv.Result, &blob); err != nil {
		t.Fatalf("recorded provenance did not scan back as JSON: %v (raw %q)", err, string(withProv.Result))
	}
	if blob.CustomerID != "9876543210" {
		t.Errorf("provenance customer_id = %q, want %q — the caller compares this against the "+
			"connection's current customer to reject a re-pointed connection", blob.CustomerID, "9876543210")
	}
	nullProv, ok := byID[idNullResult]
	if !ok {
		t.Fatalf("the NULL-result row is missing from %v", idsOf(got))
	}
	if len(nullProv.Result) != 0 && string(nullProv.Result) != "null" {
		t.Errorf("a NULL result column scanned as %q; EMPTY means \"unknown provenance\", and a "+
			"non-empty value here would let an unstamped row masquerade as a stamped one",
			string(nullProv.Result))
	}
}

func ptr(s string) *string { return &s }

// numericID mints a digits-only platform campaign id that is unique to this test run.
//
// It cannot be dbtest.UniqueID directly: that emits [a-z0-9-], and platform_campaign_id feeds a
// scope predicate that requires the canonical base-10 spelling of a positive int64. So the
// run-unique suffix is derived by hashing UniqueID's output down to digits, and `slot` is
// prefixed so the ids sort in declaration order — the positional assertions depend on
// ORDER BY platform_campaign_id ASC matching the order the wanted ids are listed in.
func numericID(t *testing.T, slot int) string {
	t.Helper()
	sum := sha256.Sum256([]byte(dbtest.UniqueID(t, "pcid")))
	// 12 digits of run entropy, well inside int64 once the single-digit slot prefix is added.
	run := binary.BigEndian.Uint64(sum[:8]) % 1_000_000_000_000
	return fmt.Sprintf("%d%012d", slot, run)
}

func idsOf(rows []model.ProjectCampaignScope) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.PlatformCampaignID)
	}
	return out
}
