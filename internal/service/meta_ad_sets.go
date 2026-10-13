// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/indexer"
)

// MetaAdSetReader is an OPTIONAL dispatcher capability (LFXV2-2665, Meta only): read the ad sets
// under a campaign with their status, budget and delivery counters. Type-asserted, so a dispatcher
// without it yields ErrMetaAdSetsUnsupported → 400. A pure read: no upstream mutation, nothing
// written to the row.
type MetaAdSetReader interface {
	ReadMetaAdSets(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, window model.MetricsWindow) (*model.MetaAdSets, error)
}

// MetaAdSetStatusToggler is an OPTIONAL dispatcher capability (LFXV2-2665, Meta only): pause or
// resume ONE ad set of a campaign. status is model.MetaAdSetStatusActive or
// model.MetaAdSetStatusPaused.
//
// Contract: every guard (provenance, ownership of the ad set, the adopted-ACTIVATE refusal) runs
// BEFORE the write, and the write is sent at most once. A nil error carries APPLIED or
// ALREADY_IN_STATE; a write whose outcome is unknown returns an error implementing
// Unconfirmed() bool; every other error means nothing was changed.
//
// beforeWrite, when non-nil, is called at most once: after every guard and the pre-write read,
// only when the single write is about to be sent, and never on ALREADY_IN_STATE. If it returns an
// error, nothing is sent and that failure is returned (wrapped) as an ordinary error.
type MetaAdSetStatusToggler interface {
	ToggleMetaAdSetStatus(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, adSetID, status string, beforeWrite MetaAdSetBeforeWrite) (*model.MetaAdSetStatusResult, error)
}

// MetaAdSetBeforeWrite is the hook MetaAdSetStatusToggler runs immediately before its one write.
type MetaAdSetBeforeWrite func(ctx context.Context) error

// SupportsMetaAdSetToggle reports whether platform's dispatcher implements
// MetaAdSetStatusToggler, so the service can answer 400 before it takes the campaign's write lock.
func (o *Orchestrator) SupportsMetaAdSetToggle(platform model.Provider) bool {
	d, ok := o.dispatchers[platform]
	if !ok {
		return false
	}
	_, ok = d.(MetaAdSetStatusToggler)
	return ok
}

// ReadMetaAdSets reads a campaign's ad sets through its dispatcher's MetaAdSetReader. The
// capability is checked FIRST, so a non-Meta row is 400 whatever its provisioning state; a row
// with no platform campaign id is then refused without contacting anything.
func (o *Orchestrator) ReadMetaAdSets(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, window model.MetricsWindow) (*model.MetaAdSets, error) {
	d, ok := o.dispatchers[platform]
	if !ok {
		return nil, fmt.Errorf("%w: no dispatcher registered for platform %s", domain.ErrMetaAdSetsUnsupported, platform)
	}
	reader, ok := d.(MetaAdSetReader)
	if !ok {
		return nil, fmt.Errorf("%w: %s", domain.ErrMetaAdSetsUnsupported, platform)
	}
	if campaign == nil || strings.TrimSpace(campaign.PlatformCampaignID) == "" {
		return nil, ErrCampaignNotProvisioned
	}
	callCtx, cancel := context.WithTimeout(ctx, metricsCallTimeout)
	defer cancel()
	start := time.Now()
	out, rerr := reader.ReadMetaAdSets(callCtx, projectID, platform, campaign, window)
	o.recordUpstream(ctx, platform, opReadMetaAdSets, start, rerr)
	if rerr != nil {
		return nil, rerr
	}
	if out == nil {
		return nil, fmt.Errorf("%s ad-set reader returned a nil result with no error", platform)
	}
	if out.AdSets == nil {
		out.AdSets = []model.MetaAdSet{}
	}
	return out, nil
}

// ToggleMetaAdSetStatus pauses or resumes one ad set through the dispatcher's
// MetaAdSetStatusToggler, under the campaign toggle's total deadline.
func (o *Orchestrator) ToggleMetaAdSetStatus(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, adSetID, status string, beforeWrite MetaAdSetBeforeWrite) (*model.MetaAdSetStatusResult, error) {
	d, ok := o.dispatchers[platform]
	if !ok {
		return nil, fmt.Errorf("%w: no dispatcher registered for platform %s", domain.ErrMetaAdSetsUnsupported, platform)
	}
	toggler, ok := d.(MetaAdSetStatusToggler)
	if !ok {
		return nil, fmt.Errorf("%w: %s", domain.ErrMetaAdSetsUnsupported, platform)
	}
	if campaign == nil || strings.TrimSpace(campaign.PlatformCampaignID) == "" {
		return nil, ErrCampaignNotProvisioned
	}
	callCtx, cancel := context.WithTimeout(ctx, toggleCallTimeout)
	defer cancel()
	start := time.Now()
	res, terr := toggler.ToggleMetaAdSetStatus(callCtx, projectID, platform, campaign, adSetID, status, beforeWrite)
	o.recordUpstream(ctx, platform, opToggleMetaAdSetStatus, start, terr)
	if terr != nil {
		return nil, terr
	}
	if res == nil {
		return nil, fmt.Errorf("%s ad-set toggler returned a nil result with no error", platform)
	}
	return res, nil
}

