// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/utm"
)

// portalLookupTimeout bounds the AuthenticatedPortalID call on BOTH paths that make it:
// Dispatch (best-effort, recording provenance) and ReadMetrics (load-bearing, checking it).
// It is deliberately much shorter than either caller's budget — providerCallTimeout (2m) and
// metricsCallTimeout (20s), both in orchestrator.go — because the HubSpot client's own retry
// policy can wait up to retryMax*maxRetryWait (180s) under sustained throttling, which alone
// exceeds both. The 20s metrics budget is the binding one: unbounded, a throttled provenance
// lookup would consume the whole read and surface as "metrics are down" rather than as
// throttling. On either path the lookup must not be able to consume the budget the real work
// needs — the mutating CloneEmail/SetSendList calls, or GetEmailMetrics.
const portalLookupTimeout = 10 * time.Second

// hubspotCreds is the credential shape stored (encrypted) for a HubSpot connection. HubSpot
// authenticates with a single private-app access token. The field name (no json tag) is the
// persisted JSON key, mirroring the sibling credential structs.
type hubspotCreds struct {
	PrivateAppToken string
}

// hubspotConfig is the per-platform campaign config the caller passes for HubSpot (the email
// channel) in CreateCampaigns' Input.Config (delivered here as the Dispatch `config`).
//
// Unlike the ad platforms (which CREATE a campaign), the email dispatcher STAGES a marketing
// email: it CLONES a caller-specified source/template email and points its send list at the
// brief's already-built audience (LFXV2-2774 populates campaign_audiences; the AI body content
// is a separate step, LFXV2-2775). So the only required input here is which template to clone.
type hubspotConfig struct {
	// SourceEmailID is the HubSpot marketing-email id to clone as the campaign's email. REQUIRED
	// — there is no default template. The clone is created in a draft state (a human reviews and
	// sends it), so staging is safe.
	SourceEmailID string `json:"sourceEmailId"`
	// UTMCampaign optionally overrides the utm_campaign applied to the email's links. OPTIONAL:
	// when unset the campaign is derived from the deterministic email name, so links are always
	// attributable. Set it to make several briefs' emails roll up to one campaign in reporting.
	UTMCampaign string `json:"utmCampaign"`
}

// audienceReader is the narrow read slice of the audience repository the email dispatcher needs:
// it looks up the brief's built HubSpot audience to resolve the send-list. Kept minimal (not the
// full domain.AudienceRepository) so the dispatcher can't mutate audiences.
type audienceReader interface {
	ListAudiences(ctx context.Context, projectID, briefID string) ([]*model.CampaignAudience, error)
}

// HubSpotDispatcher stages HubSpot marketing-email campaigns for the orchestrator. It is the
// email channel's PlatformDispatcher (LFXV2-2777, Capability C — staging).
type HubSpotDispatcher struct {
	creds     *credsSource
	audiences audienceReader
	opts      []hubspot.Option
}

// NewHubSpotDispatcher builds the adapter from the connection repo + encryptor + the audience
// reader (to resolve the brief's built send-list).
func NewHubSpotDispatcher(repo connReader, enc domain.Encryptor, audiences audienceReader, opts ...hubspot.Option) *HubSpotDispatcher {
	return &HubSpotDispatcher{creds: newCredsSource(repo, enc), audiences: audiences, opts: opts}
}

