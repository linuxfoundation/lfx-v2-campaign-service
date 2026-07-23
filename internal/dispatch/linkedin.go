// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
)

// campaignStatusGroupCreated marks a LinkedIn dispatch where the campaign GROUP was
// created but the CAMPAIGN was not — a recoverable orphan whose group id lives in
// Result. Distinct from campaignStatusCreated so the degraded state is visible, and
// PlatformCampaignID is left empty so the orchestrator's idempotency fast path does NOT
// treat it as a completed campaign (it keys reuse on a real id + terminal status). The
// retained claim then blocks a blind re-dispatch; actual recovery awaits the planned
// reconciliation/single-flight support (LFXV2-2665). See campaignFromLinkedIn.
const campaignStatusGroupCreated = "group_created"

// campaignStatusUnconfirmed marks a LinkedIn dispatch where NEITHER the campaign nor
// its group was confirmed created — a group-ambiguous partial returns both
// CampaignID == "" and CampaignGroupID == "". Distinct from campaignStatusCreated so
// the object is never falsely labelled "created" when nothing was confirmed; the
// claim is retained by the caller and Result carries the reconcile blob.
const campaignStatusUnconfirmed = "unconfirmed"

// linkedinCreds mirrors LinkedinAdsCredentials's field name (no json tag) — the
// persisted JSON key is the Go field name (AccessToken), see redditCreds. LinkedIn
// authenticates with a single OAuth2 bearer access token.
type linkedinCreds struct {
	AccessToken string
}

// linkedinConfig is the per-platform campaign config the caller passes for LinkedIn
// in CreateCampaigns' Input.Config (delivered here as the Dispatch `config`). The
// brief supplies event identity; the connection supplies account/org; this supplies
// the LinkedIn-specific campaign shape. RuntimeConfig fields that aren't persisted on
// the connection (targeting profiles, employer exclusions, extra accounts) may be
// supplied here for the client to resolve TargetingProfile against.
type linkedinConfig struct {
	BudgetUSD          float64                           `json:"budgetUsd"`
	LifetimeBudget     bool                              `json:"lifetimeBudget"`
	StartDate          string                            `json:"startDate"` // YYYY-MM-DD
	EndDate            string                            `json:"endDate"`   // YYYY-MM-DD
	GeoTargets         []linkedin.GeoTarget              `json:"geoTargets"`
	TargetingProfile   string                            `json:"targetingProfile"`
	Variants           []linkedin.CreativeVariant        `json:"variants"`
	AdAccountID        string                            `json:"adAccountId"`
	TargetingProfiles  []linkedin.TargetingProfileConfig `json:"targetingProfiles"`
	EmployerExclusions []string                          `json:"employerExclusions"`
}

// LinkedInDispatcher creates LinkedIn campaigns for the orchestrator.
type LinkedInDispatcher struct {
	creds *credsSource
	opts  []linkedin.Option
}

// NewLinkedInDispatcher builds the adapter from the connection repo + encryptor.
func NewLinkedInDispatcher(repo connReader, enc domain.Encryptor, opts ...linkedin.Option) *LinkedInDispatcher {
	return &LinkedInDispatcher{creds: newCredsSource(repo, enc), opts: opts}
}

