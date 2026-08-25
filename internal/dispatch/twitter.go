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
	// clients caches the built *twitter.Client per connection, as GoogleAdsDispatcher,
	// RedditDispatcher and MicrosoftDispatcher do. Entries are validated against the same
	// row id and version as the credential, so a rotated credential cannot be served
	// through a stale client.
	//
	// The benefit here is NOT a saved token exchange: X signs every request with stored
	// OAuth 1.0a credentials and mints nothing at construction, so unlike Google Ads and
	// Reddit there is no token round-trip to collapse. Sharing is wired because the client
	// now owns the WRITE PACER, and that only bounds the rate if callers share it: X
	// enforces 1 write/sec per ACCOUNT, so two clients for one account each pace themselves
	// and together issue ~2 writes/sec. Reuse makes the budget enforceable rather than
	// merely cheaper.
	//
	// Sharing one instance across concurrent callers is safe for this client specifically:
	// every field is written once at construction, and the only post-construction state is
	// the pacer's nextWrite, written under the client's own writeMu (see twitter.Client's
	// pace). No method stores per-call state on the receiver.
	clients *clientCache
	opts    []twitter.Option
}

// NewTwitterDispatcher builds the adapter from the connection repo + encryptor.
func NewTwitterDispatcher(repo connReader, enc domain.Encryptor, opts ...twitter.Option) *TwitterDispatcher {
	return &TwitterDispatcher{creds: newCredsSource(repo, enc), clients: newClientCache(), opts: opts}
}

// cachedTwitterClient returns the client for this connection, building it only when there is
// no live entry for the connection's current (row id, version).
//
// Every validation the callers perform still runs on every call against the FRESH row; only
// the construction is cached, so a connection that has gone inactive or lost its account id
// is refused by the caller exactly as before rather than being served a live client.
func (d *TwitterDispatcher) cachedTwitterClient(projectID string, platform model.Provider, res *resolved, creds twitterCreds, accountID, fundingID string) *twitter.Client {
	// ONE construction, called from both the cache closure and the fallback below, matching
	// cachedRedditClient's and cachedMicrosoftClient's shape. Written twice, the two copies
	// can drift: a later AccountConfig change has to be made in both, and the compiler
	// cannot notice if it is not.
	build := func() *twitter.Client {
		return twitter.NewClient(
			twitter.Credentials{
				ConsumerKey:       creds.ConsumerKey,
				ConsumerSecret:    creds.ConsumerSecret,
				AccessToken:       creds.AccessToken,
				AccessTokenSecret: creds.AccessTokenSecret,
			},
			twitter.AccountConfig{AccountID: accountID, FundingInstrumentID: fundingID},
			d.opts...,
		)
	}
	key, connID, version := res.cacheIdentity(projectID, platform)
	built, err := d.clients.buildOnce(key, connID, version, func() (any, error) {
		return build(), nil
	})
	if err != nil {
		// buildOnce only surfaces an error the build closure returned, and this one returns
		// none. Rebuild rather than propagate, so the dispatch path cannot fail on a cache
		// bookkeeping error.
		return build()
	}
	client, isClient := built.(*twitter.Client)
	if !isClient {
		// Unreachable: this cache is written only by the closure above. Rebuild rather than
		// assert, so a future second writer cannot turn a type confusion into a panic.
		return build()
	}
	return client
}

