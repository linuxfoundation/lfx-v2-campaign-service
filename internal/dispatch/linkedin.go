// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
)

// campaignStatusGroupCreated marks a LinkedIn dispatch where the campaign GROUP was
// created but the CAMPAIGN was not — a recoverable orphan whose group id lives in
// Result. Distinct from campaignStatusCreated so the degraded state is visible, and
// PlatformCampaignID is left empty so the orchestrator's idempotency fast path does NOT
// treat it as a completed campaign (it keys reuse on a real id + terminal status). The
// retained claim then blocks a blind re-dispatch; actual recovery awaits the planned
// reconciliation/single-flight support (LFXV2-2665). See campaignFromLinkedIn.
const campaignStatusGroupCreated = "group_created"

// campaignStatusUnconfirmed marks a LinkedIn dispatch where NEITHER the campaign nor
// its group was confirmed created — a group-ambiguous partial returns both
// CampaignID == "" and CampaignGroupID == "". Distinct from campaignStatusCreated so
// the object is never falsely labelled "created" when nothing was confirmed; the
// claim is retained by the caller and Result carries the reconcile blob.
const campaignStatusUnconfirmed = "unconfirmed"

// linkedinCreds mirrors LinkedinAdsCredentials's field name (no json tag) — the
// persisted JSON key is the Go field name (AccessToken), see redditCreds. LinkedIn
// authenticates with a single OAuth2 bearer access token.
type linkedinCreds struct {
	AccessToken string
}

// linkedinConfig is the per-platform campaign config the caller passes for LinkedIn
// in CreateCampaigns' Input.Config (delivered here as the Dispatch `config`). The
// brief supplies event identity; the connection supplies account/org; this supplies
// the LinkedIn-specific campaign shape. RuntimeConfig fields that aren't persisted on
// the connection (targeting profiles, employer exclusions, extra accounts) may be
// supplied here for the client to resolve TargetingProfile against.
type linkedinConfig struct {
	BudgetUSD          float64                           `json:"budgetUsd"`
	LifetimeBudget     bool                              `json:"lifetimeBudget"`
	StartDate          string                            `json:"startDate"` // YYYY-MM-DD
	EndDate            string                            `json:"endDate"`   // YYYY-MM-DD
	GeoTargets         []linkedin.GeoTarget              `json:"geoTargets"`
	TargetingProfile   string                            `json:"targetingProfile"`
	Variants           []linkedin.CreativeVariant        `json:"variants"`
	AdAccountID        string                            `json:"adAccountId"`
	TargetingProfiles  []linkedin.TargetingProfileConfig `json:"targetingProfiles"`
	EmployerExclusions []string                          `json:"employerExclusions"`
}

// LinkedInDispatcher creates LinkedIn campaigns for the orchestrator.
type LinkedInDispatcher struct {
	creds *credsSource
	opts  []linkedin.Option
}

// NewLinkedInDispatcher builds the adapter from the connection repo + encryptor.
func NewLinkedInDispatcher(repo connReader, enc domain.Encryptor, opts ...linkedin.Option) *LinkedInDispatcher {
	return &LinkedInDispatcher{creds: newCredsSource(repo, enc), opts: opts}
}

