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

// ApplyKeywordActions pauses or removes Google Ads keywords on one campaign.
//
// This MUTATES a live paid campaign: pausing or removing a keyword changes what serves. It is
// therefore validated like a create — everything that can be refused locally is refused
// before the ad platform is contacted at all, and the whole batch fails closed.
//
// It deliberately takes NO campaign write lock and requires NO If-Match, unlike
// ToggleCampaignStatus. That is not an oversight: this endpoint persists nothing. The
// campaign row is read to authorize and to reach the platform ids, and is never written, so
// there is no version to bump, no lost-update window to close, and no ETag to invalidate.
// Claiming the campaign's write lock here would block genuine writers for the length of a
// Google mutate while protecting a row this path does not touch.
//
// The keywords themselves live upstream and are not mirrored in any table here — which is
// also why no index event is published: nothing this service stores has changed.
func (s *BriefService) ApplyKeywordActions(ctx context.Context, p *briefs.ApplyKeywordActionsPayload) (*briefs.KeywordActions, error) {
	_, campaignRepo, _, orch, err := s.ready()
	if err != nil {
		return nil, err
	}

	// The (project, brief, campaign) triple IS the ownership guard, exactly as it is for the
	// toggle and the metrics read: a campaign under another project or brief simply does not
	// resolve, and mapBriefErr turns that into a 404. There is no separate belongs-to check.
	existing, gerr := campaignRepo.GetCampaign(ctx, p.ProjectID, p.BriefID, p.CampaignID)
	if gerr != nil {
		return nil, mapBriefErr(gerr)
	}

	// Platform-independent refusals first, before anything upstream is resolved or contacted.
	// Google Ads is the only platform that models keywords as addressable criteria, so any
	// other campaign is a permanent caller error rather than a transient failure.
	if existing.Platform != model.ProviderGoogleAds {
		return nil, &briefs.BadRequestError{
			Code:    "400",
			Message: "keyword actions apply to Google Ads campaigns only",
		}
	}
	// Goa enforces MinLength(1) for HTTP callers; repeated here so a direct (non-HTTP) caller
	// gets the same refusal rather than a 200 reporting that nothing was changed.
	if len(p.Actions) == 0 {
		return nil, &briefs.BadRequestError{Code: "400", Message: "at least one keyword action is required"}
	}

	actions := make([]model.KeywordAction, 0, len(p.Actions))
	for _, a := range p.Actions {
		if a == nil {
			return nil, &briefs.BadRequestError{Code: "400", Message: "a keyword action entry is empty"}
		}
		actions = append(actions, model.KeywordAction{
			AdGroupID:   a.AdGroupID,
			CriterionID: a.CriterionID,
			Action:      a.Action,
		})
	}

	outcomes, aerr := orch.ApplyKeywordActions(ctx, p.ProjectID, existing.Platform, existing, actions)
	if aerr != nil {
		return nil, s.classifyKeywordActionError(ctx, p, existing.Platform, aerr)
	}

	results := make([]*briefs.KeywordActionResult, 0, len(outcomes))
	for _, o := range outcomes {
		results = append(results, &briefs.KeywordActionResult{
			AdGroupID:    o.AdGroupID,
			CriterionID:  o.CriterionID,
			Action:       o.Action,
			ResourceName: o.ResourceName,
		})
	}
	return &briefs.KeywordActions{
		CampaignID: p.CampaignID,
		Results:    results,
		// Always equal to the number requested: the batch is atomic upstream, so a partial
		// application is not a representable outcome. Reported anyway so a consumer can assert
		// it rather than assume it.
		AppliedCount: len(results),
	}, nil
}