// metaAdSetIDOK is the design's ad_set_id rule (Pattern ^[1-9][0-9]*$, MaxLength 32), repeated for
// a direct caller. meta.ValidateAdSetID is the same rule; the service does not import the
// platform package.
func metaAdSetIDOK(id string) bool {
	if id == "" || len(id) > 32 || id[0] == '0' {
		return false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ListMetaAdSets reads a Meta campaign's ad sets live. A pure read; nothing is persisted, so there
// is no If-Match and no ETag.
func (s *BriefService) ListMetaAdSets(ctx context.Context, p *briefs.ListMetaAdSetsPayload) (*briefs.MetaAdSets, error) {
	_, campaignRepo, _, orch, err := s.ready()
	if err != nil {
		return nil, err
	}
	window := model.MetricsWindowLast30Days
	if p.Window != nil {
		window = model.MetricsWindow(*p.Window)
		if !model.IsValidMetricsWindow(window) {
			return nil, &briefs.BadRequestError{Code: "400", Message: "window must be one of: today, yesterday, last_7_days, last_14_days, last_30_days, this_month, last_month"}
		}
	}
	// The (project, brief, campaign) triple is the ownership guard. This is the ONLY 404 this
	// endpoint answers for a campaign: nothing Meta says is ever reported as absence.
	existing, gerr := campaignRepo.GetCampaign(ctx, p.ProjectID, p.BriefID, p.CampaignID)
	if gerr != nil {
		return nil, mapBriefErr(gerr)
	}
	rb, rerr := orch.ReadMetaAdSets(ctx, p.ProjectID, existing.Platform, existing, window)
	if rerr != nil {
		return nil, s.classifyMetaAdSetError(ctx, "read", p.ProjectID, p.BriefID, p.CampaignID, existing.Platform, rerr)
	}
	out := &briefs.MetaAdSets{
		CampaignID:         p.CampaignID,
		PlatformCampaignID: rb.PlatformCampaignID,
		Window:             string(window),
		Currency:           rb.Currency,
		ReadAt:             rb.ReadAt.UTC().Format(time.RFC3339),
		AdSets:             make([]*briefs.MetaAdSet, 0, len(rb.AdSets)),
	}
	for _, a := range rb.AdSets {
		var budgetType *string
		if a.BudgetType != nil {
			budgetType = optionalString(string(*a.BudgetType))
		}
		out.AdSets = append(out.AdSets, &briefs.MetaAdSet{
			ID:                 a.ID,
			Listed:             a.Listed,
			Name:               a.Name,
			Status:             a.Status,
			EffectiveStatus:    a.EffectiveStatus,
			BidStrategy:        a.BidStrategy,
			BudgetType:         budgetType,
			BudgetAmount:       a.BudgetAmount,
			Impressions:        a.Impressions,
			Clicks:             a.Clicks,
			CostMicros:         a.CostMicros,
			Ctr:                a.Ctr,
			Recorded:           a.Recorded,
			DeliberatelyPaused: a.DeliberatelyPaused,
		})
	}
	out.AdSetCount = len(out.AdSets)
	return out, nil
}

// ToggleMetaAdSetStatus pauses or resumes ONE ad set of a Meta campaign.
//
// It mirrors ToggleCampaignStatus's concurrency contract — If-Match checked against the loaded row
// before anything else (412), validation before the claim, the campaign's write lock claimed at
// the read version before the platform is contacted, and an UNCONFIRMED outcome holding that lock
// for unconfirmedLockCooldown. The ad set's STATUS is not a column of the campaign row; what the
// row does carry is the deliberate-pause record of the campaign's recorded ad set (see below).
// When that record changes the row is written (version bumped, co-indexed) and the ETag is the new
// version; otherwise nothing is persisted and the ETag comes back unchanged after
// VerifyClaimedVersion has proved no other writer changed the row while Meta was being written —
// the pause-of-a-degraded-campaign precedent.
func (s *BriefService) ToggleMetaAdSetStatus(ctx context.Context, p *briefs.ToggleMetaAdSetStatusPayload) (*briefs.MetaAdSetStatusChange, error) {
	_, campaignRepo, _, orch, err := s.ready()
	if err != nil {
		return nil, err
	}
	version, err := parseBriefIfMatch(p.IfMatch)
	if err != nil {
		return nil, err
	}
	// Both repeated for a direct (non-generated) caller; Goa enforces them over HTTP. The ad set id
	// is checked before ANY connection work, so a permanent input fault is always 400.
	if p.Status != model.MetaAdSetStatusActive && p.Status != model.MetaAdSetStatusPaused {
		return nil, &briefs.BadRequestError{Code: "400", Message: "status must be 'ACTIVE' or 'PAUSED'"}
	}
	if !metaAdSetIDOK(p.AdSetID) {
		return nil, &briefs.BadRequestError{Code: "400", Message: "ad_set_id must be a Meta ad set id: 1-32 digits without a leading zero"}
	}
	existing, gerr := campaignRepo.GetCampaign(ctx, p.ProjectID, p.BriefID, p.CampaignID)
	if gerr != nil {
		return nil, mapBriefErr(gerr)
	}
	if existing.Version != version {
		return nil, &briefs.PreconditionFailedError{Code: "412", Message: "the supplied ETag does not match the current version"}
	}
	if !orch.SupportsMetaAdSetToggle(existing.Platform) {
		return nil, &briefs.BadRequestError{Code: "400", Message: "ad-set status changes are supported for Meta campaigns only"}
	}
	// The campaign toggle's state rule, applied to the campaign the ad set lives in: a campaign
	// still provisioning or needing reconciliation must not have delivery switched ON beneath it;
	// pausing one that definitely exists upstream (created_degraded) stays allowed.
	pauseDegraded := existing.Status == model.CampaignStatusCreatedDegraded && p.Status == model.MetaAdSetStatusPaused
	if !model.CampaignStatusToggleable(existing.Status) && !pauseDegraded {
		msg := "campaign is not in a toggleable state (it is still provisioning or needs reconciliation); resolve its status before changing its ad sets"
		if existing.Status == model.CampaignStatusCreatedDegraded {
			msg = "campaign still needs reconciliation, so its ad sets cannot be activated; they can be PAUSED to stop any spend"
		}
		return nil, &briefs.ConflictError{Code: "409", Message: msg}
	}
	if strings.TrimSpace(existing.PlatformCampaignID) == "" {
		return nil, &briefs.ConflictError{Code: "409", Message: "campaign is not fully provisioned — it has no platform campaign id yet, so it has no ad sets to change"}
	}

	// The deliberate-pause record (LFXV2-2665, product decision 2026-10-13): a pause of the
	// campaign's RECORDED ad set made here must survive a campaign pause/resume, so it is kept in
	// the row's Result blob (model.WithMetaUserPausedAdSet), which the campaign toggle reads. Only
	// the recorded ad set is tracked — it is the only one the campaign ACTIVATE cascade touches —
	// so an ADOPTED row (which records none) and a hand-added ad set persist nothing, exactly as
	// before. The record is
	//   - SET before the write is sent (the beforeWrite hook), or after ALREADY_IN_STATE, so a
	//     pause whose outcome is UNCONFIRMED is ON RECORD: unknown is treated as paused, and a
	//     later campaign ACTIVATE never silently re-activates an ad set a user may have paused;
	//   - REMOVED again only when that write definitely changed nothing;
	//   - CLEARED only by a CONFIRMED resume here (APPLIED or ALREADY_IN_STATE). An UNCONFIRMED
	//     resume leaves it set — the fail-safe direction: the ad set may stay paused, never spend
	//     unasked.
	//
	// Every record write goes through writeMetaAdSetRecord, which RECONCILES a commit whose
	// outcome is unknown (domain.ErrWriteOutcomeUnknown) instead of reading it as "not written".
	// The one state this must never produce is a pause on record over an ad set that was never
	// paused — list-meta-ad-sets would report it deliberately paused while it serves — so once a
	// pause record MAY have landed, the pause is always sent.
	recorded := model.MetaRecordedAdSetID(existing.Result)
	onRecord := model.MetaUserPausedAdSetID(existing.Result)
	tracked := recorded != "" && p.AdSetID == recorded
	setRecord := tracked && p.Status == model.MetaAdSetStatusPaused && onRecord != p.AdSetID
	clearRecord := tracked && p.Status == model.MetaAdSetStatusActive && onRecord == p.AdSetID

	_, lockToken, cerr := campaignRepo.ClaimCampaignVersion(ctx, p.ProjectID, p.BriefID, p.CampaignID, version)
	if cerr != nil {
		return nil, mapBriefErr(cerr)
	}
	releaseNow := true
	defer func() {
		if releaseNow {
			_ = campaignRepo.ReleaseCampaignLock(ctx, lockToken)
		}
	}()

	// row/curVersion track the row as this call last wrote it (the claim does not bump it).
	row, curVersion := existing, version
	var intentErr error
	// intentRecorded: the pause is on record at curVersion. recordUnknown: the pause record's
	// write could not be reconciled, so it may or may not be on record (version unknown).
	intentRecorded, recordUnknown := false, false
	var beforeWrite MetaAdSetBeforeWrite
	if setRecord {
		beforeWrite = func(hctx context.Context) error {
			updated, perr := s.writeMetaAdSetRecord(hctx, campaignRepo, row, p.AdSetID, curVersion, &lockToken)
			switch {
			case perr == nil:
				row, curVersion, intentRecorded = updated, updated.Version, true
				return nil
			case errors.Is(perr, errMetaAdSetClaimLost):
				// The record's outcome is unknown and the claim is gone: nothing is sent.
				intentErr = perr
				return perr
			case errors.Is(perr, errMetaAdSetRecordUnknown):
				// The record may have landed. Suppressing the pause now could leave it on record
				// over a live ad set, so the pause is SENT — pausing never spends — and the record
				// is settled after Meta answers.
				slog.WarnContext(ctx, "meta ad set pause record write is unconfirmed; sending the pause anyway (fail-safe)",
					"campaign_id", p.CampaignID, "ad_set_id", p.AdSetID, "error", perr)
				recordUnknown = true
				return nil
			default:
				// A definite rollback: nothing is on record, so nothing is sent.
				intentErr = perr
				return perr
			}
		}
	}
	logFields := []any{"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
		"ad_set_id", p.AdSetID, "requested_status", p.Status}

	res, terr := orch.ToggleMetaAdSetStatus(ctx, p.ProjectID, existing.Platform, existing, p.AdSetID, p.Status, beforeWrite)
	if terr != nil {
		var unconfirmed interface{ Unconfirmed() bool }
		if errors.As(terr, &unconfirmed) && unconfirmed.Unconfirmed() {
			// The single write was SENT and its outcome is unknown. Answered exactly as every
			// other money lever answers it (toggle-campaign-status, budget, keyword actions): a
			// 503 that says so — never success, never "nothing changed" — with the lock held
			// for the cooldown so a second caller cannot pile another write onto it. A pause's
			// record stays set and a resume's stays uncleared: unknown is treated as paused.
			slog.WarnContext(ctx, "meta ad set status change is UNCONFIRMED (the platform may or may not reflect it)",
				append(logFields, "pause_on_record", tracked && (intentRecorded || onRecord == p.AdSetID), "error", safeErrSummary(terr))...)
			return nil, s.answerUnconfirmedAdSetWrite(ctx, campaignRepo, row, p.AdSetID, curVersion, &lockToken, recordUnknown, &releaseNow, logFields)
		}
		if errors.Is(intentErr, errMetaAdSetClaimLost) {
			slog.ErrorContext(ctx, "meta ad set pause not sent: its record could not be reconciled and the campaign's claim was lost",
				append(logFields, "error", intentErr)...)
			return nil, errAdSetClaimLost()
		}
		if intentErr != nil {
			// Recording the pause failed, so the write was never sent.
			slog.ErrorContext(ctx, "meta ad set pause not sent: recording the deliberate pause on the campaign row failed",
				append(logFields, "error", intentErr)...)
			if errors.Is(intentErr, domain.ErrPreconditionFailed) || errors.Is(intentErr, domain.ErrNotFound) {
				return nil, &briefs.ConflictError{Code: "409", Message: "this campaign was modified or deleted by another request, so the ad set's pause was not sent — nothing was changed on Meta; read the campaign before retrying"}
			}
			return nil, &briefs.ConnServiceUnavailableError{Code: "503", Message: "the ad set's pause could not be recorded on the campaign, so it was not sent — nothing was changed on Meta"}
		}
		if intentRecorded || recordUnknown {
			// The write definitely changed nothing, so the pause it recorded (or may have
			// recorded) is withdrawn. If that cannot be established, the record may now claim a
			// pause over an ad set that is still active, and the answer says so instead of
			// "nothing was changed".
			if werr := s.withdrawMetaAdSetRecord(ctx, campaignRepo, row, curVersion, &lockToken, recordUnknown, p.AdSetID); werr != nil {
				slog.ErrorContext(ctx, "meta ad set pause was not applied, and withdrawing its deliberate-pause record failed; the campaign may report the ad set deliberately paused while it is active",
					append(logFields, "error", werr, "platform_error", safeErrSummary(terr))...)
				return nil, &briefs.ConnServiceUnavailableError{Code: "503", Message: "the ad set's pause was not applied on Meta, but the pause recorded on the campaign could not be withdrawn: it may be shown as deliberately paused while it is active — resume it here to clear the record"}
			}
		}
		return nil, s.classifyMetaAdSetError(ctx, "toggle", p.ProjectID, p.BriefID, p.CampaignID, existing.Platform, terr)
	}
	switch res.Outcome {
	case model.MetaAdSetApplied, model.MetaAdSetAlreadyInState:
	default:
		// An outcome the adapter did not name is not evidence of success.
		slog.ErrorContext(ctx, "meta ad set toggle returned an unrecognised outcome; reporting it as UNCONFIRMED", logFields...)
		return nil, s.answerUnconfirmedAdSetWrite(ctx, campaignRepo, row, p.AdSetID, curVersion, &lockToken, recordUnknown, &releaseNow, logFields)
	}
	applied := res.Outcome == model.MetaAdSetApplied
	switch {
	case recordUnknown:
		// The pause reached Meta (or was already there) but whether the record landed is
		// unknown: settle it from a fresh read — the ad set is paused, so either answer is safe.
		updated, perr := s.settleMetaAdSetRecord(ctx, campaignRepo, row, p.AdSetID, curVersion, &lockToken)
		if perr != nil {
			slog.ErrorContext(ctx, "meta ad set is paused on the platform but its deliberate-pause record could not be settled",
				append(logFields, "outcome", res.Outcome, "error", perr)...)
			return nil, adSetRecordErr(perr, applied, "paused")
		}
		row, curVersion = updated, updated.Version
	case setRecord && !intentRecorded:
		// ALREADY_IN_STATE: nothing was sent, and the pause is recorded now. (APPLIED without the
		// hook having run would be an adapter defect; recording is still the safe answer.)
		pctx, cancel := persistCtxFor(ctx)
		updated, perr := s.writeMetaAdSetRecord(pctx, campaignRepo, row, p.AdSetID, curVersion, &lockToken)
		cancel()
		if perr != nil {
			slog.ErrorContext(ctx, "meta ad set is paused on the platform but recording the deliberate pause failed",
				append(logFields, "outcome", res.Outcome, "error", perr)...)
			return nil, adSetRecordErr(perr, applied, "paused")
		}
		row, curVersion = updated, updated.Version
	case clearRecord:
		// A CONFIRMED resume clears the record. ReplaceCampaign's compare-and-swap at the claimed
		// version is itself the proof that no other writer changed the row meanwhile.
		pctx, cancel := persistCtxFor(ctx)
		updated, perr := s.writeMetaAdSetRecord(pctx, campaignRepo, row, "", curVersion, &lockToken)
		cancel()
		if perr != nil {
			slog.ErrorContext(ctx, "meta ad set is active on the platform but clearing its deliberate-pause record failed",
				append(logFields, "outcome", res.Outcome, "error", perr)...)
			return nil, adSetRecordErr(perr, applied, "resumed")
		}
		row, curVersion = updated, updated.Version
	case applied:
		// The platform changed and the row was not written after it; prove the row did not
		// change under the claim before handing the caller an ETag, exactly as the
		// degraded-pause path does.
		if _, verr := campaignRepo.VerifyClaimedVersion(ctx, p.ProjectID, p.BriefID, p.CampaignID, curVersion, lockToken); verr != nil {
			slog.ErrorContext(ctx, "meta ad set status changed on the platform but the campaign row could not be verified at the claimed version",
				append(logFields, "error", verr)...)
			if errors.Is(verr, domain.ErrPreconditionFailed) || errors.Is(verr, domain.ErrNotFound) {
				return nil, &briefs.ConflictError{Code: "409", Message: "the ad set's status was changed on Meta, but this campaign was modified or deleted by another request meanwhile; read the ad sets before retrying"}
			}
			return nil, &briefs.ConnServiceUnavailableError{Code: "503", Message: "the ad set's status was changed on Meta, but verifying the campaign row failed; read the ad sets before retrying"}
		}
	}
	return &briefs.MetaAdSetStatusChange{
		CampaignID:      p.CampaignID,
		AdSetID:         p.AdSetID,
		RequestedStatus: p.Status,
		Outcome:         res.Outcome,
		PreviousStatus:  optionalString(res.PreviousStatus),
		Etag:            optStr(briefETag(curVersion)),
	}, nil
}

// persistCtxFor detaches a post-outcome row write from the request's cancellation, bounded by
// persistResultTimeout — the campaign toggle's rule: once Meta has answered, the row must catch up
// even if the client has gone.
func persistCtxFor(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), persistResultTimeout)
}

