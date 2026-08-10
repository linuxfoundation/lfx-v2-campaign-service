// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
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
		"AdoptCampaign":         adoptCampaignQuery,
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

// TestAdoptCampaign_RefusesToRepointALiveBinding pins the one thing that separates
// adoption from an upsert.
//
// UpsertCampaign's conflict arm UPDATES: re-running a create for a (brief, platform)
// pair that already has a row is how a retried dispatch converges, and overwriting is
// correct there because the row being overwritten describes the same campaign this
// service is provisioning. Adoption is the opposite case — the caller names an
// ARBITRARY upstream campaign — so the same conflict arm would repoint an existing
// binding at a different campaign, and the one it used to name keeps spending with
// nothing in this service pointing at it. DO NOTHING turns that into zero rows, which
// the Go code classifies as domain.ErrConflict and the handler as a 409.
func TestAdoptCampaign_RefusesToRepointALiveBinding(t *testing.T) {
	q := normalizeWS(adoptCampaignQuery)
	upper := strings.ToUpper(q)

	require.Contains(t, upper, "DO NOTHING",
		"AdoptCampaign must DO NOTHING on conflict. DO UPDATE would repoint a live binding at a "+
			"different upstream campaign and orphan the one it used to name, which this service cannot "+
			"stop because it never deletes upstream.")
	require.NotContains(t, upper, "DO UPDATE",
		"AdoptCampaign must never take an updating conflict arm")
	require.Contains(t, upper, "RETURNING",
		"the insert must RETURN the stored row: the handler's conflict classification is 'no rows "+
			"came back', so without RETURNING a refused adoption is indistinguishable from a successful one")
	require.Contains(t, upper, "INSERT INTO CAMPAIGNS",
		"adoption creates a NEW binding row; it must not mutate an existing one")
}

// TestUpsertCampaignDoesNotRewriteCreatedBy pins the one property of the actor columns that
// SQL alone decides and no service-layer test can reach.
//
// A campaign row is INSERTed by ClaimCampaignDispatch, and upsertCampaignQuery's conflict arm
// FINALIZES that same claim: dispatchPlatform reaches an upsert only when the claim was won,
// and every !claimed branch returns first (reuse, reconcile, skip). So no orchestrator path
// takes this arm against a row some EARLIER dispatch created — a retry re-claims and re-stamps,
// and a re-dispatch after a soft delete inserts a fresh row outside the partial unique index.
//
// The assertion is therefore about the repository's contract rather than about today's caller.
// UpsertCampaign is a general-purpose method, and a future caller reaching this arm without a
// claim in front of it would, if created_by were in the SET list, rewrite the original author
// with whoever triggered the most recent write. That is precisely what updated_by is for, and
// precisely what created_by is not: under shared system accounts the ad platform cannot supply
// the original author, so once overwritten it is gone. Pinning it now costs one assertion;
// discovering it later costs the column.
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
		"created_by is assigned in the conflict arm: any caller reaching this arm without a "+
			"claim in front of it would overwrite the original author with whoever triggered "+
			"the latest write, and under system accounts nothing else records it")

	assert.Contains(t, normalizeWS(updateArm), "updated_by=COALESCE(EXCLUDED.updated_by, campaigns.updated_by)",
		"updated_by must move on the conflict arm, and must COALESCE: an unattributed "+
			"re-dispatch passes NULL, and writing that over a real actor turns "+
			"\"we know who\" into \"we do not\"")
}

// TestClaimCampaignDispatchStampsBothActorColumns pins that the row's first INSERT sets
// updated_by alongside created_by, matching createBriefQuery's `$11,$11`.
//
// The two columns are written from the same placeholder, which is the point: at creation the
// author IS the last mover. Setting only created_by would leave a freshly claimed campaign
// with updated_by NULL, and NULL on that column already means "we never recorded who" — so a
// reader could not tell an untouched-since-creation campaign from one whose attribution was
// lost. The upsert that follows a successful claim takes the conflict arm and cannot repair
// it, because that arm only moves updated_by when it has a non-NULL actor to move.
func TestClaimCampaignDispatchStampsBothActorColumns(t *testing.T) {
	q := normalizeWS(claimCampaignDispatchQuery)
	require.Contains(t, q, "created_by, updated_by)",
		"the claim INSERT must name both actor columns:\n%s", claimCampaignDispatchQuery)
	assert.Contains(t, q, "$5, $5)",
		"both actor columns must be written from the SAME placeholder; two placeholders would "+
			"let a caller set them independently at creation time, when there is only one actor")
}