// Dispatch implements service.PlatformDispatcher for LinkedIn.
//
// RETRY CAVEAT: unlike the reddit/twitter clients, the LinkedIn client's CreateCampaign
// is NOT idempotent — dark posts and creatives have no name-based find-or-create lookup,
// so a blind re-dispatch after an ambiguous failure would DUPLICATE them. A non-nil
// partial result returned alongside an error therefore means "may exist — do NOT blindly
// retry"; the orchestrator RETAINS the claim on it, and safe re-dispatch depends on the
// planned per-(brief, platform) single-flight guard (LFXV2-2665). Callers must not treat
// a LinkedIn ambiguous error as freely retryable the way name-idempotent platforms are.
func (d *LinkedInDispatcher) Dispatch(ctx context.Context, brief *model.CampaignBrief, platform model.Provider, config json.RawMessage) (*model.Campaign, error) {
	res, err := d.creds.resolve(ctx, brief.ProjectID, platform)
	if err != nil {
		return nil, err // preCreateError
	}
	if res.status != model.StatusActive {
		return nil, notCreated(fmt.Errorf("linkedin connection for project %s is %s, not active", brief.ProjectID, res.status))
	}

	var creds linkedinCreds
	if err := json.Unmarshal(res.plaintext, &creds); err != nil {
		return nil, notCreated(fmt.Errorf("decode linkedin credentials: %w", err))
	}
	// ASSIGN the trimmed token, do not merely test it. resolveLinkedInCredentials — which the
	// discovery and toggle paths share — writes the trimmed value back, so testing it here while
	// sending the raw one makes the two paths disagree about the SAME stored credential: a
	// whitespace-padded token passes this check, lists accounts successfully through discovery,
	// and is then sent padded from here for LinkedIn to reject. That is the misleading-discovery
	// state the shared resolver exists to prevent, reintroduced by testing a value instead of
	// adopting it. (The padding is refused at bootstrap install time, so this is defence in
	// depth. The bootstrap installer refuses padded values, but that is NOT the only writer: the
	// public create-connection and set-credential APIs reach `credentialJSON`, which marshals and
	// encrypts without trimming, so a padded token is persistable through supported input today.
	// An earlier version of this comment called the path unreachable; it is not.)
	creds.AccessToken = strings.TrimSpace(creds.AccessToken)
	if creds.AccessToken == "" {
		return nil, notCreated(fmt.Errorf("linkedin credentials are incomplete (need accessToken)"))
	}

	orgID := strings.TrimSpace(res.providerConfig["org_id"])
	accountID := strings.TrimSpace(res.accountID)
	if accountID == "" || orgID == "" {
		return nil, notCreated(fmt.Errorf("linkedin connection for project %s is missing account id or org id", brief.ProjectID))
	}

	var cfg linkedinConfig
	if err := unmarshalPlatformConfig(config, "linkedInConfig", &cfg); err != nil {
		return nil, notCreated(err)
	}
	// Reject an empty variant set BEFORE any upstream create. The client also refuses
	// it (nil, err) after its own validation, but checking up front avoids the wasted
	// input build + upstream round-trip and keeps the claim-release semantics obvious
	// (a pre-create failure releases the claim). Credential/connection resolution has
	// already happened above; this only short-circuits the create itself.
	if len(cfg.Variants) == 0 {
		return nil, notCreated(fmt.Errorf("linkedin campaign creation requires at least one creative variant"))
	}
	bf, err := decodeBriefFields(brief)
	if err != nil {
		return nil, notCreated(err)
	}

	// Build the runtime config from the connection (account/org) plus any richer
	// bits the caller supplied in config (targeting profiles / exclusions the
	// connection doesn't persist). The single account is always present so
	// AdAccountID defaults resolve.
	// The runtime allowlist is sourced ONLY from the connection's own account. Do NOT
	// append a caller-supplied adAccountId — that would defeat the client's
	// cross-tenant fail-closed check (targeting.go), letting any account reachable by
	// the bearer token be treated as authorized and paired with this connection's org.
	// A caller override is therefore only honored when it MATCHES the connection's
	// account; any other value is rejected before an upstream call.
	runtime := linkedin.RuntimeConfig{
		DefaultAccountID:   accountID,
		DefaultOrgID:       orgID,
		Accounts:           []linkedin.Account{{AccountID: accountID, OrgID: orgID, Label: res.label}},
		TargetingProfiles:  cfg.TargetingProfiles,
		EmployerExclusions: cfg.EmployerExclusions,
	}
	// Trim the override once and use the TRIMMED value both for the guard AND for the
	// client input — otherwise a whitespace-padded value that matches the connection
	// passes the guard but reaches the client as a different (padded) account.
	adAccountID := strings.TrimSpace(cfg.AdAccountID)
	if adAccountID != "" && adAccountID != accountID {
		return nil, notCreated(fmt.Errorf("linkedin adAccountId %q does not match the connection's account %q — cross-account campaigns are not allowed", adAccountID, accountID))
	}

	// hsToken is a documented TOP-LEVEL config envelope field (docs/api-catalog.md);
	// a request-supplied token takes precedence over the brief blobs, so a config
	// hsToken drives the dark-post utm_campaign instead of being silently ignored.
	hsToken, err := envelopeHSToken(config)
	if err != nil {
		return nil, notCreated(err) // a wrong-typed hsToken is a caller error (pre-create)
	}
	if hsToken == "" {
		hsToken = bf.HSToken
	}

	in := linkedin.CampaignInput{
		EventName:       bf.EventName,
		RegistrationURL: bf.RegistrationURL,
		HSToken:         hsToken,
		// Project stamped from the authenticated scope, not caller JSON (api-catalog).
		Project:          brief.ProjectID,
		BudgetUSD:        cfg.BudgetUSD,
		LifetimeBudget:   cfg.LifetimeBudget,
		StartDate:        cfg.StartDate,
		EndDate:          cfg.EndDate,
		GeoTargets:       cfg.GeoTargets,
		TargetingProfile: cfg.TargetingProfile,
		Variants:         cfg.Variants,
		AdAccountID:      adAccountID,
	}

	client := linkedin.NewClient(linkedin.Credentials{AccessToken: creds.AccessToken}, runtime, d.opts...)

	// Release the claim ONLY when result==nil (definitely nothing created). Do NOT
	// gate on an empty CampaignID: LinkedIn returns a NON-NIL result even on a
	// DEFINITE campaign failure once the campaign GROUP was created (a permanent
	// resource carrying CampaignGroupID) — and on an ambiguous campaign create it
	// returns a non-nil name-only partial with an empty CampaignID. Both must RETAIN
	// the claim so a retry doesn't duplicate the group/campaign.
	result, cerr := client.CreateCampaign(ctx, in)
	if cerr != nil {
		if result == nil {
			return nil, notCreated(fmt.Errorf("linkedin campaign creation failed before any upstream create: %w", cerr))
		}
		// A non-nil result means a permanent resource exists (campaign group, and maybe
		// the campaign). This covers BOTH an ambiguous create AND a definite campaign
		// failure after a successful group create — either way the claim must be retained.
		return campaignFromLinkedIn(ctx, result, len(cfg.Variants), cfg), fmt.Errorf("linkedin campaign creation incomplete (a campaign group and/or campaign may exist): %w", cerr)
	}
	return campaignFromLinkedIn(ctx, result, len(cfg.Variants), cfg), nil
}

