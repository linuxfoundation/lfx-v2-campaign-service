// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
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
		// creds is returned ALONGSIDE the error rather than zeroed: by this point the
		// credential has passed every other check, and the discovery path needs exactly
		// that — a valid credential on a connection that has chosen no account. Every
		// dispatch caller checks err first and so cannot observe the value.
		return creds, "", fmt.Errorf("%w: %w: microsoft connection for project %s has no account id (customer account id)",
			domain.ErrConnectionNotUsable, domain.ErrAccountNotSelected, projectID)
	}
	return creds, accountID, nil
}

// ListAccounts discovers the Microsoft Advertising accounts reachable via the project's
// stored, encrypted connection credential, across every customer that credential can reach.
//
// It satisfies the service-side AccountLister interface, which Orchestrator.ReadAccounts
// type-asserts on the dispatcher for the requested platform.
//
// It deliberately does NOT require an account id. Discovery exists to answer "which ad
// account should this connection use?", so demanding one would make the endpoint reachable
// only by connections that no longer need it — the account-less connection it is meant to
// rescue is exactly the one it would refuse. Meta's discovery path draws the same
// distinction; see resolveMetaDiscoveryClient.
func (d *MicrosoftDispatcher) ListAccounts(ctx context.Context, projectID string, platform model.Provider) ([]model.AccessibleAccount, error) {
	res, err := d.creds.resolve(ctx, projectID, platform)
	if err != nil {
		return nil, err
	}
	// Shares validateMicrosoftConnection with the dispatch path rather than repeating its
	// status/decode/completeness rules: duplicating them would let the two drift, so a
	// credential rejected at dispatch could be accepted here — which makes a discovery
	// endpoint actively misleading rather than merely permissive. Only the account-id
	// outcome is tolerated, and only because it is the state this endpoint serves.
	creds, _, verr := validateMicrosoftConnection(projectID, res)
	if verr != nil && !errors.Is(verr, domain.ErrAccountNotSelected) {
		return nil, res.systemScoped(verr)
	}
	// AccountConfig is left ZERO — no AccountID and, deliberately, no CustomerID.
	//
	// Naming an ACCOUNT would narrow the response to a subset of the question, which is the
	// same reason meta's discovery client carries a zero AccountConfig. CustomerID is the
	// less obvious half: passing the connection's stored one looks like harmless scoping,
	// but discoveryCustomerIDs treats a configured customer as the COMPLETE answer and
	// returns early without enumerating any other. An ordinary configured connection would
	// therefore have listed only that one customer's accounts while this endpoint's own
	// description promises every customer the credential reaches — the endpoint would have
	// contradicted itself for exactly the connections most likely to use it.
	//
	// Empty means "discover them", which is the question being asked.
	client := microsoft.NewClient(
		microsoft.Credentials{
			ClientID:       creds.ClientID,
			ClientSecret:   creds.ClientSecret,
			DeveloperToken: creds.DeveloperToken,
			RefreshToken:   creds.RefreshToken,
		},
		microsoft.AccountConfig{},
		d.opts...,
	)
	adAccounts, lerr := client.ListAdAccounts(ctx)
	if lerr != nil {
		return nil, lerr
	}
	// make(..., 0, n), never nil: a credential that legitimately reaches zero accounts is
	// an empty ANSWER, not an error, and Orchestrator.ReadAccounts rejects a nil result as
	// a contract violation precisely so empty keeps its meaning.
	accounts := make([]model.AccessibleAccount, 0, len(adAccounts))
	for _, a := range adAccounts {
		accounts = append(accounts, model.AccessibleAccount{ID: a.ID, Label: microsoftAccountLabel(a)})
	}
	return accounts, nil
}

// microsoftAccountLabel builds the string a picker shows for one account.
//
// It never returns "" for an account carrying any identifying information: Name is nillable
// in Microsoft's schema, so it falls back to the account Number and then to the id — a blank
// row in a picker is unpickable, and the id is what actually gets stored. The Number is
// appended when both are present because it is what the Microsoft Advertising UI shows, so
// a user recognises the account by it rather than by the id.
//
// Unusable accounts are LABELLED, not filtered — the same discipline meta's discovery uses.
// Dropping them would answer "your credential reaches no accounts" about an account sitting
// right there; returning them unmarked is worse still, because a suspended, paused or
// viewer-only account then looks exactly as selectable as a writable one and the refusal
// arrives later at dispatch, with no way back to this list.
//
// The client carries purpose-built renderings for exactly this: StatusLabel() maps a
// KNOWN-BAD lifecycle status ("" for a good or unrecognised one, so an unexpected value is
// never labelled as a defect), and PauseLabel() names who paused it, rendering an
// undocumented flag verbatim rather than guessing. Role is reported separately because it is
// a different question — an ACTIVE, unpaused account the credential can only READ is still
// unusable for a create, and Usable() already encodes that deny-list.
func microsoftAccountLabel(a microsoft.AdAccount) string {
	name := strings.TrimSpace(a.Name)
	number := strings.TrimSpace(a.Number)
	switch {
	case name != "" && number != "":
		name += " (" + number + ")"
	case name == "" && number != "":
		name = number
	case name == "":
		name = a.ID
	}

	var notes []string
	if s := a.StatusLabel(); s != "" {
		notes = append(notes, s)
	}
	if p := a.PauseLabel(); p != "" {
		notes = append(notes, p)
	}
	// Only when nothing above already explains it. Usable() is false for TWO independent
	// reasons and they are not the same message to an operator: a status that is not "Active"
	// (including an ABSENT or unrecognised one, which StatusLabel renders as nothing), and a
	// role with no evidence of write. Blaming the role for a missing status sends someone to
	// check permissions that are fine — {Status:"", RoleID:41} is writable and unconfirmed,
	// not read-only. Saying "read-only" twice for an account that is also suspended would be
	// noise, which is why this arm runs only when nothing above spoke.
	if len(notes) == 0 && !a.Usable() {
		if strings.TrimSpace(a.Status) != "Active" {
			notes = append(notes, "account status could not be confirmed")
		} else {
			notes = append(notes, "not writable with this credential")
		}
	}
	if len(notes) > 0 {
		return name + " — " + strings.Join(notes, ", ")
	}
	return name
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
