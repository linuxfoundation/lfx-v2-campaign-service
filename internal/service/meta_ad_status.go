// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// MetaAdStatusToggler is an OPTIONAL dispatcher capability (#550, gap 14, Meta only): pause or
// resume ONE ad of one ad set of a campaign. Its contract is MetaAdSetStatusToggler's: every guard
// (provenance, the ad's ownership by ad_set_id and the campaign, the adopted and unrecorded
// ACTIVATE refusals) runs BEFORE the write, the write is sent at most once, a nil error carries
// APPLIED or ALREADY_IN_STATE, an unknown outcome is an error implementing Unconfirmed() bool, and
// beforeWrite runs at most once, right before the write, never on ALREADY_IN_STATE.
type MetaAdStatusToggler interface {
	ToggleMetaAdStatus(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, adSetID, adID, status string, beforeWrite MetaAdSetBeforeWrite) (*model.MetaAdStatusResult, error)
}

// SupportsMetaAdToggle reports whether platform's dispatcher implements MetaAdStatusToggler.
func (o *Orchestrator) SupportsMetaAdToggle(platform model.Provider) bool {
	d, ok := o.dispatchers[platform]
	if !ok {
		return false
	}
	_, ok = d.(MetaAdStatusToggler)
	return ok
}

// ToggleMetaAdStatus pauses or resumes one ad through the dispatcher's MetaAdStatusToggler, under
// the campaign toggle's total deadline.
func (o *Orchestrator) ToggleMetaAdStatus(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, adSetID, adID, status string, beforeWrite MetaAdSetBeforeWrite) (*model.MetaAdStatusResult, error) {
	d, ok := o.dispatchers[platform]
	if !ok {
		return nil, fmt.Errorf("%w: no dispatcher registered for platform %s", domain.ErrMetaAdSetsUnsupported, platform)
	}
	toggler, ok := d.(MetaAdStatusToggler)
	if !ok {
		return nil, fmt.Errorf("%w: %s", domain.ErrMetaAdSetsUnsupported, platform)
	}
	if campaign == nil || strings.TrimSpace(campaign.PlatformCampaignID) == "" {
		return nil, ErrCampaignNotProvisioned
	}
	callCtx, cancel := context.WithTimeout(ctx, toggleCallTimeout)
	defer cancel()
	start := time.Now()
	res, terr := toggler.ToggleMetaAdStatus(callCtx, projectID, platform, campaign, adSetID, adID, status, beforeWrite)
	o.recordUpstream(ctx, platform, opToggleMetaAdStatus, start, terr)
	if terr != nil {
		return nil, terr
	}
	if res == nil {
		return nil, fmt.Errorf("%s ad toggler returned a nil result with no error", platform)
	}
	return res, nil
}

// metaAdPauseEnabled reports whether this deployment opted in to toggle-meta-ad-status (#550).
// Read per call; only the exact value "true" enables it.
func metaAdPauseEnabled() bool {
	return os.Getenv(constants.EnvMetaAdPauseEnabled) == "true"
}