// persistMetaAdSetRecord writes row with its deliberate-pause record set to adSetID ("" clears
// it), gated on expectedVersion, on the claim holder's connection, co-indexed like every other
// campaign write. It returns the row as written (its version bumped).
func (s *BriefService) persistMetaAdSetRecord(ctx context.Context, repo domain.CampaignRepository, row *model.Campaign, adSetID string, expectedVersion int64, lockToken domain.CampaignLockToken) (*model.Campaign, error) {
	if lockToken.CampaignID == "" {
		// Never write without holding the campaign's claim: the zero token makes ReplaceCampaign
		// use a pooled connection with no lock, which would bypass another writer's claim.
		return nil, fmt.Errorf("%w: refusing to write the record without holding the claim", errMetaAdSetClaimLost)
	}
	result, err := model.WithMetaUserPausedAdSet(row.Result, adSetID)
	if err != nil {
		return nil, err
	}
	cp := *row
	cp.Result = result
	cp.UpdatedBy = attributedActor(ctx, "toggle meta ad set status")
	return repo.ReplaceCampaign(ctx, &cp, expectedVersion, lockToken, s.campaignIndexPayload(indexer.ActionUpdated))
}

// errMetaAdSetRecordUnknown: a deliberate-pause record write whose commit outcome could not be
// reconciled — it may or may not be on the row.
var errMetaAdSetRecordUnknown = errors.New("whether the ad set's deliberate-pause record was written is unknown")

