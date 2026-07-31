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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
)

// twitterCreds is the credential shape stored (encrypted) for an X/Twitter
// connection. X Ads uses OAuth1 — a 4-tuple of consumer + access key/secret pairs —
// unlike the single-bearer-token platforms.
//
// The campaignStatusCreatedDegraded status (set below when the promoted-tweet
// association is unconfirmed) is the SHARED constant defined in creds.go, used by
// every adapter that can return a partial success.

// twitterCreds mirrors TwitterAdsCredentials's field names (no json tags) — the
// persisted JSON keys are the Go field names (PascalCase), see redditCreds.
type twitterCreds struct {
	ConsumerKey       string
	ConsumerSecret    string
	AccessToken       string
	AccessTokenSecret string
}

// twitterConfig is the per-platform campaign config the caller passes for X in
// CreateCampaigns' Input.Config (delivered here as the Dispatch `config`). TweetID is
// the existing tweet to promote.
type twitterConfig struct {
	// BudgetAmount is in the ad ACCOUNT's currency, NOT USD — X serializes it as
	// daily_budget_amount_local_micro, which X interprets in the account's local
	// currency, and this service does no FX conversion (mirrors the meta config's
	// account-currency budget). The old `budgetUsd` name was misleading: a non-USD
	// account would treat "500" as 500 of its own currency, not $500.
	BudgetAmount float64 `json:"budgetAmount"`
	StartDate    string  `json:"startDate"` // YYYY-MM-DD
	EndDate      string  `json:"endDate"`   // YYYY-MM-DD
	TweetID      string  `json:"tweetId"`
}

// TwitterDispatcher creates X (Twitter) campaigns for the orchestrator.
type TwitterDispatcher struct {
	creds *credsSource
	opts  []twitter.Option
}

// NewTwitterDispatcher builds the adapter from the connection repo + encryptor.
func NewTwitterDispatcher(repo connReader, enc domain.Encryptor, opts ...twitter.Option) *TwitterDispatcher {
	return &TwitterDispatcher{creds: newCredsSource(repo, enc), opts: opts}
}

// Dispatch implements service.PlatformDispatcher for X (Twitter).
func (d *TwitterDispatcher) Dispatch(ctx context.Context, brief *model.CampaignBrief, platform model.Provider, config json.RawMessage) (*model.Campaign, error) {
	// The resolve step's error is already a preCreateError, so it passes through verbatim.
	res, err := d.creds.resolve(ctx, brief.ProjectID, platform)
	if err != nil {
		return nil, err // preCreateError
	}
	// validateTwitterConnection is shared with ToggleStatus so a create and a toggle accept
	// EXACTLY the same connections. Its failures are wrapped with notCreated HERE (create-only
	// claim semantics the toggle path must not apply).
	creds, accountID, err := validateTwitterConnection(brief.ProjectID, res)
	if err != nil {
		return nil, notCreated(err)
	}
	// The funding instrument is required to CREATE a campaign but is never used by a status
	// toggle, so it is checked here rather than in the shared validator.
	fundingID := strings.TrimSpace(res.providerConfig["funding_instrument_id"])
	if fundingID == "" {
		return nil, notCreated(fmt.Errorf("twitter connection for project %s is missing funding instrument id", brief.ProjectID))
	}

	var cfg twitterConfig
	if err := unmarshalPlatformConfig(config, "twitterConfig", &cfg); err != nil {
		return nil, notCreated(err)
	}
	bf, err := decodeBriefFields(brief)
	if err != nil {
		return nil, notCreated(err)
	}

	// hsToken is a documented TOP-LEVEL config envelope field (docs/api-catalog.md);
	// a request-supplied token takes precedence over the brief blobs so it drives the
	// promoted-tweet utm_campaign instead of being silently ignored (matches the other
	// dispatchers via the shared envelopeHSToken helper).
	hsToken, err := envelopeHSToken(config)
	if err != nil {
		return nil, notCreated(err) // a wrong-typed hsToken is a caller error (pre-create)
	}
	if hsToken == "" {
		hsToken = bf.HSToken
	}

	in := twitter.CampaignInput{
		EventName: bf.EventName,
		EventSlug: brief.EventSlug,
		// Project stamped from the authenticated scope, not caller JSON (api-catalog).
		Project:         brief.ProjectID,
		RegistrationURL: bf.RegistrationURL,
		HSToken:         hsToken,
		BudgetUsd:       cfg.BudgetAmount, // account-currency amount (client field name is legacy)
		StartDate:       cfg.StartDate,
		EndDate:         cfg.EndDate,
		TweetID:         cfg.TweetID,
	}

	client := twitter.NewClient(
		twitter.Credentials{
			ConsumerKey:       creds.ConsumerKey,
			ConsumerSecret:    creds.ConsumerSecret,
			AccessToken:       creds.AccessToken,
			AccessTokenSecret: creds.AccessTokenSecret,
		},
		twitter.AccountConfig{AccountID: accountID, FundingInstrumentID: fundingID},
		d.opts...,
	)

	// Same claim contract as the other adapters: a client (nil, err) means nothing was
	// (or may have been) created → notCreated releases the claim; a non-nil partial
	// result + err (ambiguous create / mid-flow) is handed back with the upstream id so
	// the orchestrator retains the claim and records the recoverable orphan.
	// Release the claim ONLY when result==nil. An ambiguous create (or 2xx-no-id)
	// returns a non-nil name-only partial whose CampaignID is empty but still means
	// "may exist" — gating on an empty CampaignID would wrongly release the claim.
	result, cerr := client.CreateCampaign(ctx, in)
	if cerr != nil {
		if result == nil {
			return nil, notCreated(fmt.Errorf("twitter campaign creation failed before any upstream create: %w", cerr))
		}
		return campaignFromTwitter(ctx, result, cfg), fmt.Errorf("twitter campaign creation UNCONFIRMED: %w", cerr)
	}
	// A nil-error success is DEGRADED — persisted as `created_degraded`, not clean
	// `created` — in any of these shapes, all of which mean the campaign exists but is
	// not fully/correctly wired to THIS request:
	//   1. a non-empty PromotedTweetWarning — the promoted-tweet association was
	//      attempted but failed or is unconfirmed; OR
	//   2. an empty PromotedTweetID — no tweet was attached at all (the documented
	//      manual-tweet workflow when tweetId is omitted, or a silent 2xx-no-id
	//      association); OR
	//   3. Reused — the client REUSED an existing campaign and/or line item by name and
	//      did NOT apply this request's budget/config/flight-dates, so it may be serving
	//      under a different budget or an already-ENABLED line item with different dates
	//      (an authoritative reconcile is the orchestrator's job, LFXV2-2665).
	// All are SUPPORTED (we must NOT reject them), but they are not an unqualified
	// `created`, so we make them VISIBLE for reconciliation. We do NOT return an error:
	// the campaign IS created, so failing the job would mislead and be unrecoverable by
	// retry anyway (idempotency short-circuits a re-dispatch). Details are in Result/Steps.
	camp := campaignFromTwitter(ctx, result, cfg)
	if strings.TrimSpace(result.PromotedTweetWarning) != "" || strings.TrimSpace(result.PromotedTweetID) == "" || result.Reused {
		camp.Status = campaignStatusCreatedDegraded
	}
	return camp, nil
}

