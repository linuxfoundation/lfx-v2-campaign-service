// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// audienceBuildLeaseIndex is the partial unique index migration 000018 creates: at most one
// audience per (brief, platform) in `building`. Named here because three statements translate
// a violation of THIS index — and only this one — into domain.ErrAudienceBuildInFlight, which
// tells the caller a build is already running. Matching on SQLSTATE 23505 alone would hand the
// next unique index added to campaign_audiences that same meaning, silently — the witness is
// TestAudienceLeaseMappingIgnoresOtherUniqueIndexes in the dbtest package, which builds a
// second unique index to reach a case no arrangement of rows can.
//
// pool.go's requiredIndexes shares this constant, so the value is also the name the boot guard
// looks for: change it without changing migration 000018 and the service refuses to start
// rather than mis-mapping anything. That coupling is why a test that breaks the constant tests
// the guard, not the mapping.
const audienceBuildLeaseIndex = "uq_campaign_audiences_brief_platform_building"

// AudienceRepo is a pgx-backed implementation of domain.AudienceRepository.
type AudienceRepo struct {
	db *Pool
}

// NewAudienceRepo returns an AudienceRepo backed by pool.
func NewAudienceRepo(pool *Pool) *AudienceRepo { return &AudienceRepo{db: pool} }

var _ domain.AudienceRepository = (*AudienceRepo)(nil)

// audienceCols is the column list every audience read scans, in scanAudience order.
const audienceCols = `id::text, project_id::text, brief_id::text, platform,
	platform_master_list_id, suppression_list_ids, inclusion_summary, status, version,
	created_by, created_at, updated_at`

// CreateAudience inserts a new audience row and returns it.
// CreateAudienceForApprovedBrief inserts the row only if the parent brief is APPROVED, and
// returns the brief version it observed. See the port for why it takes no expected version
// and why the plain create is not sufficient here.
//
// A guarded INSERT ... WHERE EXISTS is NOT sufficient here. Under READ COMMITTED the
// statement's snapshot can still see the approved row while a concurrent ReplaceBrief
// commits a draft/version change before this insert commits — so the build would create
// REAL HubSpot lists from a brief that is no longer approved.
func (r *AudienceRepo) CreateAudienceForApprovedBrief(ctx context.Context, a *model.CampaignAudience) (*model.CampaignAudience, int64, error) {
	// SELECT ... FOR UPDATE takes a row-level exclusive lock and re-reads the row's CURRENT
	// committed state, so this check cannot straddle a concurrent commit: whichever
	// transaction takes the lock first runs to completion before the other observes the row.
	// This guarantees that if we observe the brief as approved, it will remain approved
	// (or return ErrStaleApproval if it moved) until our INSERT completes, preventing the
	// creation of HubSpot lists from a draft or modified brief.
	// Same shape as JobRepo.CreateJobForApprovedBrief.
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("create audience for approved brief: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		status  string
		version int64
	)
	lockQ := `SELECT status, version FROM campaign_briefs WHERE id = $1 AND project_id = $2 FOR UPDATE`
	if serr := tx.QueryRow(ctx, lockQ, a.BriefID, a.ProjectID).Scan(&status, &version); serr != nil {
		if errors.Is(serr, pgx.ErrNoRows) {
			// Absent, or the caller's project does not own it. Either way there is nothing
			// approved to build from.
			return nil, 0, domain.ErrStaleApproval
		}
		return nil, 0, fmt.Errorf("create audience for approved brief: lock brief: %w", serr)
	}
	if status != "approved" {
		return nil, 0, domain.ErrStaleApproval
	}

	insertQ := `INSERT INTO campaign_audiences
		(project_id, brief_id, platform, platform_master_list_id, suppression_list_ids,
		 inclusion_summary, status, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING ` + audienceCols
	out, serr := scanAudience(tx.QueryRow(ctx, insertQ,
		a.ProjectID, a.BriefID, string(a.Platform), nullStr(a.PlatformMasterListID),
		a.SuppressionListIDs, nullStr(a.InclusionSummary), string(a.StatusOrDefault()),
		a.CreatedBy))
	if serr != nil {
		if isUniqueViolationOn(serr, audienceBuildLeaseIndex) {
			// Another build for this (brief, platform) is already in flight and holds the
			// lease. Reported as its own sentinel rather than ErrConflict: the caller has
			// not created a duplicate of anything, it has arrived second.
			return nil, 0, domain.ErrAudienceBuildInFlight
		}
		return nil, 0, fmt.Errorf("create audience for approved brief: insert: %w", serr)
	}
	if cerr := tx.Commit(ctx); cerr != nil {
		// A Commit error does not prove the server rolled back: PostgreSQL may have
		// committed the row before the connection failed to acknowledge it. If it did,
		// this INSERT's `building` row silently holds the (brief_id, platform) lease
		// (000018) forever — the caller here returns an error and `created` is nil, so
		// there is no row for it to pass to releaseUnstartedClaim. Reconcile directly:
		// best-effort move the row to `failed` under a bounded, detached context, using
		// the id/version this INSERT already observed inside the (possibly-rolled-back)
		// transaction. If the commit genuinely rolled back, no row exists at that id and
		// the UPDATE is a harmless no-op.
		reconcileAmbiguousAudienceCommit(ctx, r.db, out)
		return nil, 0, fmt.Errorf("create audience for approved brief: commit: %w", cerr)
	}
	// The version is reported from INSIDE the transaction that locked the brief, so it is the
	// version the approval was observed at rather than whatever a later read might see.
	return out, version, nil
}