// writeMetaAdSetRecord is persistMetaAdSetRecord with an ambiguous commit RECONCILED. A commit
// error (domain.ErrWriteOutcomeUnknown) does not prove a rollback, so it is settled under a
// detached deadline (persistResultTimeout per step) — and ON THE CLAIMED CONNECTION, never a
// second pooled one beside it: the claim pins its connection for the whole request, so a fresh
// connection would starve behind it on a saturated pool (certain with pool_max_conns=1), the
// trap ReplaceCampaign's own connection reuse exists to avoid.
//
//  1. The same version-guarded write is repeated on the claimed connection. That attempt is the
//     usability check: it succeeds (the first attempt rolled back; the record is now written),
//     or answers ErrPreconditionFailed — a guarded UPDATE waits on the row lock of a transaction
//     still resolving, so this is the first attempt having LANDED, accepted only when the row
//     read on the claimed connection at exactly expectedVersion+1 carries the intended record.
//  2. Any other failure means the claimed connection is unusable. The claim is then handed back
//     (ReleaseCampaignLock, which destroys a broken connection and frees its pool slot) and
//     exclusive ownership is TAKEN AGAIN the way every writer takes it (ClaimCampaignVersion,
//     a try-lock, so it never waits): at expectedVersion the first attempt rolled back and the
//     write is made on the new claim; at expectedVersion+1 it landed if that row carries the
//     record. *lockToken is replaced by the new claim (or zeroed if none could be taken — and then
//     every record write and settlement read refuses to run, errMetaAdSetClaimLost), so the
//     caller's deferred release, cooldown and later writes follow it.
//
// Anything else is errMetaAdSetRecordUnknown. The repeat is a database write, never the Meta
// mutation, so "send at most once" is untouched. A definite error (without the sentinel) is
// returned unchanged.
func (s *BriefService) writeMetaAdSetRecord(ctx context.Context, repo domain.CampaignRepository, row *model.Campaign, adSetID string, expectedVersion int64, lockToken *domain.CampaignLockToken) (*model.Campaign, error) {
	updated, err := s.persistMetaAdSetRecord(ctx, repo, row, adSetID, expectedVersion, *lockToken)
	if err == nil || !errors.Is(err, domain.ErrWriteOutcomeUnknown) {
		return updated, err
	}
	slog.WarnContext(ctx, "meta ad set deliberate-pause record commit is ambiguous; reconciling on the claimed connection",
		"campaign_id", row.ID, "expected_version", expectedVersion, "error", err)
	rctx, cancel := persistCtxFor(ctx)
	defer cancel()
	again, rerr := s.persistMetaAdSetRecord(rctx, repo, row, adSetID, expectedVersion, *lockToken)
	switch {
	case rerr == nil:
		return again, nil
	case errors.Is(rerr, domain.ErrPreconditionFailed):
		return landedMetaAdSetRecord(rctx, repo, row, adSetID, expectedVersion+1, *lockToken)
	case errors.Is(rerr, domain.ErrNotFound):
		return nil, fmt.Errorf("%w: the campaign row is gone", errMetaAdSetRecordUnknown)
	}
	slog.WarnContext(ctx, "meta ad set record reconciliation: the claimed connection is unusable; re-taking the claim",
		"campaign_id", row.ID, "error", rerr)
	cctx, ccancel := persistCtxFor(ctx)
	defer ccancel()
	return s.reclaimAndWriteMetaAdSetRecord(cctx, repo, row, adSetID, expectedVersion, lockToken)
}