// ToggleStatus pauses or resumes an existing LinkedIn campaign on the platform. It resolves
// the connection (active + access token; the toggle needs the account id but not the org id,
// which is creation-only), builds the client, and CASCADES via UpdateCampaignAndCreativesStatus:
// a RestLi PARTIAL_UPDATE of the campaign status, then discovery of the campaign's creatives
// (LinkedIn persists only a creative count, not ids) and a PARTIAL_UPDATE of each creative's
// intendedStatus — because creatives are created DRAFT, so activating only the campaign would
// not serve. campaign is the persisted row; only campaign.PlatformCampaignID (the numeric
// campaign id) is used. status is model.CampaignRunActive/Paused. An UNCONFIRMED outcome
// (including a partial cascade) is wrapped so the caller reports "verify before retry".
func (d *LinkedInDispatcher) ToggleStatus(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, status string) error {
	liStatus, err := linkedinRunStatus(status)
	if err != nil {
		return err
	}
	res, creds, err := d.resolveLinkedInCredentials(ctx, projectID, platform, d.creds.existingResolver(linkedInCreationAccountID(campaign)))
	if err != nil {
		return err
	}
	accountID := strings.TrimSpace(res.accountID)
	runtime := linkedin.RuntimeConfig{
		DefaultAccountID: accountID,
		Accounts:         []linkedin.Account{{AccountID: accountID, Label: res.label}},
	}
	client := linkedin.NewClient(linkedin.Credentials{AccessToken: creds.AccessToken}, runtime, d.opts...)
	// Provenance BEFORE the mutation, and before the creative-servability question the client
	// answers below. A campaign id is unique only within an ad account, so a connection
	// re-pointed since create would address an unrelated campaign — and this path CHANGES
	// delivery, so a collision pauses or activates something this project does not own.
	//
	// Ordering is load-bearing: UpdateCampaignAndCreativesStatus discovers the campaign's
	// creatives and refuses an activate with none servable (linkedin.IsNotServable →
	// ErrCampaignNotProvisioned). Run against a FOREIGN account that discovery describes a
	// DIFFERENT campaign, so answering "no servable creatives" there would explain the wrong
	// campaign — and it would have contacted LinkedIn to do it. The mismatch is the answer.
	if err := verifyLinkedInAccountMatch("toggle linkedin campaign status", campaign, accountID); err != nil {
		return err
	}
	// Cascade to the campaign's creatives too: CreateCampaign leaves them DRAFT, so
	// activating only the campaign would not serve (a DRAFT creative never serves, and the
	// creative's effective status is gated by the campaign). The client discovers the
	// creatives (LinkedIn persists only a count) and lifts each DRAFT→ACTIVE (or holds PAUSED).
	if uerr := client.UpdateCampaignAndCreativesStatus(ctx, campaign.PlatformCampaignID, liStatus); uerr != nil {
		// An activate refused up front because the campaign has no servable creatives is a
		// local/state error (the platform mutation never ran), so classify it as
		// ErrCampaignNotProvisioned → 409, not the default 503. Mirrors reddit/meta.
		if linkedin.IsNotServable(uerr) {
			return fmt.Errorf("%w: %s", domain.ErrCampaignNotProvisioned, uerr.Error())
		}
		if linkedin.IsOutcomeUnconfirmed(uerr) {
			return &unconfirmedToggleError{err: uerr}
		}
		return uerr
	}
	return nil
}

