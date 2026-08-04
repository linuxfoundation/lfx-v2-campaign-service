// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// ReconciliationRepo is a pgx-backed implementation of
// domain.ReconciliationRepository.
type ReconciliationRepo struct {
	db *Pool
}

// NewReconciliationRepo returns a ReconciliationRepo backed by pool.
func NewReconciliationRepo(pool *Pool) *ReconciliationRepo { return &ReconciliationRepo{db: pool} }

var _ domain.ReconciliationRepository = (*ReconciliationRepo)(nil)

// partialOrphanStatuses are the dispatcher-set statuses that mark a campaign row as a
// retained PARTIAL orphan. They are duplicated here rather than imported from
// internal/service: that package imports this one, so a real import would cycle. The
// literals are drift-guarded by TestPartialOrphanStatusesMatchService.
var partialOrphanStatuses = []string{"unconfirmed", "group_created"}

// reconcileCampaignsQuery selects every campaign row in a project that is NOT in a
// settled state, classifying each one.
//
// The classification is the whole point, so it lives in SQL next to the predicates it
// derives from rather than being re-derived in Go from a looser query:
//
//   - A row is a BARE claim (kind stuck_claim, releasable) only when it is 'pending'
//     AND has no platform_campaign_id AND no result blob. That combination is the
//     service's own definition of "carries no evidence of an upstream create" — see
//     the orchestrator's retain branch, which persists Result precisely so an
//     ambiguous create is NOT mistaken for a bare claim.
//   - Everything else non-settled is an unconfirmed_campaign: it has an upstream id,
//     or a reconcile blob, or a partial-orphan status. Never releasable.
//
// The age floor is applied by the caller (minClaimAge) so healthy in-flight dispatches
// are never listed.
const reconcileCampaignsQuery = `
	SELECT
		id::text,
		brief_id::text,
		platform,
		status,
		COALESCE(platform_campaign_id, '') AS platform_campaign_id,
		EXTRACT(EPOCH FROM (now() - created_at))::bigint AS age_seconds,
		version,
		(status = 'pending' AND platform_campaign_id IS NULL AND result IS NULL) AS bare_claim
	FROM campaigns
	WHERE project_id = $1
	  AND status = ANY($2)
	  AND created_at < now() - make_interval(secs => $3)
	ORDER BY created_at ASC
	LIMIT $4`

// reconcileCampaignsCountQuery counts the same set without the LIMIT, so a truncated
// page can report an honest total.
const reconcileCampaignsCountQuery = `
	SELECT count(*) FROM campaigns
	WHERE project_id = $1
	  AND status = ANY($2)
	  AND created_at < now() - make_interval(secs => $3)`

// reconcileAudiencesQuery selects audiences stranded mid-build. A 'building' row means
// some platform lists may already exist (the builder writes the row BEFORE its first
// HubSpot call, precisely so a crash leaves a visible reconcilable row). Never
// releasable: this service cannot tell which lists landed.
const reconcileAudiencesQuery = `
	SELECT
		a.id::text,
		a.brief_id::text,
		a.platform,
		a.status,
		COALESCE(a.platform_master_list_id, '') AS master_list_id,
		EXTRACT(EPOCH FROM (now() - a.created_at))::bigint AS age_seconds,
		a.version,
		COALESCE(a.inclusion_summary, '') AS inclusion_summary
	FROM campaign_audiences a
	WHERE a.project_id = $1
	  AND a.status = 'building'
	  AND a.created_at < now() - make_interval(secs => $2)
	ORDER BY a.created_at ASC
	LIMIT $3`

const reconcileAudiencesCountQuery = `
	SELECT count(*) FROM campaign_audiences
	WHERE project_id = $1 AND status = 'building'
	  AND created_at < now() - make_interval(secs => $2)`

// nonSettledCampaignStatuses are the campaign statuses that may need reconciliation.
// 'created', the run states, and 'created_degraded' are settled — a degraded campaign
// genuinely exists upstream and a re-dispatch cannot repair it, so it is not an
// operator action item.
func nonSettledCampaignStatuses() []string {
	return append([]string{model.CampaignStatusPending}, partialOrphanStatuses...)
}

