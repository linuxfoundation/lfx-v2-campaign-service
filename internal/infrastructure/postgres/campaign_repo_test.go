// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/migrations"
)

// livePredicate is the partial-index predicate that migration 000014 attached to
// uq_campaigns_brief_platform_live. Every ON CONFLICT targeting (brief_id, platform)
// must repeat it verbatim, and every campaigns read must filter by it.
const livePredicate = `status <> 'deleted'`

// onConflictBriefPlatform finds an ON CONFLICT clause whose target is the
// (brief_id, platform) pair, capturing whatever follows it up to the action
// keyword (DO). The capture is what the assertions inspect for the predicate.
var onConflictBriefPlatform = regexp.MustCompile(`(?is)ON\s+CONFLICT\s*\(\s*brief_id\s*,\s*platform\s*\)(.*?)\bDO\b`)

// TestCampaignRepo_OnConflictCarriesLivePredicate pins the single most dangerous
// coupling introduced by the soft-delete migration.
//
// Migration 000015 DROPS the full `UNIQUE (brief_id, platform)` constraint, leaving
// only 000014's PARTIAL unique index (`WHERE status <> 'deleted'`). PostgreSQL infers
// an arbiter index for ON CONFLICT by matching the conflict target AND its predicate:
// once the constraint is gone, a bare `ON CONFLICT (brief_id, platform)` matches
// NOTHING and the statement fails at runtime with
//
//	ERROR: there is no unique or exclusion constraint matching the ON CONFLICT specification
//
// Nothing catches that at compile time, and the two statements it would break are the
// two that matter most: ClaimCampaignDispatch (every dispatch claim) and
// UpsertCampaign (every campaign persist). Verified against PostgreSQL 16: with only
// the partial index present, the predicated form inserts/no-ops correctly and the bare
// form raises the error above.
func TestCampaignRepo_OnConflictCarriesLivePredicate(t *testing.T) {
	for name, q := range map[string]string{
		"ClaimCampaignDispatch": claimCampaignDispatchQuery,
		"UpsertCampaign":        upsertCampaignQuery,
	} {
		t.Run(name, func(t *testing.T) {
			m := onConflictBriefPlatform.FindStringSubmatch(q)
			require.NotNil(t, m, "query has no ON CONFLICT (brief_id, platform) clause; if the conflict target moved, update this test deliberately:\n%s", q)
			require.Contains(t, normalizeWS(m[1]), livePredicate,
				"ON CONFLICT (brief_id, platform) is missing the partial index predicate %q. "+
					"Migration 000015 drops the full UNIQUE constraint, so a bare conflict target infers no arbiter "+
					"index and this statement fails at runtime with \"no unique or exclusion constraint matching the "+
					"ON CONFLICT specification\".", livePredicate)
		})
	}
}

// TestCampaignRepo_ReadsExcludeSoftDeleted pins that every campaigns read filters out
// soft-deleted rows. A soft delete only flips a status column, so a read that forgets
// the filter keeps serving the deleted campaign — a 200 where the contract promises
// 404 — and, in GetCampaignByPlatform's case, does real damage: the orchestrator uses
// it to decide whether a (brief, platform) pair was already dispatched, so a leaked
// deleted row would report the pair as taken and block the very re-dispatch that
// deleting the campaign exists to enable.
func TestCampaignRepo_ReadsExcludeSoftDeleted(t *testing.T) {
	for name, q := range map[string]string{
		"GetCampaign":           getCampaignQuery,
		"GetCampaignByPlatform": getCampaignByPlatformQuery,
		"ReplaceCampaign":       replaceCampaignQuery,
	} {
		t.Run(name, func(t *testing.T) {
			require.Contains(t, normalizeWS(q), livePredicate,
				"query does not exclude soft-deleted campaigns (missing %q); a deleted campaign would remain "+
					"readable/updatable and, for the by-platform lookup, would keep its (brief, platform) slot "+
					"looking occupied.", livePredicate)
		})
	}
}

// TestDeleteCampaign_IsSoftDelete pins that the delete is a status UPDATE and never a
// row DELETE. The retained row holds platform_campaign_id — the only local pointer to
// a campaign that may still exist and still be spending on the ad platform, since this
// service never deletes upstream. A DELETE FROM here would free the slot but destroy
// the sole record needed to find and stop that campaign.
func TestDeleteCampaign_IsSoftDelete(t *testing.T) {
	q := normalizeWS(deleteCampaignQuery)
	require.NotContains(t, strings.ToUpper(q), "DELETE FROM CAMPAIGNS",
		"DeleteCampaign must SOFT-delete: the row carries platform_campaign_id, the only local pointer to a "+
			"campaign that may still be spending upstream (this service never deletes upstream).")
	require.Contains(t, q, "status='deleted'", "DeleteCampaign must set status='deleted'")
	require.Contains(t, q, "version=version+1", "a soft delete is a mutation and must bump the optimistic-concurrency version")
}

// TestDeleteCampaign_LocksRowBeforeGuards pins the FOR UPDATE row lock.
//
// A 'pending' campaign is an ACTIVE dispatch claim. Deleting one would free the
// (brief, platform) slot while an in-flight dispatch still owns it, letting a
// concurrent claim win the same pair and DOUBLE-CREATE a paid campaign upstream.
// Under READ COMMITTED a plain guarded UPDATE does not prevent this: each statement
// takes a fresh snapshot, so a claim that commits just before the statement runs is
// invisible to the predicate — and because the claim INSERTs a new row rather than
// updating this one, there is no row-level conflict for Postgres to serialize on. Only
// an explicit FOR UPDATE lock, taken before the guards, closes the window.
func TestDeleteCampaign_LocksRowBeforeGuards(t *testing.T) {
	q := strings.ToUpper(normalizeWS(deleteCampaignLockQuery))
	require.Contains(t, q, "FOR UPDATE",
		"the pre-delete read must take a FOR UPDATE row lock; without it the 'pending' (mid-dispatch) guard "+
			"is a TOCTOU race under READ COMMITTED and a concurrent claim could double-create upstream.")
	require.Contains(t, q, "SELECT STATUS, VERSION",
		"the locking read must fetch status and version so both guards are evaluated against the locked row")
}

// TestMigrations_UniqueNumbering guards against two migration files sharing a version.
// golang-migrate applies one and SILENTLY SKIPS the other, so a duplicate number means
// a migration never runs and the schema drifts with no error anywhere — the exact
// failure mode that makes parallel migration work hazardous.
func TestMigrations_UniqueNumbering(t *testing.T) {
	entries, err := fs.Glob(migrations.FS, "*.up.sql")
	require.NoError(t, err)
	require.NotEmpty(t, entries)

	seen := map[string]string{}
	for _, e := range entries {
		version, _, ok := strings.Cut(e, "_")
		require.True(t, ok, "migration %q does not follow the NNNNNN_name.up.sql convention", e)
		if prev, dup := seen[version]; dup {
			t.Fatalf("migrations %q and %q share version %s; golang-migrate applies one and silently skips the other", prev, e, version)
		}
		seen[version] = e
	}
}

// normalizeWS collapses all whitespace runs to a single space so assertions are
// insensitive to the SQL literals' line breaks and indentation.
func normalizeWS(s string) string { return strings.Join(strings.Fields(s), " ") }
