// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/indexer"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
)

// TargetingUpdateMarketingOpsInputs names the update-campaign-targeting inputs that name a
// resource on the LinkedIn ad account (#666): audienceMatchingSegments and dynamicSegments in any
// include clause or in exclude — the same facets LinkedInMarketingOpsInputs gates at create, for
// the same reason. Any non-empty list counts; every name is service-derived.
func TargetingUpdateMarketingOpsInputs(t *briefs.LinkedinTargetingUpdate) []string {
	if t == nil {
		return nil
	}
	segmentKeys := []string{linkedin.FacetAudienceMatchingSegments, linkedin.FacetDynamicSegments}
	var set []string
	for i, clause := range t.Include {
		for _, k := range segmentKeys {
			if len(clause[k]) > 0 {
				set = append(set, "targeting.include["+strconv.Itoa(i)+"]."+k)
			}
		}
	}
	for _, k := range segmentKeys {
		if len(t.Exclude[k]) > 0 {
			set = append(set, "targeting.exclude."+k)
		}
	}
	return set
}

// UpdateCampaignTargeting REPLACES a campaign's audience targeting ON THE AD PLATFORM, then
// persists the row's config snapshot as the adapter recorded it (#666). It is UpdateCampaignBid's
// twin — the same validation-before-load order, If-Match handling, claim, platform-first-then-
// persist split and error mapping — with the targeting sentinels and the global_marketing_ops
// check the segment facets need. Read UpdateCampaignBudget for the reasoning behind each step.
func (s *BriefService) UpdateCampaignTargeting(ctx context.Context, p *briefs.UpdateCampaignTargetingPayload) (*briefs.Campaign, error) {
	_, campaignRepo, _, orch, err := s.ready()
	if err != nil {
		return nil, err
	}
	version, err := parseBriefIfMatch(p.IfMatch)
	if err != nil {
		return nil, err
	}
	if p.Targeting == nil {
		return nil, &briefs.BadRequestError{Code: "400", Message: "targeting is required"}
	}

	// The ad-segment facets name resources of the ad account: ops-only, before anything is loaded.
	if fields := TargetingUpdateMarketingOpsInputs(p.Targeting); len(fields) > 0 {
		named := strings.Join(fields, ", ")
		unavailable := &briefs.ConnServiceUnavailableError{Code: "503",
			Message: "could not verify the global_marketing_ops role that these targeting inputs require (" + named + "); retry, or omit them"}
		forbidden := &briefs.ForbiddenError{Code: "403",
			Message: "targeting inputs that name a resource on the LinkedIn ad account require the global_marketing_ops role on this project: " + named}
		if err := s.requireGlobalMarketingOps(ctx, p.ProjectID, named, unavailable, forbidden); err != nil {
			return nil, err
		}
	}

	existing, gerr := campaignRepo.GetCampaign(ctx, p.ProjectID, p.BriefID, p.CampaignID)
	if gerr != nil {
		return nil, mapBriefErr(gerr)
	}
	if existing.Version != version {
		return nil, &briefs.PreconditionFailedError{Code: "412", Message: "the supplied ETag does not match the current version"}
	}
	if !model.CampaignStatusToggleable(existing.Status) && existing.Status != model.CampaignStatusCreatedDegraded {
		return nil, &briefs.ConflictError{Code: "409", Message: "campaign is not in a state where its targeting can be changed (it is still provisioning or needs reconciliation); resolve its status first"}
	}
	if existing.Platform.Kind() == model.ChannelEmail {
		return nil, &briefs.BadRequestError{Code: "400", Message: "targeting changes do not apply to the email channel"}
	}
	if existing.PlatformCampaignID == "" {
		return nil, &briefs.ConflictError{Code: "409", Message: "campaign is not fully provisioned — it has no platform campaign id yet, so there is no upstream targeting to change"}
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

	change := model.TargetingChange{Include: p.Targeting.Include, Exclude: p.Targeting.Exclude, GeoTargets: p.GeoTargets}
	// The adapter records what it sent on the row's snapshot; work on a copy so a refusal leaves
	// the loaded row untouched.
	row := *existing
	if werr := orch.WriteCampaignTargeting(ctx, p.ProjectID, existing.Platform, &row, change); werr != nil {
		return nil, targetingWriteError(ctx, p, existing, werr, campaignRepo, lockToken, &releaseNow)
	}

	row.UpdatedBy = attributedActor(ctx, "update campaign targeting")
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistResultTimeout)
	defer cancel()
	updated, uerr := campaignRepo.ReplaceCampaign(persistCtx, &row, version, lockToken, s.campaignIndexPayload(indexer.ActionUpdated))
	if uerr != nil {
		slog.ErrorContext(ctx, "campaign targeting changed on the platform but the DB row write failed (platform/DB diverged)",
			"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
			"platform", existing.Platform, "platform_campaign_id", existing.PlatformCampaignID, "error", uerr)
		return nil, mapBriefErr(uerr)
	}
	slog.InfoContext(ctx, "campaign targeting replaced on the ad platform and persisted",
		"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
		"platform", existing.Platform, "platform_campaign_id", existing.PlatformCampaignID,
		"include_clauses", len(change.Include), "geo_targets", len(change.GeoTargets))
	return campaignResult(updated), nil
}