// landedMetaAdSetRecord reads the row at version on the claim's connection and accepts it only
// if it carries the intended record.
func landedMetaAdSetRecord(ctx context.Context, repo domain.CampaignRepository, row *model.Campaign, adSetID string, version int64, lockToken domain.CampaignLockToken) (*model.Campaign, error) {
	cur, err := repo.VerifyClaimedVersion(ctx, row.ProjectID, row.BriefID, row.ID, version, lockToken)
	if err == nil && model.MetaUserPausedAdSetID(cur.Result) == adSetID {
		return cur, nil
	}
	return nil, fmt.Errorf("%w: the row at version %d does not carry the intended record (read error: %v)", errMetaAdSetRecordUnknown, version, err)
}

// errMetaAdSetClaimLost: the claim was given up to reconcile an unusable connection and could
// not be re-taken (another writer took it in the gap, or the row moved past both versions). The
// record's outcome is unknown AND this request no longer holds the campaign exclusively, so
// nothing more may be written or sent: the request fails closed.
var errMetaAdSetClaimLost = errors.New("the campaign's write claim was lost while reconciling the ad set's pause record")

// reclaimAndWriteMetaAdSetRecord is step 2 of writeMetaAdSetRecord: release the unusable claim,
// take it again, and settle the record on the new claim. Nothing continues without a SUCCESSFUL
// re-claim — another writer may take the claim in the gap (a campaign resume could then act on a
// row without the record), so a failed re-claim is errMetaAdSetClaimLost, never "unknown, carry
// on".
//
// Once re-claimed, the answer is definite wherever it can be:
//   - re-claimed at expectedVersion: the first attempt did NOT land. The write is made on the
//     new claim; only ITS OWN ambiguous commit is "unknown" — any other failure is returned
//     unchanged, a definite "not recorded";
//   - re-claimed at expectedVersion+1: the first attempt landed if that row carries the record;
//     if it does not, another writer moved the row and the first attempt did not land (had it
//     landed, that writer would have moved it to +2) — a definite ErrPreconditionFailed.
func (s *BriefService) reclaimAndWriteMetaAdSetRecord(ctx context.Context, repo domain.CampaignRepository, row *model.Campaign, adSetID string, expectedVersion int64, lockToken *domain.CampaignLockToken) (*model.Campaign, error) {
	_ = repo.ReleaseCampaignLock(ctx, *lockToken)
	*lockToken = domain.CampaignLockToken{}
	cur, tok, err := repo.ClaimCampaignVersion(ctx, row.ProjectID, row.BriefID, row.ID, expectedVersion)
	if err == nil {
		*lockToken = tok
		updated, werr := s.persistMetaAdSetRecord(ctx, repo, cur, adSetID, expectedVersion, tok)
		if werr != nil && errors.Is(werr, domain.ErrWriteOutcomeUnknown) {
			return nil, fmt.Errorf("%w: write on the re-taken claim: %v", errMetaAdSetRecordUnknown, werr)
		}
		return updated, werr
	}
	if errors.Is(err, domain.ErrPreconditionFailed) {
		cur, tok, err = repo.ClaimCampaignVersion(ctx, row.ProjectID, row.BriefID, row.ID, expectedVersion+1)
		if err == nil {
			*lockToken = tok
			if model.MetaUserPausedAdSetID(cur.Result) == adSetID {
				return cur, nil
			}
			return nil, fmt.Errorf("the row moved to version %d without the record, so it was not recorded: %w", expectedVersion+1, domain.ErrPreconditionFailed)
		}
	}
	return nil, fmt.Errorf("%w: %v", errMetaAdSetClaimLost, err)
}

