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

// maxCampaignBudget bounds the requested amount at the SERVICE layer, matching the ceiling the
// Google Ads adapter applies before converting to micros. Duplicating the number rather than
// importing it is deliberate: this bound belongs to the API contract (it is stated in the
// design's Maximum and answered 400), while the adapter's is the platform's own conversion
// guard, and a platform whose real ceiling is lower will still refuse below this one.
const maxCampaignBudget = 1_000_000_000.0

// UpdateCampaignBudget changes how much a campaign may spend ON THE AD PLATFORM, then persists
// the new amount. The platform call happens FIRST — the row is updated only after the platform
// confirms — so this service never reports a budget the platform does not have.
//
// THE ROW IS WRITTEN, AND THAT DOES NOT BREACH THE READBACK'S CONTRACT. The campaign row's
// budget columns record WHAT A DISPATCH ASKED FOR (see the Orchestrator's note on
// ReadCampaignSettings, which is why a readback must never write an observation back into
// them). A budget change is a new REQUEST, so persisting it is that column continuing to mean
// exactly what it already meant — and the readback then keeps comparing live-against-requested
// as before, rather than live-against-itself.
//
// AMOUNT ONLY. The pacing model is never changed here: a request whose budget_type differs
// from the campaign's current upstream pacing is refused (409), not translated. See the
// dispatcher's WriteBudget for why.
func (s *BriefService) UpdateCampaignBudget(ctx context.Context, p *briefs.UpdateCampaignBudgetPayload) (*briefs.Campaign, error) {
	_, campaignRepo, _, orch, err := s.ready()
	if err != nil {
		return nil, err
	}
	version, err := parseBriefIfMatch(p.IfMatch)
	if err != nil {
		return nil, err
	}

	// REQUEST VALIDATION IS THIS LAYER'S JOB, not the adapter's, and it runs before anything
	// is loaded or claimed. The design's Minimum/Maximum bound the value for generated
	// clients, but Goa's range cannot express any of the three checks that actually matter —
	// NaN, infinities, and strictly-greater-than-zero — and a direct (non-generated) caller
	// reaches this method regardless.
	//
	// NaN is checked FIRST because it fails every ordered comparison: `budget <= 0` and
	// `budget > max` are both false for NaN, so a later range check would pass it straight
	// through to the platform.
	budget := p.Budget
	if math.IsNaN(budget) || math.IsInf(budget, 0) {
		return nil, &briefs.BadRequestError{Code: "400", Message: "budget must be a finite number"}
	}
	if budget <= 0 {
		// Zero is not a budget: it is a request to stop spending, and the status-toggle
		// endpoint is what expresses that. Writing a zero amount would be accepted by the ad
		// platform as a real instruction, stopping delivery through an endpoint whose whole
		// contract is "change how much", which is why it is refused rather than forwarded.
		return nil, &briefs.BadRequestError{Code: "400", Message: "budget must be greater than zero; to stop a campaign's spend, pause it with the status endpoint instead"}
	}
	if budget > maxCampaignBudget {
		return nil, &briefs.BadRequestError{Code: "400", Message: "budget exceeds the maximum this service will set"}
	}
	budgetType := model.BudgetType(p.BudgetType)
	if budgetType != model.BudgetDaily && budgetType != model.BudgetLifetime {
		// The design enum restricts this, but validate defensively so a direct caller cannot
		// push an unsupported pacing at a platform.
		return nil, &briefs.BadRequestError{Code: "400", Message: "budget_type must be 'daily' or 'lifetime'"}
	}

	existing, gerr := campaignRepo.GetCampaign(ctx, p.ProjectID, p.BriefID, p.CampaignID)
	if gerr != nil {
		return nil, mapBriefErr(gerr)
	}
	// Check the If-Match version against the LOADED row BEFORE any state check or platform
	// call, for the reason ToggleCampaignStatus spells out: otherwise a stale ETag is
	// validated against a row the client never saw, and a state check below would report a
	// 409 about that newer row for what is actually a 412.
	if existing.Version != version {
		return nil, &briefs.PreconditionFailedError{Code: "412", Message: "the supplied ETag does not match the current version"}
	}

	// A campaign whose dispatch never resolved may have no upstream campaign at all, so there
	// is nothing to write a budget to. 'created_degraded' is ALLOWED, unlike on an activate:
	// it means the campaign definitely exists upstream with unverified wiring, and it can be
	// spending — so the campaign most likely to need its budget cut would otherwise be the one
	// this service could not cut. Nothing here activates anything, and the reconciliation
	// marker survives untouched because this endpoint writes only the budget columns.
	if !model.CampaignStatusToggleable(existing.Status) && existing.Status != model.CampaignStatusCreatedDegraded {
		return nil, &briefs.ConflictError{Code: "409", Message: "campaign is not in a state where its budget can be changed (it is still provisioning or needs reconciliation); resolve its status first"}
	}
	// Platform-independent refusals belong HERE, before the claim: claiming takes the
	// campaign's write lock on a dedicated pooled connection and blocks every other writer for
	// this campaign until release, so a request that is going to be rejected anyway must never
	// take it. Same ordering, same reason, as ToggleCampaignStatus.
	if existing.Platform.Kind() == model.ChannelEmail {
		return nil, &briefs.BadRequestError{Code: "400", Message: "budget changes do not apply to the email channel: it stages a draft for a human to send, so there is no ad spend to set"}
	}
	if existing.PlatformCampaignID == "" {
		return nil, &briefs.ConflictError{Code: "409", Message: "campaign is not fully provisioned — it has no platform campaign id yet, so there is no upstream budget to change"}
	}

	// Claim write ownership at the read version BEFORE the side-effecting platform call, for
	// every reason ToggleCampaignStatus documents at its own claim: an in-memory version
	// comparison turns away only an already-stale caller and does nothing to stop a second
	// concurrent writer from also mutating the platform and then racing the persist. What
	// makes a lost lock safe is not the claim but the compare-and-swap in ReplaceCampaign
	// below.
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

	change := model.BudgetChange{Amount: budget, Type: budgetType}
	if werr := orch.WriteCampaignBudget(ctx, p.ProjectID, existing.Platform, existing, change); werr != nil {
		var unconfirmed interface{ Unconfirmed() bool }
		switch {
		case errors.Is(werr, ErrBudgetWriteUnsupported):
			// No dispatcher, or a dispatcher that is not a BudgetWriter. The platform was
			// never contacted, and no retry adds the capability.
			return nil, &briefs.BadRequestError{Code: "400", Message: "budget changes are not supported for this campaign's platform"}
		case errors.Is(werr, ErrCampaignNotProvisioned):
			return nil, &briefs.ConflictError{Code: "409", Message: "campaign is not fully provisioned — it has no platform campaign id yet, so there is no upstream budget to change"}
		case errors.Is(werr, ErrBudgetShared):
			// The single most consequential refusal this endpoint has, and it happens BEFORE
			// the mutate, so nothing changed. Writing a shared budget through one campaign
			// moves the spend of every other campaign attached to it — including campaigns
			// this service does not own and cannot see. Permanent (it is how the budget was
			// set up), so 409 and never a retry; the remedy is a human one in the ad platform.
			slog.WarnContext(ctx, "campaign budget change refused: the campaign's upstream budget is shared across campaigns",
				"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
				"platform", existing.Platform, "platform_campaign_id", existing.PlatformCampaignID)
			return nil, &briefs.ConflictError{Code: "409", Message: "this campaign's budget is shared with other campaigns, so changing it here would change their spend too; give the campaign its own budget in the ad platform, or make the change there where its full effect is visible"}
		case errors.Is(werr, ErrBudgetUnwritable):
			// The budget could not be ADDRESSED: the platform did not report which budget
			// resource is attached, did not report whether it is shared, did not report its
			// pacing, reports a pacing this service has no mapping for, or reports a pacing
			// that is not the one this request named. Distinct from the shared case — that one
			// is addressable and deliberately refused — but 409 for the same reason: no retry
			// improves any of them. The error's own text carries which it was; it goes to the
			// log, not to the client, since each cause names upstream configuration.
			slog.WarnContext(ctx, "campaign budget change refused: the upstream budget could not be addressed for a write",
				"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
				"platform", existing.Platform, "platform_campaign_id", existing.PlatformCampaignID,
				"requested_budget_type", budgetType, "error", safeErrSummary(werr))
			return nil, &briefs.ConflictError{Code: "409", Message: "this campaign's budget could not be changed as requested — the ad platform did not report the budget it is attached to, or that budget is paced differently from the requested budget_type; this endpoint changes an amount, never a pacing model, so change the pacing in the ad platform first"}
		case errors.Is(werr, domain.ErrPlatformCampaignAbsent):
			// The platform answered and holds no such campaign. Most actionable outcome
			// available, and it must not fall into the 503 default, which would tell an
			// operator to retry a write that has nothing to write to. 404 names the campaign
			// as gone rather than the service as broken — same as the settings readback.
			slog.WarnContext(ctx, "campaign budget change refused: the platform holds no such campaign",
				"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
				"platform", existing.Platform, "platform_campaign_id", existing.PlatformCampaignID)
			return nil, &briefs.NotFoundError{Code: "404", Message: "the platform holds no campaign with this id — it may have been deleted upstream"}
		case errors.Is(werr, domain.ErrCampaignProvenanceUnknown):
			// ABOVE the mismatch arm: this row names no account to be mismatched against, so
			// "reconnect the original account" would be an instruction nobody can follow. The
			// remedy is a re-dispatch, which is why it is reported separately.
			slog.WarnContext(ctx, "campaign budget change blocked: campaign does not record which ad account it was created under",
				"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
				"platform", existing.Platform, "error", safeErrSummary(werr))
			return nil, &briefs.ConflictError{Code: "409", Message: "this campaign does not record which ad account it was created under, so its budget cannot be changed safely — it must be re-dispatched first"}
		case errors.Is(werr, ErrCampaignAccountMismatch):
			// Refused before the platform was contacted, so nothing changed upstream and a
			// retry is refused identically — 409, never a 503. The two account ids stay
			// server-side (connection configuration, not client business), and the log goes
			// through safeErrSummary for the reason the sibling arms give.
			slog.WarnContext(ctx, "campaign budget change blocked: campaign belongs to a different ad account than the current connection",
				"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
				"platform", existing.Platform, "error", safeErrSummary(werr))
			return nil, &briefs.ConflictError{Code: "409", Message: "the campaign belongs to a different ad account than this project's current connection — reconnect the original account to change its budget"}
		case errors.Is(werr, domain.ErrSystemConnectionNotUsable):
			// ABOVE the two connection arms below, because systemScoped WRAPS the usability
			// sentinels rather than replacing them: a broader match would win and tell a caller
			// who has no connection of their own to repair "this project's connection". Only an
			// operator can act on the LF system row.
			slog.ErrorContext(ctx, "the LF system connection is not usable; campaign budget changes are failing for every project without its own connection",
				"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
				"platform", existing.Platform, "reason", unusableConnectionReason(werr))
			return nil, &briefs.InternalServerError{Code: "500", Message: "the campaign budget could not be changed"}
		case errors.Is(werr, domain.ErrSystemConnectionMissing):
			// ABOVE the ErrNotFound arm, which is wrapped ALONGSIDE this sentinel and would
			// otherwise win. Forced-system mode is on and the LF row is not installed: the
			// project's own connection is exactly what forced mode ignores, so "connect it"
			// cannot work, and the operator who must install the row would never be paged.
			slog.ErrorContext(ctx, "the LF system connection is not installed; campaign budget changes are failing for every project while force-system mode is on",
				"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
				"platform", existing.Platform)
			return nil, &briefs.InternalServerError{Code: "500", Message: "the campaign budget could not be changed"}
		case errors.Is(werr, domain.ErrCredentialDecryptionFailed):
			// 500, not 409: re-saving credentials repairs a corrupted row, but a rotated key
			// is an operator's repair and GCM cannot tell the two apart from here, so this arm
			// answers for the worse one. Attributed to the row that FAILED rather than to
			// whoever asked — a project with no connection of its own falls back to the LF
			// system row, and one corrupt system row would otherwise surface as unrelated
			// failures scattered across every project that fell back to it. No error text: the
			// chain ends in the Encryptor interface's own error, and safeErrSummary is a
			// normaliser, not a redactor.
			credentialProject := p.ProjectID
			if errors.Is(werr, domain.ErrSystemConnectionOrigin) {
				credentialProject = model.SystemProjectID
			}
			slog.ErrorContext(ctx, "campaign budget change blocked: stored credentials could not be decrypted (key mismatch or corrupted row)",
				"project_id", credentialProject, "requested_by_project_id", p.ProjectID,
				"brief_id", p.BriefID, "campaign_id", p.CampaignID, "platform", existing.Platform)
			return nil, &briefs.InternalServerError{Code: "500", Message: "the campaign budget could not be changed"}
		case errors.Is(werr, domain.ErrConnectionNotUsable):
			// Refused before the platform was contacted, so nothing is ambiguous — this must
			// sit above the unconfirmed check as well as the default, whose 503 would tell the
			// caller to retry a request that cannot succeed until a human edits the connection.
			// Logged with the fixed reason token rather than the error, for the reason at
			// unusableConnectionReason.
			slog.WarnContext(ctx, "campaign budget change blocked: the project's connection is not usable",
				"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
				"platform", existing.Platform, "reason", unusableConnectionReason(werr))
			return nil, &briefs.ConflictError{Code: "409", Message: "this project's ad-platform connection is not ready — its stored credentials or provider settings need attention; repair the connection before changing a campaign budget"}
		case errors.Is(werr, domain.ErrNotFound):
			// No connection row for (project, provider) and no shared system account either.
			// PERMANENT, so not the 503 default: nothing exists to retry against.
			slog.WarnContext(ctx, "campaign budget change blocked: no connection configured for this project and provider",
				"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
				"platform", existing.Platform)
			return nil, &briefs.NotFoundError{Code: "404", Message: "this project has no connection for the campaign's channel; connect it before changing a campaign budget"}
		case errors.As(werr, &unconfirmed) && unconfirmed.Unconfirmed():
			// UNCONFIRMED: a transport error, 5xx or redirect means the mutate MAY already have
			// applied. The row is deliberately left untouched — it might already be right, or
			// not. A retry is SAFE once verified (setting the same amount twice converges on
			// identical state, which is why the client sends this mutate as idempotent), but
			// the caller is still told to verify, because the amount they see in this service
			// and the amount the platform now holds may differ until they do.
			//
			// The claim lock is held for a bounded cooldown rather than released inline: an
			// immediate release lets the next caller claim the SAME still-unbumped version and
			// write the platform again while this call's outcome is unknown. A crash still
			// releases it at once — it is a Postgres session lock.
			slog.WarnContext(ctx, "campaign budget change outcome is UNCONFIRMED (the platform may or may not reflect the new amount)",
				"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
				"platform", existing.Platform, "platform_campaign_id", existing.PlatformCampaignID,
				"requested_budget_type", budgetType, "error", werr)
			releaseNow = false
			campaignRepo.ReleaseCampaignLockAfterCooldown(lockToken, unconfirmedLockCooldown)
			return nil, &briefs.ConnServiceUnavailableError{Code: "503", Message: "the campaign budget change is unconfirmed — it may or may not have been applied on the ad platform; verify the budget in the platform before retrying"}
		default:
			// A DEFINITE platform-call failure (4xx), or a repository error while loading the
			// connection: the platform was not changed. A bare repo failure is genuinely
			// transient, which is why it keeps the 503 the arms above no longer share. The
			// underlying error is logged; the client gets only the sanitized message.
			slog.WarnContext(ctx, "campaign budget change failed on the ad platform",
				"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
				"platform", existing.Platform, "platform_campaign_id", existing.PlatformCampaignID,
				"requested_budget_type", budgetType, "error", werr)
			return nil, &briefs.ConnServiceUnavailableError{Code: "503", Message: "the campaign budget could not be changed on the ad platform; the campaign was not modified"}
		}
	}

	// The platform change ALREADY committed. The row MUST catch up even if the request context
	// is now cancelled (client disconnect / shutdown) — otherwise the platform carries the new
	// budget while the row still reports the old one, a silent divergence with no compensating
	// rollback. Persist on a cancel-detached context, BOUNDED by persistResultTimeout so a
	// stuck DB cannot hang shutdown grace. Only the budget columns are touched: Status in
	// particular is left exactly as found, which is what preserves a 'created_degraded'
	// campaign's reconciliation marker through a budget change.
	existing.BudgetAmount = &budget
	existing.BudgetType = &budgetType
	// Resolve the actor from the LIVE ctx, before persistCtx replaces it below.
	existing.UpdatedBy = attributedActor(ctx, "update campaign budget")
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistResultTimeout)
	defer cancel()
	// Gate the final write on the original claimed version: the claim took the lock but did
	// NOT bump the version, and ReplaceCampaign bumps it inside the outbox transaction, so
	// every campaign write co-commits its index event.
	updated, uerr := campaignRepo.ReplaceCampaign(persistCtx, existing, version, lockToken, s.campaignIndexPayload(indexer.ActionUpdated))
	if uerr != nil {
		// The platform WAS changed but the row write failed → platform and DB now diverge.
		// Log it loudly as a reconcile signal (the platform is authoritative) before surfacing.
		slog.ErrorContext(ctx, "campaign budget changed on the platform but the DB row write failed (platform/DB diverged)",
			"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
			"platform", existing.Platform, "platform_campaign_id", existing.PlatformCampaignID,
			"new_budget_type", budgetType, "error", uerr)
		return nil, mapBriefErr(uerr)
	}
	return campaignResult(updated), nil
}
