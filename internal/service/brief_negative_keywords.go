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

// AddNegativeKeywords adds campaign-level negative keywords to one live campaign
// (LFXV2-2665). Microsoft Advertising only today; any other platform is a 400.
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
	if existing.Platform != model.ProviderMicrosoftAds {
		return nil, &briefs.BadRequestError{
			Code:    "400",
			Message: "negative keywords can be added to Microsoft Advertising campaigns only",
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

	outcomes, aerr := orch.AddNegativeKeywords(ctx, p.ProjectID, existing.Platform, existing, keywords)
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

// classifyNegativeKeywordError maps an orchestrator failure onto this service's error set. The
// arms, their order and their reasons are classifyKeywordActionError's; only the wording names
// negative keywords. Adapter text is logged, never returned.
func (s *BriefService) classifyNegativeKeywordError(ctx context.Context, p *briefs.AddNegativeKeywordsPayload, platform model.Provider, aerr error) error {
	logFields := []any{"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID, "platform", platform}
	var unconfirmed interface{ Unconfirmed() bool }
	switch {
	case errors.Is(aerr, domain.ErrNegativeKeywordsUnsupported):
		return &briefs.BadRequestError{Code: "400", Message: "negative keywords are not supported for this campaign's platform"}
	case errors.Is(aerr, domain.ErrNegativeKeywordInvalid):
		slog.WarnContext(ctx, "negative keyword batch rejected before the platform was contacted; nothing was added",
			append(logFields, "error", safeErrSummary(aerr))...)
		return &briefs.BadRequestError{
			Code:    "400",
			Message: "the negative keywords are not valid: send 1 to 60, each Exact or Phrase, at most 100 characters of letters, digits, spaces and & ' - . with no two punctuation characters together, and each keyword at most once",
		}
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
		slog.ErrorContext(ctx, "stored credentials failed authenticated decryption; negative keywords cannot be added",
			"project_id", p.ProjectID, "platform", platform)
		return &briefs.InternalServerError{Code: "500", Message: "the negative keywords could not be added"}
	case errors.Is(aerr, domain.ErrNotFound):
		slog.WarnContext(ctx, "negative keywords blocked: no connection configured for this project and provider",
			"project_id", p.ProjectID, "platform", platform)
		return &briefs.NotFoundError{Code: "404", Message: "no " + keywordPlatformLabel(platform) + " connection is configured for this project"}
	case errors.Is(aerr, domain.ErrConnectionNotUsable):
		slog.WarnContext(ctx, "connection is not usable for negative keywords",
			"project_id", p.ProjectID, "platform", platform, "reason", unusableConnectionReason(aerr))
		return &briefs.ConflictError{Code: "409", Message: keywordConnectionUnusableMessage(platform)}
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
