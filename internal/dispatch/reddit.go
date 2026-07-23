// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
)

// redditCreds is the credential shape stored (encrypted) for a Reddit connection —
// Reddit uses OAuth2 with a long-lived refresh token. This adapter unmarshals the
// decrypted blob into this struct itself (credential shapes differ per platform, so
// there is no shared typed-credentials abstraction).
// redditCreds mirrors the generated RedditAdsCredentials field names EXACTLY (no json
// tags). The connection service persists credentials via json.Marshal on the
// tag-less generated struct, so the stored JSON keys are the Go field names
// (PascalCase: ClientID/ClientSecret/RefreshToken). Matching them field-for-field
// avoids relying on encoding/json's case-insensitive fallback.
type redditCreds struct {
	ClientID     string
	ClientSecret string
	RefreshToken string
}

// redditConfig is the per-platform campaign config the caller passes for Reddit in
// CreateCampaigns' Input.Config (delivered here as the Dispatch `config` argument).
// The brief supplies event identity (name/slug/project/registration URL); this
// supplies the Reddit-specific campaign shape.
type redditConfig struct {
	BudgetUSD         float64            `json:"budgetUsd"`
	StartDate         string             `json:"startDate"` // YYYY-MM-DD
	EndDate           string             `json:"endDate"`   // YYYY-MM-DD
	Objective         string             `json:"objective"` // awareness|traffic|conversions|video_views
	GeoTargets        []string           `json:"geoTargets"`
	Subreddits        []string           `json:"subreddits"`
	Interests         []string           `json:"interests"`
	Keywords          []string           `json:"keywords"`
	Variants          []reddit.AdVariant `json:"variants"`
	PostURL           string             `json:"postUrl"`
	ConversionPixelID string             `json:"conversionPixelId"`
	VideoGoal         string             `json:"videoGoal"`
}

// briefFields is the subset of a brief's JSON blobs the adapters read. The brief
// stores event data as opaque JSON (EventDetails/Copy). Project is deliberately NOT
// here — it must come from the authenticated brief.ProjectID, not caller JSON.
type briefFields struct {
	EventName       string `json:"eventName"`
	RegistrationURL string `json:"registrationUrl"`
	HSToken         string `json:"hsToken"`
}

// RedditDispatcher creates Reddit campaigns for the orchestrator. It resolves the
// project's Reddit connection, builds a reddit.Client from the decrypted creds, maps
// the brief + config onto reddit.CampaignInput, and returns the created campaign.
type RedditDispatcher struct {
	creds *credsSource
	// opts are extra reddit.Client options (e.g. WithBaseURL/WithTokenURL in tests).
	opts []reddit.Option
}

// NewRedditDispatcher builds the adapter from the connection repo + encryptor.
func NewRedditDispatcher(repo connReader, enc domain.Encryptor, opts ...reddit.Option) *RedditDispatcher {
	return &RedditDispatcher{creds: newCredsSource(repo, enc), opts: opts}
}

