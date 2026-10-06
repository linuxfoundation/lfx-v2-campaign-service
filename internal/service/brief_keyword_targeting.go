// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"log/slog"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// keywordTargetingPlatform reports whether a campaign's platform carries keywords as TARGETING
// (LFXV2-2665): Reddit (ad group targeting) and X (line item targeting criteria). Google Ads and
// Microsoft Advertising keywords are criteria, reached through apply-keyword-actions instead.
func keywordTargetingPlatform(p model.Provider) bool {
	return p == model.ProviderRedditAds || p == model.ProviderTwitterAds
}

// GetKeywordTargeting reads the keyword targeting of a Reddit or X campaign's one ad group / line
// item. A pure read of the platform; nothing is persisted.
func (s *BriefService) GetKeywordTargeting(ctx context.Context, p *briefs.GetKeywordTargetingPayload) (*briefs.KeywordTargeting, error) {
	_, campaignRepo, _, orch, err := s.ready()
	if err != nil {
		return nil, err
	}
	// The (project, brief, campaign) triple is the ownership guard, as for every sibling.
	existing, gerr := campaignRepo.GetCampaign(ctx, p.ProjectID, p.BriefID, p.CampaignID)
	if gerr != nil {
		return nil, mapBriefErr(gerr)
	}
	if !keywordTargetingPlatform(existing.Platform) {
		return nil, &briefs.BadRequestError{Code: "400", Message: "keyword targeting applies to Reddit and X campaigns only"}
	}
	kt, rerr := orch.ReadKeywordTargeting(ctx, p.ProjectID, existing.Platform, existing)
	if rerr != nil {
		return nil, s.classifyKeywordTargetingError(ctx, "read", p.ProjectID, p.BriefID, p.CampaignID, existing.Platform, rerr)
	}
	out := &briefs.KeywordTargeting{
		CampaignID:        p.CampaignID,
		Platform:          string(existing.Platform),
		TargetingEntityID: kt.EntityID,
		Keywords:          make([]*briefs.KeywordTargetingEntry, 0, len(kt.Keywords)),
		Revision:          optionalString(kt.Revision),
	}
	for _, k := range kt.Keywords {
		out.Keywords = append(out.Keywords, &briefs.KeywordTargetingEntry{
			Keyword:     k.Keyword,
			CriterionID: optionalString(k.CriterionID),
			MatchType:   optionalString(k.MatchType),
		})
	}
	return out, nil
}

