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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
)

// googleAdsCreds is the credential shape stored (encrypted) for a Google Ads
// connection. Google Ads authenticates with an OAuth2 application (client id/secret)
// plus a long-lived refresh token, AND a Google Ads API developer token. Field names
// mirror the generated GoogleAdsCredentials struct EXACTLY (no json tags): the
// connection service persists creds via json.Marshal on the tag-less generated struct,
// so the stored JSON keys are the Go field names (PascalCase) — matching them
// field-for-field avoids relying on encoding/json's case-insensitive fallback (see
// redditCreds).
type googleAdsCreds struct {
	ClientID       string
	ClientSecret   string
	DeveloperToken string
	RefreshToken   string
}

// googleAdsKeywordConfig is one entry in googleAdsConfig.Keywords — the JSON shape a
// caller supplies for a positive Search keyword criterion (GA-4). Maps 1:1 to
// googleads.Keyword; kept as a separate JSON-tagged type here rather than importing
// googleads' struct directly, mirroring how the rest of this file keeps the wire
// shape and the platform client's Go type independently named.
type googleAdsKeywordConfig struct {
	Text      string `json:"text"`
	MatchType string `json:"matchType"`
}

// googleAdsConfig is the per-platform campaign config the caller passes for Google Ads
// in CreateCampaigns' Input.Config (delivered here as the Dispatch `config`). The GA
// client creates a PAUSED search campaign with an ad group + a Responsive Search Ad
// (GA-3b) and can attach keyword/audience targeting to that ad group (GA-4). Budget is
// in whole units of the ad ACCOUNT's currency (NOT USD — the client does no FX),
// mirroring the meta client.
type googleAdsConfig struct {
	Budget float64 `json:"budget"`
	// Headlines/Descriptions are optional Responsive Search Ad copy overrides (GA-3b).
	// Left nil/empty, the client composes deterministic placeholder copy from the
	// brief's EventName/Project (see googleads.composeAdCopy).
	Headlines    []string `json:"headlines"`
	Descriptions []string `json:"descriptions"`
	// Keywords are optional positive Search keyword criteria (GA-4). Left empty, the
	// ad group created by GA-3 gets no criteria and can never serve — see
	// googleads.Keyword/validateKeywords.
	Keywords []googleAdsKeywordConfig `json:"keywords"`
	// AudienceSegments are optional EXISTING Google Ads audience resource names (GA-4)
	// — a Customer Match user list or custom audience the caller already built
	// elsewhere (e.g. this project's campaign_audiences resource), not created by this
	// dispatcher. See googleads.validateAudienceSegments for the accepted shapes.
	AudienceSegments []string `json:"audienceSegments"`
}

// GoogleAdsDispatcher creates Google Ads campaigns for the orchestrator.
type GoogleAdsDispatcher struct {
	creds *credsSource
	opts  []googleads.Option
}

// NewGoogleAdsDispatcher builds the adapter from the connection repo + encryptor.
func NewGoogleAdsDispatcher(repo connReader, enc domain.Encryptor, opts ...googleads.Option) *GoogleAdsDispatcher {
	return &GoogleAdsDispatcher{creds: newCredsSource(repo, enc), opts: opts}
}

