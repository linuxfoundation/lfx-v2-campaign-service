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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
)

// metaCreds is the credential shape stored (encrypted) for a Meta connection. Meta
// authenticates with a single long-lived OAuth2 access token.
type metaCreds struct {
	AccessToken string `json:"accessToken"`
}

// metaConfig is the per-platform campaign config the caller passes for Meta in
// CreateCampaigns' Input.Config (delivered here as the Dispatch `config`).
//
// Budget is in whole units of the ad ACCOUNT's currency (NOT USD — the client does
// no FX conversion). CurrencyOffset optionally overrides the account's minor-unit
// scale; when zero the client derives it from the account's ISO currency during its
// preflight.
type metaConfig struct {
	Budget         float64          `json:"budget"`
	LifetimeBudget bool             `json:"lifetimeBudget"`
	StartDate      string           `json:"startDate"` // YYYY-MM-DD
	EndDate        string           `json:"endDate"`   // YYYY-MM-DD
	Objective      string           `json:"objective"` // awareness|traffic|engagement|leads|conversions
	GeoTargets     []string         `json:"geoTargets"`
	Placements     meta.Placement   `json:"placements"`
	PixelID        string           `json:"pixelId"`
	Variants       []meta.AdVariant `json:"variants"`
	// CurrencyOffset optionally overrides the account minor-unit offset (1 for
	// zero-decimal currencies like JPY, 100 for most). Left 0 → derived by the client.
	CurrencyOffset int64 `json:"currencyOffset"`
}

// MetaDispatcher creates Meta (Facebook/Instagram) campaigns for the orchestrator.
type MetaDispatcher struct {
	creds *credsSource
	opts  []meta.Option
}

// NewMetaDispatcher builds the adapter from the connection repo + encryptor.
func NewMetaDispatcher(repo connReader, enc domain.Encryptor, opts ...meta.Option) *MetaDispatcher {
	return &MetaDispatcher{creds: newCredsSource(repo, enc), opts: opts}
}

// Dispatch implements service.PlatformDispatcher for Meta.
func (d *MetaDispatcher) Dispatch(ctx context.Context, brief *model.CampaignBrief, platform model.Provider, config json.RawMessage) (*model.Campaign, error) {
	res, err := d.creds.resolve(ctx, brief.ProjectID, platform)
	if err != nil {
		return nil, err // preCreateError
	}
	if res.status != model.StatusActive {
		return nil, notCreated(fmt.Errorf("meta connection for project %s is %s, not active", brief.ProjectID, res.status))
	}

	var creds metaCreds
	if err := json.Unmarshal(res.plaintext, &creds); err != nil {
		return nil, notCreated(fmt.Errorf("decode meta credentials: %w", err))
	}
	if strings.TrimSpace(creds.AccessToken) == "" {
		return nil, notCreated(fmt.Errorf("meta credentials are incomplete (need accessToken)"))
	}

	accountID := strings.TrimSpace(res.accountID)
	pageID := strings.TrimSpace(res.providerConfig["page_id"])
	if accountID == "" || pageID == "" {
		return nil, notCreated(fmt.Errorf("meta connection for project %s is missing account id or page id", brief.ProjectID))
	}

	var cfg metaConfig
	if err := unmarshalPlatformConfig(config, "metaConfig", &cfg); err != nil {
		return nil, notCreated(err)
	}
	bf, err := decodeBriefFields(brief)
	if err != nil {
		return nil, notCreated(err)
	}

	account := meta.AccountConfig{
		AccountID:      accountID,
		PageID:         pageID,
		Label:          res.label,
		CurrencyOffset: cfg.CurrencyOffset,
	}
	in := meta.CampaignInput{
		EventName: bf.EventName,
		EventSlug: brief.EventSlug,
		// Project stamped from the authenticated scope, not caller JSON (api-catalog).
		Project:         brief.ProjectID,
		RegistrationURL: bf.RegistrationURL,
		HSToken:         bf.HSToken,
		Objective:       cfg.Objective,
		GeoTargets:      cfg.GeoTargets,
		Budget:          cfg.Budget,
		LifetimeBudget:  cfg.LifetimeBudget,
		StartDate:       cfg.StartDate,
		EndDate:         cfg.EndDate,
		Placements:      cfg.Placements,
		PixelID:         cfg.PixelID,
		Variants:        cfg.Variants,
	}

	client := meta.NewClient(meta.Credentials{AccessToken: creds.AccessToken}, account, d.opts...)

	// Release the claim ONLY when result==nil. An ambiguous create (or a post-campaign
	// failure) returns a non-nil partial whose CampaignID may be empty but still means
	// "may exist" — gating on an empty CampaignID would wrongly release the claim.
	result, cerr := client.CreateCampaign(ctx, in)
	if cerr != nil {
		if result == nil {
			return nil, notCreated(fmt.Errorf("meta campaign creation failed before any upstream create: %w", cerr))
		}
		return campaignFromMeta(result), fmt.Errorf("meta campaign creation UNCONFIRMED: %w", cerr)
	}
	return campaignFromMeta(result), nil
}

// campaignFromMeta maps the client result to the persistence model.
func campaignFromMeta(r *meta.CampaignResult) *model.Campaign {
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