// ListReconciliationItems returns the project's stuck campaign rows and partial
// audiences, oldest first, capped at limit.
func (r *ReconciliationRepo) ListReconciliationItems(ctx context.Context, projectID string, minClaimAge time.Duration, limit int) ([]model.ReconciliationItem, int64, error) {
	statuses := nonSettledCampaignStatuses()
	ageSecs := minClaimAge.Seconds()

	items := make([]model.ReconciliationItem, 0, limit)

	rows, err := r.db.Query(ctx, reconcileCampaignsQuery, projectID, statuses, ageSecs, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("list reconciliation campaigns: %w", err)
	}
	for rows.Next() {
		var (
			it        model.ReconciliationItem
			platform  string
			ageSec    int64
			bareClaim bool
		)
		if serr := rows.Scan(&it.CampaignID, &it.BriefID, &platform, &it.Status,
			&it.PlatformCampaignID, &ageSec, &it.Version, &bareClaim); serr != nil {
			rows.Close()
			return nil, 0, fmt.Errorf("scan reconciliation campaign: %w", serr)
		}
		it.Platform = model.Provider(platform)
		it.Age = time.Duration(ageSec) * time.Second
		if bareClaim {
			it.Kind = model.ReconcileStuckClaim
			it.Resolvable = true
			it.Detail = "A dispatch claim whose holder died. It carries no upstream campaign id and no reconcile detail, so nothing is known to exist on the ad platform. Verify in the platform that no campaign exists for this brief, then release the claim to unblock future dispatches."
		} else {
			it.Kind = model.ReconcileUnconfirmedCampaign
			it.Resolvable = false
			it.Detail = unconfirmedDetail(it.PlatformCampaignID)
		}
		items = append(items, it)
	}
	if rerr := rows.Err(); rerr != nil {
		rows.Close()
		return nil, 0, fmt.Errorf("iterate reconciliation campaigns: %w", rerr)
	}
	rows.Close()

	var campaignTotal int64
	if cerr := r.db.QueryRow(ctx, reconcileCampaignsCountQuery, projectID, statuses, ageSecs).Scan(&campaignTotal); cerr != nil {
		return nil, 0, fmt.Errorf("count reconciliation campaigns: %w", cerr)
	}

	audItems, audTotal, aerr := r.listPartialAudiences(ctx, projectID, ageSecs, limit)
	if aerr != nil {
		return nil, 0, aerr
	}
	items = append(items, audItems...)

	return items, campaignTotal + audTotal, nil
}

// listPartialAudiences returns the project's audiences stranded mid-build.
func (r *ReconciliationRepo) listPartialAudiences(ctx context.Context, projectID string, ageSecs float64, limit int) ([]model.ReconciliationItem, int64, error) {
	rows, err := r.db.Query(ctx, reconcileAudiencesQuery, projectID, ageSecs, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("list reconciliation audiences: %w", err)
	}
	defer rows.Close()

	items := make([]model.ReconciliationItem, 0, limit)
	for rows.Next() {
		var (
			it       model.ReconciliationItem
			platform string
			ageSec   int64
			summary  string
			masterID string
		)
		if serr := rows.Scan(&it.AudienceID, &it.BriefID, &platform, &it.Status,
			&masterID, &ageSec, &it.Version, &summary); serr != nil {
			return nil, 0, fmt.Errorf("scan reconciliation audience: %w", serr)
		}
		it.Platform = model.Provider(platform)
		it.Age = time.Duration(ageSec) * time.Second
		it.Kind = model.ReconcilePartialAudience
		it.Resolvable = false
		it.Detail = audienceDetail(summary)
		items = append(items, it)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, 0, fmt.Errorf("iterate reconciliation audiences: %w", rerr)
	}

	var total int64
	if cerr := r.db.QueryRow(ctx, reconcileAudiencesCountQuery, projectID, ageSecs).Scan(&total); cerr != nil {
		return nil, 0, fmt.Errorf("count reconciliation audiences: %w", cerr)
	}
	return items, total, nil
}

// unconfirmedDetail explains an unconfirmed campaign in terms of what the operator must
// check. The two shapes differ in what is known, so they get different guidance.
func unconfirmedDetail(platformCampaignID string) string {
	if platformCampaignID != "" {
		return "A dispatch failed after the upstream campaign was created; its id was recorded. The campaign EXISTS on the ad platform — do not re-dispatch. Inspect it upstream and either finish it there or update this campaign row to reflect reality."
	}
	return "A dispatch failed with an ambiguous outcome: a campaign MAY exist upstream but no id was captured. Search the ad platform by campaign name before doing anything else. This service will not release the claim, because releasing it would allow a retry to create a second paid campaign."
}