// resolveLinkedInCredentials fetches the project's LinkedIn connection and validates it is
// usable, tagging each defect with domain.ErrConnectionNotUsable plus a reason sentinel so the
// endpoint stops answering from the 503 default arm.
//
// The status the caller ends up with depends on WHOSE row was defective, which is what the
// deferred systemScoped below decides: a project-owned row answers 409 ("repair your
// connection"), while an LF system fallback row additionally carries
// domain.ErrSystemConnectionNotUsable and answers 500 — the project cannot repair a row it does
// not own, so sending them to fix it would be sending them somewhere they cannot succeed.
//
// Mirrors resolveMetaCredentials, deliberately and structurally — including the `conn := res`
// binding the defer closes over, for the reason meta.go records: every not-usable return sets
// the named return to nil, and systemScoped is a no-op on a nil receiver, so reading the named
// return would silently drop system-row attribution from exactly the errors that need it.
//
// Before this existed LinkedIn repeated four inline connection checks (inactive status,
// credential decode, incomplete credentials, missing account id) across the two synchronous
// callers below, each with bare errors that fell through to 503 — "transient, retry later" for
// defects no retry can fix. No amount of retrying repairs a credential blob that is not valid
// JSON. Four CHECKS, two CALLERS: Dispatch was a third inline site but never reached that 503
// mapping, for the reason the next paragraph gives.
//
// Scoped to the toggle and metrics paths (LFXV2-3196). Dispatch keeps its own inline checks:
// they wrap in notCreated() to release the dispatch claim, which is a different contract from
// returning a classified error to a synchronous handler, and folding the two would mean this
// helper had to know which caller it had.
//
// resolveCreds selects the credential entry point (see credsResolver): discovery is governed by
// the forced-system flag; an operation on an existing campaign resolves the account that
// campaign was CREATED under (linkedInCreationAccountID), which is the LF system account for a
// campaign created while the flag was on.
func (d *LinkedInDispatcher) resolveLinkedInCredentials(ctx context.Context, projectID string, platform model.Provider, resolveCreds credsResolver) (res *resolved, creds linkedinCreds, err error) {
	res, err = resolveCreds(ctx, projectID, platform)
	if err != nil {
		return nil, linkedinCreds{}, err
	}
	conn := res
	defer func() { err = conn.systemScoped(err) }()

	if res.status != model.StatusActive {
		return nil, linkedinCreds{}, fmt.Errorf("%w: %w: linkedin connection for project %s is %s, not active",
			domain.ErrConnectionNotUsable, domain.ErrConnectionInactive, projectID, res.status)
	}
	if uerr := json.Unmarshal(res.plaintext, &creds); uerr != nil {
		// The cause is DROPPED rather than wrapped, same as meta, and for the qualified
		// reason set out in full at resolveMetaCredentials: it is the only value here
		// derived from the DECRYPTED blob. Today's stdlib happens not to quote the
		// offending bytes for a struct of string fields, but that is a behaviour rather
		// than a documented guarantee and it does not hold for every field type. Dropping
		// the cause removes the class instead of resting on it, and costs nothing — the
		// sentinel already names the only actionable remedy.
		return nil, linkedinCreds{}, fmt.Errorf("%w: %w: linkedin credentials for project %s are not valid JSON",
			domain.ErrConnectionNotUsable, domain.ErrCredentialsUndecodable, projectID)
	}
	// Trimmed ONCE, here, and the trimmed value is what both callers receive. An earlier
	// revision trimmed only for the empty check and returned the raw token, so ToggleStatus
	// passed a whitespace-padded token to NewClient while ReadMetrics trimmed it again — the
	// same credential accepted on one path and rejected upstream on the other, surfacing as a
	// retryable 503 for a token that will never work. Trimming at the single point that
	// validates it makes the two callers structurally unable to disagree.
	creds.AccessToken = strings.TrimSpace(creds.AccessToken)
	if creds.AccessToken == "" {
		return nil, linkedinCreds{}, fmt.Errorf("%w: %w: linkedin credentials are incomplete (need accessToken)",
			domain.ErrConnectionNotUsable, domain.ErrCredentialsIncomplete)
	}
	if strings.TrimSpace(res.accountID) == "" {
		// Unlike meta's metrics path, LinkedIn DOES need the account id here: its client is
		// constructed with a RuntimeConfig naming the account, so an empty one cannot reach
		// the platform at all. Tagged so the reason token names the missing CHOICE rather
		// than surfacing as an unclassified failure with no path to completion.
		// creds is returned ALONGSIDE the error, not discarded. By this point the credential
		// has passed every other check, and the discovery path needs exactly that: a valid
		// token on a connection that has chosen no account. Returning it lets discovery
		// reuse this one validation instead of re-decrypting and re-validating, which is
		// what would let the two paths drift. Every dispatch caller checks err first and so
		// cannot observe the value.
		return res, creds, fmt.Errorf("%w: %w: linkedin connection for project %s has no account id",
			domain.ErrConnectionNotUsable, domain.ErrAccountNotSelected, projectID)
	}
	return res, creds, nil
}

