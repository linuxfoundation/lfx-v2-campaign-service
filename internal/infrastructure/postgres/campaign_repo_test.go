// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/migrations"
)

// livePredicate is the partial-index predicate that migration 000013 attached to
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
// Migration 000014 DROPS the full `UNIQUE (brief_id, platform)` constraint, leaving
// only 000013's PARTIAL unique index (`WHERE status <> 'deleted'`). PostgreSQL infers
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
					"Migration 000014 drops the full UNIQUE constraint, so a bare conflict target infers no arbiter "+
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
// This asserts on the locking read's SQL only. The guard DECISION it protects — which
// statuses may be deleted — is pure logic and is tested exhaustively as
// model.CampaignStatusNeedsReconciliation (see TestCampaignStatusNeedsReconciliation);
// that split is deliberate, because this repo has no DB-backed test harness and the
// predicate is the part that actually had the bug.
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

// TestMigration000014_GuardChecksIndexDefinition pins that the drop-guard verifies the
// replacement index's DEFINITION and not merely its name.
//
// The hole this closes: 000013 builds uq_campaigns_brief_platform_live with
// CREATE UNIQUE INDEX CONCURRENTLY *IF NOT EXISTS*. Any pre-existing index that happens to
// carry that name therefore makes 000013 a silent no-op — and a guard that checks only
// name/namespace/indisvalid accepts it, after which this migration drops the sole full
// UNIQUE (brief_id, platform) constraint. The table is then left with NO enforceable
// uniqueness on the pair: every ClaimCampaignDispatch wins, and concurrent retries
// double-create paid campaigns upstream, silently, because nothing errors.
//
// Verified against PostgreSQL 16.10: with a NON-unique index of the right name in place,
// the old name-only guard returned true (would have dropped the constraint) while the
// current guard returns false and the migration RAISEs with the constraint left intact.
// The same run confirmed the guard rejects a superset key list (brief_id, platform, id), a
// reversed column order, a non-partial index, a wrong predicate, an index on a different
// table, and an INVALID index — while still accepting an equivalent predicate spelled
// `!=` or with an explicit ::text cast, since the comparison is against the text Postgres
// itself deparses.
func TestMigration000014_GuardChecksIndexDefinition(t *testing.T) {
	b, err := fs.ReadFile(migrations.FS, "000014_drop_campaigns_full_unique_platform.up.sql")
	require.NoError(t, err)
	sql := normalizeWS(string(b))

	// Each check is load-bearing; a guard missing any one of them accepts an index that
	// does not enforce what the dropped constraint enforced.
	for _, want := range []struct{ frag, why string }{
		{"i.indisunique", "a NON-unique index of the right name would pass and arbitrates no dispatch claim"},
		{"i.indnkeyatts = 2", "without pinning the key count, a superset like (brief_id, platform, id) passes and permits two live rows for the pair"},
		{"i.indrelid = 'public.campaigns'::regclass", "without pinning the relation, an index of the same name on ANOTHER table satisfies the guard"},
		{"i.indisvalid", "a failed CONCURRENTLY build leaves an INVALID index that Postgres refuses to use"},
		{"'brief_id'", "the first key column must be proven to be brief_id"},
		{"'platform'", "the second key column must be proven to be platform"},
		{"i.indpred IS NOT NULL", "a non-partial index covers deleted rows too and does not free the slot"},
		{`= '(status <> ''deleted''::text)'`, "the predicate must match what Postgres deparses for WHERE status <> 'deleted'; a different predicate covers a different row set"},
	} {
		require.Contains(t, sql, want.frag,
			"migration 000014's drop-guard is missing %q: %s", want.frag, want.why)
	}

	// The guard must still gate the DROP. A guard that RAISEs correctly but whose
	// exception is unreachable, or a DROP that runs outside it, protects nothing.
	require.Contains(t, strings.ToUpper(sql), "RAISE EXCEPTION",
		"the guard must RAISE (failing the migration and the pod's startup) rather than skip the drop silently")
	require.Contains(t, strings.ToUpper(sql), "DROP CONSTRAINT CAMPAIGNS_BRIEF_ID_PLATFORM_KEY",
		"the migration must still drop the old constraint once the guard is satisfied")
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

// TestMigrations_NoVersionGaps guards against numbering a migration ABOVE versions that do
// not exist yet in this tree — the sibling of the duplicate-version hazard above, and the
// one that actually bit this branch (it was numbered 000014/000015 while main was at 000007
// and 000008-000010 lived in other open PRs).
//
// Why a gap is dangerous rather than merely untidy: golang-migrate records the HIGHEST
// version it has applied and thereafter only applies versions above it. So if a tree
// carrying a gap deploys first, the migrations that later fill that gap are skipped
// SILENTLY and permanently — Up() reports success, and the schema is simply missing them.
//
// A contiguous sequence makes that impossible to express. It also fails loudly HERE, at
// commit time on the branch that introduced the gap, instead of after a deploy.
//
// Note the limit of this test and the one above: both see only this branch's embedded FS.
// Neither can detect that a SIBLING branch has claimed the same number, which is why
// choosing a version requires checking every open PR branch (see the concept doc).
//
// allowedVersionGaps records gaps that are KNOWN and transitional: versions claimed by a
// sibling PR that has not merged yet. A gap listed here is a merge-ORDERING obligation, not
// a numbering bug — this branch must not merge before the PR that fills it, or those
// migrations are skipped forever. Deleting an entry once its PR merges is the point: the
// list must shrink to empty, and this test then enforces strict contiguity again.
//
// 000008-000012: 000008/000009 belong to feat/LFXV2-2665-reclaim-expired-dispatch-claims
// (PR #59) and 000010 to feat/LFXV2-2814-query-service-indexing (PR #60); 000011/000012 are
// intentional headroom so a sibling renumbering its own stray version down cannot collide
// with this branch's 000013/000014.
var allowedVersionGaps = map[int]string{
	8: "PR #59 (000008/000009) and PR #60 (000010) must merge before this branch; 000011/000012 are reserved headroom",
}

func TestMigrations_NoVersionGaps(t *testing.T) {
	entries, err := fs.Glob(migrations.FS, "*.up.sql")
	require.NoError(t, err)
	require.NotEmpty(t, entries)

	versions := make([]int, 0, len(entries))
	byVersion := map[int]string{}
	for _, e := range entries {
		v, _, ok := strings.Cut(e, "_")
		require.True(t, ok, "migration %q does not follow the NNNNNN_name.up.sql convention", e)
		n, convErr := strconv.Atoi(v)
		require.NoError(t, convErr, "migration %q has a non-numeric version %q", e, v)
		versions = append(versions, n)
		byVersion[n] = e
	}
	sort.Ints(versions)

	require.Equal(t, 1, versions[0], "migrations must start at version 1, got %d (%s)", versions[0], byVersion[versions[0]])
	for i := 1; i < len(versions); i++ {
		prev, cur := versions[i-1], versions[i]
		if cur == prev+1 {
			continue
		}
		why, allowed := allowedVersionGaps[prev+1]
		require.True(t, allowed,
			"migration versions must be contiguous: %s jumps from %06d to %06d. golang-migrate records the "+
				"highest applied version and never applies a lower one afterwards, so if this tree deploys first, "+
				"any migration later filling versions %06d-%06d is skipped silently and permanently. Renumber to "+
				"the next consecutive version above every version claimed in main AND in every open PR branch, or "+
				"record the gap in allowedVersionGaps with the sibling PR that fills it.",
			byVersion[cur], prev, cur, prev+1, cur-1)
		t.Logf("tolerating known transitional gap %06d-%06d before %s: %s", prev+1, cur-1, byVersion[cur], why)
	}
}

// TestMigrations_AllowedVersionGapsAreStillOpen keeps allowedVersionGaps honest. An entry there
// suppresses a real contiguity failure, so a stale entry silently re-permits the exact hazard
// the test above exists to catch. Once the sibling PR merges and its migrations land in this
// tree, the version that opened the gap EXISTS — at which point the entry is dead and must be
// deleted rather than left to mask a future genuine gap at the same number.
func TestMigrations_AllowedVersionGapsAreStillOpen(t *testing.T) {
	entries, err := fs.Glob(migrations.FS, "*.up.sql")
	require.NoError(t, err)

	present := map[int]bool{}
	for _, e := range entries {
		if v, _, ok := strings.Cut(e, "_"); ok {
			if n, convErr := strconv.Atoi(v); convErr == nil {
				present[n] = true
			}
		}
	}

	for gapStart, why := range allowedVersionGaps {
		require.False(t, present[gapStart],
			"allowedVersionGaps[%d] is stale: version %06d now exists in this tree, so the gap it excused is closed "+
				"(%s). Delete the entry — leaving it would let a future genuine gap at this version pass unnoticed.",
			gapStart, gapStart, why)
	}
}

// normalizeWS collapses all whitespace runs to a single space so assertions are
// insensitive to the SQL literals' line breaks and indentation.
func normalizeWS(s string) string { return strings.Join(strings.Fields(s), " ") }