// ToggleMetaAdStatus pauses or resumes ONE ad of one ad set of a Meta campaign (#550, gap 14).
//
// It is ToggleMetaAdSetStatus with the ad in place of the ad set: the same If-Match, claim,
// cooldown and record contract, and the same record machinery (writeMetaAdSetRecord and its
// helpers, driven by adPauseRecord). The per-ad record lists ads of the campaign's RECORDED ad set
// (the only ones the campaign ACTIVATE cascade touches), so an ad of any other ad set, and
// anything on an ADOPTED row, records nothing. The record is
//   - SET (the ad added) before the pause is sent, or after ALREADY_IN_STATE;
//   - WITHDRAWN only when that write definitely changed nothing;
//   - CLEARED (the ad removed) only by a CONFIRMED resume here.
func (s *BriefService) ToggleMetaAdStatus(ctx context.Context, p *briefs.ToggleMetaAdStatusPayload) (*briefs.MetaAdStatusChange, error) {
	if !metaAdPauseEnabled() {
		return nil, &briefs.BadRequestError{Code: "400", Message: fmt.Sprintf("per-ad status changes are disabled on this deployment (%s is not \"true\"); pause the campaign or its ad set instead, or ask an operator to enable it", constants.EnvMetaAdPauseEnabled)}
	}
	_, campaignRepo, _, orch, err := s.ready()
	if err != nil {
		return nil, err
	}
	version, err := parseBriefIfMatch(p.IfMatch)
	if err != nil {
		return nil, err
	}
	if p.Status != model.MetaAdSetStatusActive && p.Status != model.MetaAdSetStatusPaused {
		return nil, &briefs.BadRequestError{Code: "400", Message: "status must be 'ACTIVE' or 'PAUSED'"}
	}
	if !metaAdSetIDOK(p.AdSetID) {
		return nil, &briefs.BadRequestError{Code: "400", Message: "ad_set_id must be a Meta ad set id: 1-32 digits without a leading zero"}
	}
	if !metaAdSetIDOK(p.AdID) {
		return nil, &briefs.BadRequestError{Code: "400", Message: "ad_id must be a Meta ad id: 1-32 digits without a leading zero"}
	}
	existing, gerr := campaignRepo.GetCampaign(ctx, p.ProjectID, p.BriefID, p.CampaignID)
	if gerr != nil {
		return nil, mapBriefErr(gerr)
	}
	if existing.Version != version {
		return nil, &briefs.PreconditionFailedError{Code: "412", Message: "the supplied ETag does not match the current version"}
	}
	if !orch.SupportsMetaAdToggle(existing.Platform) {
		return nil, &briefs.BadRequestError{Code: "400", Message: "ad status changes are supported for Meta campaigns only"}
	}
	pauseDegraded := existing.Status == model.CampaignStatusCreatedDegraded && p.Status == model.MetaAdSetStatusPaused
	if !model.CampaignStatusToggleable(existing.Status) && !pauseDegraded {
		msg := "campaign is not in a toggleable state (it is still provisioning or needs reconciliation); resolve its status before changing its ads"
		if existing.Status == model.CampaignStatusCreatedDegraded {
			msg = "campaign still needs reconciliation, so its ads cannot be activated; they can be PAUSED to stop any spend"
		}
		return nil, &briefs.ConflictError{Code: "409", Message: msg}
	}
	if strings.TrimSpace(existing.PlatformCampaignID) == "" {
		return nil, &briefs.ConflictError{Code: "409", Message: "campaign is not fully provisioned — it has no platform campaign id yet, so it has no ads to change"}
	}

	recorded := model.MetaRecordedAdSetID(existing.Result)
	onRecord := model.MetaAdHeldPaused(existing.Result, p.AdID)
	tracked := recorded != "" && p.AdSetID == recorded
	setRecord := tracked && p.Status == model.MetaAdSetStatusPaused && !onRecord
	clearRecord := tracked && p.Status == model.MetaAdSetStatusActive && onRecord
	setRec, clearRec := adPauseRecord(p.AdID, true), adPauseRecord(p.AdID, false)

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

	row, curVersion := existing, version
	var intentErr error
	intentRecorded, recordUnknown := false, false
	var beforeWrite MetaAdSetBeforeWrite
	if setRecord {
		beforeWrite = func(hctx context.Context) error {
			updated, perr := s.writeMetaAdSetRecord(hctx, campaignRepo, row, setRec, curVersion, &lockToken)
			switch {
			case perr == nil:
				row, curVersion, intentRecorded = updated, updated.Version, true
				return nil
			case errors.Is(perr, errMetaAdSetClaimLost):
				intentErr = perr
				return perr
			case errors.Is(perr, errMetaAdSetRecordUnknown):
				// As on the ad-set lever: the record may have landed, so the pause is sent anyway
				// (pausing never spends) and the record is settled after Meta answers.
				slog.WarnContext(ctx, "meta ad pause record write is unconfirmed; sending the pause anyway (fail-safe)",
					"campaign_id", p.CampaignID, "ad_id", p.AdID, "error", perr)
				recordUnknown = true
				return nil
			default:
				intentErr = perr
				return perr
			}
		}
	}
	logFields := []any{"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
		"ad_set_id", p.AdSetID, "ad_id", p.AdID, "requested_status", p.Status}

	res, terr := orch.ToggleMetaAdStatus(ctx, p.ProjectID, existing.Platform, existing, p.AdSetID, p.AdID, p.Status, beforeWrite)
	if terr != nil {
		var unconfirmed interface{ Unconfirmed() bool }
		if errors.As(terr, &unconfirmed) && unconfirmed.Unconfirmed() {
			slog.WarnContext(ctx, "meta ad status change is UNCONFIRMED (the platform may or may not reflect it)",
				append(logFields, "pause_on_record", tracked && (intentRecorded || onRecord), "error", safeErrSummary(terr))...)
			return nil, s.answerUnconfirmedPauseWrite(ctx, adLeverNoun, campaignRepo, row, setRec, curVersion, &lockToken, recordUnknown, &releaseNow, logFields)
		}
		if errors.Is(intentErr, errMetaAdSetClaimLost) {
			slog.ErrorContext(ctx, "meta ad pause not sent: its record could not be reconciled and the campaign's claim was lost",
				append(logFields, "error", intentErr)...)
			return nil, adLeverNoun.errClaimLost()
		}
		if intentErr != nil {
			slog.ErrorContext(ctx, "meta ad pause not sent: recording the deliberate pause on the campaign row failed",
				append(logFields, "error", intentErr)...)
			if errors.Is(intentErr, domain.ErrPreconditionFailed) || errors.Is(intentErr, domain.ErrNotFound) {
				return nil, &briefs.ConflictError{Code: "409", Message: "this campaign was modified or deleted by another request, so the ad's pause was not sent — nothing was changed on Meta; read the campaign before retrying"}
			}
			return nil, &briefs.ConnServiceUnavailableError{Code: "503", Message: "the ad's pause could not be recorded on the campaign, so it was not sent — nothing was changed on Meta"}
		}
		if intentRecorded || recordUnknown {
			if werr := s.withdrawMetaAdSetRecord(ctx, campaignRepo, row, curVersion, &lockToken, recordUnknown, setRec, clearRec); werr != nil {
				slog.ErrorContext(ctx, "meta ad pause was not applied, and withdrawing its deliberate-pause record failed; the campaign may report the ad deliberately paused while it is active",
					append(logFields, "error", werr, "platform_error", safeErrSummary(terr))...)
				return nil, &briefs.ConnServiceUnavailableError{Code: "503", Message: "the ad's pause was not applied on Meta, but the pause recorded on the campaign could not be withdrawn: it may be shown as deliberately paused while it is active — resume it here to clear the record"}
			}
		}
		return nil, s.classifyMetaAdSetError(ctx, "toggle_ad", p.ProjectID, p.BriefID, p.CampaignID, existing.Platform, terr)
	}
	switch res.Outcome {
	case model.MetaAdSetApplied, model.MetaAdSetAlreadyInState:
	default:
		slog.ErrorContext(ctx, "meta ad toggle returned an unrecognised outcome; reporting it as UNCONFIRMED", logFields...)
		return nil, s.answerUnconfirmedPauseWrite(ctx, adLeverNoun, campaignRepo, row, setRec, curVersion, &lockToken, recordUnknown, &releaseNow, logFields)
	}
	applied := res.Outcome == model.MetaAdSetApplied
	switch {
	case recordUnknown:
		updated, perr := s.settleMetaAdSetRecord(ctx, campaignRepo, row, setRec, curVersion, &lockToken)
		if perr != nil {
			slog.ErrorContext(ctx, "meta ad is paused on the platform but its deliberate-pause record could not be settled",
				append(logFields, "outcome", res.Outcome, "error", perr)...)
			return nil, pauseRecordErr(adLeverNoun, perr, applied, "paused")
		}
		row, curVersion = updated, updated.Version
	case setRecord && !intentRecorded:
		pctx, cancel := persistCtxFor(ctx)
		updated, perr := s.writeMetaAdSetRecord(pctx, campaignRepo, row, setRec, curVersion, &lockToken)
		cancel()
		if perr != nil {
			slog.ErrorContext(ctx, "meta ad is paused on the platform but recording the deliberate pause failed",
				append(logFields, "outcome", res.Outcome, "error", perr)...)
			return nil, pauseRecordErr(adLeverNoun, perr, applied, "paused")
		}
		row, curVersion = updated, updated.Version
	case clearRecord:
		pctx, cancel := persistCtxFor(ctx)
		updated, perr := s.writeMetaAdSetRecord(pctx, campaignRepo, row, clearRec, curVersion, &lockToken)
		cancel()
		if perr != nil {
			slog.ErrorContext(ctx, "meta ad is active on the platform but clearing its deliberate-pause record failed",
				append(logFields, "outcome", res.Outcome, "error", perr)...)
			return nil, pauseRecordErr(adLeverNoun, perr, applied, "resumed")
		}
		row, curVersion = updated, updated.Version
	case applied:
		if _, verr := campaignRepo.VerifyClaimedVersion(ctx, p.ProjectID, p.BriefID, p.CampaignID, curVersion, lockToken); verr != nil {
			slog.ErrorContext(ctx, "meta ad status changed on the platform but the campaign row could not be verified at the claimed version",
				append(logFields, "error", verr)...)
			if errors.Is(verr, domain.ErrPreconditionFailed) || errors.Is(verr, domain.ErrNotFound) {
				return nil, &briefs.ConflictError{Code: "409", Message: "the ad's status was changed on Meta, but this campaign was modified or deleted by another request meanwhile; read the ads before retrying"}
			}
			return nil, &briefs.ConnServiceUnavailableError{Code: "503", Message: "the ad's status was changed on Meta, but verifying the campaign row failed; read the ads before retrying"}
		}
	}
	return &briefs.MetaAdStatusChange{
		CampaignID:      p.CampaignID,
		AdSetID:         p.AdSetID,
		AdID:            p.AdID,
		RequestedStatus: p.Status,
		Outcome:         res.Outcome,
		PreviousStatus:  optionalString(res.PreviousStatus),
		Etag:            optStr(briefETag(curVersion)),
	}, nil
}