// ambiguousCommitReconcileTimeout bounds reconcileAmbiguousAudienceCommit. Short and
// detached from the caller's context for the same reason releaseUnstartedClaim's timeout
// is: the caller is already on its way out with an error, so a client disconnect must not
// be the reason a possibly-committed lease stays held.
const ambiguousCommitReconcileTimeout = 5 * time.Second

// Retry schedule for the one outcome that is NOT self-evident: zero rows updated AND no row
// visible at that id. See reconcileAmbiguousAudienceCommit for why that outcome is ambiguous.
//
// The attempt cap, not the timeout, is what normally ends the loop, and it exists to bound
// what this costs on the path where nothing is wrong. A commit the server already accepted
// becomes visible in single-digit milliseconds; 25ms doubling to a 500ms cap spans
// 25+50+100+200+400+500 = 1275ms, roughly two orders of magnitude of headroom over that. The
// alternative — retrying until the 5s timeout — would add five seconds to EVERY
// genuinely-rolled-back commit, which during a database outage means every failing request
// holds its goroutine five times longer while the database is already the bottleneck. The
// timeout stays as the hard ceiling for the case the queries themselves are slow.
//
// N attempts sleep N-1 times, so the count is one MORE than the number of delays the schedule
// above lists. It was 6, which the same paragraph described as spanning ~1.2s while actually
// spanning 775ms — and 6 attempts never reach the 500ms cap at all, so the documented "doubling
// to 500ms" was not merely mis-added but unreachable. TestAmbiguousCommitReconcileScheduleSpans
// now computes the sum from the constants, so the next change to any of the three fails a test
// rather than a comment.
const (
	ambiguousCommitReconcileAttempts = 7
	ambiguousCommitReconcileMinDelay = 25 * time.Millisecond
	ambiguousCommitReconcileMaxDelay = 500 * time.Millisecond
)