// Dispatch implements service.PlatformDispatcher for Google Ads.
func (d *GoogleAdsDispatcher) Dispatch(ctx context.Context, brief *model.CampaignBrief, platform model.Provider, config json.RawMessage) (*model.Campaign, error) {
	// Resolve creds FIRST (pre-create): a missing/undecryptable connection is a
	// not-created error → the orchestrator releases the claim.
	res, err := d.creds.resolve(ctx, brief.ProjectID, platform)
	if err != nil {
		return nil, err // already a preCreateError
	}
	// validateGoogleAdsConnection is shared with ToggleStatus so a create and a toggle accept
	// EXACTLY the same connections and cannot drift. Its failures are wrapped with notCreated
	// HERE — create-only claim semantics the toggle path must not apply.
	creds, accountID, err := validateGoogleAdsConnection(brief.ProjectID, res)
	if err != nil {
		return nil, notCreated(err)
	}

	var cfg googleAdsConfig
	if err := unmarshalPlatformConfig(config, "googleAdsConfig", &cfg); err != nil {
		return nil, notCreated(err)
	}
	bf, err := decodeBriefFields(brief)
	if err != nil {
		return nil, notCreated(err)
	}

	in := googleads.CampaignInput{
		EventName: bf.EventName,
		EventSlug: brief.EventSlug,
		// Project is stamped from the AUTHENTICATED project scope (brief.ProjectID),
		// never from caller JSON — the Project name segment is the data pipeline's
		// attribution join key (docs/api-catalog.md), so it must be the canonical LFX
		// slug (matches reddit/meta/twitter).
		Project:          brief.ProjectID,
		Budget:           cfg.Budget,
		RegistrationURL:  bf.RegistrationURL,
		Headlines:        cfg.Headlines,
		Descriptions:     cfg.Descriptions,
		Keywords:         googleAdsKeywords(cfg.Keywords),
		AudienceSegments: cfg.AudienceSegments,
		// NameSuffix = the brief id gives deterministic, at-most-once-retry names: the
		// GA client composes the budget/campaign/ad-group names from these, and a retry
		// with the same suffix hits Google's DUPLICATE_NAME (reported
		// UNCONFIRMED-already-exists) rather than creating a second paid campaign — a
		// poor-man's idempotency key until LFXV2-2665 lands provider idempotency keys.
		NameSuffix: brief.ID,
	}

	// login_customer_id is the OPTIONAL manager (MCC) account the ad account is accessed
	// through; it lives in the connection's ProviderConfig (not the credential blob).
	client := googleads.NewClient(
		googleads.Credentials{
			ClientID:       creds.ClientID,
			ClientSecret:   creds.ClientSecret,
			DeveloperToken: creds.DeveloperToken,
			RefreshToken:   creds.RefreshToken,
		},
		googleads.AccountConfig{
			CustomerID:      accountID,
			LoginCustomerID: strings.TrimSpace(res.providerConfig["login_customer_id"]),
			Label:           res.label,
		},
		d.opts...,
	)

	// The GA client's contract (mirrors reddit/meta/twitter): (nil, err) ONLY when
	// NOTHING was (or may have been) created — a validation/pre-send/definite failure.
	// Otherwise it returns a NON-NIL partial result alongside the error (an ambiguous
	// create, or a duplicate-name that means "already exists", gives a name-only result
	// whose ids may be empty but which still means "may exist"). So the release decision
	// keys on result==nil ALONE — not on an empty id, which would wrongly release the
	// claim on an ambiguous partial and risk a duplicate on retry. Note GA's two-step
	// hierarchy (budget → campaign): a PRE-attachment (budget-stage) orphan is reconciled by
	// its deterministic CampaignBudgetName; but once the campaign attaches, a non-shared
	// budget's name SYNCHRONIZES to the campaign name, so a campaign-stage partial reconciles
	// the budget by CampaignBudgetID instead (see the client's campaignNamePartial contract).
	// Both keys are preserved in the Result blob.
	//   - (nil, err)      → pre-create; notCreated releases the claim.
	//   - (result, err)   → may exist; return the (possibly id-less) campaign + error so
	//                       the orchestrator retains the claim and records the orphan.
	//   - (result, nil)   → success.
	result, cerr := client.CreateCampaign(ctx, in)
	if cerr != nil {
		if result == nil {
			return nil, notCreated(fmt.Errorf("google ads campaign creation failed before any upstream create: %w", cerr))
		}
		// A non-nil result means SOMETHING may exist upstream (an ambiguous create, a
		// duplicate-name "already exists", or a definite campaign 4xx that still left a
		// created budget orphan), so the claim is RETAINED and the orphan recorded either
		// way. Do NOT prepend "UNCONFIRMED": the client already classifies the outcome
		// precisely — "UNCONFIRMED (may exist)" for an ambiguous create vs "creation failed
		// (budget created)" for a definite 4xx — so a blanket prefix would overwrite that
		// distinction and route an operator to reconcile an ambiguous outcome for what is
		// actually a definite failure. Wrap with a neutral, provider-tagged prefix instead.
		return campaignFromGoogleAds(ctx, result, cfg), fmt.Errorf("google-ads dispatch: %w", cerr)
	}
	return campaignFromGoogleAds(ctx, result, cfg), nil
}

// googleAdsKeywords maps the wire-shaped keyword config to the platform client's
// Keyword type. Returns nil for an empty input so an omitted "keywords" field stays
// nil end-to-end rather than becoming an empty-but-non-nil slice.
func googleAdsKeywords(in []googleAdsKeywordConfig) []googleads.Keyword {
	if len(in) == 0 {
		return nil
	}
	out := make([]googleads.Keyword, len(in))
	for i, kw := range in {
		out[i] = googleads.Keyword{Text: kw.Text, MatchType: kw.MatchType}
	}
	return out
}