// resolveLinkedInDiscoveryCredentials is resolveLinkedInCredentials WITHOUT the account-id
// requirement, and that omission is the entire point.
//
// Account discovery exists to answer "which ad account should this connection use?", so
// demanding one would make the endpoint reachable only by connections that no longer need
// it — the account-less connection it is meant to rescue is exactly the one it would refuse.
// Meta's discovery path draws the same distinction for the same reason; see
// resolveMetaDiscoveryClient.
//
// Every OTHER check is shared with the TOGGLE and METRICS paths by calling through to
// resolveLinkedInCredentials and treating only the account-not-selected outcome as acceptable.
// Duplicating the status/decode/token validation instead would let those drift, so a credential
// rejected at toggle could be accepted here — the failure mode that makes a discovery endpoint
// actively misleading rather than merely permissive.
//
// NOT shared with Dispatch, and the distinction is worth stating precisely because an earlier
// version of this comment claimed it was. Dispatch calls d.creds.resolve directly and validates
// inline (see its own block above), so the two CAN drift — and did: Dispatch was sending an
// untrimmed access token while this resolver trimmed it, which is why a padded credential listed
// accounts here and failed on create. That is fixed, but by a regression test rather than by
// shared code, so the invariant this comment may claim is the narrower one.
func (d *LinkedInDispatcher) resolveLinkedInDiscoveryCredentials(ctx context.Context, projectID string, platform model.Provider) (linkedinCreds, error) {
	_, creds, err := d.resolveLinkedInCredentials(ctx, projectID, platform, d.creds.resolve)
	switch {
	case err == nil:
		return creds, nil
	case errors.Is(err, domain.ErrAccountNotSelected):
		// The one error this endpoint exists to serve: everything about the connection is
		// valid except the choice this call is meant to inform. The resolver returns the
		// validated credential alongside that error precisely so discovery can proceed
		// without re-decrypting or re-validating anything.
		return creds, nil
	default:
		// Any other failure is a real defect in the connection and must propagate with its
		// sentinel intact, so the endpoint's 400-vs-503 mapping stays pinned to the same
		// sentinels the dispatch path answers with.
		return linkedinCreds{}, err
	}
}

// ListAccounts discovers the ad accounts reachable via the project's stored, encrypted
// LinkedIn connection credential, returning the bare numeric account id the connection's
// account_id takes verbatim, plus a display label.
//
// It satisfies the service-side AccountLister interface, which Orchestrator.ReadAccounts
// type-asserts on the dispatcher for the requested platform. It deliberately does NOT
// require an account id to be selected — see resolveLinkedInDiscoveryCredentials.
func (d *LinkedInDispatcher) ListAccounts(ctx context.Context, projectID string, platform model.Provider) ([]model.AccessibleAccount, error) {
	creds, err := d.resolveLinkedInDiscoveryCredentials(ctx, projectID, platform)
	if err != nil {
		return nil, err
	}
	// RuntimeConfig is left ZERO: the accounts finder asks what the TOKEN reaches, so
	// scoping the client to one of the answers would narrow the response to a subset of
	// the question. Same rationale as meta's zero AccountConfig.
	client := linkedin.NewClient(linkedin.Credentials{AccessToken: creds.AccessToken}, linkedin.RuntimeConfig{}, d.opts...)
	adAccounts, lerr := client.ListAdAccounts(ctx)
	if lerr != nil {
		return nil, lerr
	}
	// make(..., 0, n), never nil: a token that legitimately reaches zero ad accounts is an
	// empty ANSWER, not an error, and Orchestrator.ReadAccounts rejects a nil result as a
	// contract violation precisely so empty keeps its meaning.
	accounts := make([]model.AccessibleAccount, 0, len(adAccounts))
	for _, a := range adAccounts {
		accounts = append(accounts, model.AccessibleAccount{ID: a.ID, Label: linkedInAccountLabel(a)})
	}
	return accounts, nil
}

