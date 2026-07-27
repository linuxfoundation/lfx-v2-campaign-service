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

// googleAdsConfig is the per-platform campaign config the caller passes for Google Ads
// in CreateCampaigns' Input.Config (delivered here as the Dispatch `config`). Today the
// GA client creates a PAUSED search-campaign shell, so only the budget is caller-
// supplied here; targeting/keywords land in GA-3+. Budget is in whole units of the ad
// ACCOUNT's currency (NOT USD — the client does no FX), mirroring the meta client.
type googleAdsConfig struct {
	Budget float64 `json:"budget"`
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
	if res.status != model.StatusActive {
		return nil, notCreated(fmt.Errorf("google ads connection for project %s is %s, not active", brief.ProjectID, res.status))
	}

	var creds googleAdsCreds
	if err := json.Unmarshal(res.plaintext, &creds); err != nil {
		return nil, notCreated(fmt.Errorf("decode google ads credentials: %w", err))
	}
	if creds.ClientID == "" || creds.ClientSecret == "" || creds.DeveloperToken == "" || creds.RefreshToken == "" {
		return nil, notCreated(fmt.Errorf("google ads credentials are incomplete (need clientId, clientSecret, developerToken, refreshToken)"))
	}
	if strings.TrimSpace(res.accountID) == "" {
		return nil, notCreated(fmt.Errorf("google ads connection for project %s has no account id (customer id)", brief.ProjectID))
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
		// Project is stamped from the AUTHENTICATED project scope (brief.ProjectID),
		// never from caller JSON — the Project name segment is the data pipeline's
		// attribution join key (docs/api-catalog.md), so it must be the canonical LFX
		// slug (matches reddit/meta/twitter).
		Project: brief.ProjectID,
		Budget:  cfg.Budget,
		// NameSuffix = the brief id gives deterministic, at-most-once-retry names: the
		// GA client composes the budget/campaign names from these, and a retry with the
		// same suffix hits Google's DUPLICATE_NAME (reported UNCONFIRMED-already-exists)
		// rather than creating a second paid campaign — a poor-man's idempotency key
		// until LFXV2-2665 lands provider idempotency keys.
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
			CustomerID:      res.accountID,
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
	// hierarchy (budget → campaign): an orphaned budget is reconciled by its OWN
	// deterministic name (CampaignBudgetName), preserved in the Result blob.
	//   - (nil, err)      → pre-create; notCreated releases the claim.
	//   - (result, err)   → may exist; return the (possibly id-less) campaign + error so
	//                       the orchestrator retains the claim and records the orphan.
	//   - (result, nil)   → success.
	result, cerr := client.CreateCampaign(ctx, in)
	if cerr != nil {
		if result == nil {
			return nil, notCreated(fmt.Errorf("google ads campaign creation failed before any upstream create: %w", cerr))
		}
		return campaignFromGoogleAds(ctx, result, cfg), fmt.Errorf("google ads campaign creation UNCONFIRMED: %w", cerr)
	}
	return campaignFromGoogleAds(ctx, result, cfg), nil
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