// resolveHubSpotClient decrypts the project's HubSpot connection and builds a client from
// it. Extracted once there was a second caller (ReadMetrics) rather than inlined again:
// the credential-resolution sequence is where the connection-state and
// incomplete-credential checks live, and two copies of it drift.
//
// NoUpstreamCreate is NOT added here — a READ caller has no create to disown, so marking
// that axis is the mutating caller's job. The AUDIENCE axis is different and is tagged
// here, at the point of detection, exactly as validateGoogleAdsCredentials does: each of
// the three stored-connection defects below carries domain.ErrConnectionNotUsable
// alongside the sentinel naming which defect it is. Returned bare they fall to
// GetCampaignMetrics' default arm and answer 503 — "the platform did not respond" about a
// platform that was never contacted, with a remedy (retry) that no amount of waiting can
// satisfy, since only a human editing the connection can fix it.
//
// The named return plus deferred systemScoped means a return site added later cannot
// forget to re-attribute the error to the LF system row when the credentials came from
// there; it is a no-op for project-owned connections and idempotent.
func (d *HubSpotDispatcher) resolveHubSpotClient(ctx context.Context, projectID string, platform model.Provider) (client *hubspot.Client, err error) {
	res, err := d.creds.resolve(ctx, projectID, platform)
	if err != nil {
		return nil, err // already a preCreateError
	}
	defer func() { err = res.systemScoped(err) }()

	if res.status != model.StatusActive {
		return nil, fmt.Errorf("%w: %w: hubspot connection for project %s is %s, not active",
			domain.ErrConnectionNotUsable, domain.ErrConnectionInactive, projectID, res.status)
	}

	var creds hubspotCreds
	if err := json.Unmarshal(res.plaintext, &creds); err != nil {
		// The unmarshal error is DROPPED, not wrapped — same reasoning as the google ads
		// validator. It is the one error on this path derived from the DECRYPTED
		// credential blob, and encoding/json quotes its input: a *json.SyntaxError names
		// the offending character and a *json.UnmarshalTypeError names the field it was
		// reading. Wrapping it would put credential-derived bytes into the service's log
		// line for exactly the connection whose credentials are malformed. Nothing
		// actionable is lost — the remedy is "re-save the credential", not "fix byte 41" —
		// and the sentinel keeps the condition greppable with no payload attached.
		return nil, fmt.Errorf("%w: %w: hubspot credentials for project %s are not valid JSON",
			domain.ErrConnectionNotUsable, domain.ErrCredentialsUndecodable, projectID)
	}
	// Trimmed ONCE, and the trimmed value is what reaches the client. hubspot.NewClient
	// trims again, so padding could not reach the wire either way; the point of trimming
	// HERE is that the emptiness check below must be made against the same value the
	// client will use. A whitespace-only token is an INCOMPLETE credential — a
	// configuration error the caller can fix — and saying so at this layer beats letting
	// it surface later as a generic missing-token failure from inside the client.
	token := strings.TrimSpace(creds.PrivateAppToken)
	if token == "" {
		return nil, fmt.Errorf("%w: %w: hubspot credentials are incomplete (need privateAppToken)",
			domain.ErrConnectionNotUsable, domain.ErrCredentialsIncomplete)
	}

	return hubspot.NewClient(
		hubspot.Credentials{PrivateAppToken: token},
		hubspot.AccountConfig{PortalID: res.providerConfig["portal_id"]},
		d.opts...,
	), nil
}

