// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"io/fs"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
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

// TestClaimVersionIsBackedByACompareAndSwap pins the durable half of the serialization
// protocol. ClaimCampaignVersion's advisory lock is SESSION-scoped: a failover, a pool
// eviction, or a dropped TCP connection releases it server-side while the holder is still
// inside its external platform call, and a successor can then claim the same version. The
// lock is a contention guard, not durable ownership.
//
// What makes a lost lock survivable is this compare-and-swap: ReplaceCampaign must both
// PREDICATE on the claimed version and BUMP it in the same statement. Whichever writer
// commits first wins; the other matches zero rows and gets ErrPreconditionFailed. Drop
// either half and two writers that both held the "same" claim can both persist — two
// campaign writes at one version, two outbox rows, and the later one silently overwriting
// the earlier. Asserted against the SQL text because this package has no live-database
// harness in CI.
func TestClaimVersionIsBackedByACompareAndSwap(t *testing.T) {
	q := replaceCampaignQuery

	assert.Contains(t, q, "version=version+1",
		"without the bump, a second claimant at the same version also passes the version predicate "+
			"and both writers persist — the advisory lock cannot prevent this, it is only session-scoped")
	// Matched by shape, not by "$12": the placeholder number shifts whenever a column is added
	// ahead of it in the SET list (adding updated_by moved it from $12 to $13), and a test that
	// pins the literal number fails on a change that preserves the property it exists to
	// protect — training the next person to "fix" it by editing the number rather than by
	// checking the predicate is still there.
	assert.Regexp(t, `AND version=\$\d+`, q,
		"without the version predicate, a writer whose lock was lost mid-call overwrites a newer "+
			"state rather than failing with ErrPreconditionFailed")
	assert.Contains(t, claimCampaignVersionQuery, "version=$4",
		"the claim must read AT the caller's expected version; reading the current version instead "+
			"would hand every racing writer a claim that then passes the swap")
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
		"GetCampaign":                         getCampaignQuery,
		"GetCampaignByPlatform":               getCampaignByPlatformQuery,
		"ReplaceCampaign":                     replaceCampaignQuery,
		"ReplaceCampaign (no-row classifier)": replaceCampaignExistsQuery,
		// The claim pair matters most of the three: it gates the run-status toggle,
		// which makes a PAID platform call between claiming and replacing. A claim that
		// admitted a soft-deleted row would mutate the campaign upstream and then fail
		// the local write, because ReplaceCampaign does filter deleted rows.
		"ClaimCampaignVersion":                     claimCampaignVersionQuery,
		"ClaimCampaignVersion (no-row classifier)": claimCampaignExistsQuery,
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
// model.CampaignStatusDeletable (see TestCampaignStatusDeletable); that split is
// deliberate, because this repo has no DB-backed test harness and the predicate is the
// part that actually had the bug.
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

// normalizeWS collapses all whitespace runs to a single space so assertions are
// insensitive to the SQL literals' line breaks and indentation.
func normalizeWS(s string) string { return strings.Join(strings.Fields(s), " ") }

// TestDeleteCampaign_ParticipatesInAdvisoryLockProtocol pins that DeleteCampaign takes
// the campaign advisory lock, and that it runs its transaction on the SAME connection
// that holds it.
//
// This repo has no DB-backed test harness (see TestDeleteCampaign_LocksRowBeforeGuards),
// so this asserts on the source of the method body rather than by racing two real
// transactions. Both properties it pins are structural, and both were real defects:
//
//  1. Without the advisory lock, FOR UPDATE serializes delete against the dispatch path
//     (which UPDATEs the row) but NOT against an in-flight run-state toggle. A toggle
//     holds its claim ACROSS the platform call, and between ClaimCampaignVersion and
//     ReplaceCampaign it holds no row lock at all. A delete committing inside that
//     window bumps version, so the toggle's ReplaceCampaign(expectedVersion) fails after
//     the paid side effect already landed upstream.
//
//  2. Beginning the transaction with r.db.Begin would take a SECOND pool connection
//     while this one is held, self-deadlocking whenever the pool is saturated
//     (pool_max_conns=1 guarantees it) — the delete would block waiting for a
//     connection only it could release.
func TestDeleteCampaign_ParticipatesInAdvisoryLockProtocol(t *testing.T) {
	src, err := os.ReadFile("campaign_repo.go")
	require.NoError(t, err)

	const start = "func (r *CampaignRepo) DeleteCampaign("
	i := strings.Index(string(src), start)
	require.NotEqual(t, -1, i, "DeleteCampaign not found; update this test if the method was renamed")
	body := string(src)[i:]
	// Bound the body at the next top-level func so later methods cannot satisfy these
	// assertions on DeleteCampaign's behalf.
	if j := strings.Index(body[len(start):], "\nfunc "); j != -1 {
		body = body[:len(start)+j]
	}

	require.Contains(t, body, "pg_try_advisory_lock",
		"DeleteCampaign must take the campaign advisory lock: FOR UPDATE alone does not serialize it "+
			"against an in-flight toggle, which holds its claim across the platform call and no row lock at all.")
	// The TRY form, never the blocking one. This connection is already checked out of the
	// pool, so waiting here would pin a SECOND pooled connection for the length of the
	// winner's platform call (up to 45s) — a small burst against one campaign could then
	// exhaust the pool and stall unrelated requests and the readiness probe.
	require.NotContains(t, body, `"SELECT pg_advisory_lock(`,
		"DeleteCampaign must not BLOCK on the advisory lock; use pg_try_advisory_lock and return "+
			"ErrCampaignWriteInProgress so a loser never holds a pooled connection while it waits")
	require.Contains(t, body, "domain.ErrCampaignWriteInProgress",
		"a lost claim must surface as 409 retry-shortly, not as a 412 that would send a caller with "+
			"a perfectly current ETag off to refetch")
	require.Contains(t, body, "pg_advisory_unlock",
		"DeleteCampaign must release the advisory lock; a session lock left held strands every future "+
			"claim and delete for this campaign.")
	require.Contains(t, body, "conn.Begin(ctx)",
		"the delete transaction must begin on the connection already holding the advisory lock")
	require.NotContains(t, body, "r.db.Begin(",
		"beginning on the pool takes a SECOND connection while holding the lock, which self-deadlocks "+
			"on a saturated pool")
}

// TestUpsertCampaignDoesNotRewriteCreatedBy pins the one property of the actor columns that
// SQL alone decides and no service-layer test can reach.
//
// A campaign row is INSERTed exactly once — by ClaimCampaignDispatch. Every write after that,
// for the life of the (brief, platform) pair, takes upsertCampaignQuery's conflict arm: a
// re-dispatch after a brief edit, a retry, a reconcile. If created_by appeared in that arm's
// SET list it would be rewritten on every one of them, and the column would end up recording
// the most recent re-dispatcher rather than whoever launched the campaign. That is precisely
// what updated_by is for, and precisely what created_by is not: under shared system accounts
// the ad platform cannot supply the original author, so once overwritten it is gone.
//
// The mistake is a one-word edit (copying the neighbouring EXCLUDED lines) and produces no
// build error, no test failure elsewhere, and no wrong-looking data until someone asks who
// authorized a campaign months later. This package has no live-database harness in CI, so the
// assertion is against the SQL text.
func TestUpsertCampaignDoesNotRewriteCreatedBy(t *testing.T) {
	_, updateArm, found := strings.Cut(upsertCampaignQuery, "DO UPDATE SET")
	require.True(t, found, "upsertCampaignQuery has no DO UPDATE SET arm; if the statement was "+
		"restructured, update this test deliberately:\n%s", upsertCampaignQuery)

	assert.NotContains(t, updateArm, "created_by=",
		"created_by is assigned in the conflict arm: every re-dispatch would overwrite the "+
			"original author with whoever triggered the latest run, and under system accounts "+
			"nothing else records it")

	assert.Contains(t, normalizeWS(updateArm), "updated_by=COALESCE(EXCLUDED.updated_by, campaigns.updated_by)",
		"updated_by must move on the conflict arm, and must COALESCE: a system-initiated "+
			"re-persist (recovery sweeper) passes NULL, and writing that over a real actor turns "+
			"\"we know who\" into \"we do not\"")
}
