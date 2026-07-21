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
type redditCreds struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
	RefreshToken string `json:"refreshToken"`
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

	in := reddit.CampaignInput{
		EventName:       bf.EventName,
		EventSlug:       brief.EventSlug,
		RegistrationURL: bf.RegistrationURL,
		HSToken:         bf.HSToken,
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
		reddit.AccountConfig{AccountID: res.accountID, Label: res.providerConfig["label"]},
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
	//                       so the orchestrator retains the claim and records the orphan.
	//   - (result, nil)   → success.
	result, cerr := client.CreateCampaign(ctx, in)
	if cerr != nil {
		if result == nil {
			return nil, notCreated(fmt.Errorf("reddit campaign creation failed before any upstream create: %w", cerr))
		}
		return campaignFromReddit(result), fmt.Errorf("reddit campaign creation UNCONFIRMED: %w", cerr)
	}
	return campaignFromReddit(result), nil
}

// campaignFromReddit maps the client result to the persistence model. The
// orchestrator fills project/brief/job/platform (and, for a retained ambiguous
// orphan, status); this sets what only the dispatcher knows — upstream id, name, the
// provider result blob, and a "created" status on the success path (the orchestrator
// does not set one on success, and UpsertCampaign writes Status verbatim).
func campaignFromReddit(r *reddit.CampaignResult) *model.Campaign {
	c := &model.Campaign{
		PlatformCampaignID: r.CampaignID,
		CampaignName:       r.CampaignName,
		Status:             campaignStatusCreated,
	}
	if raw, err := json.Marshal(r); err == nil {
		c.Result = raw
	}
	return c
}

// decodeBriefFields pulls the shared event fields out of the brief's opaque JSON
// blobs. EventName is required by every platform's create contract.
func decodeBriefFields(brief *model.CampaignBrief) (briefFields, error) {
	var bf briefFields
	// EventDetails is the primary source; Copy may also carry a token.
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