// ReadMetrics implements service.MetricsReader for the HubSpot email channel (LFXV2-3058):
// it reads the staged email's live statistics over one window. A pure read — nothing is
// mutated upstream and nothing is persisted.
//
// The window is validated BEFORE credentials are resolved, and the order is load-bearing:
// an unsupported window is a permanent 400 whatever the connection looks like, so it must
// not be maskable by a fault that depends on connection state. Resolving first would let
// the connection answer instead — 409 for the not-usable defects resolveHubSpotClient tags
// below, 500 when it fell back to an unusable system row, and 503 when the project has no
// connection at all — none of which tells the caller the thing they actually have to fix,
// which is the window they sent.
//
// That 503 is worth naming rather than rounding off, because it is the worst of the three to
// be masked BY. GetCampaignMetrics has no domain.ErrNotFound arm: creds.resolve passes the
// absence through untagged, so it reaches the handler's default and is reported as a
// transient platform failure. Both halves of that are wrong for the caller — an absent
// connection is a permanent configuration fault, and here it would be masking a permanent
// input fault — so a caller who sent a bad window against a project with no HubSpot
// connection would be told to retry, twice over, for a request that can never succeed.
// Validating the window first means the 400 wins; the absent-connection mapping itself is a
// contract question for every platform's metrics read, not this adapter's to change.
//
// Same ORDER as the linkedin and X adapters, but note the reason differs from linkedin's:
// linkedin's resolve returns its inactive-connection error UNTAGGED, so there the masking
// error really is a 503 telling the caller to retry something that can never succeed. This
// adapter tags with domain.ErrConnectionNotUsable, which internal/service/brief.go
// classifies as a 409. Do not copy linkedin's 503 wording back over here.
func (d *HubSpotDispatcher) ReadMetrics(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, window model.MetricsWindow) (*model.CampaignMetrics, error) {
	if campaign.PlatformCampaignID == "" {
		return nil, fmt.Errorf("campaign has no platform campaign ID")
	}
	if werr := hubspot.ValidateMetricsWindow(window); werr != nil {
		return nil, fmt.Errorf("get email metrics from hubspot: %w", errors.Join(domain.ErrMetricsWindowUnsupported, werr))
	}

	client, err := d.resolveHubSpotClient(ctx, projectID, platform)
	if err != nil {
		return nil, err
	}

	// PlatformCampaignID is a bare numeric HubSpot email id, unique only within the portal
	// that minted it, and the connection just resolved is the project's CURRENT one — a
	// credential swap can re-point it between create and read. Reading across a re-point is
	// worse than an error either way: on a same-id collision this reports ANOTHER portal's
	// opens and clicks as this campaign's, and with no collision it comes back as
	// ErrNoSentEmailInWindow — "not sent yet", the most ordinary state this channel has —
	// for an email that was sent and is fine. Neither is distinguishable downstream from
	// the truth.
	//
	// Both sides come from the TOKEN, not from providerConfig["portal_id"]. That is the
	// whole correction over the first attempt at this guard: portal_id is an optional
	// operator-supplied string the client uses only for app URLs, and a credential swap
	// leaves it untouched, so a config-on-config comparison fires exactly when an operator
	// DECLARES the change and stays silent on the undeclared token swap that is the actual
	// risk.
	//
	// An unrecorded creating portal is not permission to proceed. The id it would be
	// compared against is meaningless outside its portal, so a row that cannot name one
	// cannot be read safely at all — this refuses rather than guessing, which is the
	// difference between a guard and a formality.
	// The cost is that a row written before this recorded the portal — the email channel
	// dispatched before this change — is not readable until it is re-dispatched. That is
	// the honest outcome: nothing about such a row establishes which portal its id means,
	// and reporting numbers that might belong to somebody else is not a better answer than
	// saying so.
	//
	// Checked BEFORE the portal lookup, and deliberately so: absent provenance is a purely
	// LOCAL fact, and no value the lookup could return would change the answer. Asking
	// first inverted the outcome for exactly the rows this guard exists for — a legacy row
	// read while token-info was throttled or down returned the transient 503 below instead
	// of this deterministic 409, hiding the one remedy that fixes it (re-dispatch, which
	// writes the provenance) behind an unrelated upstream failure that no amount of
	// retrying the read will clear. It also spent up to portalLookupTimeout of the 20s
	// metrics budget on a call whose result was already irrelevant.
	//
	// For the same reason the message names no current portal: there isn't one yet, and
	// the defect is the missing record, not what it fails to match.
	created := hubSpotCreationPortalID(campaign)
	if created == "" {
		return nil, fmt.Errorf("get email metrics from hubspot: campaign %s does not record which portal email %s was created in, so its id cannot be resolved against any portal: %w",
			campaign.ID, campaign.PlatformCampaignID, errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch))
	}

	// The lookup is bounded with its OWN short deadline, separate from the ambient context:
	// the client's retry policy alone can wait up to retryMax*maxRetryWait (180s) on sustained
	// throttling, which alone exceeds the whole metrics-call budget (20s) and would burn it
	// before GetEmailMetrics even runs.
	portalCtx, cancelPortal := context.WithTimeout(ctx, portalLookupTimeout)
	current, perr := client.AuthenticatedPortalID(portalCtx)
	cancelPortal()
	if perr != nil {
		return nil, fmt.Errorf("get email metrics from hubspot: cannot establish which portal this token authenticates against: %w", perr)
	}
	if created != current {
		return nil, fmt.Errorf("get email metrics from hubspot: email %s was created in portal %s but this project's token authenticates against portal %s: %w",
			campaign.PlatformCampaignID, created, current, domain.ErrCampaignAccountMismatch)
	}

	metrics, err := client.GetEmailMetrics(ctx, campaign.PlatformCampaignID, window)
	if err != nil {
		if errors.Is(err, hubspot.ErrUnsupportedWindow) {
			return nil, fmt.Errorf("get email metrics from hubspot: %w", errors.Join(domain.ErrMetricsWindowUnsupported, err))
		}
		// An empty match is a successful read of nothing, not a platform failure. Left
		// unmarked it would take the 503 default and report an outage for the most
		// ordinary state this channel has: a staged draft nobody has sent yet.
		if errors.Is(err, hubspot.ErrNoSentEmailInWindow) {
			return nil, fmt.Errorf("get email metrics from hubspot: %w", errors.Join(domain.ErrNoMetricsInWindow, err))
		}
		return nil, fmt.Errorf("get email metrics from hubspot: %w", err)
	}
	// The platform client keys its result by the HubSpot email id it queried; the API
	// contract is that campaign_id is the SERVICE's campaign UUID, so it is restated here
	// rather than in the client, which has no way to know it.
	metrics.CampaignID = campaign.ID
	return metrics, nil
}

