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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
)

// microsoftCreds is the credential shape stored (encrypted) for a Microsoft Advertising
// connection. Microsoft authenticates with an OAuth2 (Microsoft identity platform) app
// (clientId/clientSecret), a developer token, and a refresh token exchanged for short-lived
// access tokens. Field names (no json tags) are the persisted JSON keys, mirroring the sibling
// credential structs.
type microsoftCreds struct {
	ClientID       string
	ClientSecret   string
	DeveloperToken string
	RefreshToken   string
}

// microsoftConfig is the per-platform campaign config the caller passes for Microsoft in
// CreateCampaigns' Input.Config (delivered here as the Dispatch `config`).
//
// Today the Microsoft client creates a PAUSED Search campaign shell with an ad group + a
// responsive search ad (auto-composed copy); targeting/keywords land in a later phase, so only
// the budget (and optionally a Campaign.TimeZone enum) is caller-supplied here. Budget is in
// whole units of the ad ACCOUNT's currency (NOT USD — the client does NO FX conversion), applied
// as the campaign's DAILY budget, mirroring the meta client.
type microsoftConfig struct {
	// Budget is whole units of the account currency (e.g. 2500 = 2500 USD/JPY/…), the DAILY
	// budget. Must be finite and > 0; a NaN/Inf or non-positive value is rejected by the client
	// during dispatch (a pre-create job failure, since CreateCampaigns is async).
	Budget float64 `json:"budget"`
	// TimeZone is an OPTIONAL Microsoft Campaign.TimeZone enum value. Microsoft marks the field
	// deprecated but still requires it on Add; when empty the client uses its default.
	TimeZone string `json:"timeZone"`
}

// MicrosoftDispatcher creates Microsoft Advertising (Bing) campaigns for the orchestrator.
type MicrosoftDispatcher struct {
	creds *credsSource
	opts  []microsoft.Option
}

// NewMicrosoftDispatcher builds the adapter from the connection repo + encryptor.
func NewMicrosoftDispatcher(repo connReader, enc domain.Encryptor, opts ...microsoft.Option) *MicrosoftDispatcher {
	return &MicrosoftDispatcher{creds: newCredsSource(repo, enc), opts: opts}
}

