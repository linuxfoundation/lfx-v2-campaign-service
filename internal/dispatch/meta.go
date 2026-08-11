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

// resolveMetaCredentials fetches the project's Meta connection and validates it is usable
// for ANY Meta operation — active status, decodable credentials, non-empty access token —
// tagging each defect with domain.ErrConnectionNotUsable plus a reason sentinel, mirroring
// resolveRedditClient. It deliberately does NOT check account_id: only Dispatch needs it (a
// campaign create builds Graph paths as /{accountID}/campaigns etc. — see
// internal/platform/meta/client.go's AccountID checks ahead of CreateCampaign). ToggleStatus
// and ReadMetrics target an existing campaign by id (POST /{campaignID}, GET
// /{campaignID}/insights) and never read AccountConfig.AccountID at all, so requiring one
// there would refuse a perfectly servable pause/metrics-read on a connection whose account
// selection was later cleared via PUT.
func (d *MetaDispatcher) resolveMetaCredentials(ctx context.Context, projectID string, platform model.Provider) (res *resolved, creds metaCreds, err error) {
	res, err = d.creds.resolve(ctx, projectID, platform)
	if err != nil {
		return nil, metaCreds{}, err
	}
	defer func() { err = res.systemScoped(err) }()
	if res.status != model.StatusActive {
		return nil, metaCreds{}, fmt.Errorf("%w: %w: meta connection for project %s is %s, not active",
			domain.ErrConnectionNotUsable, domain.ErrConnectionInactive, projectID, res.status)
	}
	if uerr := json.Unmarshal(res.plaintext, &creds); uerr != nil {
		// The cause is dropped, not wrapped: it derives from the DECRYPTED credential
		// blob, and encoding/json quotes its input in *json.SyntaxError /
		// *json.UnmarshalTypeError. Matches resolveRedditClient's decode error.
		return nil, metaCreds{}, fmt.Errorf("%w: %w: meta credentials for project %s are not valid JSON",
			domain.ErrConnectionNotUsable, domain.ErrCredentialsUndecodable, projectID)
	}
	if strings.TrimSpace(creds.AccessToken) == "" {
		return nil, metaCreds{}, fmt.Errorf("%w: %w: meta credentials are incomplete (need accessToken)",
			domain.ErrConnectionNotUsable, domain.ErrCredentialsIncomplete)
	}
	return res, creds, nil
}

// requireMetaAccountID returns res.accountID trimmed, or a tagged 409 naming the missing
// choice — domain.ErrAccountNotSelected, which unusableConnectionReason reports as
// "account_not_selected" — when it is empty. Called ONLY by Dispatch: an account-id-less
// connection (the credentials-only bootstrap state — see MetaAdsConnectionConfig in
// design/connection.go) can create no campaign, and this refuses it HERE, before an empty
// AccountConfig.AccountID can reach the Meta client and fail opaquely (a malformed
// "//campaigns" request) instead of with a clear, actionable 409. ToggleStatus and
// ReadMetrics do not call this — see resolveMetaCredentials for why.
func requireMetaAccountID(res *resolved, projectID string) (string, error) {
	accountID := strings.TrimSpace(res.accountID)
	if accountID == "" {
		return "", res.systemScoped(fmt.Errorf("%w: %w: meta connection for project %s has no account id selected",
			domain.ErrConnectionNotUsable, domain.ErrAccountNotSelected, projectID))
	}
	return accountID, nil
}

