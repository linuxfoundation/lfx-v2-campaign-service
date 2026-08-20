// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
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
	Budget         float64        `json:"budget"`
	LifetimeBudget bool           `json:"lifetimeBudget"`
	StartDate      string         `json:"startDate"` // YYYY-MM-DD
	EndDate        string         `json:"endDate"`   // YYYY-MM-DD
	Objective      string         `json:"objective"` // awareness|traffic|engagement|leads|conversions
	GeoTargets     []string       `json:"geoTargets"`
	Placements     meta.Placement `json:"placements"`
	PixelID        string         `json:"pixelId"`
	// InstagramUserID (IGSID) binds the ad creative to an Instagram account. REQUIRED
	// when any Instagram placement is used (the default placements include Instagram
	// Feed) — without it Meta refuses to publish the ad with "Please add Instagram
	// account". Left empty for Facebook-only campaigns.
	InstagramUserID string `json:"instagramUserId"`
	// DSABeneficiary and DSAPayor are the EU DSA advertiser/payer disclosures. Required
	// for a launch-ready ad set that targets a regulated location; Meta blocks publish
	// ("Please add Advertiser" / "Please add Payer") until both are set.
	DSABeneficiary string           `json:"dsaBeneficiary"`
	DSAPayor       string           `json:"dsaPayor"`
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
// tagging each defect with domain.ErrConnectionNotUsable plus a reason sentinel. The pattern —
// named returns, defer systemScoped, a reason sentinel under ErrConnectionNotUsable — ORIGINATES
// in Google Ads' validateGoogleAdsCredentials; resolveRedditClient adopted it from there and
// says so, and is the nearest sibling to read alongside this one. Cite the origin rather than
// the sibling: which adapter you copy from is a detail, which one DEFINES the shape is not.
// It deliberately does NOT check account_id: only Dispatch needs it (a
// campaign create builds Graph paths as /{accountID}/campaigns etc. — see
// internal/platform/meta/client.go's AccountID checks ahead of CreateCampaign). ToggleStatus
// and ReadMetrics target an existing campaign by id (POST /{campaignID}, GET
// /{campaignID}/insights) and never read AccountConfig.AccountID at all, so requiring one
// there would refuse a perfectly servable pause/metrics-read on a connection whose account
// selection was later cleared via PUT.
//
// resolveCreds selects the credential entry point: d.creds.resolve for creation and
// discovery (both governed by the forced-system flag) and d.creds.resolveExisting for an
// operation on an already-created campaign (never forced — see resolveExisting).
// Passed in rather than inferred, because only the caller knows whether it holds a campaign.
func (d *MetaDispatcher) resolveMetaCredentials(ctx context.Context, projectID string, platform model.Provider, resolveCreds credsResolver) (res *resolved, creds metaCreds, err error) {
	res, err = resolveCreds(ctx, projectID, platform)
	if err != nil {
		return nil, metaCreds{}, err
	}
	// The defer closes over `conn`, NOT the named return `res`. Every not-usable return
	// below sets res to nil before the defer runs, and systemScoped is a no-op on a nil
	// receiver — so reading the named return here would silently drop the system-row
	// attribution from exactly the errors that need it, on every caller: dispatch, toggle,
	// metrics and discovery alike. Failing open like that is the whole defect systemScoped
	// exists to prevent, and it leaves no trace, because the error is still correct in
	// every other respect. Bind the resolved connection once and read that.
	conn := res
	defer func() { err = conn.systemScoped(err) }()
	if res.status != model.StatusActive {
		return nil, metaCreds{}, fmt.Errorf("%w: %w: meta connection for project %s is %s, not active",
			domain.ErrConnectionNotUsable, domain.ErrConnectionInactive, projectID, res.status)
	}
	if uerr := json.Unmarshal(res.plaintext, &creds); uerr != nil {
		// The cause is DROPPED, not wrapped. It is the only value here derived from the
		// DECRYPTED credential blob, and encoding/json quotes its input in
		// *json.SyntaxError / *json.UnmarshalTypeError. Today's stdlib happens not to quote
		// the offending bytes for a struct of string fields — it reports "invalid character
		// 'T' after object key:value pair", not the input — but that is a behaviour, not a
		// documented guarantee, and it does not hold for every field type: a number decoded
		// into a numeric field appears in the message verbatim. Dropping the cause removes
		// the whole class rather than resting on a property of the stdlib nobody here
		// controls, and it costs nothing, because the sentinel already names the only thing
		// an operator can act on: this connection's stored credential has to be re-entered.
		// The project id below is not plaintext-derived and stays. Matches
		// resolveRedditClient's decode error. This reaches the discovery handler too, which
		// logs it and describes the not-usable arm to the caller.
		return nil, metaCreds{}, fmt.Errorf("%w: %w: meta credentials for project %s are not valid JSON",
			domain.ErrConnectionNotUsable, domain.ErrCredentialsUndecodable, projectID)
	}
	if strings.TrimSpace(creds.AccessToken) == "" {
		return nil, metaCreds{}, fmt.Errorf("%w: %w: meta credentials are incomplete (need accessToken)",
			domain.ErrConnectionNotUsable, domain.ErrCredentialsIncomplete)
	}
	return res, creds, nil
}