// Dispatch implements service.PlatformDispatcher for Microsoft Advertising. It builds the FULL
// Campaign -> AdGroup -> Ad hierarchy (all PAUSED) via the client, so the result is a usable
// paused campaign rather than an empty shell — mirroring the reddit/meta/googleads adapters.
func (d *MicrosoftDispatcher) Dispatch(ctx context.Context, brief *model.CampaignBrief, platform model.Provider, config json.RawMessage) (*model.Campaign, error) {
	// Resolve creds FIRST (pre-create): a missing/undecryptable connection is a not-created
	// error → the orchestrator releases the claim.
	res, err := d.creds.resolve(ctx, brief.ProjectID, platform)
	if err != nil {
		return nil, err // already a preCreateError
	}
	// validateMicrosoftConnection is shared with ToggleStatus so a create and a toggle accept
	// EXACTLY the same connections and cannot drift. Its failures are wrapped with notCreated
	// HERE — create-only claim semantics the toggle path must not apply.
	creds, accountID, err := validateMicrosoftConnection(brief.ProjectID, res)
	if err != nil {
		return nil, notCreated(err)
	}

	var cfg microsoftConfig
	if err := unmarshalPlatformConfig(config, "microsoftConfig", &cfg); err != nil {
		return nil, notCreated(err)
	}
	bf, err := decodeBriefFields(brief)
	if err != nil {
		return nil, notCreated(err)
	}

	in := microsoft.CampaignInput{
		EventName: bf.EventName,
		EventSlug: brief.EventSlug,
		// Project is stamped from the AUTHENTICATED project scope (brief.ProjectID), never from
		// caller JSON — the Project name segment is the data pipeline's attribution join key
		// (docs/api-catalog.md), so it must be the canonical LFX slug (matches the siblings).
		Project:         brief.ProjectID,
		Budget:          cfg.Budget,
		TimeZone:        cfg.TimeZone,
		RegistrationURL: bf.RegistrationURL,
		// NameSuffix = the brief id gives deterministic, at-most-once-retry names: Microsoft
		// enforces case-insensitive campaign-name uniqueness, so a retry composes the SAME name
		// and the client's find-first lookup cleanly REUSES the existing campaign
		// (`AlreadyExisted=true`, no error) rather than creating a second paid campaign — a
		// poor-man's idempotency key until LFXV2-2665 lands provider idempotency keys. (An
		// UNCONFIRMED partial is the distinct case below: a non-nil result WITH an error.)
		NameSuffix: brief.ID,
	}

	// customer_id is the OPTIONAL parent manager (MCC) account the ad account is accessed
	// through; it lives in the connection's ProviderConfig (not the credential blob). NOTE the
	// key is `customer_id` — the Microsoft connection service persists it there
	// (connection.go buildMicrosoftAdsResult / CreateMicrosoftAds), NOT `login_customer_id`
	// (that is the Google Ads key). Reading the wrong key would silently drop the CustomerId
	// header and break MCC-scoped accounts. Trimmed for the same reason as the account id.
	client := microsoft.NewClient(
		microsoft.Credentials{
			ClientID:       creds.ClientID,
			ClientSecret:   creds.ClientSecret,
			DeveloperToken: creds.DeveloperToken,
			RefreshToken:   creds.RefreshToken,
		},
		microsoft.AccountConfig{
			AccountID:  accountID,
			CustomerID: strings.TrimSpace(res.providerConfig["customer_id"]),
			Label:      res.label,
		},
		d.opts...,
	)

	// The Microsoft client's contract (mirrors the siblings): a non-nil result with an error is
	// an UNCONFIRMED partial (the campaign/tree MAY exist upstream — verify before retrying),
	// while (nil, err) means NOTHING was created. Release the claim ONLY when result==nil; a
	// partial whose CampaignID may be empty still means "may exist", so gating on an empty
	// CampaignID would wrongly release the claim and invite a duplicate.
	result, cerr := client.CreateCampaign(ctx, in)
	if cerr != nil {
		if result == nil {
			return nil, notCreated(fmt.Errorf("microsoft campaign creation failed before any upstream create: %w", cerr))
		}
		return campaignFromMicrosoft(ctx, result, cfg), fmt.Errorf("microsoft campaign creation UNCONFIRMED: %w", cerr)
	}
	return campaignFromMicrosoft(ctx, result, cfg), nil
}

// campaignFromMicrosoft maps the client result to the persistence model.
func campaignFromMicrosoft(ctx context.Context, r *microsoft.CampaignResult, cfg microsoftConfig) *model.Campaign {
	c := &model.Campaign{
		PlatformCampaignID: r.CampaignID,
		CampaignName:       r.CampaignName,
		Status:             campaignStatusCreated,
	}
	// Persist the budget/type/config the caller supplied (Microsoft uses a DAILY budget).
	// ConfigSnapshot captures the validated config; parity with the sibling adapters (a NULL
	// budget/type/config_snapshot row would lose the configuration).
	applyCampaignConfig(ctx, c, cfg.Budget, false, "", "", cfg)
	if raw, err := json.Marshal(r); err != nil {
		// Near-impossible for this plain struct, but do NOT swallow it: on an ambiguous-orphan
		// path Result is the sole carrier of the reconcile-by-name payload (campaign/ad-group/ad
		// names + Steps), so a silently-empty Result loses reconciliation data precisely when
		// it's most needed. Log it (the row is still persisted with its id/status). Mirrors the
		// meta/linkedin adapters.
		slog.WarnContext(ctx, "failed to marshal microsoft campaign result blob (Result left empty)",
			"campaign_id", c.PlatformCampaignID, "error", err)
	} else {
		c.Result = raw
	}
	return c
}

