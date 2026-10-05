// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"log/slog"
	"math"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/indexer"
)

// maxCampaignBid bounds the requested max CPC bid at the SERVICE layer, stated in the design's
// Maximum and answered 400. It is the CONTRACT's ceiling, deliberately loose: the amount is in
// the ad account's own currency, and a CPC bid that is absurd in USD is ordinary in JPY, KRW or
// IDR. Each adapter applies its platform's own, lower ceiling (Microsoft's create-path bound,
// for one) and refuses above it with ErrBidAmountRejected, which is also a 400.
const maxCampaignBid = 1_000_000.0

// UpdateCampaignBid changes a campaign's MANUAL max cost-per-click bid ON THE AD PLATFORM, then
// persists the new bid. It is UpdateCampaignBudget's twin, and deliberately so: the same
// validation-before-load order, the same If-Match handling, the same claim, the same
// platform-first-then-persist split and the same error mapping, with the bid sentinels in place
// of the budget ones. Read that method's comments for the reasoning behind each step; this one
// states only where the two differ.
//
// WHAT IS PERSISTED is the requested bid, onto max_cpc_bid (migration 000038) — the bid
// lever's twin of budget_amount, recording a request the platform confirmed, never an
// observation.
//
// BID ONLY, never the strategy. A campaign whose bid strategy is automated (or unreported) is
// refused 409 by the adapter rather than having a bid written that the platform would ignore or
// read as a strategy switch. See BidWriter.
func (s *BriefService) UpdateCampaignBid(ctx context.Context, p *briefs.UpdateCampaignBidPayload) (*briefs.Campaign, error) {
	_, campaignRepo, _, orch, err := s.ready()
	if err != nil {
		return nil, err
	}
	version, err := parseBriefIfMatch(p.IfMatch)
	if err != nil {
		return nil, err
	}

	// REQUEST VALIDATION, before anything is loaded or claimed — NaN first, because it fails
	// every ordered comparison and would slip through the range checks below.
	bid := p.Bid
	if math.IsNaN(bid) || math.IsInf(bid, 0) {
		return nil, &briefs.BadRequestError{Code: "400", Message: "bid must be a finite number"}
	}
	if bid <= 0 {
		// Zero is not a bid: it is a request to stop serving, and the status endpoint is what
		// expresses that.
		return nil, &briefs.BadRequestError{Code: "400", Message: "bid must be greater than zero; to stop a campaign serving, pause it with the status endpoint instead"}
	}
	if bid > maxCampaignBid {
		return nil, &briefs.BadRequestError{Code: "400", Message: "bid exceeds the maximum this service will set"}
	}
	// The same half-a-micro cutoff the budget applies, for the same reason: an amount that
	// rounds to zero micros can only be refused by an adapter, and must not take the lock first.
	if math.Round(bid*microsPerCurrencyUnit) < 1 {
		return nil, &briefs.BadRequestError{Code: "400", Message: "bid is too small to set; it rounds to zero micros (one micro is 0.000001 of the account's currency, and this amount is under half of one), which no ad platform accepts"}
	}
	// bid_type is optional with a design default of "cpc"; an empty value from a direct
	// (non-HTTP) caller takes the same default rather than being refused.
	bidType := model.BidTypeCPC
	if p.BidType != "" {
		bidType = model.BidType(p.BidType)
	}
	if bidType != model.BidTypeCPC {
		return nil, &briefs.BadRequestError{Code: "400", Message: "bid_type must be 'cpc'"}
	}

	existing, gerr := campaignRepo.GetCampaign(ctx, p.ProjectID, p.BriefID, p.CampaignID)
	if gerr != nil {
		return nil, mapBriefErr(gerr)
	}
	// If-Match against the LOADED row, before any state check — see UpdateCampaignBudget.
	if existing.Version != version {
		return nil, &briefs.PreconditionFailedError{Code: "412", Message: "the supplied ETag does not match the current version"}
	}
	// 'created_degraded' is allowed for the budget's reason: the campaign exists upstream and
	// may be serving, and cutting a runaway bid is exactly what such a campaign may need.
	if !model.CampaignStatusToggleable(existing.Status) && existing.Status != model.CampaignStatusCreatedDegraded {
		return nil, &briefs.ConflictError{Code: "409", Message: "campaign is not in a state where its bid can be changed (it is still provisioning or needs reconciliation); resolve its status first"}
	}
	if existing.Platform.Kind() == model.ChannelEmail {
		return nil, &briefs.BadRequestError{Code: "400", Message: "bid changes do not apply to the email channel: it stages a draft for a human to send, so there is no bid to set"}
	}
	if existing.PlatformCampaignID == "" {
		return nil, &briefs.ConflictError{Code: "409", Message: "campaign is not fully provisioned — it has no platform campaign id yet, so there is no upstream bid to change"}
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

	change := model.BidChange{Amount: bid, Type: bidType}
	if werr := orch.WriteCampaignBid(ctx, p.ProjectID, existing.Platform, existing, change); werr != nil {
		logArgs := []any{
			"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
			"platform", existing.Platform,
		}
		var unconfirmed interface{ Unconfirmed() bool }
		switch {
		case errors.Is(werr, ErrBidUnsupported):
			return nil, &briefs.BadRequestError{Code: "400", Message: "bid changes are not supported for this campaign's platform"}
		case errors.Is(werr, ErrCampaignNotProvisioned):
			return nil, &briefs.ConflictError{Code: "409", Message: "campaign is not fully provisioned — it has no platform campaign id yet, so there is no upstream bid to change"}
		case errors.Is(werr, ErrBidAmountRejected):
			// A platform's own floor or ceiling. The adapter's sentence names the amount and the
			// platform's published bound and nothing about the account, so it is returned.
			slog.InfoContext(ctx, "campaign bid change refused: the requested bid is outside the platform's accepted range",
				append(logArgs, "platform_campaign_id", existing.PlatformCampaignID, "error", safeErrSummary(werr))...)
			msg := "the requested bid is not accepted by this campaign's ad platform"
			var rejected interface{ BidAmountReason() string }
			if errors.As(werr, &rejected) {
				msg = msg + ": " + safeErrSummary(errors.New(rejected.BidAmountReason()))
			}
			return nil, &briefs.BadRequestError{Code: "400", Message: msg}
		case errors.Is(werr, ErrBidUnwritable):
			// Automated or unreported bid strategy, or an unaddressable ad group. The cause goes
			// to the log — it names upstream configuration — and the client gets the remedy.
			slog.WarnContext(ctx, "campaign bid change refused: the bid cannot be written as a manual bid",
				append(logArgs, "platform_campaign_id", existing.PlatformCampaignID, "error", safeErrSummary(werr))...)
			return nil, &briefs.ConflictError{Code: "409", Message: "this campaign's bid could not be changed as requested — its bid strategy is automated (or was not reported), so a manual bid would be ignored, or the ad group this service created for it could not be addressed; this endpoint sets a manual bid and never changes the bid strategy, so change the strategy in the ad platform first"}
		case errors.Is(werr, domain.ErrPlatformCampaignAbsent):
			slog.WarnContext(ctx, "campaign bid change refused: the platform holds no such campaign",
				append(logArgs, "platform_campaign_id", existing.PlatformCampaignID)...)
			return nil, &briefs.NotFoundError{Code: "404", Message: "the platform holds no campaign with this id — it may have been deleted upstream"}
		case errors.Is(werr, domain.ErrCampaignProvenanceUnknown):
			slog.WarnContext(ctx, "campaign bid change blocked: campaign does not record which ad account it was created under",
				append(logArgs, "error", safeErrSummary(werr))...)
			return nil, &briefs.ConflictError{Code: "409", Message: "this campaign does not record which ad account it was created under, so its bid cannot be changed safely — it must be re-dispatched first"}
		case errors.Is(werr, ErrCampaignAccountMismatch):
			slog.WarnContext(ctx, "campaign bid change blocked: campaign belongs to a different ad account than the current connection",
				append(logArgs, "error", safeErrSummary(werr))...)
			return nil, &briefs.ConflictError{Code: "409", Message: "the campaign belongs to a different ad account than this project's current connection — reconnect the original account to change its bid"}
		case errors.Is(werr, domain.ErrSystemConnectionNotUsable):
			slog.ErrorContext(ctx, "the LF system connection is not usable; campaign bid changes are failing for every project without its own connection",
				append(logArgs, "reason", unusableConnectionReason(werr))...)
			return nil, &briefs.InternalServerError{Code: "500", Message: "the campaign bid could not be changed"}
		case errors.Is(werr, domain.ErrSystemConnectionMissing):
			slog.ErrorContext(ctx, "the LF system connection is not installed; campaign bid changes are failing for every project while force-system mode is on",
				logArgs...)
			return nil, &briefs.InternalServerError{Code: "500", Message: "the campaign bid could not be changed"}
		case errors.Is(werr, domain.ErrCredentialDecryptionFailed):
			credentialProject := p.ProjectID
			if errors.Is(werr, domain.ErrSystemConnectionOrigin) {
				credentialProject = model.SystemProjectID
			}
			slog.ErrorContext(ctx, "campaign bid change blocked: stored credentials could not be decrypted (key mismatch or corrupted row)",
				"project_id", credentialProject, "requested_by_project_id", p.ProjectID,
				"brief_id", p.BriefID, "campaign_id", p.CampaignID, "platform", existing.Platform)
			return nil, &briefs.InternalServerError{Code: "500", Message: "the campaign bid could not be changed"}
		case errors.Is(werr, domain.ErrServiceDefect):
			slog.ErrorContext(ctx, "campaign bid change blocked by a defect in this service, not in the connection; a caller-fault status here would send an operator to audit a correct configuration",
				append(logArgs, "reason", unusableConnectionReason(werr))...)
			return nil, &briefs.InternalServerError{Code: "500", Message: "the campaign bid could not be changed"}
		case errors.Is(werr, domain.ErrAccountNotSelected):
			slog.WarnContext(ctx, "campaign bid change blocked: no ad account selected on the project's connection",
				append(logArgs, "reason", unusableConnectionReason(werr))...)
			return nil, &briefs.ConflictError{Code: "409", Message: "this project's ad-platform connection has no ad account selected — save an ad account id on the connection before changing a campaign bid"}
		case errors.Is(werr, domain.ErrConnectionNotUsable):
			slog.WarnContext(ctx, "campaign bid change blocked: the project's connection is not usable",
				append(logArgs, "reason", unusableConnectionReason(werr))...)
			return nil, &briefs.ConflictError{Code: "409", Message: "this project's ad-platform connection is not ready — its stored credentials or provider settings need attention; repair the connection before changing a campaign bid"}
		case errors.Is(werr, domain.ErrNotFound):
			slog.WarnContext(ctx, "campaign bid change blocked: no connection configured for this project and provider",
				logArgs...)
			return nil, &briefs.NotFoundError{Code: "404", Message: "this project has no connection for the campaign's channel; connect it before changing a campaign bid"}
		case errors.As(werr, &unconfirmed) && unconfirmed.Unconfirmed():
			// UNCONFIRMED: the mutate may already have applied. Row untouched, lock held for the
			// cooldown — exactly as the budget write does, and for its reasons.
			slog.WarnContext(ctx, "campaign bid change outcome is UNCONFIRMED (the platform may or may not reflect the new bid)",
				append(logArgs, "platform_campaign_id", existing.PlatformCampaignID, "error", werr)...)
			releaseNow = false
			campaignRepo.ReleaseCampaignLockAfterCooldown(lockToken, unconfirmedLockCooldown)
			return nil, &briefs.ConnServiceUnavailableError{Code: "503", Message: "the campaign bid change is unconfirmed — it may or may not have been applied on the ad platform; verify the bid in the platform before retrying"}
		default:
			slog.WarnContext(ctx, "campaign bid change failed on the ad platform",
				append(logArgs, "platform_campaign_id", existing.PlatformCampaignID, "error", werr)...)
			return nil, &briefs.ConnServiceUnavailableError{Code: "503", Message: "the campaign bid could not be changed on the ad platform; the campaign was not modified"}
		}
	}

	// The platform change ALREADY committed: persist on a cancel-detached, bounded context, and
	// touch only the bid column (Status in particular stays as found).
	existing.MaxCPCBid = &bid
	existing.UpdatedBy = attributedActor(ctx, "update campaign bid")
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistResultTimeout)
	defer cancel()
	updated, uerr := campaignRepo.ReplaceCampaign(persistCtx, existing, version, lockToken, s.campaignIndexPayload(indexer.ActionUpdated))
	if uerr != nil {
		slog.ErrorContext(ctx, "campaign bid changed on the platform but the DB row write failed (platform/DB diverged)",
			"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
			"platform", existing.Platform, "platform_campaign_id", existing.PlatformCampaignID, "error", uerr)
		return nil, mapBriefErr(uerr)
	}
	// A success log, for the budget's reason: an amount's history is not reconstructable from
	// the row alone.
	slog.InfoContext(ctx, "campaign bid changed on the ad platform and persisted",
		"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
		"platform", existing.Platform, "platform_campaign_id", existing.PlatformCampaignID,
		"bid_type", bidType, "max_cpc_bid", bid)
	return campaignResult(updated), nil
}