// Dispatch implements service.PlatformDispatcher for Reddit.
func (d *RedditDispatcher) Dispatch(ctx context.Context, brief *model.CampaignBrief, platform model.Provider, config json.RawMessage) (*model.Campaign, error) {
	// Resolve creds FIRST (pre-create): a missing/undecryptable connection is a
	// not-created error → the orchestrator releases the claim.
	res, err := d.creds.resolve(ctx, brief.ProjectID, platform)
	if err != nil {
		return nil, err // already a preCreateError
	}
	if res.status != model.StatusActive {
		return nil, notCreated(fmt.Errorf("reddit connection for project %s is %s, not active", brief.ProjectID, res.status))
	}

	var creds redditCreds
	if err := json.Unmarshal(res.plaintext, &creds); err != nil {
		return nil, notCreated(fmt.Errorf("decode reddit credentials: %w", err))
	}
	if creds.ClientID == "" || creds.ClientSecret == "" || creds.RefreshToken == "" {
		return nil, notCreated(fmt.Errorf("reddit credentials are incomplete (need clientId, clientSecret, refreshToken)"))
	}
	if strings.TrimSpace(res.accountID) == "" {
		return nil, notCreated(fmt.Errorf("reddit connection for project %s has no account id", brief.ProjectID))
	}

	var cfg redditConfig
	if err := unmarshalPlatformConfig(config, "redditConfig", &cfg); err != nil {
		return nil, notCreated(err)
	}
	bf, err := decodeBriefFields(brief)
	if err != nil {
		return nil, notCreated(err)
	}

	// hsToken is a documented TOP-LEVEL field of the config envelope (docs/api-catalog.md
	// — sibling to redditConfig, NOT nested inside it), so read it from the envelope. A
	// request-supplied hsToken takes precedence; the brief's EventDetails/Copy token is
	// only a fallback. Without this a valid hsToken is silently ignored and the client
	// falls back to the event slug for utm_campaign, losing the HubSpot attribution.
	hsToken, err := envelopeHSToken(config)
	if err != nil {
		return nil, notCreated(err) // a wrong-typed hsToken is a caller error (pre-create)
	}
	if hsToken == "" {
		hsToken = bf.HSToken
	}

	in := reddit.CampaignInput{
		EventName:       bf.EventName,
		EventSlug:       brief.EventSlug,
		RegistrationURL: bf.RegistrationURL,
		HSToken:         hsToken,
		// Project is stamped from the AUTHENTICATED project scope (brief.ProjectID),
		// never from caller-controlled brief JSON — the Project name segment is the
		// data pipeline's attribution join key (docs/api-catalog.md), so it must be
		// the canonical LFX slug, not free text.
		Project:           brief.ProjectID,
		BudgetUSD:         cfg.BudgetUSD,
		StartDate:         cfg.StartDate,
		EndDate:           cfg.EndDate,
		Objective:         cfg.Objective,
		GeoTargets:        cfg.GeoTargets,
		Subreddits:        cfg.Subreddits,
		Interests:         cfg.Interests,
		Keywords:          cfg.Keywords,
		Variants:          cfg.Variants,
		PostURL:           cfg.PostURL,
		ConversionPixelID: cfg.ConversionPixelID,
		VideoGoal:         cfg.VideoGoal,
	}

	client := reddit.NewClient(
		reddit.Credentials{ClientID: creds.ClientID, ClientSecret: creds.ClientSecret, RefreshToken: creds.RefreshToken},
		reddit.AccountConfig{AccountID: res.accountID, Label: res.label},
		d.opts...,
	)

	// The reddit client's contract: (nil, err) ONLY when NOTHING was (or may have
	// been) created — a validation/pre-send/definite-4xx failure. Otherwise it
	// returns a NON-NIL partial result alongside the error (an ambiguous create, or a
	// 2xx with no id, gives a name-only result whose CampaignID is EMPTY but which
	// still means "may exist"). So the release decision keys on result==nil ALONE —
	// NOT on an empty CampaignID, which would wrongly release the claim on an
	// ambiguous partial and risk a duplicate on retry.
	//   - (nil, err)      → pre-create; notCreated releases the claim.
	//   - (result, err)   → may exist; return the (possibly id-less) campaign + error
	//                       so the orchestrator RETAINS THE CLAIM (blocking a duplicate
	//                       on retry) AND persists the partial for reconciliation — it
	//                       writes the row whenever the campaign carries an upstream id
	//                       OR a Result reconcile blob (so an id-less ambiguous partial
	//                       with its name/blob is recorded as a pending orphan, not a
	//                       bare anonymous claim). A retry then classifies that pending
	//                       orphan as reconciliation-required, not a false success.
	//   - (result, nil)   → success.
	result, cerr := client.CreateCampaign(ctx, in)
	if cerr != nil {
		if result == nil {
			return nil, notCreated(fmt.Errorf("reddit campaign creation failed before any upstream create: %w", cerr))
		}
		return campaignFromReddit(ctx, result, cfg), fmt.Errorf("reddit campaign creation UNCONFIRMED: %w", cerr)
	}
	// A nil error with a non-empty AdWarning is a DEGRADED success: the campaign + ad
	// group were created, but the promoted-post ad failed or is unconfirmed
	// (client.go sets AdWarning on that path). We do NOT return an error — the campaign
	// IS created, so failing the job would mislead (the paid campaign exists) and be
	// unrecoverable by retry anyway (the orchestrator persists PlatformCampaignID and a
	// re-dispatch short-circuits on idempotency, never re-running the ad step). Instead
	// the degraded state is made VISIBLE in the persisted row: a distinct
	// `created_degraded` status (the warning text is already carried in Result). A
	// human/monitor reconciles the ad; the campaign is not silently "succeeded".
	// Mirrors the twitter adapter's PromotedTweetWarning handling.
	camp := campaignFromReddit(ctx, result, cfg)
	if strings.TrimSpace(result.AdWarning) != "" {
		camp.Status = campaignStatusCreatedDegraded
	}
	return camp, nil
}

