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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
)

// metaCreds is the credential shape stored (encrypted) for a Meta connection. Meta
// authenticates with a single long-lived OAuth2 access token.
// metaCreds mirrors MetaAdsCredentials's field name (no json tag) — the persisted
// JSON key is the Go field name (AccessToken), see redditCreds.
type metaCreds struct {
	AccessToken string
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
	// CurrencyOffset is a FALLBACK minor-unit scale (1 for zero-decimal currencies like
	// JPY, 100 for most), NOT an unconditional override: the client's preflight derives
	// the offset from the account's currency and that is authoritative — a supplied value
	// is used only when the currency can't be resolved, and a value conflicting with a
	// recognized account currency is REJECTED by the client during dispatch. Because
	// CreateCampaigns is asynchronous (a 202 is returned before dispatch runs), that
	// rejection fails the platform job BEFORE any mutating Meta call — it is a pre-create
	// dispatch failure, not a synchronous 4xx on the campaign request. Left 0 → derived.
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
	// hsToken is a documented TOP-LEVEL config envelope field (docs/api-catalog.md —
	// sibling to metaConfig, NOT nested in it), read via the shared envelope helper. A
	// request-supplied token takes precedence over the brief blobs; without this a
	// documented config.hsToken is silently ignored and the client falls back to the
	// event slug for utm_campaign, losing the HubSpot attribution.
	hsToken, err := envelopeHSToken(config)
	if err != nil {
		return nil, notCreated(err) // a wrong-typed hsToken is a caller error (pre-create)
	}
	if hsToken == "" {
		hsToken = bf.HSToken
	}

	in := meta.CampaignInput{
		EventName: bf.EventName,
		EventSlug: brief.EventSlug,
		// Project stamped from the authenticated scope, not caller JSON (api-catalog).
		Project:         brief.ProjectID,
		RegistrationURL: bf.RegistrationURL,
		HSToken:         hsToken,
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
		return campaignFromMeta(ctx, result, cfg), fmt.Errorf("meta campaign creation UNCONFIRMED: %w", cerr)
	}
	// Meta creates one ad per requested variant but treats per-variant ad failures as
	// NON-fatal (the client records them in Steps and continues), so a nil error can
	// still come back with AdCount < the number of variants requested — a DEGRADED
	// success. We do NOT return an error: the campaign IS created, so failing the job
	// would mislead and be unrecoverable by retry (idempotency short-circuits a
	// re-dispatch, never re-running the ad steps). Instead the shortfall is made VISIBLE
	// as a distinct created_degraded status (per-variant failures are in Result.Steps)
	// for a human/monitor to reconcile. Mirrors the reddit/twitter partial-ad handling.
	// All requested variants are valid here (the client fails fast on a malformed
	// variant), so len(cfg.Variants) is the requested count.
	camp := campaignFromMeta(ctx, result, cfg)
	if result.AdCount < len(cfg.Variants) {
		camp.Status = campaignStatusCreatedDegraded
	}
	return camp, nil
}