// Dispatch implements service.PlatformDispatcher for the HubSpot email channel. It clones the
// caller's template email and sets its send list to the brief's built audience. The returned
// campaign's PlatformCampaignID is the cloned email's HubSpot id.
func (d *HubSpotDispatcher) Dispatch(ctx context.Context, brief *model.CampaignBrief, platform model.Provider, config json.RawMessage) (*model.Campaign, error) {
	// Resolve creds FIRST (pre-create): a missing/undecryptable connection is a not-created
	// error → the orchestrator releases the claim. resolve() already returns a preCreateError,
	// so that one is passed through unwrapped; everything resolveHubSpotClient adds on top is
	// pre-create too and gets wrapped here.
	client, err := d.resolveHubSpotClient(ctx, brief.ProjectID, platform)
	if err != nil {
		// Same shape as the reddit adapter: an already-marked error passes through rather
		// than being double-wrapped, and everything else is marked here.
		var nuc interface{ NoUpstreamCreate() bool }
		if errors.As(err, &nuc) && nuc.NoUpstreamCreate() {
			return nil, err
		}
		return nil, notCreated(err)
	}

	var cfg hubspotConfig
	if err := unmarshalPlatformConfig(config, "hubspotConfig", &cfg); err != nil {
		return nil, notCreated(err)
	}
	if strings.TrimSpace(cfg.SourceEmailID) == "" {
		return nil, notCreated(fmt.Errorf("hubspot campaign requires a sourceEmailId (the template email to clone)"))
	}
	// NOTE: unlike the ad adapters, this does NOT call decodeBriefFields — that helper REQUIRES a
	// non-empty eventName (every ad platform's create contract needs it), but email staging only
	// uses the event name to LABEL the cloned draft, and composeEmailName falls back to the event
	// slug / brief id. A brief with no eventName in its details must still be able to stage an
	// email, so the name is read leniently instead.
	eventName := lenientEventName(brief)

	// Resolve the brief's BUILT audience: the send list is the audience's HubSpot master list.
	// All of this is pre-create (no HubSpot mutation yet), so any failure releases the claim.
	masterListID, suppressionIDs, aerr := d.resolveBuiltAudience(ctx, brief.ProjectID, brief.ID)
	if aerr != nil {
		return nil, notCreated(aerr)
	}
	// Pre-flight the master/suppression conflict BEFORE cloning: SetSendList rejects when the
	// master list also appears in the suppression set (it would exclude the whole audience), but
	// discovering that only after CloneEmail would orphan a draft. This is pure validation (no
	// HubSpot call), so a conflict fails cleanly with nothing created.
	for _, s := range suppressionIDs {
		if s == masterListID {
			return nil, notCreated(fmt.Errorf("hubspot: the audience master list %q is also in its suppression set — the send list would exclude the entire audience", masterListID))
		}
	}

	// Resolve the portal this token authenticates against BEFORE anything is created, so the
	// row can record where its email id means something. Deliberately BEST-EFFORT: it is one
	// more network round trip before a send that is otherwise ready, and a provenance lookup
	// is not worth failing a campaign over. The cost of an empty value lands entirely on
	// ReadMetrics, which refuses rather than guessing — a campaign that sends and cannot be
	// measured beats one that does not send.
	//
	// "Best-effort" here does NOT mean the lookup is expected to fail. It reads the
	// private-apps token-info endpoint, which a private-app token can always call; an earlier
	// version read /account-info/v3/details, which requires the `oauth` scope no private app
	// can hold, so it failed in EVERY account and this warning would have been the steady
	// state rather than the exception. If this warning is common in the logs, that is a real
	// problem to investigate, not background noise.
	//
	// Bounded with its OWN short deadline, separate from providerCallTimeout: the client's
	// retry policy alone can wait up to retryMax*maxRetryWait (180s) on sustained throttling,
	// which exceeds the whole 2-minute provider-call budget and would hand CloneEmail a
	// context that is already cancelled. A best-effort lookup is not worth spending the
	// mutating calls' budget on.
	portalCtx, cancelPortal := context.WithTimeout(ctx, portalLookupTimeout)
	portalID, perr := client.AuthenticatedPortalID(portalCtx)
	cancelPortal()
	if perr != nil {
		slog.WarnContext(ctx, "could not resolve the hubspot portal for this token; the campaign will be created without one and its metrics will not be readable",
			"project_id", brief.ProjectID, "error", perr)
	}

	// STEP 1 (mutating): clone the template email. From here a failure MAY have created the
	// clone upstream, so classify by whether the outcome is confirmable.
	cloneName := composeEmailName(eventName, brief.EventSlug, brief.ID)
	email, cerr := client.CloneEmail(ctx, cfg.SourceEmailID, cloneName)
	if cerr != nil {
		// A clone is the FIRST mutating call: an UNCONFIRMED outcome (transport/5xx) may have
		// created the email, so retain the claim with a name-only partial for reconcile; a
		// definite failure created nothing (claim released).
		if hubspot.IsUnconfirmed(cerr) {
			return emailPartial(ctx, cloneName), fmt.Errorf("hubspot email clone UNCONFIRMED (an email named %q may exist — verify before retrying): %w", cloneName, cerr)
		}
		return nil, notCreated(fmt.Errorf("hubspot email clone failed: %w", cerr))
	}

	// STEP 2 (mutating): point the cloned email's send list at the built audience. The clone
	// already exists, so ANY failure here is a PARTIAL application — return the campaign (with
	// the clone id) so the orchestrator retains the claim and the email is reconcilable, and
	// surface the error so the caller verifies rather than reporting a clean success.
	if _, serr := client.SetSendList(ctx, email.ID, masterListID, suppressionIDs); serr != nil {
		camp := campaignFromHubSpot(ctx, email, cfg, portalID)
		return camp, fmt.Errorf("hubspot email %s cloned but setting its send list failed (verify before retrying): %w", email.ID, serr)
	}

	// STEP 3 (mutating, BEST-EFFORT): tag the draft's links with UTM parameters so email
	// traffic is attributable in the warehouse. Deliberately LAST and non-fatal: the email is
	// already cloned and pointed at the right audience, so it is a working campaign. An
	// untagged email is a reporting gap; failing here would turn that gap into a failed send
	// and leave a configured draft behind anyway.
	tagEmailLinks(ctx, client, email.ID, cloneName, cfg.UTMCampaign)

	return campaignFromHubSpot(ctx, email, cfg, portalID), nil
}