// TestDeleteCampaignStampsTheDeletingActor pins the actor on the soft delete.
//
// The row is KEPT on delete precisely because it may still point at a campaign spending real
// money upstream, so "who retired this" is a question actually asked of it later. Leave
// updated_by alone and the answer is whoever last EDITED the campaign — worse than NULL,
// because it reads as knowledge and names the wrong person. COALESCE for the usual reason:
// an unauthenticated delete records nothing rather than erasing the actor we do know.
func TestDeleteCampaignStampsTheDeletingActor(t *testing.T) {
	q := normalizeWS(deleteCampaignQuery)
	assert.Contains(t, q, "updated_by=COALESCE($2, updated_by)",
		"the soft delete must stamp updated_by, and must COALESCE so an unattributed delete "+
			"does not clear a known actor:\n%s", deleteCampaignQuery)
}

// campaignColumnOrder mirrors campaignCols, in order. Same contract as briefColumnOrder:
// changing one of campaignCols / scanCampaign's destination list without the other is a
// silent data-corruption bug, and this constant makes the third edit deliberate.
var campaignColumnOrder = []string{
	"id", "project_id", "brief_id", "job_id", "platform", "platform_campaign_id",
	"campaign_name", "status", "budget_amount", "budget_type", "start_date", "end_date",
	"config_snapshot", "result", "version", "created_by", "updated_by", "created_at", "updated_at",
}

// fakeCampaignRow is a pgx.Row handing scanCampaign a fixed, positionally ordered result set.
// Separate from brief_repo_test.go's fakeRow because the diagnostics name the campaign column
// at each index.
type fakeCampaignRow struct{ vals []any }

func (r fakeCampaignRow) Scan(dest ...any) error {
	if len(dest) != len(r.vals) {
		return fmt.Errorf("scanCampaign requested %d destinations, row has %d columns: the "+
			"destination list and campaignCols have drifted apart", len(dest), len(r.vals))
	}
	for i, d := range dest {
		if r.vals[i] == nil {
			continue // leave the destination at its zero value, as a SQL NULL would
		}
		dv := reflect.ValueOf(d).Elem()
		sv := reflect.ValueOf(r.vals[i])
		if !sv.Type().AssignableTo(dv.Type()) {
			return fmt.Errorf("column %d (%s): cannot scan %s into %s — the destination at this "+
				"position does not match the column campaignCols selects there",
				i, campaignColumnOrder[i], sv.Type(), dv.Type())
		}
		dv.Set(sv)
	}
	return nil
}

// TestCampaignCols_MatchesTheDeclaredOrder proves the SELECT list is what campaignColumnOrder
// says. On its own it says nothing about scanCampaign — that is what the next test is for.
func TestCampaignCols_MatchesTheDeclaredOrder(t *testing.T) {
	var got []string
	for _, c := range strings.Split(normalizeWS(campaignCols), ",") {
		c = strings.TrimSpace(c)
		if i := strings.Index(c, "::"); i != -1 { // id::text and friends
			c = c[:i]
		}
		got = append(got, c)
	}
	require.Equal(t, campaignColumnOrder, got,
		"campaignCols and campaignColumnOrder disagree; if a column moved, move it in "+
			"scanCampaign's destination list too")
}