// targetingWriteError maps a WriteCampaignTargeting failure to the API error, logging each. An
// UNCONFIRMED outcome keeps the claim for the cooldown, as the bid lever does.
func targetingWriteError(ctx context.Context, p *briefs.UpdateCampaignTargetingPayload, existing *model.Campaign, werr error, campaignRepo domain.CampaignRepository, lockToken domain.CampaignLockToken, releaseNow *bool) error {
	const failed = "the campaign targeting could not be changed"
	logArgs := []any{
		"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
		"platform", existing.Platform, "platform_campaign_id", existing.PlatformCampaignID,
	}
	var unconfirmed interface{ Unconfirmed() bool }
	var rejected interface{ TargetingRejectedReason() string }
	switch {
	case errors.Is(werr, ErrTargetingUnsupported):
		return &briefs.BadRequestError{Code: "400", Message: "targeting changes are supported for LinkedIn campaigns only"}
	case errors.Is(werr, ErrCampaignNotProvisioned):
		return &briefs.ConflictError{Code: "409", Message: "campaign is not fully provisioned — it has no platform campaign id yet, so there is no upstream targeting to change"}
	case errors.As(werr, &rejected):
		slog.InfoContext(ctx, "campaign targeting change refused: the requested targeting is not accepted",
			append(logArgs, "error", safeErrSummary(werr))...)
		return &briefs.BadRequestError{Code: "400", Message: "the requested targeting is not accepted: " + safeErrSummary(errors.New(rejected.TargetingRejectedReason()))}
	case errors.Is(werr, ErrTargetingRejected):
		return &briefs.BadRequestError{Code: "400", Message: "the requested targeting is not accepted"}
	case errors.Is(werr, domain.ErrCampaignProvenanceUnknown):
		slog.WarnContext(ctx, "campaign targeting change blocked: campaign does not record which ad account it was created under",
			append(logArgs, "error", safeErrSummary(werr))...)
		return &briefs.ConflictError{Code: "409", Message: "this campaign does not record which ad account it was created under, so its targeting cannot be changed safely — it must be re-dispatched first"}
	case errors.Is(werr, ErrCampaignAccountMismatch):
		slog.WarnContext(ctx, "campaign targeting change blocked: campaign belongs to a different ad account than the current connection",
			append(logArgs, "error", safeErrSummary(werr))...)
		return &briefs.ConflictError{Code: "409", Message: "the campaign belongs to a different ad account than this project's current connection — reconnect the original account to change its targeting"}
	case errors.Is(werr, ErrTargetingUnwritable):
		slog.WarnContext(ctx, "campaign targeting change refused: the targeting cannot be replaced here",
			append(logArgs, "error", safeErrSummary(werr))...)
		return &briefs.ConflictError{Code: "409", Message: "this campaign's targeting cannot be changed here: it was adopted, its row does not record the employer exclusions it was created with (rows created before this lever, or whose create reused an existing campaign), or LinkedIn reports it ended or omits its account or status — change it in LinkedIn Campaign Manager"}
	case errors.Is(werr, domain.ErrPlatformCampaignAbsent):
		slog.WarnContext(ctx, "campaign targeting change refused: the platform holds no such campaign", logArgs...)
		return &briefs.NotFoundError{Code: "404", Message: "the platform holds no campaign with this id — it may have been deleted upstream"}
	case errors.Is(werr, domain.ErrSystemConnectionNotUsable):
		slog.ErrorContext(ctx, "the LF system connection is not usable; campaign targeting changes are failing for every project without its own connection",
			append(logArgs, "reason", unusableConnectionReason(werr))...)
		return &briefs.InternalServerError{Code: "500", Message: failed}
	case errors.Is(werr, domain.ErrSystemConnectionMissing):
		slog.ErrorContext(ctx, "the LF system connection is not installed; campaign targeting changes are failing while force-system mode is on", logArgs...)
		return &briefs.InternalServerError{Code: "500", Message: failed}
	case errors.Is(werr, domain.ErrCredentialDecryptionFailed):
		slog.ErrorContext(ctx, "campaign targeting change blocked: stored credentials could not be decrypted (key mismatch or corrupted row)", logArgs...)
		return &briefs.InternalServerError{Code: "500", Message: failed}
	case errors.Is(werr, domain.ErrServiceDefect):
		slog.ErrorContext(ctx, "campaign targeting change blocked by a defect in this service, not in the connection",
			append(logArgs, "reason", unusableConnectionReason(werr))...)
		return &briefs.InternalServerError{Code: "500", Message: failed}
	case errors.Is(werr, domain.ErrAccountNotSelected):
		return &briefs.ConflictError{Code: "409", Message: "this project's ad-platform connection has no ad account selected — save an ad account id on the connection before changing a campaign's targeting"}
	case errors.Is(werr, domain.ErrConnectionNotUsable):
		slog.WarnContext(ctx, "campaign targeting change blocked: the project's connection is not usable",
			append(logArgs, "reason", unusableConnectionReason(werr))...)
		return &briefs.ConflictError{Code: "409", Message: "this project's ad-platform connection is not ready — repair the connection before changing a campaign's targeting"}
	case errors.Is(werr, domain.ErrNotFound):
		return &briefs.NotFoundError{Code: "404", Message: "this project has no connection for the campaign's channel; connect it before changing a campaign's targeting"}
	case errors.As(werr, &unconfirmed) && unconfirmed.Unconfirmed():
		slog.WarnContext(ctx, "campaign targeting change outcome is UNCONFIRMED (the platform may or may not reflect the new targeting)",
			append(logArgs, "error", werr)...)
		*releaseNow = false
		campaignRepo.ReleaseCampaignLockAfterCooldown(lockToken, unconfirmedLockCooldown)
		return &briefs.ConnServiceUnavailableError{Code: "503", Message: "the campaign targeting change is unconfirmed — it may or may not have been applied on LinkedIn; verify the targeting in Campaign Manager before retrying"}
	default:
		slog.WarnContext(ctx, "campaign targeting change failed on the ad platform", append(logArgs, "error", werr)...)
		return &briefs.ConnServiceUnavailableError{Code: "503", Message: "the campaign targeting could not be changed on the ad platform; the campaign was not modified"}
	}
}