// linkedInAccountLabel builds the string a picker shows for one ad account.
//
// It never returns "" for an account carrying any identifying information: an account with
// no name falls back to its id, because a blank row in a picker is unpickable and the id is
// what actually gets stored.
//
// Everything that decides whether the account can actually be USED is rendered, because a
// lifecycle status alone does not answer it. LinkedIn reports three independent things and
// the client carries a purpose-built rendering for each:
//
//   - StatusLabel() — a KNOWN-BAD lifecycle status, "" for ACTIVE/absent/unrecognized. An
//     empty label is not a claim the account is fine, only that this package has nothing to
//     say, so it is never rendered as reassurance.
//   - ServingHolds() — why an otherwise-ACTIVE account cannot serve. An account can be
//     ACTIVE and on BILLING_HOLD simultaneously; showing only the status would present it
//     as normal.
//   - Test — LinkedIn's immutable test-account flag. Test accounts never serve, never bill,
//     and auto-reject creatives, so a real campaign bound to one silently does nothing.
//     Surfaced rather than filtered: someone wiring up an integration is looking for
//     exactly this account.
//
// Currency rides along because budgets and bids are denominated in it and this client does
// no FX conversion — a picker offering a USD and a JPY account with the same number beside
// them is offering two very different things.
func linkedInAccountLabel(a linkedin.AdAccount) string {
	name := strings.TrimSpace(a.Name)
	if name == "" {
		name = a.ID
	}
	if c := strings.TrimSpace(a.Currency); c != "" {
		name += " [" + c + "]"
	}

	var notes []string
	if s := a.StatusLabel(); s != "" {
		notes = append(notes, s)
	}
	if a.Test {
		notes = append(notes, "TEST account — never serves")
	}
	notes = append(notes, a.ServingHolds()...)
	// Lifecycle and serving are INDEPENDENT, and only the first is a deny-list. StatusLabel()
	// returns "" for ACTIVE, absent AND unrecognized alike, so a silent status cannot be read
	// as a healthy one: an account whose lifecycle this package cannot vouch for still reports
	// servingStatuses ["RUNNABLE"], and would otherwise render EXACTLY like a good account —
	// the one answer a picker must never give, because the operator's next act is to bind a
	// paid campaign to it. Active() is what distinguishes "ACTIVE" from "not confirmed", so
	// qualify on it rather than on StatusLabel()'s emptiness.
	if !a.Active() && a.StatusLabel() == "" {
		notes = append(notes, "account status could not be confirmed")
	}
	// Servable() is an ALLOW-list: an absent or unrecognized servingStatuses is not evidence
	// the account can spend. Say so only when nothing above already explains it, so an
	// unrecognized hold is still visible rather than silently reading as fine.
	if len(notes) == 0 && !a.Servable() {
		notes = append(notes, "cannot currently serve")
	}
	if len(notes) > 0 {
		return name + " — " + strings.Join(notes, ", ")
	}
	return name
}

// ReadMetrics returns live campaign metrics from LinkedIn's Ad Analytics API for the
// given campaign during the specified time window. campaign is the persisted row;
// campaign.PlatformCampaignID is the BARE numeric LinkedIn campaign id (trailingID of
// the campaign URN, as persisted by campaignFromLinkedIn) — linkedin.GetCampaignMetrics
// builds the sponsoredCampaign/sponsoredAccount URNs the Ad Analytics finder requires.
func (d *LinkedInDispatcher) ReadMetrics(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, window model.MetricsWindow) (*model.CampaignMetrics, error) {
	if campaign.PlatformCampaignID == "" {
		return nil, fmt.Errorf("campaign has no platform campaign ID")
	}

	// Validated BEFORE credential resolution, and this order is load-bearing. An unsupported
	// window is a permanent 400 whatever the connection looks like, and it names the one thing
	// the caller can actually change. Resolving credentials first would answer a connection
	// error instead — since LFXV2-3196 that is a 409 for a project-owned row, or a 500 for an
	// unusable LF system fallback — sending the caller to repair a connection when the request
	// would still be rejected on the window. Same order as the X adapter (twitter.go).
	if werr := linkedin.ValidateMetricsWindow(window); werr != nil {
		return nil, fmt.Errorf("get campaign metrics from linkedin: %w", errors.Join(domain.ErrMetricsWindowUnsupported, werr))
	}

	res, creds, err := d.resolveLinkedInCredentials(ctx, projectID, platform, d.creds.existingResolver(linkedInCreationAccountID(campaign)))
	if err != nil {
		return nil, err
	}
	// Already trimmed and non-empty: resolveLinkedInCredentials does both.
	accessToken := creds.AccessToken

	accountID := strings.TrimSpace(res.accountID)

	runtime := linkedin.RuntimeConfig{
		DefaultAccountID: accountID,
		Accounts:         []linkedin.Account{{AccountID: accountID, Label: res.label}},
	}
	client := linkedin.NewClient(linkedin.Credentials{AccessToken: accessToken}, runtime, d.opts...)

	// Prove the persisted campaign belongs to the account this read will be scoped to. The
	// Ad Analytics finder is scoped by the sponsoredAccount URN built from accountID below,
	// which is the project's CURRENT connection and may have been re-pointed since create;
	// querying the stored campaign id under a different account yields either an empty result
	// indistinguishable from genuinely zero activity, or ANOTHER campaign's numbers presented
	// as this campaign's measurement.
	if err := verifyLinkedInAccountMatch("read linkedin campaign metrics", campaign, accountID); err != nil {
		return nil, err
	}

	// Call GetCampaignMetrics with the bare campaign id and account id.
	metrics, err := client.GetCampaignMetrics(ctx, accountID, campaign.PlatformCampaignID, window)
	if err != nil {
		if errors.Is(err, linkedin.ErrUnsupportedWindow) {
			return nil, fmt.Errorf("get campaign metrics from linkedin: %w", errors.Join(domain.ErrMetricsWindowUnsupported, err))
		}
		return nil, fmt.Errorf("get campaign metrics from linkedin: %w", err)
	}

	return metrics, nil
}