// requireMetaAccountID returns res.accountID trimmed, or — when it is empty — an error
// naming the missing choice: domain.ErrAccountNotSelected alongside
// domain.ErrConnectionNotUsable, which unusableConnectionReason reports as
// "account_not_selected". That pair is a CLASSIFICATION, not a status code, and the reason
// token reaches an operator through the LOG — not the job result. Be precise about this,
// because the natural assumption is wrong: the only caller is Dispatch, which is queued work,
// and dispatchPlatform collapses EVERY dispatcher error into the same
// "platform campaign creation failed" job result (internal/service/orchestrator.go). Nothing
// returned here reaches the caller as text. Google Ads' create path is in exactly the same
// position and says so at validateGoogleAdsConnection's call site; classification there buys
// log hygiene and claim semantics, and it buys the same here.
//
// The same sentinels DO drive a synchronous 409 (internal/service/brief.go) — which is why
// they are used rather than a bespoke error — but only from the status toggle and metrics
// read, and only for providers whose toggle/metrics need an account id. Meta's do not (they
// target the campaign node by id), so for Meta this sentinel has no synchronous call site at
// all today. Its value is that the fixed-vocabulary reason token identifies the missing
// choice in the dispatch-failure log line instead of leaving an unclassified error there.
//
// An account-id-less connection (the credentials-only bootstrap state — see
// MetaAdsConnectionConfig in design/connection.go) can create no campaign, and this refuses
// it HERE, before an empty AccountConfig.AccountID can reach the Meta client and fail
// opaquely with a malformed "//campaigns" request instead of a reason naming the fix.
// ToggleStatus and ReadMetrics do not call this — see resolveMetaCredentials for why.
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
	res, creds, err := d.resolveMetaCredentials(ctx, brief.ProjectID, platform, d.creds.resolve)
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
		InstagramUserID: cfg.InstagramUserID,
		DSABeneficiary:  cfg.DSABeneficiary,
		DSAPayor:        cfg.DSAPayor,
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
	res, creds, err := d.resolveMetaCredentials(ctx, projectID, platform, d.creds.resolveExisting)
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
	// Provenance BEFORE the provisioning guard below, deliberately. A campaign id is unique
	// only within an ad account, so a connection re-pointed since create would address an
	// unrelated campaign — and this path CHANGES delivery, so a collision pauses or activates
	// something this project does not own. "This row has no ad set" is only a meaningful
	// answer once the row is known to belong to the resolved account: on a re-pointed
	// connection the persisted ad set id describes a campaign in a DIFFERENT account, so
	// answering 409-not-provisioned there would explain the wrong campaign. Ordering the two
	// the other way makes a foreign-account ACTIVATE report a missing ad set instead of the
	// mismatch — the trap microsoft.go records at the same seam.
	if err := verifyMetaAccountMatch("toggle meta campaign status", campaign, res.accountID); err != nil {
		return err
	}
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
	res, creds, err := d.resolveMetaCredentials(ctx, projectID, platform, d.creds.resolveExisting)
	if err != nil {
		return nil, err
	}
	metaWindow, err := metaMetricsWindow(window)
	if err != nil {
		return nil, err
	}
	client := meta.NewClient(meta.Credentials{AccessToken: creds.AccessToken}, meta.AccountConfig{AccountID: strings.TrimSpace(res.accountID), Label: res.label}, d.opts...)
	// Prove the persisted campaign belongs to the account this read is scoped to.
	// resolveMetaCredentials returns the project's CURRENT connection, which can have been
	// re-pointed since create; GET /{campaignID}/insights under a different account yields
	// either a false "no data" or ANOTHER campaign's numbers presented as this campaign's
	// measurement — the failure-as-measurement class this path refuses throughout.
	if err := verifyMetaAccountMatch("read meta campaign metrics", campaign, res.accountID); err != nil {
		return nil, err
	}
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