// ToggleStatus pauses or resumes an existing Meta campaign on the platform. It resolves the
// connection (an inactive/undecryptable/incomplete connection is a clean error), builds the
// client, and CASCADES the status to the campaign, its ad set, and every ad — Meta's create
// PAUSES all three, so toggling only the campaign to ACTIVE would not serve. campaign is the
// persisted row; the ad set id is read from its CampaignResult (Meta persists the ad set id
// but not the individual ad ids, which the client discovers via GET /{adSetID}/ads). status
// is model.CampaignRunActive or model.CampaignRunPaused. Returns nil only when the platform
// confirms; an UNCONFIRMED outcome (including a partial cascade) is wrapped so the caller
// reports "verify before retry" (via the Unconfirmed() behavioral interface).
func (d *MetaDispatcher) ToggleStatus(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, status string) error {
	metaStatus, err := metaRunStatus(status)
	if err != nil {
		return err
	}
	res, err := d.creds.resolve(ctx, projectID, platform)
	if err != nil {
		return err
	}
	if res.status != model.StatusActive {
		return fmt.Errorf("meta connection for project %s is %s, not active", projectID, res.status)
	}
	var creds metaCreds
	if err := json.Unmarshal(res.plaintext, &creds); err != nil {
		return fmt.Errorf("decode meta credentials: %w", err)
	}
	if strings.TrimSpace(creds.AccessToken) == "" {
		return fmt.Errorf("meta credentials are incomplete (need accessToken)")
	}
	// A status update targets the campaign node by id (POST /{campaignID}); it needs no
	// account id or page id, so those are not required here (unlike Dispatch).
	client := meta.NewClient(meta.Credentials{AccessToken: creds.AccessToken}, meta.AccountConfig{AccountID: strings.TrimSpace(res.accountID), Label: res.label}, d.opts...)
	// Cascade to the ad set (and its ads) as well as the campaign: CreateCampaign PAUSES the
	// campaign, ad set, and every ad, so toggling only the campaign to ACTIVE would not serve.
	// The ad set id is read from the persisted CampaignResult (Meta stores it, but not the
	// individual ad ids — the client discovers those via GET /{adSetID}/ads).
	adSetID := metaAdSetID(campaign)
	if uerr := client.UpdateCampaignAndChildrenStatus(ctx, campaign.PlatformCampaignID, adSetID, metaStatus); uerr != nil {
		if meta.IsOutcomeUnconfirmed(uerr) {
			return &unconfirmedToggleError{err: uerr}
		}
		return uerr
	}
	return nil
}

// metaAdSetID pulls the ad set id the create path stored in the persisted CampaignResult
// blob. A missing/unparseable blob yields "" (the campaign is toggled alone — the service
// already blocks toggling a degraded campaign).
func metaAdSetID(campaign *model.Campaign) string {
	if campaign == nil || len(campaign.Result) == 0 {
		return ""
	}
	var blob struct {
		AdSetID string `json:"AdSetID"`
	}
	if err := json.Unmarshal(campaign.Result, &blob); err != nil {
		return ""
	}
	return blob.AdSetID
}

// metaRunStatus maps the service run state (active/paused) to Meta's status enum.
func metaRunStatus(status string) (string, error) {
	switch status {
	case model.CampaignRunActive:
		return meta.StatusActive, nil
	case model.CampaignRunPaused:
		return meta.StatusPaused, nil
	default:
		return "", fmt.Errorf("unsupported campaign run status %q (want %q or %q)", status, model.CampaignRunActive, model.CampaignRunPaused)
	}
}

// campaignFromMeta maps the client result to the persistence model.
func campaignFromMeta(ctx context.Context, r *meta.CampaignResult, cfg metaConfig) *model.Campaign {
	c := &model.Campaign{
		PlatformCampaignID: r.CampaignID,
		CampaignName:       r.CampaignName,
		Status:             campaignStatusCreated,
	}
	// Persist the budget/schedule/config the caller supplied (Meta honors a
	// lifetime-vs-daily budget flag). ConfigSnapshot captures the validated config.
	applyCampaignConfig(ctx, c, cfg.Budget, cfg.LifetimeBudget, cfg.StartDate, cfg.EndDate, cfg)
	if raw, err := json.Marshal(r); err != nil {
		// A marshal failure should be near-impossible for this plain struct, but do NOT
		// swallow it: on the degraded/ambiguous-orphan paths Result is the sole carrier
		// of the per-variant failure Steps and the reconcile-by-name payload, so a
		// silently-empty Result loses reconciliation data precisely when it's most
		// needed. Log it (the row is still persisted with its id/status). Mirrors the
		// linkedin adapter.
		slog.WarnContext(ctx, "failed to marshal meta campaign result blob (Result left empty)",
			"campaign_id", c.PlatformCampaignID, "error", err)
	} else {
		c.Result = raw
	}
	return c
}