// campaignFromGoogleAds maps the client result to the persistence model. The
// orchestrator fills project/brief/job/platform (and, for a retained ambiguous orphan,
// status); this sets what only the dispatcher knows — upstream id, name, the persisted
// budget/type/config, the provider result blob, and a "created" status on the success path.
func campaignFromGoogleAds(ctx context.Context, r *googleads.CampaignResult, cfg googleAdsConfig) *model.Campaign {
	c := &model.Campaign{
		PlatformCampaignID: r.CampaignID,
		CampaignName:       r.CampaignName,
		Status:             campaignStatusCreated,
	}
	// Persist the caller-supplied budget + validated config, mirroring the sibling adapters
	// (a NULL budget/type/config_snapshot row otherwise loses the campaign's configuration).
	// GA's shell uses a DAILY budget (no lifetime flag) and sets no flight dates here — those
	// land with GA-3+; ConfigSnapshot captures the validated config regardless.
	applyCampaignConfig(ctx, c, cfg.Budget, false, "", "", cfg)
	if raw, err := json.Marshal(r); err != nil {
		// A marshal failure should be near-impossible for this plain struct, but do NOT
		// swallow it: Result is the sole carrier of the reconcile-by-name payload (the
		// deterministic CampaignBudgetName) on the ambiguous-orphan path, so a silently-empty
		// Result loses reconciliation data precisely when it's most needed. Log it (the row is
		// still persisted with its id/status/config). Mirrors the meta/twitter/linkedin adapters.
		slog.WarnContext(ctx, "failed to marshal google ads campaign result blob (Result left empty)",
			"campaign_id", c.PlatformCampaignID, "error", err)
	} else {
		c.Result = raw
	}
	return c
}

// validateGoogleAdsConnection checks a resolved connection is usable and returns the decoded
// credentials + trimmed customer id. Shared by Dispatch and ToggleStatus so a create and a
// toggle accept EXACTLY the same connections; each caller applies its own error wrapping
// (Dispatch wraps with notCreated for claim semantics, the toggle path does not).
//
// The customer id is trimmed ONCE and the trimmed value returned, so a whitespace-padded id
// can't pass the empty check here and then fail the client's digits-only validator as a
// confusing downstream error.
func validateGoogleAdsConnection(projectID string, res *resolved) (googleAdsCreds, string, error) {
	var creds googleAdsCreds
	if res.status != model.StatusActive {
		return creds, "", fmt.Errorf("google ads connection for project %s is %s, not active", projectID, res.status)
	}
	if err := json.Unmarshal(res.plaintext, &creds); err != nil {
		return creds, "", fmt.Errorf("decode google ads credentials: %w", err)
	}
	if creds.ClientID == "" || creds.ClientSecret == "" || creds.DeveloperToken == "" || creds.RefreshToken == "" {
		return creds, "", fmt.Errorf("google ads credentials are incomplete (need clientId, clientSecret, developerToken, refreshToken)")
	}
	accountID := strings.TrimSpace(res.accountID)
	if accountID == "" {
		return creds, "", fmt.Errorf("google ads connection for project %s has no account id (customer id)", projectID)
	}
	return creds, accountID, nil
}

// resolveGoogleAdsClient resolves + validates the project's connection and builds a client
// for the TOGGLE path (see validateGoogleAdsConnection for the shared rules).
func (d *GoogleAdsDispatcher) resolveGoogleAdsClient(ctx context.Context, projectID string, platform model.Provider) (*googleads.Client, error) {
	res, err := d.creds.resolve(ctx, projectID, platform)
	if err != nil {
		return nil, err
	}
	creds, accountID, err := validateGoogleAdsConnection(projectID, res)
	if err != nil {
		return nil, err
	}
	return googleads.NewClient(
		googleads.Credentials{
			ClientID:       creds.ClientID,
			ClientSecret:   creds.ClientSecret,
			DeveloperToken: creds.DeveloperToken,
			RefreshToken:   creds.RefreshToken,
		},
		googleads.AccountConfig{
			CustomerID:      accountID,
			LoginCustomerID: strings.TrimSpace(res.providerConfig["login_customer_id"]),
			Label:           res.label,
		},
		d.opts...,
	), nil
}

// googleAdsRunStatus maps the service's run-state vocabulary to Google's campaign status.
// Note Google spells the serving state ENABLED, not ACTIVE.
func googleAdsRunStatus(status string) (string, error) {
	switch status {
	case model.CampaignRunActive:
		return googleads.StatusEnabled, nil
	case model.CampaignRunPaused:
		return googleads.StatusPaused, nil
	default:
		return "", fmt.Errorf("unsupported campaign run status %q (want %q or %q)", status, model.CampaignRunActive, model.CampaignRunPaused)
	}
}