// linkedInCreationAccountID reports the sponsored ad account the campaign was CREATED
// under, or "" when the persisted result blob does not record it.
//
// Prefers the explicit accountId the create path now stamps (linkedin.CampaignResult), and
// falls back to the account segment of the linkedInUrl the blob has always carried — the
// create path builds that as ".../campaignmanager/accounts/<accountID>/campaigns[/<id>]",
// making it a faithful record of the same value, so rows written BEFORE the explicit field
// existed stay checkable rather than silently unguarded. Mirrors microsoftCreationAccountID
// (accountId + aid=) and googleAdsCreationCustomerID (customerId + ocid=).
//
// An EMPTY return means "unknown, proceed": absence must not become a new failure signal for
// pre-existing rows, so only a present-AND-different id is treated as a mismatch by callers.
func linkedInCreationAccountID(campaign *model.Campaign) string {
	if campaign == nil || len(campaign.Result) == 0 {
		return ""
	}
	var blob struct {
		AccountID   string `json:"accountId"`
		LinkedInURL string `json:"linkedInUrl"`
	}
	if err := json.Unmarshal(campaign.Result, &blob); err != nil {
		return ""
	}
	if id := strings.TrimSpace(blob.AccountID); id != "" {
		return id
	}
	u, err := url.Parse(blob.LinkedInURL)
	if err != nil {
		return ""
	}
	// ".../campaignmanager/accounts/<accountID>/campaigns..." — take the segment that
	// FOLLOWS "accounts". Indexing a fixed position would silently read the wrong segment
	// if the path shape ever changes; requiring the literal marker fails closed to "unknown".
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, seg := range segs {
		if seg == "accounts" && i+1 < len(segs) {
			return strings.TrimSpace(segs[i+1])
		}
	}
	return ""
}

// verifyLinkedInAccountMatch refuses an operation on a campaign that was created under a
// DIFFERENT sponsored ad account than the project's current connection resolves to.
//
// LinkedIn campaign ids are unique only WITHIN an ad account, and the project's connection
// can be re-pointed between create and a later read/toggle. Without this check the stored
// PlatformCampaignID is addressed against the NEW account, where it either matches nothing —
// rendered as a campaign with genuinely zero activity on the read path — or collides with an
// unrelated campaign, whose numbers become this campaign's measurement or whose delivery is
// changed by the toggle.
//
// Shared by ReadMetrics and ToggleStatus so the two cannot drift, and returns
// domain.ErrCampaignAccountMismatch exactly as the google-ads and microsoft adapters do.
//
// Takes the account id as a plain string rather than the client the microsoft/reddit/twitter
// siblings accept: those build their client inside a resolve* helper and the caller never holds
// the raw id, so client.AccountID() is their only accessible source. Here — and on meta — the
// call site already has res.accountID in hand, so threading the client through would add a
// dependency for no additional information. This helper owns the trimming so both callers and
// the comparison speak one form.
func verifyLinkedInAccountMatch(op string, campaign *model.Campaign, accountID string) error {
	created := linkedInCreationAccountID(campaign)
	current := strings.TrimSpace(accountID)
	// Neither unknown is a mismatch. An absent CREATED id is the pre-existing-row case. An
	// empty CURRENT id cannot prove anything either — "not selected" is an absence, not a
	// different account, and reporting one would render as "resolves to account " with an
	// empty name. resolveLinkedInCredentials already refuses an account-less connection with
	// ErrAccountNotSelected, so this arm is unreachable today; it is stated rather than relied
	// upon so the guard stays correct if that precondition is ever relaxed, as it is on meta.
	if created == "" || current == "" || created == current {
		return nil
	}
	return fmt.Errorf("%s: campaign %s was created under linkedin ad account %s but the project's current connection resolves to account %s: %w",
		op, campaign.PlatformCampaignID, created, current, domain.ErrCampaignAccountMismatch)
}

