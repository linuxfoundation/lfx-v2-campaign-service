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
type MetaAdSetStatusToggler interface {
	ToggleMetaAdSetStatus(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, adSetID, status string) (*model.MetaAdSetStatusResult, error)
}

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
func (o *Orchestrator) ToggleMetaAdSetStatus(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, adSetID, status string) (*model.MetaAdSetStatusResult, error) {
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
	res, terr := toggler.ToggleMetaAdSetStatus(callCtx, projectID, platform, campaign, adSetID, status)
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
			ID:              a.ID,
			Listed:          a.Listed,
			Name:            a.Name,
			Status:          a.Status,
			EffectiveStatus: a.EffectiveStatus,
			BidStrategy:     a.BidStrategy,
			BudgetType:      budgetType,
			BudgetAmount:    a.BudgetAmount,
			Impressions:     a.Impressions,
			Clicks:          a.Clicks,
			CostMicros:      a.CostMicros,
			Ctr:             a.Ctr,
			Recorded:        a.Recorded,
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
// for unconfirmedLockCooldown — with ONE deliberate difference: an ad set's status is not a column
// of the campaign row, so nothing is persisted and the version is NOT bumped. That is the
// pause-of-a-degraded-campaign precedent: the ETag comes back unchanged, after
// VerifyClaimedVersion has proved no other writer changed the row while the platform was being
// written.
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

	// Etag mirrors the claimed version: nothing below persists, so a 200 hands back the row's
	// unchanged version.
	out := &briefs.MetaAdSetStatusChange{
		CampaignID:      p.CampaignID,
		AdSetID:         p.AdSetID,
		RequestedStatus: p.Status,
		Etag:            optStr(briefETag(version)),
	}
	res, terr := orch.ToggleMetaAdSetStatus(ctx, p.ProjectID, existing.Platform, existing, p.AdSetID, p.Status)
	if terr != nil {
		var unconfirmed interface{ Unconfirmed() bool }
		if errors.As(terr, &unconfirmed) && unconfirmed.Unconfirmed() {
			// The single write was SENT and its outcome is unknown. Answered exactly as every
			// other money lever answers it (toggle-campaign-status, budget, keyword actions): a
			// 503 that says so — never success, never "nothing changed" — with the lock held
			// for the cooldown so a second caller cannot pile another write onto it.
			slog.WarnContext(ctx, "meta ad set status change is UNCONFIRMED (the platform may or may not reflect it)",
				"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
				"ad_set_id", p.AdSetID, "requested_status", p.Status, "error", safeErrSummary(terr))
			releaseNow = false
			campaignRepo.ReleaseCampaignLockAfterCooldown(lockToken, unconfirmedLockCooldown)
			return nil, errAdSetUnconfirmed()
		}
		return nil, s.classifyMetaAdSetError(ctx, "toggle", p.ProjectID, p.BriefID, p.CampaignID, existing.Platform, terr)
	}
	switch res.Outcome {
	case model.MetaAdSetApplied, model.MetaAdSetAlreadyInState:
	default:
		// An outcome the adapter did not name is not evidence of success.
		slog.ErrorContext(ctx, "meta ad set toggle returned an unrecognised outcome; reporting it as UNCONFIRMED",
			"project_id", p.ProjectID, "campaign_id", p.CampaignID, "ad_set_id", p.AdSetID)
		releaseNow = false
		campaignRepo.ReleaseCampaignLockAfterCooldown(lockToken, unconfirmedLockCooldown)
		return nil, errAdSetUnconfirmed()
	}
	out.Outcome = res.Outcome
	out.PreviousStatus = optionalString(res.PreviousStatus)
	if res.Outcome == model.MetaAdSetApplied {
		// The platform changed; prove the row did not change under the claim before handing the
		// caller an ETag, exactly as the degraded-pause path does.
		if _, verr := campaignRepo.VerifyClaimedVersion(ctx, p.ProjectID, p.BriefID, p.CampaignID, version, lockToken); verr != nil {
			slog.ErrorContext(ctx, "meta ad set status changed on the platform but the campaign row could not be verified at the claimed version",
				"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
				"ad_set_id", p.AdSetID, "requested_status", p.Status, "error", verr)
			if errors.Is(verr, domain.ErrPreconditionFailed) || errors.Is(verr, domain.ErrNotFound) {
				return nil, &briefs.ConflictError{Code: "409", Message: "the ad set's status was changed on Meta, but this campaign was modified or deleted by another request meanwhile; read the ad sets before retrying"}
			}
			return nil, &briefs.ConnServiceUnavailableError{Code: "503", Message: "the ad set's status was changed on Meta, but verifying the campaign row failed; read the ad sets before retrying"}
		}
	}
	return out, nil
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