// Dispatch implements service.PlatformDispatcher for LinkedIn.
//
// RETRY CAVEAT: unlike the reddit/twitter clients, the LinkedIn client's CreateCampaign
// is NOT idempotent — dark posts and creatives have no name-based find-or-create lookup,
// so a blind re-dispatch after an ambiguous failure would DUPLICATE them. A non-nil
// partial result returned alongside an error therefore means "may exist — do NOT blindly
// retry"; the orchestrator RETAINS the claim on it, and safe re-dispatch depends on the
// planned per-(brief, platform) single-flight guard (LFXV2-2665). Callers must not treat
// a LinkedIn ambiguous error as freely retryable the way name-idempotent platforms are.
func (d *LinkedInDispatcher) Dispatch(ctx context.Context, brief *model.CampaignBrief, platform model.Provider, config json.RawMessage) (*model.Campaign, error) {
	res, err := d.creds.resolve(ctx, brief.ProjectID, platform)
	if err != nil {
		return nil, err // preCreateError
	}
	if res.status != model.StatusActive {
		return nil, notCreated(fmt.Errorf("linkedin connection for project %s is %s, not active", brief.ProjectID, res.status))
	}

	var creds linkedinCreds
	if err := json.Unmarshal(res.plaintext, &creds); err != nil {
		return nil, notCreated(fmt.Errorf("decode linkedin credentials: %w", err))
	}
	if strings.TrimSpace(creds.AccessToken) == "" {
		return nil, notCreated(fmt.Errorf("linkedin credentials are incomplete (need accessToken)"))
	}

	orgID := strings.TrimSpace(res.providerConfig["org_id"])
	accountID := strings.TrimSpace(res.accountID)
	if accountID == "" || orgID == "" {
		return nil, notCreated(fmt.Errorf("linkedin connection for project %s is missing account id or org id", brief.ProjectID))
	}

	var cfg linkedinConfig
	if err := unmarshalPlatformConfig(config, "linkedInConfig", &cfg); err != nil {
		return nil, notCreated(err)
	}
	// Reject an empty variant set BEFORE any upstream create. The client also refuses
	// it (nil, err) after its own validation, but checking up front avoids the wasted
	// input build + upstream round-trip and keeps the claim-release semantics obvious
	// (a pre-create failure releases the claim). Credential/connection resolution has
	// already happened above; this only short-circuits the create itself.
	if len(cfg.Variants) == 0 {
		return nil, notCreated(fmt.Errorf("linkedin campaign creation requires at least one creative variant"))
	}
	bf, err := decodeBriefFields(brief)
	if err != nil {
		return nil, notCreated(err)
	}

	// Build the runtime config from the connection (account/org) plus any richer
	// bits the caller supplied in config (targeting profiles / exclusions the
	// connection doesn't persist). The single account is always present so
	// AdAccountID defaults resolve.
	// The runtime allowlist is sourced ONLY from the connection's own account. Do NOT
	// append a caller-supplied adAccountId — that would defeat the client's
	// cross-tenant fail-closed check (targeting.go), letting any account reachable by
	// the bearer token be treated as authorized and paired with this connection's org.
	// A caller override is therefore only honored when it MATCHES the connection's
	// account; any other value is rejected before an upstream call.
	runtime := linkedin.RuntimeConfig{
		DefaultAccountID:   accountID,
		DefaultOrgID:       orgID,
		Accounts:           []linkedin.Account{{AccountID: accountID, OrgID: orgID, Label: res.label}},
		TargetingProfiles:  cfg.TargetingProfiles,
		EmployerExclusions: cfg.EmployerExclusions,
	}
	// Trim the override once and use the TRIMMED value both for the guard AND for the
	// client input — otherwise a whitespace-padded value that matches the connection
	// passes the guard but reaches the client as a different (padded) account.
	adAccountID := strings.TrimSpace(cfg.AdAccountID)
	if adAccountID != "" && adAccountID != accountID {
		return nil, notCreated(fmt.Errorf("linkedin adAccountId %q does not match the connection's account %q — cross-account campaigns are not allowed", adAccountID, accountID))
	}

	// hsToken is a documented TOP-LEVEL config envelope field (docs/api-catalog.md);
	// a request-supplied token takes precedence over the brief blobs, so a config
	// hsToken drives the dark-post utm_campaign instead of being silently ignored.
	hsToken, err := envelopeHSToken(config)
	if err != nil {
		return nil, notCreated(err) // a wrong-typed hsToken is a caller error (pre-create)
	}
	if hsToken == "" {
		hsToken = bf.HSToken
	}

	in := linkedin.CampaignInput{
		EventName:       bf.EventName,
		RegistrationURL: bf.RegistrationURL,
		HSToken:         hsToken,
		// Project stamped from the authenticated scope, not caller JSON (api-catalog).
		Project:          brief.ProjectID,
		BudgetUSD:        cfg.BudgetUSD,
		LifetimeBudget:   cfg.LifetimeBudget,
		StartDate:        cfg.StartDate,
		EndDate:          cfg.EndDate,
		GeoTargets:       cfg.GeoTargets,
		TargetingProfile: cfg.TargetingProfile,
		Variants:         cfg.Variants,
		AdAccountID:      adAccountID,
	}

	client := linkedin.NewClient(linkedin.Credentials{AccessToken: creds.AccessToken}, runtime, d.opts...)

	// Release the claim ONLY when result==nil (definitely nothing created). Do NOT
	// gate on an empty CampaignID: LinkedIn returns a NON-NIL result even on a
	// DEFINITE campaign failure once the campaign GROUP was created (a permanent
	// resource carrying CampaignGroupID) — and on an ambiguous campaign create it
	// returns a non-nil name-only partial with an empty CampaignID. Both must RETAIN
	// the claim so a retry doesn't duplicate the group/campaign.
	result, cerr := client.CreateCampaign(ctx, in)
	if cerr != nil {
		if result == nil {
			return nil, notCreated(fmt.Errorf("linkedin campaign creation failed before any upstream create: %w", cerr))
		}
		// A non-nil result means a permanent resource exists (campaign group, and maybe
		// the campaign). This covers BOTH an ambiguous create AND a definite campaign
		// failure after a successful group create — either way the claim must be retained.
		return campaignFromLinkedIn(ctx, result, len(cfg.Variants), cfg), fmt.Errorf("linkedin campaign creation incomplete (a campaign group and/or campaign may exist): %w", cerr)
	}
	return campaignFromLinkedIn(ctx, result, len(cfg.Variants), cfg), nil
}