// classifyKeywordActionError maps an orchestrator failure onto this service's error set.
//
// The arms mirror ToggleCampaignStatus's, because the failure modes are the same ones and a
// caller acting on them takes the same remedies. Every 409 below is refused BEFORE Google is
// contacted, which is why none of them is a 503: waiting changes nothing.
func (s *BriefService) classifyKeywordActionError(ctx context.Context, p *briefs.ApplyKeywordActionsPayload, platform model.Provider, aerr error) error {
	switch {
	case errors.Is(aerr, domain.ErrKeywordActionsUnsupported):
		return &briefs.BadRequestError{Code: "400", Message: "keyword actions are not supported for this campaign's platform"}
	case errors.Is(aerr, domain.ErrKeywordActionInvalid):
		// A permanent input fault: a malformed id, an unsupported action, a duplicated
		// criterion, or a criterion that does not belong to this campaign's ad group. The
		// adapter's own text is logged rather than returned — it names ad group ids, which
		// are account configuration a caller acting on one campaign has no need to be told.
		slog.WarnContext(ctx, "keyword action batch rejected before contacting the platform",
			"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
			"platform", platform, "error", safeErrSummary(aerr))
		return &briefs.BadRequestError{
			Code:    "400",
			Message: "the keyword actions are not valid: each must name a digits-only ad group and criterion id belonging to this campaign, use PAUSE or REMOVE, and address each keyword at most once",
		}
	case errors.Is(aerr, ErrCampaignNotProvisioned), errors.Is(aerr, domain.ErrCampaignNotProvisioned):
		return &briefs.ConflictError{
			Code:    "409",
			Message: "campaign is not fully provisioned — it has no platform campaign id or no ad group, so it has no keywords to act on",
		}
	case errors.Is(aerr, ErrCampaignAccountMismatch), errors.Is(aerr, domain.ErrCampaignAccountMismatch):
		// The two customer ids stay server-side: which ad account a project is connected to
		// is connection configuration. Logged through safeErrSummary because the error embeds
		// the connection's account_id, which is arbitrary operator-supplied text.
		slog.WarnContext(ctx, "keyword actions blocked: campaign belongs to a different ad account than the current connection",
			"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
			"platform", platform, "error", safeErrSummary(aerr))
		return &briefs.ConflictError{
			Code:    "409",
			Message: "the campaign belongs to a different ad account than this project's current connection — reconnect the original account before changing its keywords",
		}
	case errors.Is(aerr, domain.ErrSystemConnectionNotUsable):
		// Must sit ABOVE the ErrConnectionNotUsable arm: systemScoped WRAPS rather than
		// replaces, so a broad match would win and tell this caller to repair "this project's
		// connection" — a scope they do not have and cannot address.
		slog.ErrorContext(ctx, "the LF system connection is not usable; keyword actions are failing for every project without its own connection",
			"project_id", p.ProjectID, "platform", platform, "reason", unusableConnectionReason(aerr))
		return &briefs.InternalServerError{Code: "500", Message: "the keyword actions could not be applied"}
	case errors.Is(aerr, domain.ErrCredentialDecryptionFailed):
		// The stored blob failed GCM authentication: a rotated CREDENTIAL_ENCRYPTION_KEY or a
		// corrupted row, and this path cannot tell them apart. Re-saving credentials repairs
		// the corrupted row but no reconnect touches a rotated key, so the conservative answer
		// is the operator-only one. No error text is logged on a decrypt arm.
		slog.ErrorContext(ctx, "stored credentials failed authenticated decryption; keyword actions cannot proceed",
			"project_id", p.ProjectID, "platform", platform)
		return &briefs.InternalServerError{Code: "500", Message: "the keyword actions could not be applied"}
	case errors.Is(aerr, domain.ErrNotFound):
		// No connection row for this project and provider, and the shared system row did not
		// cover it. Permanent — there is nothing to repair, so the caller is told to connect
		// rather than to wait. Deliberately not a 409, which would name a scope they do not own.
		slog.WarnContext(ctx, "keyword actions blocked: no connection configured for this project and provider",
			"project_id", p.ProjectID, "platform", platform)
		return &briefs.NotFoundError{Code: "404", Message: "no google ads connection is configured for this project"}
	case errors.Is(aerr, domain.ErrConnectionNotUsable):
		// The connection exists but cannot be used as it stands. The platform is never
		// contacted and none of these improve with time, so a 503 would be a false promise.
		// Neither the cause nor its text leaves this function: one of the conditions behind
		// this arm is detected by decoding the DECRYPTED credential blob.
		slog.WarnContext(ctx, "connection is not usable for keyword actions",
			"project_id", p.ProjectID, "platform", platform, "reason", unusableConnectionReason(aerr))
		return &briefs.ConflictError{
			Code:    "409",
			Message: "the stored google ads connection cannot be used as configured: check that it is active, that the stored credential is valid json with every field set, and that login_customer_id is digits only",
		}
	default:
		// Includes the UNCONFIRMED outcomes the client reports when a mutate may have been
		// applied. The adapter's text is logged, never returned.
		slog.WarnContext(ctx, "keyword actions failed upstream",
			"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
			"platform", platform, "error", safeErrSummary(aerr))
		return &briefs.ConnServiceUnavailableError{Code: "503", Message: "the keyword actions could not be applied"}
	}
}
