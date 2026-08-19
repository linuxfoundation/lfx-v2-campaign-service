// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
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
	EventName string `json:"eventName"`
	// Name is the SAME value under the spelling the UI actually writes. `event_details` is
	// typed `Any` in the design (design/brief.go:37), so nothing arbitrates the key, and the
	// UI's own `CampaignEventDetails` interface has spelled it `name` all along — the persist
	// path spreads that object verbatim. Every paid create therefore decoded EventName as ""
	// and was refused before reaching the ad platform. Accepting both spellings fixes the
	// briefs ALREADY STORED, which a writer-side change could not.
	//
	// `lenientEventName` (hubspot.go) is lenient about PRESENCE — it returns "" rather than
	// erroring, because an email must still stage without a name — but it was not lenient
	// about SPELLING, so it too missed the UI's `name` and labelled every cloned email from
	// the fallback. Both now read both keys.
	Name            string `json:"name"`
	RegistrationURL string `json:"registrationUrl"`
	HSToken         string `json:"hsToken"`
}

// RedditDispatcher creates Reddit campaigns for the orchestrator. It resolves the
// project's Reddit connection, builds a reddit.Client from the decrypted creds, maps
// the brief + config onto reddit.CampaignInput, and returns the created campaign.
type RedditDispatcher struct {
	creds *credsSource
	// clients caches the built *reddit.Client per connection, exactly as GoogleAdsDispatcher
	// does. The credential cache alone does not remove the OAuth exchange: the access token is
	// cached ON the client instance (reddit.Client's cachedToken/tokenExpireAt), so a client
	// rebuilt per call re-mints it however cheap the credential lookup became. Entries are
	// validated against the same row id and version as the credential, so a rotated credential
	// cannot be served through a stale client.
	//
	// Sharing one instance across concurrent callers is safe for this client specifically: the
	// only fields written after construction are the token cache and the in-flight refresh
	// handle, both exclusively under c.mu (client.go refreshToken/fetchToken), and no method
	// stores per-call state on the receiver — the account config is written once at construction
	// and thereafter read-only.
	clients *clientCache
	// opts are extra reddit.Client options (e.g. WithBaseURL/WithTokenURL in tests).
	opts []reddit.Option
}

// NewRedditDispatcher builds the adapter from the connection repo + encryptor.
func NewRedditDispatcher(repo connReader, enc domain.Encryptor, opts ...reddit.Option) *RedditDispatcher {
	return &RedditDispatcher{creds: newCredsSource(repo, enc), clients: newClientCache(), opts: opts}
}