// locateMetaAdSetRow finds the row after a record write whose outcome is unknown: at base (it
// did not land) or base+1 (it did, or another write did), read on the claim's connection.
func locateMetaAdSetRow(ctx context.Context, repo domain.CampaignRepository, row *model.Campaign, base int64, lockToken domain.CampaignLockToken) (*model.Campaign, error) {
	if lockToken.CampaignID == "" {
		// Settlement acts only under this request's claim; without it another writer may own the
		// row, so nothing is read or written on its behalf.
		return nil, fmt.Errorf("%w: refusing to settle the record without holding the claim", errMetaAdSetClaimLost)
	}
	var lastErr error
	for _, v := range []int64{base, base + 1} {
		cur, err := repo.VerifyClaimedVersion(ctx, row.ProjectID, row.BriefID, row.ID, v, lockToken)
		if err == nil {
			return cur, nil
		}
		if !errors.Is(err, domain.ErrPreconditionFailed) {
			return nil, fmt.Errorf("%w: re-read failed: %v", errMetaAdSetRecordUnknown, err)
		}
		lastErr = err
	}
	return nil, fmt.Errorf("%w: the row is at neither version %d nor %d: %v", errMetaAdSetRecordUnknown, base, base+1, lastErr)
}

// settleMetaAdSetRecord makes the pause of adSetID be on record when an earlier write of it (from
// version base) could not be reconciled: a row that already shows it is accepted; otherwise it is
// written at the row's version (an ambiguous commit is reconciled as above).
func (s *BriefService) settleMetaAdSetRecord(ctx context.Context, repo domain.CampaignRepository, row *model.Campaign, adSetID string, base int64, lockToken *domain.CampaignLockToken) (*model.Campaign, error) {
	pctx, cancel := persistCtxFor(ctx)
	defer cancel()
	cur, err := locateMetaAdSetRow(pctx, repo, row, base, *lockToken)
	if err != nil {
		return nil, err
	}
	if model.MetaUserPausedAdSetID(cur.Result) == adSetID {
		return cur, nil
	}
	return s.writeMetaAdSetRecord(pctx, repo, cur, adSetID, cur.Version, lockToken)
}

// withdrawMetaAdSetRecord removes a pause record before a write that then definitely changed
// nothing. With fromRead (the record's write was never reconciled; version is the one before it)
// the row is located first: no record means nothing to withdraw. A nil error means the record is
// established to be absent.
func (s *BriefService) withdrawMetaAdSetRecord(ctx context.Context, repo domain.CampaignRepository, row *model.Campaign, version int64, lockToken *domain.CampaignLockToken, fromRead bool, adSetID string) error {
	pctx, cancel := persistCtxFor(ctx)
	defer cancel()
	if fromRead {
		cur, err := locateMetaAdSetRow(pctx, repo, row, version, *lockToken)
		if err != nil {
			return err
		}
		if model.MetaUserPausedAdSetID(cur.Result) != adSetID {
			return nil
		}
		row, version = cur, cur.Version
	}
	_, err := s.writeMetaAdSetRecord(pctx, repo, row, "", version, lockToken)
	return err
}

