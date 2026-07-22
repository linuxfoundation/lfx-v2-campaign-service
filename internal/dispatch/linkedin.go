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
// PlatformCampaignID is left empty so a retry re-attempts the campaign (see
// campaignFromLinkedIn).
const campaignStatusGroupCreated = "group_created"

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
	// connection resolve / input build and keeps the claim-release semantics obvious
	// (a pre-create failure releases the claim).
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
	hsToken := envelopeHSToken(config)
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
		return campaignFromLinkedIn(ctx, result, len(cfg.Variants)), fmt.Errorf("linkedin campaign creation incomplete (a campaign group and/or campaign may exist): %w", cerr)
	}
	return campaignFromLinkedIn(ctx, result, len(cfg.Variants)), nil
}

// campaignFromLinkedIn maps the client result to the persistence model (upstream id,
// name, result blob, and a "created" status on success — see campaignFromReddit).
// requestedVariants is how many creatives the caller asked for, used to detect a
// creative shortfall (degraded).
func campaignFromLinkedIn(ctx context.Context, r *linkedin.CampaignResult, requestedVariants int) *model.Campaign {
	c := &model.Campaign{
		PlatformCampaignID: r.CampaignID,
		CampaignName:       r.CampaignName,
		Status:             campaignStatusCreated,
	}
	// LinkedIn can create the campaign GROUP but fail/leave-ambiguous the CAMPAIGN,
	// returning CampaignGroupID with an EMPTY CampaignID. We must NOT stuff the group
	// id into PlatformCampaignID: the orchestrator's idempotency treats ANY non-empty
	// PlatformCampaignID as "campaign finished upstream" and short-circuits a later
	// dispatch to success — so a group-only orphan would look permanently succeeded
	// and the campaign would never be created on retry. PlatformCampaignID stays EMPTY
	// (no campaign exists) so a retry re-attempts; the group orphan is preserved in
	// Result (below, CampaignGroupID) + the group_created status for reconciliation.
	if c.PlatformCampaignID == "" && r.CampaignGroupID != "" {
		c.Status = campaignStatusGroupCreated
	} else if r.CampaignID != "" && r.CreativeCount < requestedVariants {
		// The campaign exists but fewer creatives were created than requested — a
		// DEGRADED success (mirrors the reddit/meta/twitter created_degraded handling).
		// NOTE: today the LinkedIn client aborts (returns an error) on the FIRST creative
		// failure, so a clean (result, nil) success normally has CreativeCount ==
		// requested; this guard is defensive so a shortfall is never silently reported as
		// a clean `created` (and it also flags the count on the retained-error path).
		c.Status = campaignStatusCreatedDegraded
	}
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