// reconcileAmbiguousAudienceCommit runs after tx.Commit returns an error that does not prove
// the transaction rolled back. If it actually committed, the row may be in ANY status
// (building, built, or failed) — CreateAudienceForApprovedBrief and CreateAudience return
// nil for `created` on this path, so the service layer has no way to know. The reconcile must
// determine what was actually persisted and converge to THAT:
//
//   - If the row is 'building': move it to 'failed' to release the (brief_id, platform) build
//     lease (migration 000018) held only by 'building' rows. This matches what the service
//     layer would have done had it received the row and called releaseUnstartedClaim.
//   - If the row is 'built' or 'failed': do NOT move it. A 'built' row is a successful build
//     with a valid master-list pointer; a 'failed' row is already terminal. Both are stable
//     end states that must not be corrupted.
//   - If the commit genuinely rolled back, no row exists at that id and there is nothing to
//     reconcile.
//
// The last of those is the one that cannot be decided by a single statement, and it is why
// this retries. A zero-row UPDATE does not prove a rollback. The commit whose result we never
// received may still have been IN PROGRESS on the server when this runs: it executes on a
// different pooled connection, so it takes its own snapshot, and under READ COMMITTED a row
// inserted by a transaction that has not yet committed is not merely locked but invisible —
// the UPDATE does not block on it, it matches nothing and returns. Milliseconds later the
// commit lands and the row is there, 'building', holding the (brief_id, platform) lease from
// migration 000018 that only a manual PATCH to 'failed' can then release. That window is
// precisely the one this function exists for, so treating the first zero-row result as final
// leaves it blind to a subset of its own reason for existing.
//
// Retrying "no visible row" is therefore the fix, and only that outcome is retried: a row
// observed in 'built' or 'failed' is a stable end state and settles the question immediately.
func reconcileAmbiguousAudienceCommit(ctx context.Context, pool *Pool, row *model.CampaignAudience) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ambiguousCommitReconcileTimeout)
	defer cancel()

	delay := ambiguousCommitReconcileMinDelay
	// Remembers whether any attempt CONFIRMED the row present and still 'building'. It is what
	// separates the two ways this loop can run out, and they are not the same event: "never saw
	// a row" is the ordinary rolled-back commit, while "saw the row, still building" is a lease
	// this process knows is held and could not release. Reporting the second as the first is
	// how a real stranded lease gets logged at warn and read as routine.
	confirmedHeld := false
	for attempt := 1; ; attempt++ {
		outcome, err := tryReleaseAudienceBuildLease(rctx, pool, row)
		if err != nil {
			slog.ErrorContext(ctx, "failed to reconcile an audience build row after an ambiguous commit error",
				"audience_id", row.ID, "attempt", attempt, "error", err)
		}
		if outcome == audienceReconcileSettled {
			return
		}
		confirmedHeld = confirmedHeld || outcome == audienceReconcileHeld
		if attempt >= ambiguousCommitReconcileAttempts {
			reportUnreconciledAudience(ctx, row, attempt, confirmedHeld,
				"could not confirm an audience row's fate after an ambiguous commit error")
			return
		}
		select {
		case <-rctx.Done():
			reportUnreconciledAudience(ctx, row, attempt, confirmedHeld,
				"ran out of time confirming an audience row's fate after an ambiguous commit error")
			return
		case <-time.After(delay):
		}
		if delay *= 2; delay > ambiguousCommitReconcileMaxDelay {
			delay = ambiguousCommitReconcileMaxDelay
		}
	}
}

// reportUnreconciledAudience logs the end of a reconcile that never settled.
//
// The two ways it can end are different events and get different levels. "Never saw a row" is
// the overwhelmingly likely reading — the commit really did roll back, which is what "no row"
// means every other time — so it is a warn: an error on every rolled-back commit is an error
// nobody reads. "Saw the row, still building" is not that. It is a confirmed held lease that
// no automatic path will release, since migration 000018's escape hatch is deliberately manual
// (the row may own real HubSpot lists), so it is an error and it states the fact rather than
// hedging it.
func reportUnreconciledAudience(ctx context.Context, row *model.CampaignAudience, attempts int, confirmedHeld bool, what string) {
	attrs := []any{
		"audience_id", row.ID, "brief_id", row.BriefID, "project_id", row.ProjectID,
		"platform", string(row.Platform), "attempts", attempts,
	}
	if confirmedHeld {
		slog.ErrorContext(ctx, what+"; the row was CONFIRMED present and still 'building', so its "+
			"build lease is held and blocks every later build of this (brief, platform) until an "+
			"operator PATCHes the row to 'failed'", attrs...)
		return
	}
	slog.WarnContext(ctx, what+"; if the commit did land, its build lease is held until an "+
		"operator PATCHes the row to 'failed'", attrs...)
}

// audienceReconcileOutcome is what one release attempt learned about the row.
type audienceReconcileOutcome int