// adSetRecordErr answers a failed record write made AFTER Meta's definite outcome. applied says
// whether Meta's status changed in this call; verb is "paused" or "resumed".
func adSetRecordErr(err error, applied bool, verb string) error {
	what := "the ad set is already " + verb + " on Meta (nothing was sent)"
	if applied {
		what = "the ad set WAS " + verb + " on Meta"
	}
	consequence := "a campaign resume would re-activate it; pause it here again"
	if verb == "resumed" {
		consequence = "until it is, a campaign resume leaves this ad set paused; resume it here again"
	}
	if errors.Is(err, domain.ErrPreconditionFailed) || errors.Is(err, domain.ErrNotFound) {
		return &briefs.ConflictError{Code: "409", Message: what + ", but this campaign was modified or deleted by another request meanwhile, so the change was not recorded: " + consequence + " after reading the campaign"}
	}
	if errors.Is(err, errMetaAdSetClaimLost) {
		return errAdSetClaimLost()
	}
	if errors.Is(err, errMetaAdSetRecordUnknown) {
		return &briefs.ConnServiceUnavailableError{Code: "503", Message: what + ", but whether that was recorded on the campaign could not be confirmed: read the ad sets (deliberately_paused); if it is not shown, " + consequence}
	}
	return &briefs.ConnServiceUnavailableError{Code: "503", Message: what + ", but recording it on the campaign failed: " + consequence}
}

// answerUnconfirmedAdSetWrite answers a Meta write whose outcome is unknown: the fixed 503, with
// the claim held for the cooldown. When the pause's record could not be confirmed before the
// write (recordUnknown), it is SETTLED first: an unconfirmed pause counts as paused, so the
// record must be on the row — otherwise a later campaign resume, finding none, would re-activate
// an ad set Meta may well have paused. The settlement runs before the cooldown starts, so the
// cooldown holds whichever claim it leaves (it may have re-taken it). If the record cannot be
// confirmed either, the 503 says so.
func (s *BriefService) answerUnconfirmedAdSetWrite(ctx context.Context, repo domain.CampaignRepository, row *model.Campaign, adSetID string, base int64, lockToken *domain.CampaignLockToken, recordUnknown bool, releaseNow *bool, logFields []any) error {
	var settleErr error
	if recordUnknown {
		if _, settleErr = s.settleMetaAdSetRecord(ctx, repo, row, adSetID, base, lockToken); settleErr != nil {
			slog.ErrorContext(ctx, "meta ad set pause is UNCONFIRMED and its deliberate-pause record could not be settled; a campaign resume may re-activate it",
				append(logFields, "error", settleErr)...)
		}
	}
	*releaseNow = false
	repo.ReleaseCampaignLockAfterCooldown(*lockToken, unconfirmedLockCooldown)
	if settleErr != nil {
		return &briefs.ConnServiceUnavailableError{Code: "503", Message: "the ad set status change is unconfirmed — it may or may not have been applied on Meta — and whether its pause was recorded on the campaign could not be confirmed either: read the ad sets before retrying; if it is not shown as deliberately_paused, pause it here again before resuming the campaign"}
	}
	return errAdSetUnconfirmed()
}

// errAdSetClaimLost answers a request that lost the campaign's claim while reconciling the pause
// record: nothing further was sent to Meta by it, but the record may or may not be on the row.
func errAdSetClaimLost() error {
	return &briefs.ConnServiceUnavailableError{Code: "503", Message: "the campaign's write lock was lost while recording the ad set's pause, so nothing further was sent to Meta; whether the pause is recorded on the campaign, and the ad set's status, are unconfirmed — read the ad sets before retrying (one shown deliberately_paused while it is active is cleared by resuming it here)"}
}

// errAdSetUnconfirmed is the fixed answer for a write whose outcome is unknown.
func errAdSetUnconfirmed() error {
	return &briefs.ConnServiceUnavailableError{Code: "503", Message: "the ad set status change is unconfirmed — it may or may not have been applied on Meta; read the ad sets before retrying"}
}