// campaignFromLinkedIn maps the client result to the persistence model: upstream id,
// name, result blob, the budget/schedule/ConfigSnapshot (via applyCampaignConfig), and
// a status derived from what was confirmed created — one of `created`,
// `created_degraded` (creative shortfall), `group_created` (group only), or
// `unconfirmed` (neither id). requestedVariants is how many creatives the caller asked
// for, used to detect a creative shortfall.
// ToggleStatus pauses or resumes an existing LinkedIn campaign on the platform. It resolves
// the connection (active + access token; a status update needs the account id but not the
// org id, which is creation-only), builds the client, and issues the RestLi PARTIAL_UPDATE.
// platformCampaignID is the numeric campaign id; status is model.CampaignRunActive/Paused. An
// UNCONFIRMED outcome is wrapped so the caller reports "verify before retry".
func (d *LinkedInDispatcher) ToggleStatus(ctx context.Context, projectID string, platform model.Provider, platformCampaignID, status string) error {
	liStatus, err := linkedinRunStatus(status)
	if err != nil {
		return err
	}
	res, err := d.creds.resolve(ctx, projectID, platform)
	if err != nil {
		return err
	}
	if res.status != model.StatusActive {
		return fmt.Errorf("linkedin connection for project %s is %s, not active", projectID, res.status)
	}
	var creds linkedinCreds
	if err := json.Unmarshal(res.plaintext, &creds); err != nil {
		return fmt.Errorf("decode linkedin credentials: %w", err)
	}
	if strings.TrimSpace(creds.AccessToken) == "" {
		return fmt.Errorf("linkedin credentials are incomplete (need accessToken)")
	}
	accountID := strings.TrimSpace(res.accountID)
	if accountID == "" {
		return fmt.Errorf("linkedin connection for project %s has no account id", projectID)
	}
	runtime := linkedin.RuntimeConfig{
		DefaultAccountID: accountID,
		Accounts:         []linkedin.Account{{AccountID: accountID, Label: res.label}},
	}
	client := linkedin.NewClient(linkedin.Credentials{AccessToken: creds.AccessToken}, runtime, d.opts...)
	if uerr := client.UpdateCampaignStatus(ctx, platformCampaignID, liStatus); uerr != nil {
		if linkedin.IsOutcomeUnconfirmed(uerr) {
			return &unconfirmedToggleError{err: uerr}
		}
		return uerr
	}
	return nil
}

