// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
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
	// — Customer Match user-list resources the caller has already built elsewhere,
	// not created by this dispatcher. See googleads.validateAudienceSegments for the
	// accepted shapes.
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

	// ComposeName is deterministic in the brief, so a RETRIED dispatch asks for the same
	// name a previous attempt may already have created. Adopt it rather than create a
	// second PAID campaign. Only a verified absence licenses the create below:
	// FindCampaignByName errors on anything it cannot verify (transport failure, a name
	// that does not match the WHERE clause, a campaign in another customer), so the
	// lookup never reduces an unverifiable response to a clean "not found".
	campaignName := googleads.ComposeName("Search Campaign", in)
	adoptID, adoptErr := client.FindCampaignByName(ctx, campaignName)
	if adoptErr != nil {
		// notCreated: nothing was created upstream, so the orchestrator releases the
		// claim. FindCampaignByName already wraps with its own context.
		return nil, notCreated(adoptErr)
	}
	if adoptID != "" {
		return campaignFromGoogleAdsAdoption(ctx, adoptID, campaignName, accountID, res.label), nil
	}
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

// campaignFromGoogleAdsAdoption builds the campaign model for an ADOPTED campaign. The lookup
// answers only "a campaign with this name exists here" — it says nothing about the budget, ad
// group and ad a create would also have made, so an adoption of a campaign whose previous
// attempt died mid-sequence yields a campaign that will not serve. It is still the right
// outcome: the alternative is a duplicate paid campaign, and the shell is now recorded and
// visible for reconciliation rather than orphaned. Completing a partial adoption is
// LFXV2-3042's follow-up, and needs an ad-group lookup this client does not yet have.
func campaignFromGoogleAdsAdoption(ctx context.Context, campaignID, campaignName, accountID, accountLabel string) *model.Campaign {
	c := &model.Campaign{
		PlatformCampaignID: campaignID,
		CampaignName:       campaignName,
		Status:             campaignStatusCreated,
	}
	// The blob must carry CustomerID: googleAdsCreationCustomerID reads it to detect a
	// later read/toggle against a DIFFERENT customer, and treats an absent one as
	// "unknown, proceed" — so omitting it would silently disable that check.
	adoptionResult := &googleads.CampaignResult{
		Platform:     "google-ads",
		AccountLabel: accountLabel,
		CustomerID:   accountID,
		CampaignID:   campaignID,
		CampaignName: campaignName,
		GoogleAdsURL: "https://ads.google.com/aw/campaigns?ocid=" + accountID,
		Steps:        []string{"Campaign adopted: " + campaignID + " (already exists on account, no budget/ad group created)"},
	}
	if raw, err := json.Marshal(adoptionResult); err != nil {
		slog.WarnContext(ctx, "failed to marshal adoption result blob (Result left empty)",
			"campaign_id", campaignID, "error", err)
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
// GA-3b created ad group + ad under the campaign; GA-3c wired the dispatcher-level cascade;
// GA-4 enables ACTIVATE once targeting exists. Both directions cascade, in OPPOSITE orders.
//
// PAUSE cascades from the campaign FIRST (stops delivery immediately, regardless of whether the
// children can be reached) down to the ad group/ad via the persisted ids stored in the campaign's
// Result blob.
//
// ACTIVATE is refused with ErrCampaignNotProvisioned (→409, raised locally without calling
// Google) unless the Result blob shows the ad group/ad were fully provisioned AND at least one
// keyword criterion was persisted by GA-4's targeting step. A campaign without targeting cannot
// deliver, so activating it would report false success — the exact lie ErrCampaignNotProvisioned
// exists to prevent. When the guard passes, ACTIVATE cascades children-first (children activated
// before campaign) so a campaign never reports ENABLED before its children do.
func (d *GoogleAdsDispatcher) ToggleStatus(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, status string) error {
	gaStatus, err := googleAdsRunStatus(status)
	if err != nil {
		return err
	}
	// Refuse ACTIVATE if targeting was not successfully provisioned: GA-4 requires at least
	// one keyword criterion before allowing activation (audience criteria alone are
	// observation-only and do not qualify for activation, so they don't satisfy this gate).
	// Checked below via the persisted KeywordCriteriaIDs in the Result blob — empty means
	// keyword targeting was never attempted or failed before any criterion resource name
	// could be parsed.
	adGroupID, adID := googleAdsChildIDs(campaign)
	if gaStatus == googleads.StatusEnabled {
		// Refuse ACTIVATE if the ad group/ad were never fully provisioned: a duplicate-name
		// orphan or unconfirmed create (see createAdGroupAndAd) leaves no id to cascade to, so
		// enabling just the campaign would report success while nothing can serve.
		if strings.TrimSpace(adGroupID) == "" || strings.TrimSpace(adID) == "" {
			return fmt.Errorf("%w: google ads campaign %s cannot be activated because its ad group/ad were not fully provisioned", domain.ErrCampaignNotProvisioned, campaign.PlatformCampaignID)
		}
		var result googleads.CampaignResult
		if campaign.Result != nil {
			_ = json.Unmarshal(campaign.Result, &result)
		}
		if len(result.KeywordCriteriaIDs) == 0 {
			return fmt.Errorf("%w: google ads campaign %s cannot be activated because keyword targeting is not yet provisioned (at least one keyword criterion is required)", domain.ErrCampaignNotProvisioned, campaign.PlatformCampaignID)
		}
	}
	client, err := d.resolveGoogleAdsClient(ctx, projectID, platform)
	if err != nil {
		return err
	}
	// Same identity invariant ReadMetrics enforces, and it matters MORE here because this
	// path MUTATES. The stored campaign/ad-group/ad ids are bare numerics, unique only
	// within the customer they were created under, while the connection just resolved is the
	// project's CURRENT one — UpdateGoogleAds can re-point it between create and toggle. On
	// an id collision the mutate succeeds against ANOTHER account's resources, pausing or
	// enabling something this project does not own. Fail before contacting Google, for both
	// PAUSE and ACTIVATE (the check sits above both branches deliberately).
	if created := googleAdsCreationCustomerID(campaign); created != "" && created != client.CustomerID() {
		return fmt.Errorf("toggle google ads campaign status: campaign %s was created under customer %s but the project's current connection resolves to customer %s: %w",
			campaign.PlatformCampaignID, created, client.CustomerID(), domain.ErrCampaignAccountMismatch)
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

	// ACTIVATE: children first (both ids are confirmed present, and keyword targeting is
	// confirmed provisioned, by the guard above), campaign last — so the campaign only
	// reports ENABLED once its ad group/ad already do.
	if uerr := client.UpdateAdGroupAndAdStatus(ctx, adGroupID, adID, gaStatus); uerr != nil {
		// UpdateAdGroupAndAdStatus tries ad group first, then ad (children-first ordering).
		// A definite first-child failure (4xx from adGroups:mutate) is NOT a partial cascade
		// (nothing changed). A definite second-child failure (4xx from adGroupAds:mutate after
		// adGroups succeeded) IS a partial cascade and returns partialCascadeError, which
		// wrapUnconfirmed correctly classifies as unconfirmed. Ambiguous outcomes (5xx/timeout)
		// are also wrapped as unconfirmed.
		return wrapUnconfirmed(uerr)
	}
	if uerr := client.UpdateCampaignStatus(ctx, campaign.PlatformCampaignID, gaStatus); uerr != nil {
		// After the children succeed, a campaign failure (even a definite 4xx) is a partial
		// cascade: the children already changed but the campaign's outcome is unknown. Wrap it
		// as Unconfirmed unconditionally so the caller knows to verify state before retry —
		// mirrors the PAUSE path's child-after-campaign wrap above.
		return &unconfirmedToggleError{err: uerr}
	}
	return nil
}

// ReadMetrics implements service.MetricsReader for Google Ads. It resolves the same
// connection ToggleStatus does and reads the campaign's live GAQL metrics.
//
// The platform-agnostic window is translated to Google's GAQL literal by
// googleads.WindowFor — in the platform package, not here, so the GAQL dialect stays behind
// that boundary. A window Google cannot express is reported as
// domain.ErrMetricsWindowUnsupported (400, caller input) rather than a 503, and the platform
// is never contacted for it.
func (d *GoogleAdsDispatcher) ReadMetrics(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, window model.MetricsWindow) (*model.CampaignMetrics, error) {
	gaWindow, err := googleads.WindowFor(window)
	if err != nil {
		return nil, fmt.Errorf("read google ads metrics: %w", errors.Join(domain.ErrMetricsWindowUnsupported, err))
	}
	client, err := d.resolveGoogleAdsClient(ctx, projectID, platform)
	if err != nil {
		return nil, err
	}
	// The stored PlatformCampaignID is a bare numeric id, unique only within the customer it
	// was created under, and the connection just resolved is the project's CURRENT one —
	// UpdateGoogleAds can re-point it at a different account between create and read. Querying
	// the id under the wrong customer does not error: it returns no rows, which is
	// indistinguishable from a campaign with genuinely zero activity, and on an id collision
	// it returns ANOTHER account's numbers. Fail loudly instead, before contacting Google.
	if created := googleAdsCreationCustomerID(campaign); created != "" && created != client.CustomerID() {
		return nil, fmt.Errorf("read google ads metrics: campaign %s was created under customer %s but the project's current connection resolves to customer %s: %w",
			campaign.PlatformCampaignID, created, client.CustomerID(), domain.ErrCampaignAccountMismatch)
	}
	m, err := client.GetCampaignMetrics(ctx, campaign.PlatformCampaignID, gaWindow)
	if err != nil {
		return nil, err
	}
	return &model.CampaignMetrics{
		CampaignID: m.CampaignID,
		// The request window, not the client's echoed GAQL literal: the API contract is the
		// platform-agnostic vocabulary, and translating back would reintroduce the dialect.
		Window:      window,
		Impressions: m.Impressions,
		Clicks:      m.Clicks,
		CostMicros:  m.CostMicros,
		Ctr:         m.Ctr,
	}, nil
}

// googleAdsCreationCustomerID recovers the ad account the campaign was CREATED under from
// the persisted googleads.CampaignResult blob, mirroring googleAdsChildIDs.
//
// Rows written before CampaignResult carried customerId have no such field, so it falls back
// to the ocid query parameter of the stored googleAdsUrl — the create path builds that URL as
// ".../aw/campaigns?ocid=" + customerID, making it a faithful record of the same value. An
// empty return means "unknown", and the caller must treat that as permission to proceed: a
// legacy row cannot prove a mismatch, and refusing every one of them would break reads that
// work today.
func googleAdsCreationCustomerID(campaign *model.Campaign) string {
	if campaign == nil || len(campaign.Result) == 0 {
		return ""
	}
	var blob struct {
		CustomerID   string `json:"customerId"`
		GoogleAdsURL string `json:"googleAdsUrl"`
	}
	if err := json.Unmarshal(campaign.Result, &blob); err != nil {
		return ""
	}
	if blob.CustomerID != "" {
		return blob.CustomerID
	}
	u, err := url.Parse(blob.GoogleAdsURL)
	if err != nil {
		return ""
	}
	return u.Query().Get("ocid")
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