// campaignFromTwitter maps the client result to the persistence model.
func campaignFromTwitter(ctx context.Context, r *twitter.CampaignResult, cfg twitterConfig) *model.Campaign {
	c := &model.Campaign{
		PlatformCampaignID: r.CampaignID,
		CampaignName:       r.CampaignName,
		Status:             campaignStatusCreated,
	}
	// Persist the budget/schedule/config the caller supplied. BudgetAmount is a daily
	// cap in the account currency (X has no lifetime-budget flag on this path).
	// ConfigSnapshot captures the validated config.
	applyCampaignConfig(ctx, c, cfg.BudgetAmount, false, cfg.StartDate, cfg.EndDate, cfg)
	if raw, err := json.Marshal(r); err != nil {
		// A marshal failure should be near-impossible for this plain struct, but do NOT
		// swallow it: on the created_degraded / UNCONFIRMED paths Result is the main
		// carrier of the Steps, warnings, and the Reused reuse/config-drift signal, so a
		// silently-empty Result loses reconciliation data when it matters most. Log it
		// (the row is still persisted with its id/status). Mirrors the linkedin/meta
		// adapters.
		slog.WarnContext(ctx, "failed to marshal twitter campaign result blob (Result left empty)",
			"campaign_id", c.PlatformCampaignID, "error", err)
	} else {
		c.Result = raw
	}
	return c
}

// validateTwitterConnection checks a resolved connection is usable and returns the decoded
// credentials + account id. Shared by Dispatch and ToggleStatus so a create and a toggle
// accept EXACTLY the same connections; each caller applies its own error wrapping (Dispatch
// wraps with notCreated for claim semantics, the toggle path does not). The funding
// instrument is NOT checked here — it is a create-only field a toggle never uses.
func validateTwitterConnection(projectID string, res *resolved) (twitterCreds, string, error) {
	var creds twitterCreds
	if res.status != model.StatusActive {
		return creds, "", fmt.Errorf("twitter connection for project %s is %s, not active", projectID, res.status)
	}
	if err := json.Unmarshal(res.plaintext, &creds); err != nil {
		return creds, "", fmt.Errorf("decode twitter credentials: %w", err)
	}
	if creds.ConsumerKey == "" || creds.ConsumerSecret == "" || creds.AccessToken == "" || creds.AccessTokenSecret == "" {
		return creds, "", fmt.Errorf("twitter credentials are incomplete (need consumerKey, consumerSecret, accessToken, accessTokenSecret)")
	}
	accountID := strings.TrimSpace(res.accountID)
	if accountID == "" {
		return creds, "", fmt.Errorf("twitter connection for project %s is missing account id", projectID)
	}
	return creds, accountID, nil
}

