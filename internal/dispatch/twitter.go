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
	res, err := d.creds.resolve(ctx, brief.ProjectID, platform)
	if err != nil {
		return nil, err // preCreateError
	}
	if res.status != model.StatusActive {
		return nil, notCreated(fmt.Errorf("twitter connection for project %s is %s, not active", brief.ProjectID, res.status))
	}

	var creds twitterCreds
	if err := json.Unmarshal(res.plaintext, &creds); err != nil {
		return nil, notCreated(fmt.Errorf("decode twitter credentials: %w", err))
	}
	if creds.ConsumerKey == "" || creds.ConsumerSecret == "" || creds.AccessToken == "" || creds.AccessTokenSecret == "" {
		return nil, notCreated(fmt.Errorf("twitter credentials are incomplete (need consumerKey, consumerSecret, accessToken, accessTokenSecret)"))
	}

	accountID := strings.TrimSpace(res.accountID)
	fundingID := strings.TrimSpace(res.providerConfig["funding_instrument_id"])
	if accountID == "" || fundingID == "" {
		return nil, notCreated(fmt.Errorf("twitter connection for project %s is missing account id or funding instrument id", brief.ProjectID))
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
