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

// AddNegativeKeywords adds negative keywords to one live campaign (LFXV2-2665): Microsoft
// Advertising at campaign level, Reddit in the ad group's excluded keywords, X as negated line-item
// keyword criteria. Any other platform is a 400.
//
// It follows ApplyKeywordActions rather than the toggle or budget write, for the reason that
// handler documents: it PERSISTS NOTHING. The negatives live upstream and are mirrored in no
// table, so the row is read to authorize and to reach the platform ids and is never written —
// no version to bump, no If-Match to require, no ETag to return, no write lock to hold across
// the platform call, and no index event to publish.
func (s *BriefService) AddNegativeKeywords(ctx context.Context, p *briefs.AddNegativeKeywordsPayload) (*briefs.NegativeKeywords, error) {
	_, campaignRepo, _, orch, err := s.ready()
	if err != nil {
		return nil, err
	}
	// The (project, brief, campaign) triple is the ownership guard, as for every sibling.
	existing, gerr := campaignRepo.GetCampaign(ctx, p.ProjectID, p.BriefID, p.CampaignID)
	if gerr != nil {
		return nil, mapBriefErr(gerr)
	}
	// Refused BEFORE the orchestrator: no dispatcher of another platform is ever asked.
	// The orchestrator's capability assertion is the second line, for a direct caller.
	if !negativeKeywordPlatform(existing.Platform) {
		return nil, &briefs.BadRequestError{
			Code:    "400",
			Message: "negative keywords can be added to Microsoft Advertising, Reddit and X campaigns only",
		}
	}
	// Goa enforces MinLength(1) for HTTP callers; repeated for a direct caller.
	if len(p.NegativeKeywords) == 0 {
		return nil, &briefs.BadRequestError{Code: "400", Message: "at least one negative keyword is required"}
	}
	keywords := make([]model.NegativeKeyword, 0, len(p.NegativeKeywords))
	for _, k := range p.NegativeKeywords {
		if k == nil {
			return nil, &briefs.BadRequestError{Code: "400", Message: "a negative keyword entry is empty"}
		}
		keywords = append(keywords, model.NegativeKeyword{Text: k.Text, MatchType: k.MatchType})
	}

	// PRESENCE, not value, decides a non-Reddit refusal: an explicit "" collapses to the same ""
	// as an absent field once converted, so the adapters alone would accept {"revision": ""} on
	// Microsoft and X, which the contract says refuse any revision (PR #321 review). Reddit takes
	// the value, and its adapter refuses a blank one as an invalid batch.
	revision := ""
	if p.Revision != nil {
		if existing.Platform != model.ProviderRedditAds {
			return nil, &briefs.BadRequestError{Code: "400", Message: "revision applies to Reddit campaigns only — omit it, even empty, for Microsoft Advertising and X"}
		}
		revision = *p.Revision
	}

	outcomes, aerr := orch.AddNegativeKeywords(ctx, p.ProjectID, existing.Platform, existing, keywords, revision)
	if aerr != nil {
		return nil, s.classifyNegativeKeywordError(ctx, p, existing.Platform, aerr)
	}
	results := make([]*briefs.NegativeKeywordResult, 0, len(outcomes))
	applied := 0
	for _, o := range outcomes {
		outcome := o.Outcome
		if outcome == "" {
			// Every adapter of this capability is non-atomic and must name each outcome. An
			// unnamed one is not evidence of success.
			outcome = model.KeywordOutcomeUnconfirmed
		}
		results = append(results, &briefs.NegativeKeywordResult{
			Text:              o.Text,
			MatchType:         o.MatchType,
			Outcome:           outcome,
			NegativeKeywordID: optionalString(o.NegativeKeywordID),
			ErrorCode:         optionalString(o.ErrorCode),
		})
		if model.NegativeKeywordPresent(outcome) {
			applied++
		}
	}
	return &briefs.NegativeKeywords{CampaignID: p.CampaignID, Results: results, AppliedCount: applied}, nil
}