// linkedinRunStatus maps the service run state (active/paused) to LinkedIn's status enum.
func linkedinRunStatus(status string) (string, error) {
	switch status {
	case model.CampaignRunActive:
		return linkedin.StatusActive, nil
	case model.CampaignRunPaused:
		return linkedin.StatusPaused, nil
	default:
		return "", fmt.Errorf("unsupported campaign run status %q (want %q or %q)", status, model.CampaignRunActive, model.CampaignRunPaused)
	}
}

// campaignFromLinkedIn maps the client result to the persistence model: upstream id,
// name, result blob, the budget/schedule/ConfigSnapshot (via applyCampaignConfig), and
// a status derived from what was confirmed created — one of `created`,
// `created_degraded` (creative shortfall), `group_created` (group only), or
// `unconfirmed` (neither id). requestedVariants is how many creatives the caller asked
// for, used to detect a creative shortfall.
func campaignFromLinkedIn(ctx context.Context, r *linkedin.CampaignResult, requestedVariants int, cfg linkedinConfig) *model.Campaign {
	c := &model.Campaign{
		PlatformCampaignID: r.CampaignID,
		CampaignName:       r.CampaignName,
	}
	// Derive the status from what was actually confirmed created. Start UNCONFIRMED and
	// only claim `created` once a real campaign id exists — a group-*ambiguous* partial
	// returns BOTH CampaignID == "" and CampaignGroupID == "" (client.go buildResult on
	// the group-create failure path), and defaulting to `created` there would stamp
	// "created" on an object where nothing was confirmed.
	switch {
	case r.CampaignID != "":
		c.Status = campaignStatusCreated
		if r.CreativeCount < requestedVariants {
			// The campaign exists but fewer creatives were created than requested — a
			// DEGRADED success (mirrors the reddit/meta/twitter created_degraded handling).
			// NOTE: today the LinkedIn client aborts (returns an error) on the FIRST
			// creative failure, so a clean (result, nil) success normally has
			// CreativeCount == requested; this guard is defensive so a shortfall is never
			// silently reported as a clean `created` (and flags the count on the
			// retained-error path).
			c.Status = campaignStatusCreatedDegraded
		}
	case r.CampaignGroupID != "":
		// The campaign GROUP was created but the CAMPAIGN failed/left-ambiguous with an
		// EMPTY CampaignID. We must NOT stuff the group id into PlatformCampaignID: the
		// orchestrator's idempotency treats ANY non-empty PlatformCampaignID as "campaign
		// finished upstream" and short-circuits a later dispatch to success — so a
		// group-only orphan would look permanently succeeded and the campaign would never
		// be created on retry. PlatformCampaignID stays EMPTY (no campaign exists) so the
		// idempotency fast path does NOT treat it as complete; the retained claim then
		// blocks a blind re-dispatch (recovery awaits LFXV2-2665). The group orphan is
		// preserved in Result (CampaignGroupID) + the group_created status for reconciliation.
		c.Status = campaignStatusGroupCreated
	default:
		// Neither id present — a group-ambiguous partial where even the group create is
		// unconfirmed. Leave the status unconfirmed rather than falsely `created`; the
		// claim is retained by the caller and Result carries the reconcile blob.
		c.Status = campaignStatusUnconfirmed
	}
	// Persist the budget/schedule/config the caller supplied (LinkedIn honors a
	// lifetime-vs-daily budget flag). ConfigSnapshot captures the validated config.
	applyCampaignConfig(ctx, c, cfg.BudgetUSD, cfg.LifetimeBudget, cfg.StartDate, cfg.EndDate, cfg)
	if raw, err := json.Marshal(r); err != nil {
		// A marshal failure should be near-impossible for this plain struct, but do NOT
		// swallow it: leaving Result empty would make an orphaned campaign harder to
		// reconcile. Log it (the row is still persisted with its id/status).
		slog.WarnContext(ctx, "failed to marshal linkedin campaign result blob (Result left empty)",
			"campaign_id", c.PlatformCampaignID, "error", err)
	} else {
		c.Result = raw
	}
	return c
}