// ToggleStatus implements service.StatusToggler for Google Ads.
//
// GA-3b gave the create path a real ad group + ad, so this now cascades like the reddit
// adapter: PAUSE flips the campaign first (stops delivery immediately, regardless of whether
// the children can be reached), ACTIVATE flips the children FIRST and the campaign last (so a
// campaign never reports ENABLED before its ad group/ad do — the reverse order could have the
// campaign live for a moment with paused children). ACTIVATE is still refused up front when
// either child id is unknown — a campaign whose ad-group/ad create hit a duplicate-name orphan
// or an unconfirmed outcome (see createAdGroupAndAd) has no id to cascade to, so enabling just
// the campaign would report success while nothing can serve. That is exactly the lie
// ErrCampaignNotProvisioned exists to prevent (the service maps it to a 409 without calling
// Google), matching the guard reddit/microsoft apply when a child is missing.
func (d *GoogleAdsDispatcher) ToggleStatus(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, status string) error {
	gaStatus, err := googleAdsRunStatus(status)
	if err != nil {
		return err
	}
	// Refuse ACTIVATE: GA-3b creates ad group + ad but NO targeting criteria. A campaign
	// without keywords/audience targeting cannot deliver, so activating reports false
	// success (exactly what ErrCampaignNotProvisioned prevents). Activation will be
	// reconsidered in GA-4 once targeting provisioning is implemented and the Result blob
	// carries the targeting IDs.
	if gaStatus == googleads.StatusEnabled {
		return fmt.Errorf("%w: google ads campaign %s cannot be activated because keyword/audience targeting is not yet provisioned (GA-4 pending)", domain.ErrCampaignNotProvisioned, campaign.PlatformCampaignID)
	}
	adGroupID, adID := googleAdsChildIDs(campaign)
	client, err := d.resolveGoogleAdsClient(ctx, projectID, platform)
	if err != nil {
		return err
	}

	wrapUnconfirmed := func(uerr error) error {
		if googleads.IsOutcomeUnconfirmed(uerr) {
			return &unconfirmedToggleError{err: uerr}
		}
		return uerr
	}

	if gaStatus == googleads.StatusPaused {
		// Campaign first: pausing the parent stops delivery immediately even if the
		// child update below fails/is unconfirmed.
		if uerr := client.UpdateCampaignStatus(ctx, campaign.PlatformCampaignID, gaStatus); uerr != nil {
			return wrapUnconfirmed(uerr)
		}
		// If the ad group/ad ids are absent (e.g. a campaign shell with no fully-created
		// children), there is nothing to pause downstream — only the campaign is toggled.
		if strings.TrimSpace(adGroupID) == "" || strings.TrimSpace(adID) == "" {
			return nil
		}
		if uerr := client.UpdateAdGroupAndAdStatus(ctx, adGroupID, adID, gaStatus); uerr != nil {
			// After the campaign status succeeds, a child failure (even a definite 4xx) is a
			// partial cascade: the parent changed but the child's outcome is unknown. Wrap it
			// as Unconfirmed so the caller knows to verify the state before retry.
			return &unconfirmedToggleError{err: uerr}
		}
		return nil
	}

	// ACTIVATE (unreachable in GA-3c, re-enabled in GA-4): children first (both ids are
	// confirmed present by the guard above), campaign last — so the campaign only reports
	// ENABLED once its ad group/ad already do.
	if uerr := client.UpdateAdGroupAndAdStatus(ctx, adGroupID, adID, gaStatus); uerr != nil {
		// Any failure once we're past the guard is a partial cascade.
		return &unconfirmedToggleError{err: uerr}
	}
	if uerr := client.UpdateCampaignStatus(ctx, campaign.PlatformCampaignID, gaStatus); uerr != nil {
		// After the children succeed, a campaign failure is a partial cascade: children are
		// active but the parent's outcome is unknown.
		return &unconfirmedToggleError{err: uerr}
	}
	return nil
}

// googleAdsChildIDs pulls the ad group + ad ids the create path stored in the persisted
// CampaignResult blob (googleads.CampaignResult's AdGroupId/AdId), mirroring
// redditChildIDs. A missing/unparseable blob yields empty ids.
func googleAdsChildIDs(campaign *model.Campaign) (adGroupID, adID string) {
	if campaign == nil || len(campaign.Result) == 0 {
		return "", ""
	}
	var blob struct {
		AdGroupID string `json:"adGroupId"`
		AdID      string `json:"adId"`
	}
	if err := json.Unmarshal(campaign.Result, &blob); err != nil {
		return "", ""
	}
	return blob.AdGroupID, blob.AdID
}