const (
	// audienceReconcileUnseen: no row is visible at that id. Either the commit rolled back or
	// it has not landed yet, and nothing here can tell those apart — the only outcome worth
	// waiting on.
	audienceReconcileUnseen audienceReconcileOutcome = iota
	// audienceReconcileSettled: the row's fate is KNOWN — released by this attempt, or already
	// in a terminal state that holds no lease.
	audienceReconcileSettled
	// audienceReconcileHeld: the row is confirmed present and still 'building' after this
	// attempt tried twice to release it. Distinct from Unseen because the caller must not
	// report it as a probable rollback.
	audienceReconcileHeld
)

// audienceReconcileDB is the slice of *Pool the reconcile uses. It exists so the attempt below
// can be driven directly in a unit test: the two outcomes that matter most here — a confirmed
// held lease, and a query that FAILS after the row has already been observed 'building' — both
// require the row to appear between an attempt's two statements, which nothing can schedule
// into against a live database. The live tests cover the paths that can be staged; this covers
// the ones that cannot.
type audienceReconcileDB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// tryReleaseAudienceBuildLease performs one attempt of the reconcile above.
//
// An error is returned for the caller to log but never settles anything: this runs precisely
// when the connection is known to be unreliable, so a failed query is the least trustworthy
// evidence of absence there is.
//
// What an error means for the OUTCOME depends on what this attempt has already seen, though,
// and conflating the two lost information the caller cannot recover. Once the first pass has
// read the row as 'building', this process KNOWS a lease is held; a second-pass query failing
// after that does not unlearn it. Returning the zero value (audienceReconcileUnseen) on those
// paths left confirmedHeld false in the retry loop, so a lease this process had confirmed was
// held got reported as the ordinary probable-rollback warn — the exact downgrade the tri-state
// outcome was introduced to prevent.
func tryReleaseAudienceBuildLease(ctx context.Context, pool audienceReconcileDB, row *model.CampaignAudience) (audienceReconcileOutcome, error) {
	// What an unsettled result means, given everything seen so far in THIS attempt. It starts
	// as Unseen and can only ever be promoted, never demoted: absence is evidence only until
	// the row is observed, and after that it is not evidence at all.
	unsettled := audienceReconcileUnseen
	// Two passes, not one. Observing 'building' after a zero-row UPDATE means the row became
	// visible BETWEEN this attempt's two statements — they run on separately-pooled connections
	// with their own snapshots — so the UPDATE merely ran too early and an immediate second one
	// matches. Leaving that to the caller's next attempt was a real gap: on the LAST attempt, or
	// when the reconcile context is already done, there is no next attempt, and a row this
	// process had just CONFIRMED was holding the lease got abandoned to the manual escape hatch.
	// Retrying here costs one statement and closes it without touching the retry budget, which
	// exists for a different question (has the commit landed at all?).
	for pass := 0; pass < 2; pass++ {
		// The release is ONE statement, not a SELECT followed by an UPDATE. The
		// `status='building'` predicate is what enforces the do-not-downgrade invariant, and it
		// has to be part of the write to enforce it at all: a separate status read is a TOCTOU
		// window in which a concurrent PATCH can land, and a version predicate does not close it
		// — a PATCH that edits another field while leaving the status 'building' still bumps the
		// version, so the UPDATE would match zero rows, report no error, and abandon the row
		// holding the lease forever. Predicating on the status instead of the version is also
		// what makes the write idempotent under retry, which this depends on twice over now.
		//
		// Scoped by brief_id and project_id too, matching UpdateAudience's predicate — id alone
		// would still be correct (it's the primary key), but every other write to this table
		// carries the tenant scope, and this is a write path: worth keeping the pattern uniform
		// rather than carving out a silent exception here.
		tag, err := pool.Exec(ctx, releaseAudienceBuildLeaseQ,
			row.ID, row.BriefID, row.ProjectID, string(model.AudienceBuilding))
		if err != nil {
			return unsettled, fmt.Errorf("release audience build lease: %w", err)
		}
		if tag.RowsAffected() > 0 {
			return audienceReconcileSettled, nil
		}

		// Zero rows has two meanings and they need opposite handling, so ask which one it was.
		// This read is NOT the TOCTOU the single-statement write above avoids: it runs only
		// after the write declined to match, it decides nothing about what to write, and its
		// failure modes all fall through to another attempt rather than to an early return.
		var status string
		switch err := pool.QueryRow(ctx,
			`SELECT status FROM campaign_audiences WHERE id=$1 AND brief_id=$2 AND project_id=$3`,
			row.ID, row.BriefID, row.ProjectID).Scan(&status); {
		case errors.Is(err, pgx.ErrNoRows):
			// Rolled back, or not yet visible. Indistinguishable here; the caller retries.
			return audienceReconcileUnseen, nil
		case err != nil:
			return unsettled, fmt.Errorf("check audience row after a zero-row reconcile: %w", err)
		case status != string(model.AudienceBuilding):
			// 'built' or 'failed': a stable end state that holds no lease and must not be
			// touched. Re-read on the second pass too, so a concurrent PATCH landing between
			// the passes is reported as settled rather than as a held lease.
			return audienceReconcileSettled, nil
		}
		// Still 'building': the lease is confirmed held from here on, so a later failure in this
		// attempt must not report absence.
		unsettled = audienceReconcileHeld
		// Fall through to the second pass, or out of the loop.
	}
	return audienceReconcileHeld, nil
}

