// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
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
		// A non-nil result means the create got far enough to have produced upstream state
		// (a campaign, or a campaign + ad group), so the outcome is UNCONFIRMED and the claim
		// is RETAINED for reconcile rather than released.
		//
		// This deliberately does NOT try to separate "definitely rejected" from "genuinely
		// ambiguous". The client sets AlreadyExisted only on the SUCCESS path
		// (adgroup_ad.go:363, immediately before `return r, nil`), so it is always false here
		// — a check on it would be dead code that reads like a real distinction. Making that
		// separation real needs the client to classify its own partials, which is its own
		// change.
		return campaignFromMicrosoft(ctx, result, cfg), fmt.Errorf("microsoft campaign creation UNCONFIRMED (a partial campaign may exist — verify before retrying): %w", cerr)
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
// Every defect below is tagged for its AUDIENCE here, at the point of detection, following
// validateGoogleAdsCredentials. Untagged, all four fell to each handler's default arm and
// answered 503 — "the platform did not respond" about a platform that was never contacted,
// with a remedy (retry) that no amount of waiting can satisfy, since only a human editing
// the connection can fix it. The 409 arm at internal/service/brief.go is the correct answer.
// Tagging HERE rather than in each caller is what keeps Dispatch and ToggleStatus from
// having to agree about it; the named return plus defer means a return site added later
// cannot forget to re-attribute the error to the LF system row.
func validateMicrosoftConnection(projectID string, res *resolved) (creds microsoftCreds, accountID string, err error) {
	defer func() { err = res.systemScoped(err) }()
	if res.status != model.StatusActive {
		return creds, "", fmt.Errorf("%w: %w: microsoft connection for project %s is %s, not active",
			domain.ErrConnectionNotUsable, domain.ErrConnectionInactive, projectID, res.status)
	}
	if err := json.Unmarshal(res.plaintext, &creds); err != nil {
		// The unmarshal error is DROPPED, not wrapped: it is derived from the DECRYPTED
		// credential blob and encoding/json quotes its input. Full rationale on
		// validateGoogleAdsCredentials, which this follows.
		return creds, "", fmt.Errorf("%w: %w: microsoft credentials for project %s are not valid JSON",
			domain.ErrConnectionNotUsable, domain.ErrCredentialsUndecodable, projectID)
	}
	// TRIM before the completeness check and RETURN the trimmed values. Without the trim a
	// whitespace-only credential passes as "present", so CreateCampaign runs and its first
	// lookup fails on the bad credential — a failure that returns a non-nil partial and is
	// therefore classified UNCONFIRMED, RETAINING the claim for what is really a local config
	// error where nothing was ever created upstream.
	creds.ClientID = strings.TrimSpace(creds.ClientID)
	creds.ClientSecret = strings.TrimSpace(creds.ClientSecret)
	creds.DeveloperToken = strings.TrimSpace(creds.DeveloperToken)
	creds.RefreshToken = strings.TrimSpace(creds.RefreshToken)
	if creds.ClientID == "" || creds.ClientSecret == "" || creds.DeveloperToken == "" || creds.RefreshToken == "" {
		return creds, "", fmt.Errorf("%w: %w: microsoft credentials are incomplete (need clientId, clientSecret, developerToken, refreshToken)",
			domain.ErrConnectionNotUsable, domain.ErrCredentialsIncomplete)
	}
	accountID = strings.TrimSpace(res.accountID)
	if accountID == "" {
		// BOTH sentinels: ErrConnectionNotUsable decides the HTTP status, ErrAccountNotSelected
		// names the reason for the log line's fixed vocabulary (unusableConnectionReason).
		return creds, "", fmt.Errorf("%w: %w: microsoft connection for project %s has no account id (customer account id)",
			domain.ErrConnectionNotUsable, domain.ErrAccountNotSelected, projectID)
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
// "active". Pausing needs no child ids, EXCEPT that an ad id with no ad-group id is refused:
// the Ads PUT is scoped by AdGroupId, so the ad cannot be addressed at all.
//
// Both refusals are ErrCampaignNotProvisioned, so the service maps them to a 409 STATE error
// rather than a platform failure — they are facts about the persisted row, not about
// Microsoft. Both are checked BEFORE resolving credentials, so a row that can never be
// toggled costs no decrypt and no upstream call.
func (d *MicrosoftDispatcher) ToggleStatus(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, status string) error {
	msStatus, err := microsoftRunStatus(status)
	if err != nil {
		return err
	}
	adGroupID, adID := microsoftChildIDs(campaign)
	adGroupID, adID = strings.TrimSpace(adGroupID), strings.TrimSpace(adID)
	if msStatus == microsoft.StatusActive && (adGroupID == "" || adID == "") {
		return fmt.Errorf("%w: microsoft campaign %s cannot be activated because it has no fully-created ad group + ad to serve", domain.ErrCampaignNotProvisioned, campaign.PlatformCampaignID)
	}
	// An ad with no known parent cannot be changed (the client refuses the pair), and sending
	// the campaign anyway would report success while the ad's status remained unchanged.
	if adID != "" && adGroupID == "" {
		return fmt.Errorf("%w: microsoft campaign %s records ad %s but no ad group id, so the orphaned ad cannot be addressed in a status update", domain.ErrCampaignNotProvisioned, campaign.PlatformCampaignID, adID)
	}
	client, err := d.resolveMicrosoftClient(ctx, projectID, platform)
	if err != nil {
		return err
	}
	if uerr := client.UpdateCampaignAndChildrenStatus(ctx, campaign.PlatformCampaignID, adGroupID, adID, msStatus); uerr != nil {
		if microsoft.IsOutcomeUnconfirmed(uerr) {
			return &unconfirmedToggleError{err: uerr}
		}
		return uerr
	}
	return nil
}

// ReadMetrics implements the OPTIONAL service.MetricsReader capability for Microsoft
// Advertising, reading campaign performance through the v13 Reporting service.
//
// Microsoft's reporting pipeline is asynchronous — submit a report request, poll until it
// builds, then download a zipped CSV — unlike every other platform here, which answers with
// one synchronous JSON call. The client bounds the submit+poll phase well under the platform
// ingress timeout and returns microsoft.ErrReportNotReady rather than hanging the caller;
// that propagates as an ordinary error below rather than either metrics sentinel, because a
// report still building is a timing condition, NOT a campaign with no data. Reporting it as
// zeroes would turn that timing condition into a measurement. Note the retry it invites
// restarts the report rather than resuming it — the error message says so explicitly, and
// microsoft.ErrReportNotReady explains why.
//
// Because of that, this capability is OFF unless MICROSOFT_METRICS_ENABLED is set to "true".
// Merely declaring this method is the capability switch — Orchestrator.ReadCampaignMetrics
// discovers MetricsReader by type assertion, and the published endpoint then calls it — so
// without the flag an UNVERIFIED request/response shape would ship as production metrics that
// return 200 and look authoritative. The v13 Reporting contract this client implements was
// written from Microsoft's published documentation and has not been exercised against a live
// Microsoft Advertising account; nothing in the response carries that caveat. The gate is
// checked here rather than at construction so a deployment can flip it without a rebuild.
//
// Disabled reads answer domain.ErrMetricsUnsupported, which the service maps to the same 400
// a platform with no metrics support at all returns — the accurate answer while the contract
// is unverified. Delete the gate once the shape is confirmed against a live ad account.
func (d *MicrosoftDispatcher) ReadMetrics(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, window model.MetricsWindow) (*model.CampaignMetrics, error) {
	if os.Getenv(constants.EnvMicrosoftMetricsEnabled) != "true" {
		return nil, fmt.Errorf("microsoft metrics reads are disabled (%s is not \"true\") while the reporting contract is unverified: %w",
			constants.EnvMicrosoftMetricsEnabled, domain.ErrMetricsUnsupported)
	}
	if campaign == nil || campaign.PlatformCampaignID == "" {
		return nil, fmt.Errorf("campaign has no platform campaign ID")
	}
	client, err := d.resolveMicrosoftClient(ctx, projectID, platform)
	if err != nil {
		return nil, err
	}
	metrics, err := client.GetCampaignMetrics(ctx, campaign.PlatformCampaignID, window)
	if err != nil {
		// Classify the two conditions the caller must NOT read as "no data":
		// an unsupported window, and a report that had not finished building.
		if errors.Is(err, microsoft.ErrUnsupportedWindow) {
			return nil, fmt.Errorf("get campaign metrics from microsoft: %w: %w", domain.ErrMetricsWindowUnsupported, err)
		}
		// A report still building is deliberately NOT mapped to either metrics sentinel:
		// both mean 400 ("this cannot work"), and a retryable timing condition is neither
		// unsupported nor permanent. It propagates as an ordinary error — a 500 the caller
		// can retry — until a distinct retryable sentinel exists (there is none today; see
		// internal/domain/errors.go, which defines only ErrMetricsUnsupported and
		// ErrMetricsWindowUnsupported). Returning zeroes here instead would be the worse
		// failure: a timing condition rendered as a measurement.
		// Success-with-no-rows: the platform answered, but the adapter cannot tell "no
		// activity" from "no such campaign in scope". Same mapping hubspot.go uses.
		if errors.Is(err, microsoft.ErrNoRowsInReport) {
			return nil, fmt.Errorf("get campaign metrics from microsoft: %w", errors.Join(domain.ErrNoMetricsInWindow, err))
		}
		if errors.Is(err, microsoft.ErrReportNotReady) {
			// State the retry's real semantics rather than an encouraging "retry shortly":
			// the pending ReportRequestId does not survive the call (see
			// microsoft.ErrReportNotReady), so a retry SUBMITS A NEW REPORT and never
			// collects the one that has since finished. A campaign whose report reliably
			// outlasts the client's poll budget is therefore unreadable through this path
			// no matter how often it is retried.
			return nil, fmt.Errorf("get campaign metrics from microsoft (the report was still building when the poll budget expired; "+
				"retrying SUBMITS A NEW REPORT and will not pick up the pending one, so a report that reliably takes longer than the "+
				"budget stays unreadable here): %w", err)
		}
		return nil, fmt.Errorf("get campaign metrics from microsoft: %w", err)
	}
	return metrics, nil
}