// ToggleStatus pauses or resumes an existing reddit campaign on the platform. It resolves
// the connection (same pre-check as Dispatch: an inactive/undecryptable connection is a
// clean error), builds the client, and PATCHes configured_status. platformCampaignID is the
// upstream Reddit campaign id (from the stored row); status is model.CampaignRunActive or
// model.CampaignRunPaused. Returns nil only when the platform confirms the change.
func (d *RedditDispatcher) ToggleStatus(ctx context.Context, projectID string, platform model.Provider, platformCampaignID, status string) error {
	res, err := d.creds.resolve(ctx, projectID, platform)
	if err != nil {
		return err
	}
	if res.status != model.StatusActive {
		return fmt.Errorf("reddit connection for project %s is %s, not active", projectID, res.status)
	}
	var creds redditCreds
	if err := json.Unmarshal(res.plaintext, &creds); err != nil {
		return fmt.Errorf("decode reddit credentials: %w", err)
	}
	if creds.ClientID == "" || creds.ClientSecret == "" || creds.RefreshToken == "" {
		return fmt.Errorf("reddit credentials are incomplete (need clientId, clientSecret, refreshToken)")
	}
	if strings.TrimSpace(res.accountID) == "" {
		return fmt.Errorf("reddit connection for project %s has no account id", projectID)
	}
	redditStatus, err := redditRunStatus(status)
	if err != nil {
		return err
	}
	client := reddit.NewClient(
		reddit.Credentials{ClientID: creds.ClientID, ClientSecret: creds.ClientSecret, RefreshToken: creds.RefreshToken},
		reddit.AccountConfig{AccountID: res.accountID, Label: res.label},
		d.opts...,
	)
	return client.UpdateCampaignStatus(ctx, platformCampaignID, redditStatus)
}

// redditRunStatus maps the service-level run state (active/paused) to the reddit client's
// configured_status enum.
func redditRunStatus(status string) (string, error) {
	switch status {
	case model.CampaignRunActive:
		return reddit.StatusActive, nil
	case model.CampaignRunPaused:
		return reddit.StatusPaused, nil
	default:
		return "", fmt.Errorf("unsupported campaign run status %q (want %q or %q)", status, model.CampaignRunActive, model.CampaignRunPaused)
	}
}

// campaignFromReddit maps the client result to the persistence model. The
// orchestrator fills project/brief/job/platform (and, for a retained ambiguous
// orphan, status); this sets what only the dispatcher knows — upstream id, name, the
// provider result blob, and a "created" status on the success path (the orchestrator
// does not set one on success, and UpsertCampaign writes Status verbatim).
func campaignFromReddit(ctx context.Context, r *reddit.CampaignResult, cfg redditConfig) *model.Campaign {
	c := &model.Campaign{
		PlatformCampaignID: r.CampaignID,
		CampaignName:       r.CampaignName,
		Status:             campaignStatusCreated,
	}
	// Persist the budget/schedule/config the caller supplied. The Reddit client always
	// creates campaigns with goal_type LIFETIME_SPEND (client.go) — budgetUsd is a
	// LIFETIME spend cap, not a daily one — so the persisted budget_type is lifetime.
	// ConfigSnapshot captures the validated config for reconciliation, but with PostURL
	// SANITIZED: a post URL may carry secrets in its query/fragment (the client's step
	// log redacts them via redactURL for exactly this reason), and config_snapshot is
	// stored UNENCRYPTED in Postgres — so we strip the query/fragment before snapshotting.
	snapshot := cfg
	snapshot.PostURL = sanitizeSnapshotURL(cfg.PostURL)
	applyCampaignConfig(ctx, c, cfg.BudgetUSD, true, cfg.StartDate, cfg.EndDate, snapshot)
	if raw, err := json.Marshal(r); err == nil {
		c.Result = raw
	}
	return c
}

// decodeBriefFields pulls the shared event fields out of the brief. EventName is
// required by every platform's create contract.
func decodeBriefFields(brief *model.CampaignBrief) (briefFields, error) {
	var bf briefFields
	// The event/course destination is the brief's TOP-LEVEL url field (design/brief.go),
	// not a nested JSON key — use it as the RegistrationURL.
	bf.RegistrationURL = strings.TrimSpace(brief.URL)
	// EventDetails is the primary source for the remaining fields; Copy may also carry
	// a token; a nested registrationUrl is a fallback only if the top-level url is empty.
	for _, blob := range []json.RawMessage{brief.EventDetails, brief.Copy} {
		if len(blob) == 0 {
			continue
		}
		var partial briefFields
		if err := json.Unmarshal(blob, &partial); err != nil {
			continue // a blob that isn't this shape is fine; skip it
		}
		if bf.EventName == "" {
			bf.EventName = partial.EventName
		}
		if bf.RegistrationURL == "" {
			bf.RegistrationURL = partial.RegistrationURL
		}
		if bf.HSToken == "" {
			bf.HSToken = partial.HSToken
		}
	}
	if strings.TrimSpace(bf.EventName) == "" {
		return bf, fmt.Errorf("brief %s has no eventName in its details", brief.ID)
	}
	return bf, nil
}