// releaseAudienceBuildLeaseQ moves a 'building' audience row to 'failed', releasing the
// (brief_id, platform) build lease from migration 000018.
//
// It is shared by the ambiguous-commit reconcile and by ReleaseAudienceBuildLease so the two
// cannot drift: both need the same status predicate for the same reason, and a second copy of
// this statement gated on the version instead is exactly the defect the predicate exists to
// prevent.
const releaseAudienceBuildLeaseQ = `UPDATE campaign_audiences
	SET status='failed', version=version+1, updated_at=now()
	WHERE id=$1 AND brief_id=$2 AND project_id=$3 AND status=$4`

// ReleaseAudienceBuildLease implements domain.AudienceRepository.
func (r *AudienceRepo) ReleaseAudienceBuildLease(ctx context.Context, projectID, briefID, id string) error {
	// Status-gated and unversioned, which is the whole point of it existing separately from
	// UpdateAudience: the caller is releasing a lease it has given up on, and gating that on a
	// version it read earlier means a concurrent PATCH — which bumps the version even when it
	// leaves the status 'building' — turns the release into a no-op and strands the lease.
	//
	// Silent no-op on zero rows, deliberately. The row being absent or already terminal are
	// both "the lease is not held", which is the caller's goal; reporting either as an error
	// would only produce a log line for a state nobody needs to act on.
	if _, err := r.db.Exec(ctx, releaseAudienceBuildLeaseQ,
		id, briefID, projectID, string(model.AudienceBuilding)); err != nil {
		return fmt.Errorf("release audience build lease: %w", err)
	}
	return nil
}