// RemoveKeywordTargeting removes keywords from the keyword targeting of a Reddit or X campaign.
//
// Like ApplyKeywordActions it PERSISTS NOTHING — the targeting lives upstream — so it takes no
// write lock, no If-Match, returns no ETag and publishes no index event. The row is read to
// authorize and to reach the platform ids, and never written.
func (s *BriefService) RemoveKeywordTargeting(ctx context.Context, p *briefs.RemoveKeywordTargetingPayload) (*briefs.KeywordTargetingRemovals, error) {
	_, campaignRepo, _, orch, err := s.ready()
	if err != nil {
		return nil, err
	}
	existing, gerr := campaignRepo.GetCampaign(ctx, p.ProjectID, p.BriefID, p.CampaignID)
	if gerr != nil {
		return nil, mapBriefErr(gerr)
	}
	if !keywordTargetingPlatform(existing.Platform) {
		return nil, &briefs.BadRequestError{Code: "400", Message: "keyword targeting applies to Reddit and X campaigns only"}
	}
	// Goa enforces MinLength(1) for HTTP callers; repeated for a direct caller.
	if len(p.Keywords) == 0 {
		return nil, &briefs.BadRequestError{Code: "400", Message: "at least one keyword to remove is required"}
	}
	removals := make([]model.KeywordTargetingRemoval, 0, len(p.Keywords))
	for _, k := range p.Keywords {
		if k == nil {
			return nil, &briefs.BadRequestError{Code: "400", Message: "a keyword removal entry is empty"}
		}
		r := model.KeywordTargetingRemoval{}
		if k.Keyword != nil {
			r.Keyword = *k.Keyword
		}
		if k.CriterionID != nil {
			r.CriterionID = *k.CriterionID
		}
		removals = append(removals, r)
	}
	revision := ""
	if p.Revision != nil {
		revision = *p.Revision
	}

	outcomes, rerr := orch.RemoveKeywordTargeting(ctx, p.ProjectID, existing.Platform, existing, removals, revision)
	if rerr != nil {
		return nil, s.classifyKeywordTargetingError(ctx, "remove", p.ProjectID, p.BriefID, p.CampaignID, existing.Platform, rerr)
	}
	results := make([]*briefs.KeywordTargetingRemovalResult, 0, len(outcomes))
	applied := 0
	for _, o := range outcomes {
		outcome := o.Outcome
		switch outcome {
		case model.KeywordOutcomeApplied, model.KeywordOutcomeFailed, model.KeywordOutcomeUnconfirmed:
		default:
			// An outcome the adapter did not name is not evidence of success.
			outcome = model.KeywordOutcomeUnconfirmed
		}
		results = append(results, &briefs.KeywordTargetingRemovalResult{
			Keyword:     optionalString(o.Keyword),
			CriterionID: optionalString(o.CriterionID),
			Outcome:     outcome,
			ErrorCode:   optionalString(o.ErrorCode),
		})
		if outcome == model.KeywordOutcomeApplied {
			applied++
		}
	}
	return &briefs.KeywordTargetingRemovals{CampaignID: p.CampaignID, Results: results, AppliedCount: applied}, nil
}

// keywordTargetingPlatformLabel names the platform in a caller-facing message.
func keywordTargetingPlatformLabel(platform model.Provider) string {
	if platform == model.ProviderTwitterAds {
		return "x ads"
	}
	return "reddit ads"
}