// TestScanCampaign_MapsEachColumnToItsField drives the real scanCampaign and asserts every
// column lands on the right field.
//
// The two actor columns are both JSONB and the two timestamps are both time.Time, so a
// destination-order swap inside scanCampaign cannot fail at the type level: created_by and
// updated_by would simply trade places and every existing test would stay green while the
// audit trail named the wrong person on every campaign in the database. Distinct values per
// column are what make the swap visible.
func TestScanCampaign_MapsEachColumnToItsField(t *testing.T) {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	updated := time.Date(2026, 6, 7, 8, 9, 10, 0, time.UTC)
	start := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	jobID, pcID, budgetType := "j1", "gads-123", "daily"
	amount := 250.5

	c, err := scanCampaign(fakeCampaignRow{vals: []any{
		"c1", "cncf", "b1", &jobID, "google-ads", &pcID, "Spring launch", "created",
		&amount, &budgetType, &start, &end,
		json.RawMessage(`{"cfg":1}`), json.RawMessage(`{"res":2}`), int64(9),
		[]byte(`{"email":"ada@lf.dev"}`), []byte(`{"email":"grace@lf.dev"}`), created, updated,
	}})
	require.NoError(t, err)

	assert.Equal(t, "c1", c.ID)
	assert.Equal(t, "cncf", c.ProjectID)
	assert.Equal(t, "b1", c.BriefID)
	require.NotNil(t, c.JobID)
	assert.Equal(t, "j1", *c.JobID)
	assert.Equal(t, model.ProviderGoogleAds, c.Platform)
	assert.Equal(t, "gads-123", c.PlatformCampaignID)
	assert.Equal(t, "Spring launch", c.CampaignName)
	assert.Equal(t, "created", c.Status)
	require.NotNil(t, c.BudgetAmount)
	assert.InDelta(t, 250.5, *c.BudgetAmount, 0.0001)
	require.NotNil(t, c.BudgetType)
	assert.Equal(t, model.BudgetType("daily"), *c.BudgetType)
	assert.Equal(t, &start, c.StartDate)
	assert.Equal(t, &end, c.EndDate)
	assert.JSONEq(t, `{"cfg":1}`, string(c.ConfigSnapshot))
	assert.JSONEq(t, `{"res":2}`, string(c.Result))
	assert.Equal(t, int64(9), c.Version)
	assert.Equal(t, created, c.CreatedAt)
	assert.Equal(t, updated, c.UpdatedAt)

	// The pair the type system cannot separate. Distinct emails are the only thing standing
	// between a swapped destination list and a silently inverted audit trail.
	require.NotNil(t, c.CreatedBy)
	assert.Equal(t, "ada@lf.dev", c.CreatedBy.Email,
		"created_by must come from the 16th column; a swap with updated_by would attribute "+
			"every campaign to whoever last touched it")
	require.NotNil(t, c.UpdatedBy)
	assert.Equal(t, "grace@lf.dev", c.UpdatedBy.Email)
}

// TestScanCampaign_NullActorsDecodeToNil pins that a NULL actor column is an ordinary value,
// not an error. Both columns are nullable by migration 000016 and every row written before it
// has both NULL, so a scan that failed here would make every pre-migration campaign unreadable.
func TestScanCampaign_NullActorsDecodeToNil(t *testing.T) {
	c, err := scanCampaign(fakeCampaignRow{vals: []any{
		"c1", "cncf", "b1", nil, "google-ads", nil, "n", "created",
		nil, nil, nil, nil, nil, nil, int64(1),
		nil, nil, time.Time{}, time.Time{},
	}})
	require.NoError(t, err)
	assert.Nil(t, c.CreatedBy, "a NULL created_by means \"not recorded\", which is nil, not an error")
	assert.Nil(t, c.UpdatedBy)
	// The other nullable columns travel the same path and must not be invented either.
	assert.Nil(t, c.JobID)
	assert.Empty(t, c.PlatformCampaignID)
	assert.Nil(t, c.BudgetType)
}