func (r *AudienceRepo) CreateAudience(ctx context.Context, a *model.CampaignAudience) (*model.CampaignAudience, error) {
	// Gate the insert on an ACTIVE parent brief scoped by BOTH (project_id, brief_id).
	// A bare brief_id FK check would let a caller authorized for project A supply a
	// brief id from project B (tenant/parent disagree), and would accept an archived
	// brief. INSERT...SELECT...WHERE EXISTS inserts zero rows when the active,
	// same-project parent is absent, which we map to ErrNotFound.
	//
	// This executes in an explicit transaction to handle the ambiguous-commit case:
	// if the connection drops after PostgreSQL commits but before pgx receives the
	// RETURNING result, a `building` row is committed and holds the lease, but we
	// have no way to know its ID for reconciliation if this isn't a transaction that
	// kept the ID in scope. This matches CreateAudienceForApprovedBrief's pattern.
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("create audience: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := `INSERT INTO campaign_audiences
		(project_id, brief_id, platform, platform_master_list_id, suppression_list_ids,
		 inclusion_summary, status, created_by)
		SELECT $1,$2,$3,$4,$5,$6,$7,$8
		WHERE EXISTS (
			SELECT 1 FROM campaign_briefs
			WHERE id=$2 AND project_id=$1 AND status <> 'archived'
		)
		RETURNING ` + audienceCols
	created, err := scanAudience(tx.QueryRow(ctx, q,
		a.ProjectID, a.BriefID, string(a.Platform), nullStr(a.PlatformMasterListID),
		nullJSON(a.SuppressionListIDs), nullStr(a.InclusionSummary), string(a.StatusOrDefault()),
		nullJSON(a.CreatedBy),
	))
	if errors.Is(err, pgx.ErrNoRows) {
		// No active parent brief for (project, brief) → the parent is missing,
		// archived, or belongs to another project.
		return nil, domain.ErrNotFound
	}
	if isUniqueViolationOn(err, audienceBuildLeaseIndex) {
		// The plain create defaults status to 'building' too, so it takes the same lease
		// (000018) and can lose it to an in-flight BuildAudience. Same sentinel: this
		// caller is second, not duplicating.
		return nil, domain.ErrAudienceBuildInFlight
	}
	if err != nil {
		return nil, fmt.Errorf("create audience: insert: %w", err)
	}
	if cerr := tx.Commit(ctx); cerr != nil {
		// A Commit error does not prove the server rolled back: PostgreSQL may have
		// committed the row before the connection failed to acknowledge it. If it did,
		// this INSERT's `building` row silently holds the (brief_id, platform) lease
		// (000018) forever — the caller here returns an error, so there is no row to
		// pass to releaseUnstartedClaim. Reconcile directly: best-effort move the row
		// to `failed` under a bounded, detached context, using the id/version this
		// INSERT already observed inside the (possibly-rolled-back) transaction. If the
		// commit genuinely rolled back, no row exists at that id and the UPDATE is a
		// harmless no-op.
		reconcileAmbiguousAudienceCommit(ctx, r.db, created)
		return nil, fmt.Errorf("create audience: commit: %w", cerr)
	}
	return created, nil
}

// GetAudience returns one audience by id, scoped to (project, brief), or ErrNotFound.
func (r *AudienceRepo) GetAudience(ctx context.Context, projectID, briefID, id string) (*model.CampaignAudience, error) {
	// Require an ACTIVE parent brief, consistent with ListAudiences and CreateAudience:
	// once a brief is archived its audiences are no longer part of the live lifecycle, so
	// get/update must 404 rather than list 404-ing while get/patch still succeed on the
	// same nested resource. The EXISTS keeps this a single round-trip. (Update loads via
	// this method, so guarding Get covers the patch path too.)
	q := `SELECT ` + audienceCols + ` FROM campaign_audiences ca
		WHERE ca.id=$1 AND ca.brief_id=$2 AND ca.project_id=$3
		AND EXISTS (
			SELECT 1 FROM campaign_briefs b
			WHERE b.id=ca.brief_id AND b.project_id=ca.project_id AND b.status <> 'archived'
		)`
	a, err := scanAudience(r.db.QueryRow(ctx, q, id, briefID, projectID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("get audience: %w", err)
	}
	return a, nil
}

// ListAudiences returns a brief's audiences (newest first), scoped to the project.
// maxAudiencesPerList bounds a single ListAudiences response. Audiences accumulate
// over time (per platform / per build), so an unbounded list would grow without
// limit; this caps the query cost and response size. A stable (created_at, id) order
// makes the cap deterministic (newest first).
const maxAudiencesPerList = 200

func (r *AudienceRepo) ListAudiences(ctx context.Context, projectID, briefID string) ([]*model.CampaignAudience, error) {
	// Verify the ACTIVE parent brief exists for (project, brief) first — otherwise a
	// missing / cross-project / archived brief would return 200 with an empty array
	// instead of the NotFound the endpoint declares (the child-only query can't
	// distinguish "no audiences yet" from "no such brief").
	var exists bool
	if err := r.db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM campaign_briefs WHERE id=$1 AND project_id=$2 AND status <> 'archived')`,
		briefID, projectID,
	).Scan(&exists); err != nil {
		return nil, fmt.Errorf("verify parent brief: %w", err)
	}
	if !exists {
		return nil, domain.ErrNotFound
	}

	q := `SELECT ` + audienceCols + ` FROM campaign_audiences
		WHERE brief_id=$1 AND project_id=$2
		ORDER BY created_at DESC, id DESC
		LIMIT $3`
	rows, err := r.db.Query(ctx, q, briefID, projectID, maxAudiencesPerList)
	if err != nil {
		return nil, fmt.Errorf("list audiences: %w", err)
	}
	defer rows.Close()

	var out []*model.CampaignAudience
	for rows.Next() {
		a, sErr := scanAudience(rows)
		if sErr != nil {
			return nil, fmt.Errorf("scan audience row: %w", sErr)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate audience rows: %w", err)
	}
	return out, nil
}

// UpdateAudience replaces the mutable fields under an optimistic-concurrency guard on
// expectedVersion (ErrPreconditionFailed on mismatch, ErrNotFound when absent).
func (r *AudienceRepo) UpdateAudience(ctx context.Context, a *model.CampaignAudience, expectedVersion int64) (*model.CampaignAudience, error) {
	// UPDATE ... RETURNING returns the row THIS statement wrote, atomically — so the
	// caller always gets the state + ETag produced by its OWN write. A separate
	// post-update re-read would race: a concurrent version N+1 could land between the
	// UPDATE and the read, handing this caller the other writer's row and ETag.
	q := `UPDATE campaign_audiences SET
		platform_master_list_id=$1, suppression_list_ids=$2, inclusion_summary=$3,
		status=$4, version=version+1, updated_at=now()
		WHERE id=$5 AND brief_id=$6 AND project_id=$7 AND version=$8
		RETURNING ` + audienceCols
	updated, err := scanAudience(r.db.QueryRow(ctx, q,
		nullStr(a.PlatformMasterListID), nullJSON(a.SuppressionListIDs), nullStr(a.InclusionSummary),
		string(a.StatusOrDefault()), a.ID, a.BriefID, a.ProjectID, expectedVersion,
	))
	if err == nil {
		return updated, nil
	}
	if isUniqueViolationOn(err, audienceBuildLeaseIndex) {
		// A PATCH that moves a 'failed' or 'built' row BACK to 'building' takes the lease
		// (000018) and can find it held. Worth the branch even though it is the rarest way
		// in: this is the retry path an operator reaches for after reconciling a stuck
		// build, and the unmapped form would surface as a bare 500.
		return nil, domain.ErrAudienceBuildInFlight
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("update audience: %w", err)
	}
	// No row matched the (id, brief, project, version) predicate. Distinguish "absent"
	// from "version mismatch" by re-reading (this read is only for classifying the
	// no-op — it never becomes the returned row — so it can't race the success path),
	// and surface a transient re-fetch error rather than masking it as a precondition
	// failure (consistent with ReplaceCampaign / ConnectionRepo.Update).
	_, gerr := r.GetAudience(ctx, a.ProjectID, a.BriefID, a.ID)
	switch {
	case errors.Is(gerr, domain.ErrNotFound):
		return nil, domain.ErrNotFound
	case gerr != nil:
		return nil, gerr
	default:
		return nil, domain.ErrPreconditionFailed
	}
}

// scanAudience reads one campaign_audiences row in audienceCols order.
func scanAudience(row pgx.Row) (*model.CampaignAudience, error) {
	var (
		a         model.CampaignAudience
		platform  string
		masterID  *string
		suppress  []byte
		inclusion *string
		status    string
		createdBy []byte
	)
	if err := row.Scan(
		&a.ID, &a.ProjectID, &a.BriefID, &platform,
		&masterID, &suppress, &inclusion, &status, &a.Version,
		&createdBy, &a.CreatedAt, &a.UpdatedAt,
	); err != nil {
		return nil, err
	}
	a.Platform = model.Provider(platform)
	if masterID != nil {
		a.PlatformMasterListID = *masterID
	}
	if inclusion != nil {
		a.InclusionSummary = *inclusion
	}
	a.SuppressionListIDs = suppress
	a.CreatedBy = createdBy
	a.Status = model.AudienceStatus(status)
	return &a, nil
}