// resolveTwitterClient resolves + validates the project's connection and builds an X Ads
// client for the TOGGLE path (see validateTwitterConnection for the shared rules).
func (d *TwitterDispatcher) resolveTwitterClient(ctx context.Context, projectID string, platform model.Provider) (*twitter.Client, error) {
	res, err := d.creds.resolve(ctx, projectID, platform)
	if err != nil {
		return nil, err
	}
	creds, accountID, err := validateTwitterConnection(projectID, res)
	if err != nil {
		return nil, err
	}
	return twitter.NewClient(
		twitter.Credentials{
			ConsumerKey:       creds.ConsumerKey,
			ConsumerSecret:    creds.ConsumerSecret,
			AccessToken:       creds.AccessToken,
			AccessTokenSecret: creds.AccessTokenSecret,
		},
		// FundingInstrumentID is a create-time field; a toggle only PUTs entity_status on
		// entities that already exist, so it is deliberately left empty here.
		twitter.AccountConfig{AccountID: accountID, FundingInstrumentID: strings.TrimSpace(res.providerConfig["funding_instrument_id"])},
		d.opts...,
	), nil
}

// twitterRunStatus maps the service's run-state vocabulary to X's entity_status.
func twitterRunStatus(status string) (string, error) {
	switch status {
	case model.CampaignRunActive:
		return twitter.StatusActive, nil
	case model.CampaignRunPaused:
		return twitter.StatusPaused, nil
	default:
		return "", fmt.Errorf("unsupported campaign run status %q (want %q or %q)", status, model.CampaignRunActive, model.CampaignRunPaused)
	}
}

// twitterChildIDs pulls the line-item id out of the persisted result blob.
//
// The blob is campaignFromTwitter's json.Marshal of an untagged twitter.CampaignResult, so
// the key is the Go field name "LineItemID" (the reddit blob uses lowerCamel — the shapes
// differ per platform). Casing itself is forgiving (encoding/json matches keys
// case-insensitively), but a renamed or nested field would silently yield "" and turn every
// ACTIVATE into a spurious not-provisioned 409, so the shape is pinned by a round-trip test.
func twitterChildIDs(campaign *model.Campaign) (lineItemID string) {
	if campaign == nil || len(campaign.Result) == 0 {
		return ""
	}
	var blob struct {
		LineItemID string `json:"LineItemID"`
	}
	if err := json.Unmarshal(campaign.Result, &blob); err != nil {
		return ""
	}
	return blob.LineItemID
}

// ToggleStatus implements service.StatusToggler for X (Twitter) Ads.
//
// Scope is the campaign + line item; the promoted tweet is intentionally untouched (see
// twitter.UpdateCampaignAndChildrenStatus — the line item is X's delivery gate and the
// association is always ACTIVE). ACTIVATE requires a known line-item id, since activating
// the campaign alone would leave the line item PAUSED and nothing serving while the row
// claimed "active"; that refusal is ErrCampaignNotProvisioned so the service maps it to a
// 409 state error without ever calling X. Pausing needs no child id.
func (d *TwitterDispatcher) ToggleStatus(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, status string) error {
	twitterStatus, err := twitterRunStatus(status)
	if err != nil {
		return err
	}
	client, err := d.resolveTwitterClient(ctx, projectID, platform)
	if err != nil {
		return err
	}
	lineItemID := twitterChildIDs(campaign)
	if twitterStatus == twitter.StatusActive && strings.TrimSpace(lineItemID) == "" {
		return fmt.Errorf("%w: twitter campaign %s cannot be activated because its line item is not known, so nothing would serve", domain.ErrCampaignNotProvisioned, campaign.PlatformCampaignID)
	}
	if uerr := client.UpdateCampaignAndChildrenStatus(ctx, campaign.PlatformCampaignID, lineItemID, twitterStatus); uerr != nil {
		// An ambiguous outcome (transport/5xx/mutating-3xx/mutating-429) may have applied
		// upstream, so report "verify before retry" rather than a flat "not applied".
		if twitter.IsOutcomeUnconfirmed(uerr) {
			return &unconfirmedToggleError{err: uerr}
		}
		return uerr
	}
	return nil
}