// classifyKeywordTargetingError maps an orchestrator failure onto this service's error set. The
// arms and their order are classifyKeywordActionError's, plus the four targeting-specific
// refusals. verb is "read" or "remove". Adapter text is logged, never returned.
func (s *BriefService) classifyKeywordTargetingError(ctx context.Context, verb, projectID, briefID, campaignID string, platform model.Provider, aerr error) error {
	logFields := []any{"project_id", projectID, "brief_id", briefID, "campaign_id", campaignID, "platform", platform, "op", verb + "_keyword_targeting"}
	var unconfirmed interface{ Unconfirmed() bool }
	failed := "the keyword targeting could not be read"
	if verb == "remove" {
		failed = "the keywords could not be removed"
	}
	switch {
	case errors.Is(aerr, domain.ErrKeywordTargetingUnsupported):
		return &briefs.BadRequestError{Code: "400", Message: "keyword targeting changes are not supported or not enabled for this campaign's platform"}
	case errors.Is(aerr, domain.ErrKeywordTargetingInvalid):
		slog.WarnContext(ctx, "keyword targeting removal refused as invalid; nothing was changed",
			append(logFields, "error", safeErrSummary(aerr))...)
		return &briefs.BadRequestError{
			Code:    "400",
			Message: "the keyword removals are not valid: send 1 to 20, each naming a keyword the campaign currently targets at most once — `keyword` with the read's revision on Reddit, `criterion_id` and no revision on X",
		}
	case errors.Is(aerr, ErrCampaignNotProvisioned), errors.Is(aerr, domain.ErrCampaignNotProvisioned):
		return &briefs.ConflictError{Code: "409", Message: "campaign is not fully provisioned — it has no platform campaign id, so it has no keyword targeting"}
	case errors.Is(aerr, domain.ErrKeywordTargetingChanged):
		return &briefs.ConflictError{Code: "409", Message: "the ad group's targeting changed since it was read — nothing was removed; read the keyword targeting again and resend with its revision"}
	case errors.Is(aerr, domain.ErrKeywordTargetingWouldEmpty):
		return &briefs.ConflictError{Code: "409", Message: "this would remove every keyword, which stops keyword targeting and widens delivery — nothing was removed; keep at least one keyword, or remove the last one in the ad platform"}
	case errors.Is(aerr, domain.ErrKeywordTargetingUnaddressable):
		slog.WarnContext(ctx, "keyword targeting cannot be addressed",
			append(logFields, "error", safeErrSummary(aerr))...)
		return &briefs.ConflictError{Code: "409", Message: "the campaign's keyword targeting cannot be addressed: this service has no recorded ad group or line item for it, the platform no longer holds it or reports it under another campaign, or its targeting could not be read — nothing was changed"}
	case errors.Is(aerr, domain.ErrCampaignProvenanceUnknown):
		slog.WarnContext(ctx, "keyword targeting blocked: campaign does not record which ad account it was created under",
			append(logFields, "error", safeErrSummary(aerr))...)
		return &briefs.ConflictError{Code: "409", Message: "this campaign does not record which ad account it was created under, so its keyword targeting cannot be changed safely — it must be re-dispatched first"}
	case errors.Is(aerr, ErrCampaignAccountMismatch), errors.Is(aerr, domain.ErrCampaignAccountMismatch):
		slog.WarnContext(ctx, "keyword targeting blocked: campaign belongs to a different ad account than the current connection",
			append(logFields, "error", safeErrSummary(aerr))...)
		return &briefs.ConflictError{Code: "409", Message: "the campaign belongs to a different ad account than this project's current connection — reconnect the original account first"}
	case errors.Is(aerr, domain.ErrSystemConnectionNotUsable):
		slog.ErrorContext(ctx, "the LF system connection is not usable; keyword targeting is failing for every project without its own connection",
			"project_id", projectID, "platform", platform, "reason", unusableConnectionReason(aerr))
		return &briefs.InternalServerError{Code: "500", Message: failed}
	case errors.Is(aerr, domain.ErrCredentialDecryptionFailed):
		// Attributed to the row that failed (credentialOwnerProject), not to the requester.
		slog.ErrorContext(ctx, "stored credentials failed authenticated decryption; keyword targeting cannot proceed",
			"project_id", credentialOwnerProject(aerr, projectID), "requested_by_project_id", projectID, "platform", platform)
		return &briefs.InternalServerError{Code: "500", Message: failed}
	case errors.Is(aerr, domain.ErrNotFound):
		return &briefs.NotFoundError{Code: "404", Message: "no " + keywordTargetingPlatformLabel(platform) + " connection is configured for this project"}
	case errors.Is(aerr, domain.ErrConnectionNotUsable):
		slog.WarnContext(ctx, "connection is not usable for keyword targeting",
			"project_id", projectID, "platform", platform, "reason", unusableConnectionReason(aerr))
		return &briefs.ConflictError{Code: "409", Message: "the stored " + keywordTargetingPlatformLabel(platform) + " connection cannot be used as configured: check that it is active, that its stored credential is complete, and that an ad account is selected"}
	case errors.As(aerr, &unconfirmed) && unconfirmed.Unconfirmed():
		// Above the default: an outcome that MAY have applied must not get the "retry" answer
		// a definite failure gets.
		slog.WarnContext(ctx, "keyword targeting removal is UNCONFIRMED (the platform may or may not reflect it)",
			append(logFields, "error", safeErrSummary(aerr))...)
		return &briefs.ConnServiceUnavailableError{
			Code:    "503",
			Message: "the keyword removals are unconfirmed — they may or may not have been applied on the ad platform; read the keyword targeting again before retrying",
		}
	default:
		slog.WarnContext(ctx, "keyword targeting failed upstream",
			append(logFields, "error", safeErrSummary(aerr))...)
		return &briefs.ConnServiceUnavailableError{Code: "503", Message: failed}
	}
}