// TestScanCampaign_MalformedActorJSONIsAnError pins that undecodable actor JSON FAILS the scan
// rather than yielding a zero actor.
//
// The column is JSONB, so Postgres will not store a non-JSON value — but it will happily store
// a JSON value of the wrong SHAPE (an array, a bare string) if some future writer marshals the
// wrong thing. Swallowing that would hand callers a campaign whose CreatedBy is silently nil,
// i.e. indistinguishable from "not recorded", which is exactly the confusion the NULL semantics
// exist to avoid. The error names the column so the bad writer is findable.
func TestScanCampaign_MalformedActorJSONIsAnError(t *testing.T) {
	base := func(createdBy, updatedBy []byte) []any {
		return []any{
			"c1", "cncf", "b1", nil, "google-ads", nil, "n", "created",
			nil, nil, nil, nil, nil, nil, int64(1),
			createdBy, updatedBy, time.Time{}, time.Time{},
		}
	}
	_, err := scanCampaign(fakeCampaignRow{vals: base([]byte(`["not","an","actor"]`), nil)})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "created_by",
		"the error must name the column so the writer that stored the wrong shape is findable")

	_, err = scanCampaign(fakeCampaignRow{vals: base(nil, []byte(`"grace"`))})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "updated_by")
}

// TestMigration000016_AddsCampaignActorColumns is the campaigns-side counterpart of
// TestMigration000015_AddsBriefActorColumns, and it exists for the same reason: every
// statement above that names created_by or updated_by on campaigns compiles against
// columns this migration is the ONLY thing that creates. A migration edited to add just
// one of the pair leaves the scan and the upsert failing at runtime on the other, with
// nothing in this package objecting first.
//
// It also pins the table. 000015 and 000016 are near-identical files touching different
// tables; a copy-paste that leaves ALTER TABLE campaign_briefs here would apply cleanly,
// be a no-op (000015 already added those columns IF NOT EXISTS), and leave campaigns
// without the columns the repository writes to.
func TestMigration000016_AddsCampaignActorColumns(t *testing.T) {
	up, err := fs.ReadFile(migrations.FS, "000016_campaign_actor_columns.up.sql")
	require.NoError(t, err)
	upSQL := normalizeWS(string(up))

	require.Contains(t, upSQL, "ALTER TABLE campaigns",
		"migration 000016 must alter campaigns; campaign_briefs got its columns in 000015")
	require.NotContains(t, upSQL, "ALTER TABLE campaign_briefs",
		"000016 alters campaign_briefs, which is 000015's table — a copy-paste that keeps the "+
			"sibling's target applies cleanly as a no-op and leaves campaigns without the columns")
	for _, col := range []string{"created_by", "updated_by"} {
		require.Regexp(t, regexp.MustCompile(`(?i)ADD COLUMN IF NOT EXISTS `+col+` JSONB`), upSQL,
			"000016 does not add %s as JSONB. marshalActor writes a JSONB document, matching "+
				"connections/campaign_audiences/campaign_briefs; a text column would round-trip "+
				"but lose the ability to query into the actor.", col)
	}
	// NOT NULL would be unrunnable, not merely strict: every campaign row that predates
	// this migration has no actor to backfill, so the ALTER would fail outright on any
	// deployed database. Nullability is load-bearing, and nothing else asserts it.
	require.NotRegexp(t, regexp.MustCompile(`(?i)(created_by|updated_by) JSONB[^,]*NOT NULL`), upSQL,
		"000016 declares an actor column NOT NULL; pre-existing campaign rows have no actor "+
			"to backfill, so the migration cannot run on a deployed database")

	down, err := fs.ReadFile(migrations.FS, "000016_campaign_actor_columns.down.sql")
	require.NoError(t, err)
	downSQL := normalizeWS(string(down))
	require.Contains(t, downSQL, "ALTER TABLE campaigns",
		"the down migration must drop from campaigns, the table the up migration altered")
	for _, col := range []string{"created_by", "updated_by"} {
		require.Regexp(t, regexp.MustCompile(`(?i)DROP COLUMN IF EXISTS `+col), downSQL,
			"down migration leaves %s behind, so a down-then-up cycle hits an already-present "+
				"column and the pair drift apart", col)
	}
}