// Dispatch implements service.PlatformDispatcher for X (Twitter).
func (d *TwitterDispatcher) Dispatch(ctx context.Context, brief *model.CampaignBrief, platform model.Provider, config json.RawMessage) (camp *model.Campaign, err error) {
	// The resolve step's error is already a preCreateError, so it passes through verbatim.
	res, err := d.creds.resolve(ctx, brief.ProjectID, platform)
	if err != nil {
		return nil, err // preCreateError
	}
	// Record WHICH ACCOUNT served this campaign on every exit that returns a row —
	// including the UNCONFIRMED/degraded paths that return a campaign alongside an error.
	// See stampProvenance for why this is a defer on the named return, not a per-return call.
	defer func() { res.stampProvenance(camp) }()
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

	// Built through the client cache, so concurrent dispatches for this account share ONE
	// client and therefore ONE write pacer — which is what keeps the aggregate write rate
	// inside X's 1/sec budget. See cachedTwitterClient.
	client := d.cachedTwitterClient(brief.ProjectID, platform, res, creds, accountID, fundingID)

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
	camp = campaignFromTwitter(ctx, result, cfg)
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
//
// Every defect below is tagged for its AUDIENCE here, at the point of detection, following
// validateGoogleAdsCredentials. Untagged, all four fell to each handler's default arm and
// answered 503 — "the platform did not respond" about a platform that was never contacted,
// with a remedy (retry) that no amount of waiting can satisfy, since only a human editing
// the connection can fix it. The 409 arm at internal/service/brief.go is the correct answer.
// Tagging HERE rather than in each caller is what keeps Dispatch and ToggleStatus from
// having to agree about it; the named return plus defer means a return site added later
// cannot forget to re-attribute the error to the LF system row.
func validateTwitterConnection(projectID string, res *resolved) (creds twitterCreds, accountID string, err error) {
	defer func() { err = res.systemScoped(err) }()
	if res.status != model.StatusActive {
		return creds, "", fmt.Errorf("%w: %w: twitter connection for project %s is %s, not active",
			domain.ErrConnectionNotUsable, domain.ErrConnectionInactive, projectID, res.status)
	}
	if err := json.Unmarshal(res.plaintext, &creds); err != nil {
		// The unmarshal error is DROPPED, not wrapped: it is derived from the DECRYPTED
		// credential blob and encoding/json quotes its input. Full rationale on
		// validateGoogleAdsCredentials, which this follows.
		return creds, "", fmt.Errorf("%w: %w: twitter credentials for project %s are not valid JSON",
			domain.ErrConnectionNotUsable, domain.ErrCredentialsUndecodable, projectID)
	}
	if creds.ConsumerKey == "" || creds.ConsumerSecret == "" || creds.AccessToken == "" || creds.AccessTokenSecret == "" {
		return creds, "", fmt.Errorf("%w: %w: twitter credentials are incomplete (need consumerKey, consumerSecret, accessToken, accessTokenSecret)",
			domain.ErrConnectionNotUsable, domain.ErrCredentialsIncomplete)
	}
	accountID = strings.TrimSpace(res.accountID)
	if accountID == "" {
		// BOTH sentinels: ErrConnectionNotUsable decides the HTTP status, ErrAccountNotSelected
		// names the reason for the log line's fixed vocabulary (unusableConnectionReason).
		return creds, "", fmt.Errorf("%w: %w: twitter connection for project %s has no account id",
			domain.ErrConnectionNotUsable, domain.ErrAccountNotSelected, projectID)
	}
	return creds, accountID, nil
}

// resolveTwitterClient resolves + validates the project's connection and builds an X Ads
// client for the TOGGLE and METRICS paths (see validateTwitterConnection for the shared rules).
//
// Both callers operate on an ALREADY-CREATED campaign, so it resolves via resolveExisting,
// which follows the account the campaign was CREATED under (twitterCreationAccountID) — the
// project's, or the LF system one when the forced-system flag governed its creation.
// verifyTwitterAccountMatch would refuse a campaign addressed under any other account.
func (d *TwitterDispatcher) resolveTwitterClient(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign) (*twitter.Client, error) {
	res, err := d.creds.resolveExisting(ctx, projectID, platform, twitterCreationAccountID(campaign))
	if err != nil {
		return nil, err
	}
	creds, accountID, err := validateTwitterConnection(projectID, res)
	if err != nil {
		return nil, err
	}
	// FundingInstrumentID is populated for consistency with the create path, but is INERT
	// on the toggle path: UpdateCampaignAndChildrenStatus only PUTs entity_status on
	// entities that already exist, so the field never reaches the wire here. It is
	// deliberately NOT required by validateTwitterConnection for the same reason —
	// demanding a create-time field would refuse an otherwise-valid pause.
	//
	// Shares the create path's cache entry: same (project, provider, row id, version) key and
	// the same AccountConfig, so a toggle and a dispatch for one connection reuse ONE client
	// and one write pacer rather than pacing independently against the same account budget.
	return d.cachedTwitterClient(projectID, platform, res, creds, accountID,
		strings.TrimSpace(res.providerConfig["funding_instrument_id"])), nil
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

// twitterCreationAccountID reports the ad account the campaign was CREATED under, or "" when
// the persisted result blob does not record it.
//
// The blob is campaignFromTwitter's json.Marshal of an UNTAGGED twitter.CampaignResult, so the
// key is the Go field name "AccountID" — the same convention twitterChildIDs follows for
// "LineItemID", and pinned by the same round-trip test.
//
// Unlike the google-ads, microsoft, linkedin and meta siblings there is NO recoverable
// fallback: twitter.CampaignResult's TwitterURL is the bare ads-manager constant
// ("https://ads.x.com") and has never carried an account id, so a row written before the
// explicit AccountID field existed records no provenance anywhere and cannot be checked. Such
// a row is waved through as "unknown"; only a re-dispatch can give it a provenance.
//
// An EMPTY return means "unknown, proceed": absence must not become a new failure signal for
// pre-existing rows, so only a present-AND-different id is treated as a mismatch by callers.
func twitterCreationAccountID(campaign *model.Campaign) string {
	if campaign == nil || len(campaign.Result) == 0 {
		return ""
	}
	var blob struct {
		// PascalCase deliberately: the blob is an UNTAGGED marshal, so the persisted key is
		// the Go field name. See the docblock above before "correcting" this to accountId.
		AccountID string `json:"AccountID"`
	}
	if err := json.Unmarshal(campaign.Result, &blob); err != nil {
		return ""
	}
	return strings.TrimSpace(blob.AccountID)
}

// verifyTwitterAccountMatch refuses an operation on a campaign that was created under a
// DIFFERENT ad account than the project's current connection resolves to.
//
// X Ads campaign ids are unique only WITHIN an ad account — every endpoint this client calls is
// account-scoped, whether by the nested /accounts/{account_id}/... form the mutating calls use
// or the /stats/accounts/{account_id} form the metrics read uses — and a connection can be
// re-pointed
// between create and a later read/toggle. Without this check the stored PlatformCampaignID is
// addressed against the NEW account, where it either matches nothing — rendered on the read
// path as a campaign with genuinely zero activity — or collides with an unrelated campaign,
// whose numbers become this campaign's measurement or whose delivery is changed by the toggle.
//
// Shared by ReadMetrics and ToggleStatus so the two cannot drift, and returns
// domain.ErrCampaignAccountMismatch exactly as the google-ads and microsoft adapters do.
func verifyTwitterAccountMatch(op string, campaign *model.Campaign, client *twitter.Client) error {
	created := twitterCreationAccountID(campaign)
	current := strings.TrimSpace(client.AccountID())
	// Neither unknown is a mismatch. An absent CREATED id is the pre-existing-row case. An
	// empty CURRENT id cannot prove anything either — "not selected" is an absence, not a
	// different account, and reporting one would render as "resolves to account " with an
	// empty name. validateTwitterConnection already refuses an account-less connection with
	// ErrAccountNotSelected, so this arm is unreachable today; it is stated rather than relied
	// upon so the guard stays correct if that precondition is ever relaxed, as it is on meta.
	if created == "" || current == "" || created == current {
		return nil
	}
	return fmt.Errorf("%s: campaign %s was created under x ads account %s but the project's current connection resolves to account %s: %w",
		op, campaign.PlatformCampaignID, created, current, domain.ErrCampaignAccountMismatch)
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
	client, err := d.resolveTwitterClient(ctx, projectID, platform, campaign)
	if err != nil {
		return err
	}
	// Provenance BEFORE the provisioning guard below, deliberately. A campaign id is unique
	// only within an ad account, so a connection re-pointed since create would address an
	// unrelated campaign — and this path CHANGES delivery, so a collision pauses or activates
	// something this project does not own. "This row's line item is not known" is only a
	// meaningful answer once the row is known to belong to the resolved account: on a
	// re-pointed connection the persisted line-item id describes a campaign in a DIFFERENT
	// account, so answering 409-not-provisioned there would explain the wrong campaign.
	// Ordering the two the other way makes a foreign-account ACTIVATE report a missing line
	// item instead of the mismatch — the trap microsoft.go records at the same seam.
	if err := verifyTwitterAccountMatch("toggle x ads campaign status", campaign, client); err != nil {
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

// twitterMetricsWindow maps the platform-agnostic model.MetricsWindow vocabulary to X Ads'
// own MetricsWindow literals. X Ads supports YESTERDAY, TODAY, and LAST_7_DAYS (the stats
// endpoint caps queryable date ranges at 7 days per request); every other foundation window
// (LAST_14_DAYS, LAST_30_DAYS, THIS_MONTH, LAST_MONTH) is not representable and returns
// twitter.ErrUnsupportedWindow rather than being silently approximated (no averaging,
// truncation, or extrapolation).
func twitterMetricsWindow(w model.MetricsWindow) (twitter.MetricsWindow, error) {
	switch w {
	case model.MetricsWindowYesterday:
		return twitter.WindowYesterday, nil
	case model.MetricsWindowToday:
		return twitter.WindowToday, nil
	case model.MetricsWindowLast7Days:
		return twitter.WindowLast7Days, nil
	default:
		return "", fmt.Errorf("%w: %w: %q (X Ads only supports yesterday, today, and last_7_days)", domain.ErrMetricsWindowUnsupported, twitter.ErrUnsupportedWindow, w)
	}
}

// ReadMetrics implements service.MetricsReader for X (Twitter) Ads. It resolves
// the same connection ToggleStatus does and reads the campaign's live metrics,
// mapping the foundation's platform-agnostic window to X Ads' own vocabulary via
// twitterMetricsWindow.
//
// Note: X Ads API caps queryable date ranges at 7 days per request. Windows longer
// than 7 days are NOT supported — no averaging, no truncation, no extrapolation.
func (d *TwitterDispatcher) ReadMetrics(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, window model.MetricsWindow) (*model.CampaignMetrics, error) {
	xWindow, err := twitterMetricsWindow(window)
	if err != nil {
		return nil, err
	}
	client, err := d.resolveTwitterClient(ctx, projectID, platform, campaign)
	if err != nil {
		return nil, err
	}
	// Prove the persisted campaign belongs to the account this read is scoped to.
	// resolveTwitterClient returns the project's CURRENT connection, which can have been
	// re-pointed since create; the stats endpoint is account-scoped as
	// /stats/accounts/{account_id} (a trailing segment, not the nested form the mutating calls
	// use), so reading the stored campaign id under a different account yields either a false
	// "no data" or ANOTHER campaign's numbers presented as this campaign's measurement.
	if err := verifyTwitterAccountMatch("read x ads campaign metrics", campaign, client); err != nil {
		return nil, err
	}
	m, err := client.GetCampaignMetrics(ctx, campaign.PlatformCampaignID, xWindow)
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
		// Conversions is deliberately LEFT NIL. X has no single conversions metric to read:
		// its analytics endpoint splits them across per-event-type metrics
		// (conversion_purchases, conversion_sign_ups, conversion_site_visits, …), each a JSON
		// OBJECT carrying counts and sale amounts rather than a scalar, and only under the
		// WEB_CONVERSION / MOBILE_CONVERSION metric groups this client does not request.
		// Totalling those would require picking which event types count as conversions —
		// the same per-advertiser decision the Meta adapter declines to make.
	}, nil
}

// ListAccounts discovers the X Ads accounts reachable via the project's stored, encrypted
// connection credential, returning the alphanumeric account handle the connection's
// account_id takes verbatim, plus a display label.
//
// It satisfies the service-side AccountLister interface, which Orchestrator.ReadAccounts
// type-asserts on the dispatcher for the requested platform.
//
// It deliberately does NOT require an account id. Discovery exists to answer "which ad
// account should this connection use?", so demanding one would make the endpoint reachable
// only by connections that no longer need it — the account-less connection it is meant to
// rescue is exactly the one it would refuse. The Meta, LinkedIn and Microsoft discovery
// paths draw the same distinction.
//
// It shares validateTwitterConnection with Dispatch and ToggleStatus rather than repeating
// its status / decode / completeness rules, tolerating exactly ONE of its outcomes:
// ErrAccountNotSelected, the state this endpoint serves. Every other sentinel propagates
// intact, so the endpoint's 400-vs-503 mapping stays pinned to the same sentinels the
// dispatch path answers with, and a credential rejected at dispatch cannot be accepted here
// — which would make a discovery endpoint actively misleading rather than merely permissive.
//
// X is the strongest form of that guarantee available, and it is worth stating precisely
// because it differs by provider: validateTwitterConnection is called by Dispatch ITSELF
// (not only by the toggle and metrics paths), so discovery and create genuinely cannot
// diverge. That matches Microsoft; LinkedIn's equivalent is shared by toggle and metrics
// but NOT by Dispatch, which validates inline, and it has already drifted once in practice.
//
// The funding instrument is NOT required. It is a create-only field that
// validateTwitterConnection already declines to check, and an account-less connection has
// no reason to have chosen one yet — requiring it would refuse the very connection this
// endpoint exists to complete.
func (d *TwitterDispatcher) ListAccounts(ctx context.Context, projectID string, platform model.Provider) ([]model.AccessibleAccount, error) {
	res, err := d.creds.resolve(ctx, projectID, platform)
	if err != nil {
		return nil, err
	}
	creds, _, verr := validateTwitterConnection(projectID, res)
	if verr != nil && !errors.Is(verr, domain.ErrAccountNotSelected) {
		return nil, res.systemScoped(verr)
	}
	// AccountConfig is left ZERO. ListAdAccounts asks what the CREDENTIAL reaches, so
	// naming an account would narrow the response to a subset of the question — the same
	// reason meta's, linkedin's and microsoft's discovery clients carry a zero account
	// config. Empty means "discover them", which is the question being asked.
	//
	// **This zero is a convention here, not an enforced invariant, and the difference is
	// recorded because a mutation SURVIVED it.** Populating AccountID on this call changes
	// nothing observable today: twitter.ListAdAccounts builds its URL from baseURL and
	// apiVersion only and never reads AccountConfig, so no test can distinguish the two —
	// and the mutation that passes the stored id here compiles and passes the whole suite.
	// That is a real gap, not a test defect, and weakening the mutation to "fix" it would
	// only hide it. What DOES bind is one layer down, where
	// TestListAdAccounts_RequestsTheCollectionNotTheConfiguredAccount asserts the outbound
	// path and query against a client built WITH an account id — so the moment the client
	// starts consulting AccountConfig, that test fails. This zero therefore protects
	// against a future client change, and the client-side request assertion is what
	// protects against the scoping bug itself. LinkedIn's discovery path carries the same
	// shape and the same caveat.
	client := twitter.NewClient(
		twitter.Credentials{
			ConsumerKey:       creds.ConsumerKey,
			ConsumerSecret:    creds.ConsumerSecret,
			AccessToken:       creds.AccessToken,
			AccessTokenSecret: creds.AccessTokenSecret,
		},
		twitter.AccountConfig{},
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
		accounts = append(accounts, model.AccessibleAccount{ID: a.ID, Label: twitterAccountLabel(a)})
	}
	return accounts, nil
}

// twitterAccountLabel builds the string a picker shows for one X Ads account.
//
// It never returns "" for an account carrying any identifying information: Name may be
// empty on a real account, so it falls back to the id — a blank row in a picker is
// unpickable, and the id is what actually gets stored.
//
// Unusable accounts are LABELLED, not filtered — the same discipline the other discovery
// paths use. Dropping them would answer "your credential reaches no accounts" about an
// account sitting right there; returning them unmarked is worse still, because an account
// under review or rejected then looks exactly as selectable as an accepted one and the
// refusal arrives later at dispatch, with no way back to this list.
//
// ApprovalLabel renders only a KNOWN-BAD approval_status and returns "" for an accepted,
// absent or unrecognised one, so an unexpected value is never labelled as a defect — X
// publishes no complete enum for the field. The `deleted` flag is labelled separately
// because it is a different question that X answers with its own field.
//
// The timezone is rendered into the name the way linkedInAccountLabel renders Currency, and
// for the same reason: X reports campaign schedules and daily budget resets against the
// account's timezone, so two otherwise identically-named accounts differing only by timezone
// are genuinely different choices and a picker that hides it makes them indistinguishable. It
// is a PROPERTY of the account, not a defect, so it joins the name rather than the notes —
// notes are reserved for reasons an account may not be usable.
//
// The TrimSpace calls are defence-in-depth, not the only trim: ListAdAccounts already trims
// both Name and Timezone as it decodes, so no value arriving from the client can be untrimmed
// and neither call here is independently revert-binding. They guard hand-constructed
// AdAccount values — this function takes the struct, not a response body, so a caller can
// build one directly — and keep the empty-name fallback keyed on the same notion of "empty"
// the client uses. Do not read a test that exercises whitespace here as covering the client's
// trim; that path is pinned in internal/platform/twitter/accounts_test.go.
func twitterAccountLabel(a twitter.AdAccount) string {
	name := strings.TrimSpace(a.Name)
	if name == "" {
		name = a.ID
	}
	if tz := strings.TrimSpace(a.Timezone); tz != "" {
		name += " [" + tz + "]"
	}
	var notes []string
	if s := a.ApprovalLabel(); s != "" {
		notes = append(notes, s)
	}
	if a.Deleted {
		notes = append(notes, "deleted")
	}
	if len(notes) == 0 {
		return name
	}
	return name + " — " + strings.Join(notes, ", ")
}