// negativeKeywordPlatform reports whether add-negative-keywords serves the platform.
func negativeKeywordPlatform(platform model.Provider) bool {
	switch platform {
	case model.ProviderMicrosoftAds, model.ProviderRedditAds, model.ProviderTwitterAds:
		return true
	}
	return false
}

// negativeKeywordPlatformLabel names the platform in a caller-facing message.
func negativeKeywordPlatformLabel(platform model.Provider) string {
	if keywordTargetingPlatform(platform) {
		return keywordTargetingPlatformLabel(platform)
	}
	return keywordPlatformLabel(platform)
}

// negativeKeywordConnectionUnusableMessage is the 409 for an unusable connection, naming the
// platform's own stored fields where a sibling lever already words them.
func negativeKeywordConnectionUnusableMessage(platform model.Provider) string {
	if keywordTargetingPlatform(platform) {
		return "the stored " + keywordTargetingPlatformLabel(platform) + " connection cannot be used as configured: check that it is active, that its stored credential is complete, and that an ad account is selected"
	}
	return keywordConnectionUnusableMessage(platform)
}

// classifyNegativeKeywordError maps an orchestrator failure onto this service's error set. The
// arms, their order and their reasons are classifyKeywordActionError's; only the wording names
// negative keywords. Adapter text is logged, never returned.
func (s *BriefService) classifyNegativeKeywordError(ctx context.Context, p *briefs.AddNegativeKeywordsPayload, platform model.Provider, aerr error) error {
	logFields := []any{"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID, "platform", platform}
	var unconfirmed interface{ Unconfirmed() bool }
	switch {
	case errors.Is(aerr, domain.ErrNegativeKeywordsUnsupported):
		return &briefs.BadRequestError{Code: "400", Message: "negative keywords are not supported, or not enabled in this deployment, for this campaign's platform"}
	case errors.Is(aerr, domain.ErrNegativeKeywordInvalid):
		slog.WarnContext(ctx, "negative keyword batch rejected before the platform was mutated; nothing was added",
			append(logFields, "error", safeErrSummary(aerr))...)
		return &briefs.BadRequestError{
			Code: "400",
			Message: "the negative keywords are not valid: send 1 to 60 (1 to 20 on X), each at most 100 characters of letters, digits, spaces and & ' - . with no two punctuation characters together, each keyword at most once, " +
				"with a match type the platform offers (Microsoft Advertising: Exact or Phrase; X: Broad, Phrase or Exact; Reddit: Broad) — and on Reddit the revision get-keyword-targeting returned, which the other platforms refuse",
		}
	case errors.Is(aerr, domain.ErrKeywordTargetingChanged):
		return &briefs.ConflictError{Code: "409", Message: "the ad group's targeting changed since it was read — nothing was added; read the keyword targeting again and resend with its revision"}
	case errors.Is(aerr, domain.ErrNegativeKeywordLimit):
		slog.WarnContext(ctx, "negative keywords refused: the platform's limit would be exceeded; nothing was added",
			append(logFields, "error", safeErrSummary(aerr))...)
		return &briefs.ConflictError{Code: "409", Message: "these negative keywords would take the ad group over the platform's limit on excluded keywords — nothing was added"}
	case errors.Is(aerr, domain.ErrKeywordTargetingUnaddressable):
		slog.WarnContext(ctx, "negative keywords blocked: the campaign's ad group or line item cannot be addressed",
			append(logFields, "error", safeErrSummary(aerr))...)
		return &briefs.ConflictError{Code: "409", Message: "the campaign's ad group or line item cannot be addressed: this service has no record of it, the platform no longer holds it or reports it under another campaign, or its targeting could not be read — nothing was added"}
	case errors.Is(aerr, ErrCampaignNotProvisioned), errors.Is(aerr, domain.ErrCampaignNotProvisioned):
		return &briefs.ConflictError{
			Code:    "409",
			Message: "campaign is not fully provisioned — it has no platform campaign id, so there is no campaign to add negative keywords to",
		}
	case errors.Is(aerr, domain.ErrCampaignProvenanceUnknown):
		slog.WarnContext(ctx, "negative keywords blocked: campaign does not record which ad account it was created under",
			append(logFields, "error", safeErrSummary(aerr))...)
		return &briefs.ConflictError{
			Code:    "409",
			Message: "this campaign does not record which ad account it was created under, so negative keywords cannot be added safely — it must be re-dispatched first",
		}
	case errors.Is(aerr, ErrCampaignAccountMismatch), errors.Is(aerr, domain.ErrCampaignAccountMismatch):
		slog.WarnContext(ctx, "negative keywords blocked: campaign belongs to a different ad account than the current connection",
			append(logFields, "error", safeErrSummary(aerr))...)
		return &briefs.ConflictError{
			Code:    "409",
			Message: "the campaign belongs to a different ad account than this project's current connection — reconnect the original account before adding negative keywords",
		}
	case errors.Is(aerr, domain.ErrSystemConnectionNotUsable):
		slog.ErrorContext(ctx, "the LF system connection is not usable; negative keywords are failing for every project without its own connection",
			"project_id", p.ProjectID, "platform", platform, "reason", unusableConnectionReason(aerr))
		return &briefs.InternalServerError{Code: "500", Message: "the negative keywords could not be added"}
	case errors.Is(aerr, domain.ErrCredentialDecryptionFailed):
		// Attributed to the row that FAILED, as the bid and budget handlers do: a project with no
		// connection of its own falls back to the LF system row, and blaming the requester would
		// make one corrupt system row look like many unrelated project failures.
		slog.ErrorContext(ctx, "stored credentials failed authenticated decryption; negative keywords cannot be added",
			"project_id", credentialOwnerProject(aerr, p.ProjectID), "requested_by_project_id", p.ProjectID, "platform", platform)
		return &briefs.InternalServerError{Code: "500", Message: "the negative keywords could not be added"}
	case errors.Is(aerr, domain.ErrNotFound):
		slog.WarnContext(ctx, "negative keywords blocked: no connection configured for this project and provider",
			"project_id", p.ProjectID, "platform", platform)
		return &briefs.NotFoundError{Code: "404", Message: "no " + negativeKeywordPlatformLabel(platform) + " connection is configured for this project"}
	case errors.Is(aerr, domain.ErrConnectionNotUsable):
		slog.WarnContext(ctx, "connection is not usable for negative keywords",
			"project_id", p.ProjectID, "platform", platform, "reason", unusableConnectionReason(aerr))
		return &briefs.ConflictError{Code: "409", Message: negativeKeywordConnectionUnusableMessage(platform)}
	case errors.As(aerr, &unconfirmed) && unconfirmed.Unconfirmed():
		slog.WarnContext(ctx, "negative keywords outcome is UNCONFIRMED (the platform may or may not reflect the change)",
			append(logFields, "error", safeErrSummary(aerr))...)
		return &briefs.ConnServiceUnavailableError{
			Code:    "503",
			Message: "the negative keywords are unconfirmed — they may or may not have been added on the ad platform; verify this campaign's negative keywords in the platform before retrying",
		}
	default:
		slog.WarnContext(ctx, "negative keywords failed upstream",
			append(logFields, "error", safeErrSummary(aerr))...)
		return &briefs.ConnServiceUnavailableError{Code: "503", Message: "the negative keywords could not be added"}
	}
}

// credentialOwnerProject names the project whose credential row produced err: the LF system row
// when the resolver marked the error with domain.ErrSystemConnectionOrigin (the requester had no
// connection of its own and fell back), otherwise the requesting project. A decrypt failure is
// logged against this, beside requested_by_project_id, so one corrupt system row reads as one
// incident rather than as failures scattered across every project that fell back to it.
func credentialOwnerProject(err error, requestingProject string) string {
	if errors.Is(err, domain.ErrSystemConnectionOrigin) {
		return model.SystemProjectID
	}
	return requestingProject
}