// classifyMetaAdSetError maps an orchestrator failure onto this service's error set. verb is
// "read" or "toggle". Upstream text is logged (bounded), never returned: every client message is a
// fixed sentence.
//
// Two arms answer 404, as on every sibling per-campaign lever: a missing campaign ROW (decided
// before the orchestrator is called) and a project with no Meta connection at all — matched on
// domain.ErrConnectionAbsent, the sentinel only the connection lookup's absence carries, never on a
// bare ErrNotFound some other layer might wrap. Nothing Meta says (Graph 100/33 included) is ever
// a 404.
func (s *BriefService) classifyMetaAdSetError(ctx context.Context, verb, projectID, briefID, campaignID string, platform model.Provider, aerr error) error {
	logFields := []any{"project_id", projectID, "brief_id", briefID, "campaign_id", campaignID, "platform", platform, "op", verb + "_meta_ad_sets"}
	failed := "the campaign's ad sets could not be read from Meta"
	nothing := ""
	if verb == "toggle" {
		failed = "the ad set's status could not be changed; nothing was changed on Meta"
		nothing = " — nothing was changed"
	}
	switch {
	case errors.Is(aerr, domain.ErrMetaAdSetsUnsupported):
		return &briefs.BadRequestError{Code: "400", Message: "ad-set operations are supported for Meta campaigns only"}
	case errors.Is(aerr, domain.ErrMetaAdSetInvalid):
		return &briefs.BadRequestError{Code: "400", Message: "ad_set_id must be a Meta ad set id: 1-32 digits without a leading zero"}
	case errors.Is(aerr, domain.ErrMetricsWindowUnsupported):
		return &briefs.BadRequestError{Code: "400", Message: "this window is not supported for Meta ad sets"}
	case errors.Is(aerr, ErrCampaignNotProvisioned):
		if verb == "toggle" {
			slog.WarnContext(ctx, "meta ad set activation refused: the campaign records no ad set of its own",
				append(logFields, "error", safeErrSummary(aerr))...)
			return &briefs.ConflictError{Code: "409", Message: "this campaign's ad sets cannot be activated here: the campaign records no ad set of its own — it was ADOPTED (adoption does not verify targeting) or never fully provisioned; activate it in Meta Ads Manager. Pausing is always allowed" + nothing}
		}
		return &briefs.ConflictError{Code: "409", Message: "campaign is not fully provisioned — it has no platform campaign id yet, so it has no ad sets to read"}
	case errors.Is(aerr, domain.ErrMetaAdSetNotInCampaign):
		slog.WarnContext(ctx, "meta ad set refused: it is not this campaign's under the connected ad account",
			append(logFields, "error", safeErrSummary(aerr))...)
		return &briefs.ConflictError{Code: "409", Message: "the ad set does not belong to this campaign under the connected ad account" + nothing}
	case errors.Is(aerr, domain.ErrMetaAdSetNotRecorded):
		return &briefs.ConflictError{Code: "409", Message: "only the ad set this service created for the campaign can be activated here — this one's targeting was never verified by this service; activate it in Meta Ads Manager. Pausing any of the campaign's ad sets is allowed" + nothing}
	case errors.Is(aerr, domain.ErrStoredPlatformIDInvalid):
		slog.WarnContext(ctx, "meta ad sets blocked: the campaign row's stored platform id is not a valid Meta id",
			append(logFields, "error", safeErrSummary(aerr))...)
		return &briefs.ConflictError{Code: "409", Message: "the campaign's stored Meta id is not valid, so its ad sets cannot be addressed" + nothing}
	case errors.Is(aerr, domain.ErrMetaAdSetUnwritable):
		return &briefs.ConflictError{Code: "409", Message: "the ad set is deleted or archived on Meta, so its status cannot be changed" + nothing}
	case errors.Is(aerr, domain.ErrCampaignProvenanceUnknown):
		slog.WarnContext(ctx, "meta ad sets blocked: campaign does not record which ad account it was created under",
			append(logFields, "error", safeErrSummary(aerr))...)
		return &briefs.ConflictError{Code: "409", Message: "this campaign does not record which ad account it was created under, so its ad sets cannot be addressed safely — it must be re-dispatched first" + nothing}
	case errors.Is(aerr, domain.ErrCampaignUpstreamIdentityMismatch):
		slog.WarnContext(ctx, "meta ad sets blocked: Meta's record of the campaign no longer matches the recorded identity",
			append(logFields, "error", safeErrSummary(aerr))...)
		return &briefs.ConflictError{Code: "409", Message: "Meta reports this campaign's ad sets under a different ad account than the one this service recorded, so they cannot be addressed safely — re-dispatch the campaign" + nothing}
	case errors.Is(aerr, ErrCampaignAccountMismatch):
		slog.WarnContext(ctx, "meta ad sets blocked: campaign belongs to a different ad account than the current connection",
			append(logFields, "error", safeErrSummary(aerr))...)
		return &briefs.ConflictError{Code: "409", Message: "the campaign belongs to a different ad account than this project's current connection — reconnect the original account first" + nothing}
	case errors.Is(aerr, domain.ErrSystemConnectionNotUsable):
		slog.ErrorContext(ctx, "the LF system connection is not usable; meta ad-set operations are failing for every project without its own connection",
			"project_id", projectID, "platform", platform, "reason", unusableConnectionReason(aerr))
		return &briefs.InternalServerError{Code: "500", Message: failed}
	case errors.Is(aerr, domain.ErrSystemConnectionMissing):
		slog.ErrorContext(ctx, "the LF system connection is not installed; meta ad-set operations are failing while force-system mode is on",
			"project_id", projectID, "platform", platform)
		return &briefs.InternalServerError{Code: "500", Message: failed}
	case errors.Is(aerr, domain.ErrServiceDefect):
		slog.ErrorContext(ctx, "meta ad-set operation blocked by a defect in this service",
			"project_id", projectID, "platform", platform, "reason", unusableConnectionReason(aerr))
		return &briefs.InternalServerError{Code: "500", Message: failed}
	case errors.Is(aerr, domain.ErrCredentialDecryptionFailed):
		slog.ErrorContext(ctx, "stored credentials failed authenticated decryption; meta ad-set operation cannot proceed",
			"project_id", credentialOwnerProject(aerr, projectID), "requested_by_project_id", projectID, "platform", platform)
		return &briefs.InternalServerError{Code: "500", Message: failed}
	case errors.Is(aerr, domain.ErrAccountNotSelected):
		slog.WarnContext(ctx, "meta ad sets blocked: no ad account selected on the project's connection",
			"project_id", projectID, "platform", platform, "reason", unusableConnectionReason(aerr))
		return &briefs.ConflictError{Code: "409", Message: "this project's Meta connection has no ad account selected — save an ad account id on the connection first" + nothing}
	case errors.Is(aerr, domain.ErrConnectionAbsent):
		slog.WarnContext(ctx, "meta ad sets blocked: no connection configured for this project and provider",
			"project_id", projectID, "platform", platform)
		return &briefs.NotFoundError{Code: "404", Message: "this project has no connection for the campaign's ad platform; connect Meta before reading or changing its ad sets"}
	case errors.Is(aerr, domain.ErrConnectionNotUsable):
		slog.WarnContext(ctx, "connection is not usable for meta ad sets",
			"project_id", projectID, "platform", platform, "reason", unusableConnectionReason(aerr))
		return &briefs.ConflictError{Code: "409", Message: "this project's Meta connection is not usable — check that it is active, that its stored credential is complete, and that an ad account is selected" + nothing}
	default:
		slog.WarnContext(ctx, "meta ad-set operation failed upstream", append(logFields, "error", safeErrSummary(aerr))...)
		return &briefs.ConnServiceUnavailableError{Code: "503", Message: failed}
	}
}