// tagEmailLinks rewrites the cloned draft's links to carry UTM parameters. Best-effort by
// contract: every failure is logged and swallowed, because the campaign is already valid
// without it (see the call site).
//
// campaignUTM is the value configured on the brief's platform config, when set; otherwise the
// campaign is derived from the deterministic email name.
func tagEmailLinks(ctx context.Context, client *hubspot.Client, emailID, emailName, campaignUTM string) {
	res := utm.Resolve(campaignUTM, emailName)

	widgets, err := client.GetEmailHTMLWidgets(ctx, emailID)
	if err != nil {
		slog.WarnContext(ctx, "could not read the email draft to tag its links; the email will send untagged",
			"email_id", emailID, "error", err)
		return
	}

	// Iterate widgets in a STABLE order and carry the link count across them. Go map order is
	// randomized, so without sorting the same email would number its links differently on each
	// run; without carrying the count, every widget would restart at "body-link-1" and a
	// multi-widget email would emit duplicate utm_content values that no report can tell apart.
	keys := make([]string, 0, len(widgets))
	for k := range widgets {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	tagged := make(map[string]string, len(widgets))
	tagCount := 0
	for _, key := range keys {
		body := widgets[key]
		out, n, terr := utm.TagHTMLLinksFrom(body, res.Params, "", tagCount)
		if terr != nil {
			// TagHTMLLinks returns the ORIGINAL body alongside its error, so skipping this
			// widget leaves it exactly as it was rather than writing back something mangled.
			slog.WarnContext(ctx, "could not tag a widget's links; leaving it untagged",
				"email_id", emailID, "widget", key, "error", terr)
			continue
		}
		tagCount += n
		if out != body {
			tagged[key] = out
		}
	}
	if len(tagged) == 0 {
		// Nothing to write: no links, or every link was already tagged. Not a failure.
		return
	}
	if _, perr := client.SetEmailHTMLWidgets(ctx, emailID, tagged); perr != nil {
		// "MAY send untagged", not "will": a PATCH error does not prove the write did not land.
		// patchEmail returns an UNCONFIRMED error for a 2xx with a null or undecodable body,
		// where the update may well have applied. Telling an operator the draft is definitely
		// untagged would send them to re-tag a draft that is already correct. The draft is a
		// human-reviewed artefact, so the honest instruction is "check it".
		slog.WarnContext(ctx, "could not confirm tagged links were written back to the email draft; it may send untagged — verify the draft",
			"email_id", emailID, "widgets", len(tagged), "error", perr)
		return
	}
	slog.InfoContext(ctx, "tagged email links with utm parameters",
		"email_id", emailID, "widgets", len(tagged), "utm_campaign", res.Params.Campaign, "utm_source_of_campaign", res.Source)
}

// resolveBuiltAudience finds the brief's most-recent BUILT HubSpot audience and returns its
// master list id + suppression list ids. It fails (a pre-create error) when no audience exists
// or the newest one is not yet built — activating an email against a missing/incomplete audience
// would send to the wrong (or no) recipients, so this refuses rather than send blindly.
func (d *HubSpotDispatcher) resolveBuiltAudience(ctx context.Context, projectID, briefID string) (masterListID string, suppressionIDs []string, err error) {
	auds, lerr := d.audiences.ListAudiences(ctx, projectID, briefID)
	if lerr != nil {
		return "", nil, fmt.Errorf("hubspot: could not load the brief's audience: %w", lerr)
	}
	// ListAudiences returns newest-first; take the newest HubSpot audience that is BUILT.
	for _, a := range auds {
		if a.Platform != model.ProviderHubSpot {
			continue
		}
		if a.Status != model.AudienceBuilt {
			// The newest hubspot audience isn't built yet (still building / failed) — refuse; a
			// retry after it builds will succeed. A stale older audience must NOT be substituted.
			return "", nil, fmt.Errorf("hubspot: the brief's audience is %s, not built — build the audience before staging the email", a.Status)
		}
		if strings.TrimSpace(a.PlatformMasterListID) == "" {
			return "", nil, fmt.Errorf("hubspot: the built audience has no master list id")
		}
		ids, derr := decodeSuppressionIDs(a.SuppressionListIDs)
		if derr != nil {
			return "", nil, derr
		}
		return strings.TrimSpace(a.PlatformMasterListID), ids, nil
	}
	return "", nil, fmt.Errorf("hubspot: no audience found for this brief — build one before staging the email")
}

// decodeSuppressionIDs parses the audience's SuppressionListIDs JSON (a string array) into a
// trimmed, non-empty slice. A null/absent field yields no suppressions (not an error).
func decodeSuppressionIDs(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var ids []string
	if err := json.Unmarshal(raw, &ids); err != nil {
		return nil, fmt.Errorf("hubspot: could not decode the audience suppression list ids: %w", err)
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if s := strings.TrimSpace(id); s != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

// lenientEventName reads the brief's event name from its detail blobs WITHOUT requiring it (used
// only to label the cloned email; composeEmailName falls back when it is empty). It returns ""
// rather than erroring on a brief that has no eventName — email staging must still proceed.
func lenientEventName(brief *model.CampaignBrief) string {
	for _, blob := range []json.RawMessage{brief.EventDetails, brief.Copy} {
		if len(blob) == 0 {
			continue
		}
		var partial struct {
			EventName string `json:"eventName"`
		}
		if err := json.Unmarshal(blob, &partial); err != nil {
			continue
		}
		if s := strings.TrimSpace(partial.EventName); s != "" {
			return s
		}
	}
	return ""
}

// composeEmailName builds the cloned email's name from the event + brief id so it is
// deterministic (a retry composes the same name, aiding reconcile) and human-recognizable.
func composeEmailName(eventName, eventSlug, briefID string) string {
	base := strings.TrimSpace(eventName)
	if base == "" {
		base = strings.TrimSpace(eventSlug)
	}
	if base == "" {
		base = "LFX Campaign"
	}
	return fmt.Sprintf("%s — %s", base, briefID)
}

// emailPartial builds a name-only reconcilable campaign for an UNCONFIRMED clone (no id known).
// It MUST populate Result (not just CampaignName): the orchestrator persists a retained,
// id-less orphan only when PlatformCampaignID != "" OR len(Result) > 0, so an empty Result would
// drop the row and lose the deterministic name needed to reconcile the maybe-created draft. The
// status is `unconfirmed` (not `created`) so the object is never falsely labelled created when
// nothing was confirmed. Mirrors the linkedin group-ambiguous partial.
func emailPartial(ctx context.Context, name string) *model.Campaign {
	c := &model.Campaign{CampaignName: name, Status: campaignStatusUnconfirmed}
	// Carry the name in Result so the orchestrator's len(Result) > 0 persist condition keeps
	// the row and a reconciler can look the draft up by name.
	if raw, err := json.Marshal(struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}{Name: name, Status: campaignStatusUnconfirmed}); err != nil {
		slog.WarnContext(ctx, "failed to marshal hubspot unconfirmed-clone result blob (Result left empty)",
			"campaign_name", name, "error", err)
	} else {
		c.Result = raw
	}
	return c
}

// campaignFromHubSpot maps the cloned email to the persistence model. The email's HubSpot id is
// the campaign's PlatformCampaignID.
//
// portalID is the hub the TOKEN authenticated against when the clone was made, recorded in
// Result beside the email. PlatformCampaignID is a bare numeric that means nothing outside its
// portal, so without this the row cannot say what its own id refers to — see
// hubSpotCreationPortalID. It rides on a wrapper rather than inside the marshalled
// hubspot.Email because it is a property of the connection that made the call, not a field
// HubSpot returns. It may be empty: the lookup is best-effort at the call site.
func campaignFromHubSpot(ctx context.Context, e *hubspot.Email, cfg hubspotConfig, portalID string) *model.Campaign {
	c := &model.Campaign{
		PlatformCampaignID: e.ID,
		CampaignName:       e.Name,
		Status:             campaignStatusCreated,
	}
	// The email channel has no numeric budget/schedule config; ConfigSnapshot still captures the
	// validated hubspotConfig (the cloned template id) for provenance/reconcile.
	applyCampaignConfig(ctx, c, 0, false, "", "", cfg)
	if raw, err := json.Marshal(struct {
		*hubspot.Email
		PortalID string `json:"portalId,omitempty"`
	}{Email: e, PortalID: portalID}); err != nil {
		slog.WarnContext(ctx, "failed to marshal hubspot email result blob (Result left empty)",
			"campaign_id", c.PlatformCampaignID, "error", err)
	} else {
		c.Result = raw
	}
	return c
}

// hubSpotCreationPortalID recovers the portal the campaign's email was created in from the
// persisted Result blob, mirroring googleAdsCreationCustomerID.
//
// An empty return means UNKNOWN, and unlike the Google Ads original the caller must NOT read
// that as permission to proceed: a HubSpot email id is meaningless outside its portal, so a
// row that cannot name one has nothing to check against.
func hubSpotCreationPortalID(campaign *model.Campaign) string {
	if campaign == nil || len(campaign.Result) == 0 {
		return ""
	}
	var blob struct {
		PortalID string `json:"portalId"`
	}
	if err := json.Unmarshal(campaign.Result, &blob); err != nil {
		return ""
	}
	return strings.TrimSpace(blob.PortalID)
}

// SearchEmails implements service.EmailSearcher for the HubSpot email channel. It resolves the
// same connection every other HubSpot path does — so the three stored-connection defects arrive
// tagged with domain.ErrConnectionNotUsable rather than as bare errors — and searches the
// portal's marketing emails by name or subject.
//
// That tag answers **400** here, not the 409 the campaign endpoints return for the same
// sentinel. The status is chosen by the CALLER, and this one is a connections endpoint:
// classifyDiscoveryError maps ErrConnectionNotUsable to BadRequestError, exactly as account
// discovery does. Both are the right answer for their surface — a campaign toggle's 409 says
// "this campaign cannot be acted on as things stand", while a connection read's 400 says "the
// connection you are asking about is misconfigured" — but only one of them is this function's,
// and naming the wrong one in a comment is how a caller ends up handling a status that never
// arrives.
//
// This is a TEMPLATE picker, not an account picker. A HubSpot connection is already scoped to
// the portal its private-app token authenticates against, so there is no account to choose; what
// the caller must choose is the email to clone, because hubspotConfig.SourceEmailID is required
// and has no default (see the Dispatch contract above).
//
// Archived and draft emails are RETURNED rather than filtered, for the same reason the Meta
// account picker returns disabled accounts with the reason in the label: hiding the row the user
// is looking for answers "your portal has no such email" about an email sitting right there. The
// caller gets State and decides.
func (d *HubSpotDispatcher) SearchEmails(ctx context.Context, projectID string, platform model.Provider, query string) ([]model.MarketingEmail, error) {
	client, err := d.resolveHubSpotClient(ctx, projectID, platform)
	if err != nil {
		return nil, err
	}

	emails, err := client.SearchEmails(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("search hubspot marketing emails: %w", err)
	}

	// make(..., 0, n), never a nil slice: service.EmailSearcher requires a non-nil result on
	// success so "this portal has no matching email" stays distinguishable from "the searcher
	// fell through a branch". Orchestrator.SearchEmails rejects (nil, nil) as a contract
	// violation precisely so an empty picker cannot be reported as fact by accident.
	out := make([]model.MarketingEmail, 0, len(emails))
	for _, e := range emails {
		out = append(out, model.MarketingEmail{
			ID:        e.ID,
			Name:      e.Name,
			Subject:   e.Subject,
			State:     e.State,
			UpdatedAt: e.UpdatedAt,
		})
	}
	return out, nil
}