// audienceDetail explains a stranded audience build, carrying through the builder's own
// recorded provenance when it left any.
func audienceDetail(summary string) string {
	base := "An audience build stopped partway: some platform lists may already exist. Verify in HubSpot which lists were created before retrying, or the retry will duplicate them."
	if summary != "" {
		return base + " Recorded progress: " + summary
	}
	return base
}

// releaseLockQuery locks the candidate row and re-reads every field the decision
// depends on. FOR UPDATE both serializes against a concurrent writer of this row and
// re-reads the LATEST committed version, which a plain guarded DELETE cannot do under
// READ COMMITTED.
const releaseLockQuery = `
	SELECT
		brief_id::text,
		platform,
		status,
		COALESCE(platform_campaign_id, '') AS platform_campaign_id,
		result IS NOT NULL AS has_result,
		version,
		EXTRACT(EPOCH FROM (now() - created_at))::bigint AS age_seconds
	FROM campaigns
	WHERE id = $1 AND brief_id = $2 AND project_id = $3
	FOR UPDATE`

// ReleaseDispatchClaimByID deletes a stranded bare claim inside one transaction, after
// re-verifying every precondition under a row lock.
//
// Why the transaction is necessary. The operator reads the inventory in one request and
// releases in another. Between those, a new dispatch can legitimately re-claim the same
// (brief, platform) pair — which UPDATEs the row, bumping version and resetting
// created_at. A DELETE guarded only on status/id/result would happily delete that LIVE
// claim, freeing the pair while a provider call is in flight and letting a concurrent
// dispatch create a SECOND paid campaign. Verified against a real database: the
// status-only guard deletes the re-claimed row, while this version+age gate under
// FOR UPDATE correctly refuses.
func (r *ReconciliationRepo) ReleaseDispatchClaimByID(ctx context.Context, projectID, briefID, campaignID string, expectedVersion int64, minAge time.Duration) (*model.ReconciliationItem, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("release dispatch claim: begin tx: %w", err)
	}
	// Roll back unless we explicitly commit; a no-op after a successful Commit.
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		lockedBriefID string
		platform      string
		status        string
		pcID          string
		hasResult     bool
		version       int64
		ageSeconds    int64
	)
	if serr := tx.QueryRow(ctx, releaseLockQuery, campaignID, briefID, projectID).Scan(
		&lockedBriefID, &platform, &status, &pcID, &hasResult, &version, &ageSeconds); serr != nil {
		if errors.Is(serr, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("release dispatch claim: lock row: %w", serr)
	}

	// Version first: it is the operator's explicit statement about WHICH row state
	// they inspected, so a mismatch is a precondition failure (412) rather than a
	// conflict — the caller should re-read the report and decide again.
	if version != expectedVersion {
		return nil, domain.ErrPreconditionFailed
	}

	// Now the safety predicates, re-evaluated on the LOCKED row. Each corresponds to a
	// way the row could carry evidence that a paid campaign exists upstream.
	if status != model.CampaignStatusPending || pcID != "" || hasResult {
		return nil, domain.ErrConflict
	}
	// The age floor is the backstop for the case the version gate cannot see: a claim
	// DELETEd and re-INSERTed by a new dispatch resets version to 1, which could
	// coincidentally match. A fresh row is young, so the floor rejects it.
	if time.Duration(ageSeconds)*time.Second < minAge {
		return nil, domain.ErrConflict
	}

	del := `DELETE FROM campaigns WHERE id = $1`
	tag, derr := tx.Exec(ctx, del, campaignID)
	if derr != nil {
		return nil, fmt.Errorf("release dispatch claim: delete: %w", derr)
	}
	if tag.RowsAffected() != 1 {
		// Cannot happen: the row is locked and was just read. Treat defensively as a
		// conflict rather than reporting a success that did not occur.
		return nil, domain.ErrConflict
	}
	if cerr := tx.Commit(ctx); cerr != nil {
		return nil, fmt.Errorf("release dispatch claim: commit: %w", cerr)
	}

	return &model.ReconciliationItem{
		Kind:       model.ReconcileStuckClaim,
		BriefID:    lockedBriefID,
		Platform:   model.Provider(platform),
		CampaignID: campaignID,
		Status:     status,
		Age:        time.Duration(ageSeconds) * time.Second,
		Version:    version,
		Resolvable: false,
		Detail:     "Claim released. The (brief, platform) pair is now free to dispatch again.",
	}, nil
}