// metaCreationAccountID reports the ad account the campaign was CREATED under, normalised to
// Meta's documented "act_<digits>" form, or "" when the persisted result blob does not record
// it.
//
// Prefers the explicit AccountID the create path now stamps, and falls back to the act= query
// parameter of the MetaURL the blob has always carried — the create path builds that as
// ".../adsmanager/manage/campaigns?act=" + the account id with its "act_" prefix STRIPPED, so
// the fallback re-adds the prefix to yield the same vocabulary the connection uses. Rows
// written BEFORE the explicit field existed therefore stay checkable rather than silently
// unguarded. Mirrors microsoftCreationAccountID and googleAdsCreationCustomerID.
//
// It unmarshals into the SAME meta.CampaignResult type the create path marshals into Result
// (campaignFromMeta), for the reason metaAdSetID records: one definition of the persisted wire
// shape, so reader and writer move together rather than desyncing.
//
// An EMPTY return means "unknown, proceed": absence must not become a new failure signal for
// pre-existing rows, so only a present-AND-different id is treated as a mismatch by callers.
func metaCreationAccountID(campaign *model.Campaign) string {
	if campaign == nil || len(campaign.Result) == 0 {
		return ""
	}
	var blob meta.CampaignResult
	if err := json.Unmarshal(campaign.Result, &blob); err != nil {
		return ""
	}
	if id := normalizeMetaAccountID(blob.AccountID); id != "" {
		return id
	}
	u, err := url.Parse(blob.MetaURL)
	if err != nil {
		return ""
	}
	return normalizeMetaAccountID(u.Query().Get("act"))
}