// Dispatch implements service.PlatformDispatcher for Meta.
func (d *MetaDispatcher) Dispatch(ctx context.Context, brief *model.CampaignBrief, platform model.Provider, config json.RawMessage) (*model.Campaign, error) {
	res, creds, err := d.resolveMetaCredentials(ctx, brief.ProjectID, platform)
	if err != nil {
		return nil, notCreated(err)
	}
	accountID, err := requireMetaAccountID(res, brief.ProjectID)
	if err != nil {
		return nil, notCreated(err)
	}
	pageID := strings.TrimSpace(res.providerConfig["page_id"])
	if pageID == "" {
		// page_id is Required at connection creation (design/connection.go), so this is
		// unreachable through normal API validation; it only fires if a row somehow
		// stored an empty value. CreateCampaigns already returned 202 by the time Dispatch
		// runs, so this can't surface as a synchronous 4xx — notCreated marks it
		// NoUpstreamCreate so the orchestrator releases the pending claim instead of
		// retaining it for a create that may have partially landed, and the sentinel/reason
		// chain (ErrConnectionNotUsable/ErrProviderConfigInvalid) is what a human reads back
		// from the async job's failure log, same as every other stored-state defect here.
		return nil, notCreated(res.systemScoped(fmt.Errorf("%w: %w: meta connection for project %s is missing page id",
			domain.ErrConnectionNotUsable, domain.ErrProviderConfigInvalid, brief.ProjectID)))
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
// connection (an inactive/undecryptable/incomplete connection is a clean 409, not a 503 —
// see resolveMetaCredentials), builds the client, and CASCADES the status to the campaign,
// its ad set, and every ad — Meta's create PAUSES all three, so toggling only the campaign
// to ACTIVE would not serve. campaign is the persisted row; the ad set id is read from its
// CampaignResult (Meta persists the ad set id but not the individual ad ids, which the
// client discovers via GET /{adSetID}/ads). status is model.CampaignRunActive or
// model.CampaignRunPaused. Returns nil only when the platform confirms; an UNCONFIRMED
// outcome (including a partial cascade) is wrapped so the caller reports "verify before
// retry" (via the Unconfirmed() behavioral interface).
func (d *MetaDispatcher) ToggleStatus(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, status string) error {
	metaStatus, err := metaRunStatus(status)
	if err != nil {
		return err
	}
	res, creds, err := d.resolveMetaCredentials(ctx, projectID, platform)
	if err != nil {
		return err
	}
	// A status update targets the campaign node by id (POST /{campaignID}); it needs
	// neither page id nor account id (unlike Dispatch — see resolveMetaCredentials), so an
	// account cleared via PUT after the campaign was created does not block pausing or
	// resuming it.
	client := meta.NewClient(meta.Credentials{AccessToken: creds.AccessToken}, meta.AccountConfig{AccountID: strings.TrimSpace(res.accountID), Label: res.label}, d.opts...)
	// Cascade to the ad set (and its ads) as well as the campaign: CreateCampaign PAUSES the
	// campaign, ad set, and every ad, so toggling only the campaign to ACTIVE would not serve.
	// The ad set id is read from the persisted CampaignResult (Meta stores it, but not the
	// individual ad ids — the client discovers those via GET /{adSetID}/ads).
	adSetID := metaAdSetID(campaign)
	// ACTIVATE requires a servable tree. A legacy/incomplete "created" row can lack the ad
	// set id (absent/unparseable Result), so activating would fail without ever serving.
	// Refuse before any HTTP call and return ErrCampaignNotProvisioned so the service maps it
	// to a 409 state error (the platform is never contacted), not the default 503 — matching
	// the reddit path. Pausing needs no child id (pausing the parent stops delivery).
	if metaStatus == meta.StatusActive && strings.TrimSpace(adSetID) == "" {
		return fmt.Errorf("%w: meta campaign %s cannot be activated because it has no ad set to serve", domain.ErrCampaignNotProvisioned, campaign.PlatformCampaignID)
	}
	if uerr := client.UpdateCampaignAndChildrenStatus(ctx, campaign.PlatformCampaignID, adSetID, metaStatus); uerr != nil {
		// An activate refused up front because the ad set has zero ads is a local/state error
		// (the platform mutation never ran), so classify it as ErrCampaignNotProvisioned → 409,
		// not the default 503 — deterministic "reprovision", not a transient "verify/retry".
		// Mirrors the LinkedIn dispatcher's zero-creatives handling.
		if meta.IsNotServable(uerr) {
			return fmt.Errorf("%w: %s", domain.ErrCampaignNotProvisioned, uerr.Error())
		}
		if meta.IsOutcomeUnconfirmed(uerr) {
			return &unconfirmedToggleError{err: uerr}
		}
		return uerr
	}
	return nil
}

// metaMetricsWindow maps the platform-agnostic model.MetricsWindow vocabulary to Meta's
// own MetricsWindow literals (Insights date_preset values). All seven shared windows are
// supported, so this is a pure rename, not a subset like X Ads' 7-day-capped mapping.
func metaMetricsWindow(w model.MetricsWindow) (meta.MetricsWindow, error) {
	switch w {
	case model.MetricsWindowToday:
		return meta.WindowToday, nil
	case model.MetricsWindowYesterday:
		return meta.WindowYesterday, nil
	case model.MetricsWindowLast7Days:
		return meta.WindowLast7Days, nil
	case model.MetricsWindowLast14Days:
		return meta.WindowLast14Days, nil
	case model.MetricsWindowLast30Days:
		return meta.WindowLast30Days, nil
	case model.MetricsWindowThisMonth:
		return meta.WindowThisMonth, nil
	case model.MetricsWindowLastMonth:
		return meta.WindowLastMonth, nil
	default:
		return "", fmt.Errorf("unsupported metrics window %q", w)
	}
}

// ReadMetrics implements service.MetricsReader for Meta. It resolves the same connection
// ToggleStatus does (no page id or account id required — a metrics read targets the
// campaign node by id via GET /{campaignID}/insights, like the status update; see
// resolveMetaCredentials) and reads the campaign's live Insights metrics, mapping the
// platform-agnostic window to Meta's own vocabulary via metaMetricsWindow before calling
// the client.
func (d *MetaDispatcher) ReadMetrics(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, window model.MetricsWindow) (*model.CampaignMetrics, error) {
	res, creds, err := d.resolveMetaCredentials(ctx, projectID, platform)
	if err != nil {
		return nil, err
	}
	metaWindow, err := metaMetricsWindow(window)
	if err != nil {
		return nil, err
	}
	client := meta.NewClient(meta.Credentials{AccessToken: creds.AccessToken}, meta.AccountConfig{AccountID: strings.TrimSpace(res.accountID), Label: res.label}, d.opts...)
	m, err := client.GetCampaignMetrics(ctx, campaign.PlatformCampaignID, metaWindow)
	if err != nil {
		return nil, err
	}
	return &model.CampaignMetrics{
		CampaignID:  m.CampaignID,
		Window:      window,
		Impressions: m.Impressions,
		Clicks:      m.Clicks,
		CostMicros:  m.CostMicros,
		Ctr:         m.Ctr,
	}, nil
}

// metaAdSetID pulls the ad set id the create path stored in the persisted CampaignResult
// blob. A missing/unparseable blob yields "" (the campaign is toggled alone — the service
// already blocks toggling a degraded campaign, and on Meta the CAMPAIGN status is the
// effective delivery gate, so a PAUSE without the ad set id still stops serving; only an
// ACTIVATE requires it, and ToggleStatus refuses that up front — see the guard above).
//
// It unmarshals into the SAME meta.CampaignResult type the create path marshals into Result
// (campaignFromMeta), rather than a private struct with a hardcoded "AdSetID" key. This keeps
// ONE definition of the persisted wire shape: if the CampaignResult field/tag is ever renamed,
// reader and writer move together instead of silently desyncing (the previous inline struct
// matched only by coincidence of Go's default field-name marshaling). Making the dependency on
// the create-path result explicit was the intent behind the review note; a dedicated
// model.Campaign.PlatformAdSetID column was considered but is Meta-specific (no other platform
// has an ad set) and would need a schema migration on the shared campaigns table — this keeps
// the fix proportional to a status-toggle PR while removing the fragility.
func metaAdSetID(campaign *model.Campaign) string {
	if campaign == nil || len(campaign.Result) == 0 {
		return ""
	}
	var blob meta.CampaignResult
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

// resolveMetaDiscoveryClient builds a Meta client for the ACCOUNT-DISCOVERY path.
//
// It applies the same stored-state checks the dispatch and metrics paths apply — the
// connection must be active, the decrypted blob must decode, and the access token must be
// present — and deliberately does NOT require an account id. That omission is the point:
// the endpoint exists to answer "which ad account should this connection use?", so
// demanding one would make it reachable only by connections that no longer need it.
//
// The lifecycle this serves TODAY is re-pointing: reading the choices before a PUT moves an
// existing connection to a different account. First-time bootstrap — creating a connection
// with credentials only and choosing an account afterwards, the way Google Ads works — is
// NOT reachable for Meta, because MetaAdsConnectionConfig still declares
// Required("account_id") and the create is rejected as a 400 before this code is involved.
// Google Ads dropped that requirement precisely because it had a discovery endpoint; Meta
// now has one too, but the change is not free here. Dispatch — the ONE Meta path that needs
// an account id, since ToggleStatus and ReadMetrics both target the campaign node by id and
// say so — would have to tag its empty-id failure with domain.ErrAccountNotSelected the way
// resolveGoogleAdsClient does, or a connection parked mid-bootstrap answers a create with a
// generic error instead of one that names the missing choice. That is tracked separately
// (LFXV2-3061); this resolver is
// already correct for it, which is why the account id is not consulted here.
//
// AccountConfig is left ZERO for the same reason. GET /me/adaccounts is account-agnostic:
// it asks what the TOKEN reaches, so scoping the client to one of the answers would narrow
// the response to a subset of the question.
//
// Every check here inspects stored state, before any request exists, so a failure means
// the connection needs EDITING rather than retrying — each is tagged with
// domain.ErrConnectionNotUsable alongside the sentinel naming the specific defect, which
// is what the handler classifies on to answer 400 instead of 503. Errors from
// creds.resolve are deliberately left untagged: that layer distinguishes ErrNotFound (no
// connection at all — a 404) from a storage failure (genuinely transient — a 503), and
// flattening both into "not usable" would lose it.
//
// Every not-usable return below also passes through res.systemScoped, via a named return and
// a defer, for the reason validateGoogleAdsCredentials documents: the three defects here are
// in STORED STATE, and on a project that owns no connection that stored state belongs to the
// LF system row it fell back to. Untagged, the shared handler answers such a project a 400
// telling it to go and edit a connection it does not have and cannot address, instead of the
// 500 that pages whoever installed the system credential. The defer rather than three call
// sites is deliberate: this is a defect class the Google Ads path already had and fixed once
// by exactly this means, and a fourth return added later must not be able to forget.
// systemScoped is a no-op for project-owned rows and idempotent, so it costs nothing here.
func (d *MetaDispatcher) resolveMetaDiscoveryClient(ctx context.Context, projectID string, platform model.Provider) (client *meta.Client, err error) {
	res, err := d.creds.resolve(ctx, projectID, platform)
	if err != nil {
		return nil, err
	}
	defer func() { err = res.systemScoped(err) }()
	if res.status != model.StatusActive {
		return nil, fmt.Errorf("%w: %w: meta connection for project %s is %s, not active",
			domain.ErrConnectionNotUsable, domain.ErrConnectionInactive, projectID, res.status)
	}
	var creds metaCreds
	if uerr := json.Unmarshal(res.plaintext, &creds); uerr != nil {
		// The unmarshal error is DROPPED, not wrapped. It is the only value in this
		// function derived from the DECRYPTED credential blob, and this error reaches the
		// discovery handler, which logs it and describes the not-usable arm to the caller.
		// Today's encoding/json happens not to quote the offending bytes for a struct of
		// string fields — it reports "invalid character 'T' after object key:value pair",
		// not the input — but that is a behaviour, not a documented guarantee, and it does
		// not hold for every field type: a number decoded into a numeric field appears in
		// the message verbatim. Dropping the cause removes the whole class instead of
		// resting on a property of the stdlib nobody here controls, and it costs nothing:
		// the sentinel already names the only thing an operator can act on, which is that
		// this connection's stored credential has to be re-entered.
		return nil, fmt.Errorf("%w: %w", domain.ErrConnectionNotUsable, domain.ErrCredentialsUndecodable)
	}
	if strings.TrimSpace(creds.AccessToken) == "" {
		return nil, fmt.Errorf("%w: %w: meta credentials need accessToken",
			domain.ErrConnectionNotUsable, domain.ErrCredentialsIncomplete)
	}
	return meta.NewClient(meta.Credentials{AccessToken: creds.AccessToken}, meta.AccountConfig{}, d.opts...), nil
}

// ListAccounts discovers the ad accounts reachable via the project's stored, encrypted
// Meta connection credential, returning minimal identifying information (the act_-prefixed
// account id and a display label).
//
// It satisfies the service-side AccountLister interface, which Orchestrator.ReadAccounts
// type-asserts on the dispatcher for the requested platform; a platform whose dispatcher
// does not implement it gets ErrAccountsUnsupported and the ad platform is never contacted.
// The error contract of resolveMetaDiscoveryClient is what the endpoint's status mapping
// relies on.
func (d *MetaDispatcher) ListAccounts(ctx context.Context, projectID string, platform model.Provider) ([]model.AccessibleAccount, error) {
	client, err := d.resolveMetaDiscoveryClient(ctx, projectID, platform)
	if err != nil {
		return nil, err
	}
	adAccounts, lerr := client.ListAdAccounts(ctx)
	if lerr != nil {
		return nil, lerr
	}
	// make(..., 0, n) rather than a nil var: a token that legitimately reaches zero ad
	// accounts is an empty list, not an error, and the two must stay distinguishable at
	// the service boundary — Orchestrator.ReadAccounts rejects a nil result as a contract
	// violation precisely so an empty answer keeps its meaning.
	accounts := make([]model.AccessibleAccount, 0, len(adAccounts))
	for _, a := range adAccounts {
		accounts = append(accounts, model.AccessibleAccount{ID: a.ID, Label: metaAccountLabel(a)})
	}
	return accounts, nil
}

// metaAccountLabel builds the string a picker shows for one ad account.
//
// It never returns "" for an account that has any identifying information: an account with
// no `name` falls back to its id, because a blank row in a picker is unpickable and the id
// is what actually gets stored. A KNOWN-BAD account_status is appended in parentheses so
// the user sees WHY the account they were about to choose will be refused by
// CreateCampaign's preflight — which reads the same map — rather than choosing it and
// meeting the refusal one step later, at dispatch, with no way back to this list.
//
// An unrecognized or absent status appends nothing. Meta omits account_status on accounts
// it will not report on, and treating absence as a defect would label a working account.
func metaAccountLabel(a meta.AdAccount) string {
	label := a.Name
	if label == "" {
		label = a.ID
	}
	if reason := a.StatusLabel(); reason != "" {
		label += " (" + reason + ")"
	}
	return label
}