// linkedinRunStatus maps the service run state (active/paused) to LinkedIn's status enum.
func linkedinRunStatus(status string) (string, error) {
	switch status {
	case model.CampaignRunActive:
		return linkedin.StatusActive, nil
	case model.CampaignRunPaused:
		return linkedin.StatusPaused, nil
	default:
		return "", fmt.Errorf("unsupported campaign run status %q (want %q or %q)", status, model.CampaignRunActive, model.CampaignRunPaused)
	}
}

func campaignFromLinkedIn(ctx context.Context, r *linkedin.CampaignResult, requestedVariants int, cfg linkedinConfig) *model.Campaign {
	c := &model.Campaign{
		PlatformCampaignID: r.CampaignID,
		CampaignName:       r.CampaignName,
	}
	// Derive the status from what was actually confirmed created. Start UNCONFIRMED and
	// only claim `created` once a real campaign id exists — a group-*ambiguous* partial
	// returns BOTH CampaignID == "" and CampaignGroupID == "" (client.go buildResult on
	// the group-create failure path), and defaulting to `created` there would stamp
	// "created" on an object where nothing was confirmed.
	switch {
	case r.CampaignID != "":
		c.Status = campaignStatusCreated
		if r.CreativeCount < requestedVariants {
			// The campaign exists but fewer creatives were created than requested — a
			// DEGRADED success (mirrors the reddit/meta/twitter created_degraded handling).
			// NOTE: today the LinkedIn client aborts (returns an error) on the FIRST
			// creative failure, so a clean (result, nil) success normally has
			// CreativeCount == requested; this guard is defensive so a shortfall is never
			// silently reported as a clean `created` (and flags the count on the
			// retained-error path).
			c.Status = campaignStatusCreatedDegraded
		}
	case r.CampaignGroupID != "":
		// The campaign GROUP was created but the CAMPAIGN failed/left-ambiguous with an
		// EMPTY CampaignID. We must NOT stuff the group id into PlatformCampaignID: the
		// orchestrator's idempotency treats ANY non-empty PlatformCampaignID as "campaign
		// finished upstream" and short-circuits a later dispatch to success — so a
		// group-only orphan would look permanently succeeded and the campaign would never
		// be created on retry. PlatformCampaignID stays EMPTY (no campaign exists) so the
		// idempotency fast path does NOT treat it as complete; the retained claim then
		// blocks a blind re-dispatch (recovery awaits LFXV2-2665). The group orphan is
		// preserved in Result (CampaignGroupID) + the group_created status for reconciliation.
		c.Status = campaignStatusGroupCreated
	default:
		// Neither id present — a group-ambiguous partial where even the group create is
		// unconfirmed. Leave the status unconfirmed rather than falsely `created`; the
		// claim is retained by the caller and Result carries the reconcile blob.
		c.Status = campaignStatusUnconfirmed
	}
	// Persist the budget/schedule/config the caller supplied (LinkedIn honors a
	// lifetime-vs-daily budget flag). ConfigSnapshot captures the validated config.
	applyCampaignConfig(ctx, c, cfg.BudgetUSD, cfg.LifetimeBudget, cfg.StartDate, cfg.EndDate, cfg)
	if raw, err := json.Marshal(r); err != nil {
		// A marshal failure should be near-impossible for this plain struct, but do NOT
		// swallow it: leaving Result empty would make an orphaned campaign harder to
		// reconcile. Log it (the row is still persisted with its id/status).
		slog.WarnContext(ctx, "failed to marshal linkedin campaign result blob (Result left empty)",
			"campaign_id", c.PlatformCampaignID, "error", err)
	} else {
		c.Result = raw
	}
	return c
}