// normalizeMetaAccountID puts a Meta ad account id into the single "act_<digits>" vocabulary
// both sides of the provenance comparison must speak. The connection stores the prefixed form
// while MetaURL carries the bare digits, so comparing them raw would report every legacy row
// as a mismatch — a false 409 on a campaign that is perfectly in scope.
//
// Anything that is not a well-formed id normalises to "" — "unknown" — rather than to some
// non-empty token. That is what keeps a malformed value in the guard's "proceed" arm instead
// of letting it act as a REAL account: a bare "act_" (or a stray "act_abc") carries no account,
// but returning it non-empty would compare unequal to every legitimate connection and
// manufacture a false mismatch on a campaign nobody can re-point. Rejecting to "" costs
// nothing, because such a value could never have named an account in the first place.
//
// Meta's documented form is "act_<digits>" (design/connection.go constrains the stored
// connection id to ^act_[0-9]+$), so digits are the whole of the accepted shape and the
// prefix is stripped at most once — "act_act_777" names no account either.
func normalizeMetaAccountID(id string) string {
	digits := strings.TrimPrefix(strings.TrimSpace(id), "act_")
	if digits == "" {
		return ""
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return "act_" + digits
}

// verifyMetaAccountMatch refuses an operation on a campaign that was created under a DIFFERENT
// ad account than the project's current connection resolves to.
//
// Meta campaign ids are unique only WITHIN an ad account, and a project's connection can be
// re-pointed between create and a later read/toggle. Without this check the stored
// PlatformCampaignID is addressed against the NEW account, where it either matches nothing —
// rendered on the read path as a campaign with genuinely zero activity — or collides with an
// unrelated campaign, whose numbers become this campaign's measurement or whose delivery is
// changed by the toggle.
//
// Shared by ReadMetrics and ToggleStatus so the two cannot drift, and returns
// domain.ErrCampaignAccountMismatch exactly as the google-ads and microsoft adapters do.
//
// BOTH sides may be unknown, and neither unknown is a mismatch. An absent CREATED id is the
// pre-existing-row case every adapter documents. An empty CURRENT id is specific to Meta:
// unlike every sibling, toggle and metrics deliberately do NOT require an account selection —
// they address the campaign node by id (POST /{campaignID}, GET /{campaignID}/insights) and
// never read AccountConfig.AccountID, so a connection whose account was cleared via PUT can
// still pause a campaign and read its metrics (see resolveMetaCredentials, and the
// NoAccountIDNeeded tests that pin it). "Not selected" is an ABSENCE, not a different account:
// treating it as one would 409 exactly those paths for any row that records provenance — which,
// via the MetaURL act= fallback, is nearly every historical row — turning a working pause into
// a failure. It would also render the message as "resolves to account " with an empty name.
//
// Takes the account id as a plain string rather than the client the microsoft/reddit/twitter
// siblings accept: those build their client inside a resolve* helper and the caller never holds
// the raw id, so client.AccountID() is their only accessible source. Here — and on linkedin —
// the call site already has res.accountID in hand. normalizeMetaAccountID is applied inside, so
// callers pass the connection value untouched and one place owns the vocabulary.
func verifyMetaAccountMatch(op string, campaign *model.Campaign, accountID string) error {
	created := metaCreationAccountID(campaign)
	current := normalizeMetaAccountID(accountID)
	if created == "" || current == "" || created == current {
		return nil
	}
	return fmt.Errorf("%s: campaign %s was created under meta ad account %s but the project's current connection resolves to account %s: %w",
		op, campaign.PlatformCampaignID, created, current, domain.ErrCampaignAccountMismatch)
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
	// lifetime-vs-daily budget flag). ConfigSnapshot captures the validated config,
	// but with every variant's ImageURL SANITIZED: a creative image URL is
	// caller-supplied and may be PRE-SIGNED, whose signature is a bearer credential
	// granting time-boxed read access, and config_snapshot is stored UNENCRYPTED in
	// Postgres. This is the SUCCESS path — it runs on every create with an image, not
	// only on failures — so scrubbing the error sinks alone never covered it. Same
	// reason and same helper as campaignFromReddit's PostURL.
	snapshot := cfg
	if len(cfg.Variants) > 0 {
		// Copy the slice before mutating: cfg is passed by value but Variants shares its
		// backing array with the caller's config, and the FULL url must still reach Meta.
		snapshot.Variants = make([]meta.AdVariant, len(cfg.Variants))
		copy(snapshot.Variants, cfg.Variants)
		for i := range snapshot.Variants {
			snapshot.Variants[i].ImageURL = sanitizeSnapshotURL(snapshot.Variants[i].ImageURL)
		}
	}
	applyCampaignConfig(ctx, c, cfg.Budget, cfg.LifetimeBudget, cfg.StartDate, cfg.EndDate, snapshot)
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
// The stored-state checks are resolveMetaCredentials' — active status, decodable blob,
// non-empty access token, each tagged with domain.ErrConnectionNotUsable plus its reason
// sentinel and passed through systemScoped. This function deliberately does not repeat
// them. It did once, and that duplication was the defect: the two copies classified the
// same three conditions, so a later change to either — a fourth check, a different sentinel,
// a message that stops dropping the decode cause — would have silently applied to only one
// of "can this connection dispatch?" and "can this connection be asked what it reaches?".
// One source, one error contract, and the discovery endpoint's 400-vs-503 mapping stays
// pinned to the same sentinels the dispatch path answers with.
//
// What IS specific to discovery is what is not required: no account id. That omission is the
// point of the endpoint — it exists to answer "which ad account should this connection
// use?", so demanding one would make it reachable only by connections that no longer need
// it. resolveMetaCredentials never consults the account id (see its godoc for why the
// metrics and toggle paths do not either); Dispatch adds that requirement separately with
// requireMetaAccountID, and this path simply does not call it. That is exactly what makes
// credentials-only bootstrap work: a connection created with an access token and a page id
// but no account_id is usable HERE, which is how its owner discovers the id to PUT.
//
// AccountConfig is left ZERO for the same reason. GET /me/adaccounts is account-agnostic:
// it asks what the TOKEN reaches, so scoping the client to one of the answers would narrow
// the response to a subset of the question.
func (d *MetaDispatcher) resolveMetaDiscoveryClient(ctx context.Context, projectID string, platform model.Provider) (*meta.Client, error) {
	_, creds, err := d.resolveMetaCredentials(ctx, projectID, platform, d.creds.resolve)
	if err != nil {
		return nil, err
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