// Dispatch implements service.PlatformDispatcher for Reddit.
func (d *RedditDispatcher) Dispatch(ctx context.Context, brief *model.CampaignBrief, platform model.Provider, config json.RawMessage) (*model.Campaign, error) {
	// Resolve creds + build the client FIRST (pre-create): a missing/undecryptable/
	// inactive connection is a not-created error → the orchestrator releases the claim.
	// resolveRedditClient is shared with ToggleStatus so both accept EXACTLY the same
	// connections; here its (bare) error is wrapped as notCreated for the claim contract.
	// The credsSource.resolve error is already a preCreateError, so it is passed through
	// untouched; the post-resolve validation errors are wrapped.
	client, err := d.resolveRedditClient(ctx, brief.ProjectID, platform)
	if err != nil {
		// resolve() already returns a preCreateError (NoUpstreamCreate); the post-resolve
		// validation errors are bare and must be wrapped so the orchestrator still releases
		// the claim (nothing was created either way).
		var nuc interface{ NoUpstreamCreate() bool }
		if errors.As(err, &nuc) && nuc.NoUpstreamCreate() {
			return nil, err
		}
		return nil, notCreated(err)
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

// resolveRedditClient runs the shared pre-flight both Dispatch and ToggleStatus need:
// resolve the connection, require it ACTIVE, decode + completeness-check the credentials,
// require an account id, and build the reddit client. Centralising it keeps the credential
// shape / active-status rule in ONE place so a create and a toggle can never diverge on
// which connections they accept (the block was previously duplicated).
//
// Every defect below is tagged for its AUDIENCE here, at the point of detection, following
// validateGoogleAdsCredentials. Untagged, all four fell to each handler's default arm and
// answered 503 — "the platform did not respond" about a platform that was never contacted,
// with a remedy (retry) that no amount of waiting can satisfy, since only a human editing
// the connection can fix it. The 409 arm at internal/service/brief.go is the correct answer.
// The named return plus defer means a return site added later cannot forget to re-attribute
// the error to the LF system row; systemScoped is a no-op for project-owned rows and
// idempotent, so a caller that also tags costs nothing.
func (d *RedditDispatcher) resolveRedditClient(ctx context.Context, projectID string, platform model.Provider) (c *reddit.Client, err error) {
	res, err := d.creds.resolve(ctx, projectID, platform)
	if err != nil {
		return nil, err
	}
	defer func() { err = res.systemScoped(err) }()
	if res.status != model.StatusActive {
		return nil, fmt.Errorf("%w: %w: reddit connection for project %s is %s, not active",
			domain.ErrConnectionNotUsable, domain.ErrConnectionInactive, projectID, res.status)
	}
	var creds redditCreds
	if err := json.Unmarshal(res.plaintext, &creds); err != nil {
		// The unmarshal error is DROPPED, not wrapped: it is derived from the DECRYPTED
		// credential blob and encoding/json quotes its input. Full rationale on
		// validateGoogleAdsCredentials, which this follows.
		return nil, fmt.Errorf("%w: %w: reddit credentials for project %s are not valid JSON",
			domain.ErrConnectionNotUsable, domain.ErrCredentialsUndecodable, projectID)
	}
	if creds.ClientID == "" || creds.ClientSecret == "" || creds.RefreshToken == "" {
		return nil, fmt.Errorf("%w: %w: reddit credentials are incomplete (need clientId, clientSecret, refreshToken)",
			domain.ErrConnectionNotUsable, domain.ErrCredentialsIncomplete)
	}
	if strings.TrimSpace(res.accountID) == "" {
		// BOTH sentinels: ErrConnectionNotUsable decides the HTTP status, ErrAccountNotSelected
		// names the reason for the log line's fixed vocabulary (unusableConnectionReason).
		return nil, fmt.Errorf("%w: %w: reddit connection for project %s has no account id",
			domain.ErrConnectionNotUsable, domain.ErrAccountNotSelected, projectID)
	}
	// Build through the client cache, so a dispatch burst or a polling dashboard reuses ONE
	// client — and therefore ONE OAuth token — instead of re-minting per call. Every check
	// above still runs on every call against the FRESH row: only the construction is cached,
	// so a connection that has gone inactive or lost its account id is refused here exactly as
	// before rather than being served a live client from the cache.
	key, connID, version := res.cacheIdentity(projectID, platform)
	built, err := d.clients.buildOnce(key, connID, version, func() (any, error) {
		return reddit.NewClient(
			reddit.Credentials{ClientID: creds.ClientID, ClientSecret: creds.ClientSecret, RefreshToken: creds.RefreshToken},
			// The pixel travels with the ACCOUNT, matching where it is stored. An absent key
			// yields "", which CreateCampaign refuses with a message naming the connection --
			// the empty case is a connection saved before the column existed, and guessing a
			// value would attribute conversions to a pixel that is not this advertiser's.
			reddit.AccountConfig{
				AccountID:         res.accountID,
				Label:             res.label,
				ConversionPixelID: res.providerConfig["conversion_pixel_id"],
			},
			d.opts...,
		), nil
	})
	if err != nil {
		return nil, err
	}
	client, isClient := built.(*reddit.Client)
	if !isClient {
		// Unreachable: this cache is written only by the closure above. Rebuild rather than
		// assert, so a future second writer cannot turn a type confusion into a panic.
		return reddit.NewClient(
			reddit.Credentials{ClientID: creds.ClientID, ClientSecret: creds.ClientSecret, RefreshToken: creds.RefreshToken},
			reddit.AccountConfig{
				AccountID:         res.accountID,
				Label:             res.label,
				ConversionPixelID: res.providerConfig["conversion_pixel_id"],
			},
			d.opts...,
		), nil
	}
	return client, nil
}

// ToggleStatus pauses or resumes an existing reddit campaign on the platform. It resolves
// the connection (same pre-check as Dispatch: an inactive/undecryptable connection is a
// clean error), builds the client, and PATCHes configured_status on the campaign AND its
// child ad group + ad. status is model.CampaignRunActive or model.CampaignRunPaused;
// returns nil only when the platform confirms every change.
//
// The cascade matters because CreateCampaign sets configured_status to PAUSED on all THREE
// entities (campaign, ad group, ad). Toggling only the campaign to ACTIVE would leave the
// ad group/ad PAUSED, so the campaign would not actually serve. The child ids are read from
// the persisted CampaignResult blob (Result), which the create path stored; if they are
// absent (a degraded/partial create) only the campaign is toggled — such campaigns are
// already rejected as non-toggleable by the service guard, so this is just defensive.
func (d *RedditDispatcher) ToggleStatus(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, status string) error {
	redditStatus, err := redditRunStatus(status)
	if err != nil {
		return err
	}
	client, err := d.resolveRedditClient(ctx, projectID, platform)
	if err != nil {
		return err
	}
	// Provenance BEFORE the provisioning guard below, deliberately. A campaign id is unique
	// only within an ad account, so a connection re-pointed since create would address an
	// unrelated campaign — and this path CHANGES delivery, so a collision pauses or activates
	// something this project does not own. "This row has no fully-created ad group + ad" is
	// only a meaningful answer once the row is known to belong to the resolved account: on a
	// re-pointed connection the persisted child ids describe a campaign in a DIFFERENT
	// account, so answering 409-not-provisioned there would explain the wrong campaign.
	// Ordering the two the other way makes a foreign-account ACTIVATE report missing children
	// instead of the mismatch — the trap microsoft.go records at the same seam.
	if err := verifyRedditAccountMatch("toggle reddit campaign status", campaign, client); err != nil {
		return err
	}
	adGroupID, adID := redditChildIDs(campaign)
	// ACTIVATE requires the FULL servable tree. A reddit create can legitimately land a
	// campaign + ad group but NO ad (the no-PostURL path returns AdCount 0 / empty AdID) and
	// the dispatcher still persists that row as "created" — which passes the service's
	// toggleability guard. Activating it would PATCH the campaign + ad group, silently skip
	// the absent ad, and report "active" even though the campaign cannot serve (no ad). Refuse
	// unless BOTH child ids are known, and return ErrCampaignNotProvisioned so the service
	// classifies it as a 409 state error (the platform is never called), not a 503. Pausing
	// needs no child ids — pausing the parent already stops delivery.
	if redditStatus == reddit.StatusActive && (strings.TrimSpace(adGroupID) == "" || strings.TrimSpace(adID) == "") {
		return fmt.Errorf("%w: reddit campaign %s cannot be activated because it has no fully-created ad group + ad to serve", domain.ErrCampaignNotProvisioned, campaign.PlatformCampaignID)
	}
	if uerr := client.UpdateCampaignAndChildrenStatus(ctx, campaign.PlatformCampaignID, adGroupID, adID, redditStatus); uerr != nil {
		// An UNCONFIRMED outcome (transport/5xx/3xx-mutating) means the PATCH MAY have
		// applied upstream — wrap it in an error that reports Unconfirmed() so the caller
		// (across the package boundary, via errors.As on the behavioral interface — same
		// pattern as NoUpstreamCreate) reports "verify before retry", not a flat "not
		// applied". A definite rejection passes through as an ordinary error.
		if reddit.IsOutcomeUnconfirmed(uerr) {
			return &unconfirmedToggleError{err: uerr}
		}
		return uerr
	}
	return nil
}

// ReadMetrics returns live campaign metrics from Reddit's Ads v3 reporting endpoint for
// the given campaign during the specified time window.
//
// The request/response shape this calls is now taken from Reddit's official public
// OpenAPI spec rather than guessed — see the contract note on
// reddit.Client.GetCampaignMetrics (LFXV2-3282). That supersedes the LFXV2-2995 finding
// that no public documentation existed, which was the original reason for this gate.
//
// The gate nevertheless STAYS ON, because the remaining unknown is a different one: no
// request has ever been made against a live Reddit ad account. Matching a published
// schema does not establish what the endpoint actually returns for a campaign with no
// activity, whether ends_at is inclusive of its final hour, or whether the account's
// attribution window shifts the figures. Merely declaring this method is the capability
// switch — Orchestrator.ReadCampaignMetrics discovers MetricsReader by type assertion,
// and the published endpoint then calls it — so without the flag an unexercised read
// would ship as production metrics that return 200 and look authoritative. Nothing in the
// response carries the caveats. The gate is checked here rather than at construction so a
// deployment can flip it without a rebuild, and so the disabled path costs nothing but an
// env read.
//
// Disabled reads answer domain.ErrMetricsUnsupported, which the service maps to the same 400
// a platform with no metrics support at all returns — the accurate answer while no read has
// been exercised. Delete the gate once the shape is confirmed against a live ad account:
// that is now the ONLY thing standing between this and general availability.
func (d *RedditDispatcher) ReadMetrics(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, window model.MetricsWindow) (*model.CampaignMetrics, error) {
	if os.Getenv(constants.EnvRedditMetricsEnabled) != "true" {
		return nil, fmt.Errorf("reddit metrics reads are disabled (%s is not \"true\") until the reporting contract is exercised against a live ad account: %w",
			constants.EnvRedditMetricsEnabled, domain.ErrMetricsUnsupported)
	}
	if campaign.PlatformCampaignID == "" {
		return nil, fmt.Errorf("campaign has no platform campaign ID")
	}
	// Validated BEFORE credential resolution, and this order is load-bearing — the same
	// order linkedin.go and twitter.go use, and the order reddit.ValidateMetricsWindow was
	// made package-level and clock-free to allow. An unsupported window is a permanent 400
	// whatever the connection looks like, and it names the one thing the caller can actually
	// change. Resolving first answers a connection error instead, sending the caller to
	// repair a connection when the request would still be rejected on the window.
	if werr := reddit.ValidateMetricsWindow(window); werr != nil {
		return nil, fmt.Errorf("get campaign metrics from reddit: %w", errors.Join(domain.ErrMetricsWindowUnsupported, werr))
	}
	client, err := d.resolveRedditClient(ctx, projectID, platform)
	if err != nil {
		return nil, err
	}
	// Prove the persisted campaign belongs to the account this read is scoped to.
	// resolveRedditClient returns the project's CURRENT connection, which can have been
	// re-pointed since create; reading the stored campaign id under a different account
	// yields either a false "no data" or ANOTHER campaign's numbers presented as this
	// campaign's measurement.
	if err := verifyRedditAccountMatch("read reddit campaign metrics", campaign, client); err != nil {
		return nil, err
	}
	metrics, err := client.GetCampaignMetrics(ctx, campaign.PlatformCampaignID, window)
	if err != nil {
		if errors.Is(err, reddit.ErrUnsupportedWindow) {
			return nil, fmt.Errorf("get campaign metrics from reddit: %w: %w", domain.ErrMetricsWindowUnsupported, err)
		}
		return nil, fmt.Errorf("get campaign metrics from reddit: %w", err)
	}
	return metrics, nil
}

// redditCreationAccountID reports the ad account the campaign was CREATED under, or "" when
// the persisted result blob does not record it.
//
// Unlike the google-ads, microsoft, linkedin and meta siblings there is NO recoverable
// fallback: reddit.CampaignResult's RedditURL is the bare ads-manager constant
// ("https://ads.reddit.com") and has never carried an account id, so a row written before the
// explicit accountId field existed records no provenance anywhere and cannot be checked. Such
// a row is waved through as "unknown"; only a re-dispatch can give it a provenance.
//
// An EMPTY return means "unknown, proceed": absence must not become a new failure signal for
// pre-existing rows, so only a present-AND-different id is treated as a mismatch by callers.
func redditCreationAccountID(campaign *model.Campaign) string {
	if campaign == nil || len(campaign.Result) == 0 {
		return ""
	}
	var blob struct {
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(campaign.Result, &blob); err != nil {
		return ""
	}
	return strings.TrimSpace(blob.AccountID)
}

// verifyRedditAccountMatch refuses an operation on a campaign that was created under a
// DIFFERENT ad account than the project's current connection resolves to.
//
// Reddit campaign ids are unique only WITHIN an ad account, and a project's connection can be
// re-pointed between create and a later read/toggle. Without this check the stored
// PlatformCampaignID is addressed against the NEW account, where it either matches nothing —
// rendered on the read path as a campaign with genuinely zero activity — or collides with an
// unrelated campaign, whose numbers become this campaign's measurement or whose delivery is
// changed by the toggle.
//
// Shared by ReadMetrics and ToggleStatus so the two cannot drift, and returns
// domain.ErrCampaignAccountMismatch exactly as the google-ads and microsoft adapters do.
func verifyRedditAccountMatch(op string, campaign *model.Campaign, client *reddit.Client) error {
	created := redditCreationAccountID(campaign)
	current := strings.TrimSpace(client.AccountID())
	// Neither unknown is a mismatch. An absent CREATED id is the pre-existing-row case. An
	// empty CURRENT id cannot prove anything either — "not selected" is an absence, not a
	// different account, and reporting one would render as "resolves to account " with an
	// empty name. resolveRedditClient already refuses an account-less connection with
	// ErrAccountNotSelected, so this arm is unreachable today; it is stated rather than relied
	// upon so the guard stays correct if that precondition is ever relaxed, as it is on meta.
	if created == "" || current == "" || created == current {
		return nil
	}
	return fmt.Errorf("%s: campaign %s was created under reddit ad account %s but the project's current connection resolves to account %s: %w",
		op, campaign.PlatformCampaignID, created, current, domain.ErrCampaignAccountMismatch)
}

// redditChildIDs pulls the ad group + ad ids the create path stored in the persisted
// CampaignResult blob. A missing/unparseable blob yields empty ids (only the campaign is
// toggled) rather than an error — the service already blocks toggling a degraded campaign.
func redditChildIDs(campaign *model.Campaign) (adGroupID, adID string) {
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

// unconfirmedToggleError wraps a toggle whose platform outcome is unknowable (the change may
// have been applied). Callers detect it via the Unconfirmed() behavioral interface with
// errors.As — no shared sentinel needed across the dispatch/service package boundary (mirrors
// preCreateError / NoUpstreamCreate).
type unconfirmedToggleError struct{ err error }

func (e *unconfirmedToggleError) Error() string {
	return "status change outcome is unconfirmed (it may have been applied): " + e.err.Error()
}
func (e *unconfirmedToggleError) Unwrap() error     { return e.err }
func (e *unconfirmedToggleError) Unconfirmed() bool { return true }

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
		// TRIMMED on assignment, matching decodeEventDetails (audience_build.go). Storing the
		// raw value and trimming only at the emptiness gate below left the two decoders
		// disagreeing on the same blob: `{"eventName":"  Foo  "}` yielded "  Foo  " here and
		// "Foo" there, and this value becomes the upstream campaign NAME.
		if bf.EventName == "" {
			bf.EventName = strings.TrimSpace(partial.EventName)
		}
		// `eventName` wins where both are present; `name` is the fallback rather than an
		// equal, so a blob that carries the explicit spelling is never overridden by the
		// generic one.
		//
		// Emptiness is SEMANTIC (TrimSpace), matching the final validation below and the
		// sibling decoders. A plain `== ""` let `{"eventName":" ","name":"Valid UI name"}`
		// skip the fallback and then fail that validation — a usable name discarded because
		// the other key held a space.
		if bf.EventName == "" {
			bf.EventName = strings.TrimSpace(partial.Name)
		}
		// `eventName` wins where both are present; `name` is the fallback rather than an
		// equal, so a blob that carries the explicit spelling is never overridden by the
		// generic one.
		//
		// Emptiness is SEMANTIC (TrimSpace), matching the final validation below and the
		// sibling decoders. A plain `== ""` let `{"eventName":" ","name":"Valid UI name"}`
		// skip the fallback and then fail that validation — a usable name discarded because
		// the other key held a space.
		if strings.TrimSpace(bf.EventName) == "" {
			bf.EventName = partial.Name
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