// validateMicrosoftConnection checks a resolved connection is usable and returns the decoded
// credentials + trimmed account id. Shared by Dispatch and ToggleStatus so a create and a
// toggle accept EXACTLY the same connections; each caller applies its own error wrapping
// (Dispatch wraps with notCreated for claim semantics, the toggle path does not).
//
// The account id is trimmed ONCE and the trimmed value returned, so a whitespace-padded id
// can't pass the empty check here and then fail the client's digits-only validation as a
// confusing downstream error.
func validateMicrosoftConnection(projectID string, res *resolved) (microsoftCreds, string, error) {
	var creds microsoftCreds
	if res.status != model.StatusActive {
		return creds, "", fmt.Errorf("microsoft connection for project %s is %s, not active", projectID, res.status)
	}
	if err := json.Unmarshal(res.plaintext, &creds); err != nil {
		return creds, "", fmt.Errorf("decode microsoft credentials: %w", err)
	}
	if creds.ClientID == "" || creds.ClientSecret == "" || creds.DeveloperToken == "" || creds.RefreshToken == "" {
		return creds, "", fmt.Errorf("microsoft credentials are incomplete (need clientId, clientSecret, developerToken, refreshToken)")
	}
	accountID := strings.TrimSpace(res.accountID)
	if accountID == "" {
		return creds, "", fmt.Errorf("microsoft connection for project %s has no account id (customer account id)", projectID)
	}
	return creds, accountID, nil
}

// resolveMicrosoftClient resolves + validates the project's connection and builds a client
// for the TOGGLE path (see validateMicrosoftConnection for the shared rules).
func (d *MicrosoftDispatcher) resolveMicrosoftClient(ctx context.Context, projectID string, platform model.Provider) (*microsoft.Client, error) {
	res, err := d.creds.resolve(ctx, projectID, platform)
	if err != nil {
		return nil, err
	}
	creds, accountID, err := validateMicrosoftConnection(projectID, res)
	if err != nil {
		return nil, err
	}
	return microsoft.NewClient(
		microsoft.Credentials{
			ClientID:       creds.ClientID,
			ClientSecret:   creds.ClientSecret,
			DeveloperToken: creds.DeveloperToken,
			RefreshToken:   creds.RefreshToken,
		},
		microsoft.AccountConfig{
			AccountID:  accountID,
			CustomerID: strings.TrimSpace(res.providerConfig["customer_id"]),
			Label:      res.label,
		},
		d.opts...,
	), nil
}

// microsoftRunStatus maps the service's run-state vocabulary to Microsoft's Status enum.
func microsoftRunStatus(status string) (string, error) {
	switch status {
	case model.CampaignRunActive:
		return microsoft.StatusActive, nil
	case model.CampaignRunPaused:
		return microsoft.StatusPaused, nil
	default:
		return "", fmt.Errorf("unsupported campaign run status %q (want %q or %q)", status, model.CampaignRunActive, model.CampaignRunPaused)
	}
}

// microsoftChildIDs pulls the ad-group and ad ids out of the persisted result blob. The blob
// is campaignFromMicrosoft's marshal of microsoft.CampaignResult, whose json tags are
// lowerCamel; the shape is pinned by a round-trip test.
func microsoftChildIDs(campaign *model.Campaign) (adGroupID, adID string) {
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

// ToggleStatus implements service.StatusToggler for Microsoft Advertising.
//
// FULL CASCADE, like reddit: the create path builds the whole Campaign -> AdGroup -> Ad tree
// PAUSED, so toggling only the campaign would leave the children paused and nothing serving.
// ACTIVATE requires BOTH child ids — a missing child would stay Paused while the row claimed
// "active" — and that refusal is ErrCampaignNotProvisioned, so the service maps it to a 409
// state error without ever calling Microsoft. Pausing needs no child ids.
func (d *MicrosoftDispatcher) ToggleStatus(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, status string) error {
	msStatus, err := microsoftRunStatus(status)
	if err != nil {
		return err
	}
	client, err := d.resolveMicrosoftClient(ctx, projectID, platform)
	if err != nil {
		return err
	}
	adGroupID, adID := microsoftChildIDs(campaign)
	if msStatus == microsoft.StatusActive && (strings.TrimSpace(adGroupID) == "" || strings.TrimSpace(adID) == "") {
		return fmt.Errorf("%w: microsoft campaign %s cannot be activated because it has no fully-created ad group + ad to serve", domain.ErrCampaignNotProvisioned, campaign.PlatformCampaignID)
	}
	if uerr := client.UpdateCampaignAndChildrenStatus(ctx, campaign.PlatformCampaignID, adGroupID, adID, msStatus); uerr != nil {
		if microsoft.IsOutcomeUnconfirmed(uerr) {
			return &unconfirmedToggleError{err: uerr}
		}
		return uerr
	}
	return nil
}
