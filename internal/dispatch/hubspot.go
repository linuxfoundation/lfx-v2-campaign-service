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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
)

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

// Dispatch implements service.PlatformDispatcher for the HubSpot email channel. It clones the
// caller's template email and sets its send list to the brief's built audience. The returned
// campaign's PlatformCampaignID is the cloned email's HubSpot id.
func (d *HubSpotDispatcher) Dispatch(ctx context.Context, brief *model.CampaignBrief, platform model.Provider, config json.RawMessage) (*model.Campaign, error) {
	// Resolve creds FIRST (pre-create): a missing/undecryptable connection is a not-created
	// error → the orchestrator releases the claim.
	res, err := d.creds.resolve(ctx, brief.ProjectID, platform)
	if err != nil {
		return nil, err // already a preCreateError
	}
	if res.status != model.StatusActive {
		return nil, notCreated(fmt.Errorf("hubspot connection for project %s is %s, not active", brief.ProjectID, res.status))
	}

	var creds hubspotCreds
	if err := json.Unmarshal(res.plaintext, &creds); err != nil {
		return nil, notCreated(fmt.Errorf("decode hubspot credentials: %w", err))
	}
	if strings.TrimSpace(creds.PrivateAppToken) == "" {
		return nil, notCreated(fmt.Errorf("hubspot credentials are incomplete (need privateAppToken)"))
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

	client := hubspot.NewClient(
		hubspot.Credentials{PrivateAppToken: creds.PrivateAppToken},
		hubspot.AccountConfig{PortalID: res.providerConfig["portal_id"]},
		d.opts...,
	)

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
		camp := campaignFromHubSpot(ctx, email, cfg)
		return camp, fmt.Errorf("hubspot email %s cloned but setting its send list failed (verify before retrying): %w", email.ID, serr)
	}

	return campaignFromHubSpot(ctx, email, cfg), nil
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
func campaignFromHubSpot(ctx context.Context, e *hubspot.Email, cfg hubspotConfig) *model.Campaign {
	c := &model.Campaign{
		PlatformCampaignID: e.ID,
		CampaignName:       e.Name,
		Status:             campaignStatusCreated,
	}
	// The email channel has no numeric budget/schedule config; ConfigSnapshot still captures the
	// validated hubspotConfig (the cloned template id) for provenance/reconcile.
	applyCampaignConfig(ctx, c, 0, false, "", "", cfg)
	if raw, err := json.Marshal(e); err != nil {
		slog.WarnContext(ctx, "failed to marshal hubspot email result blob (Result left empty)",
			"campaign_id", c.PlatformCampaignID, "error", err)
	} else {
		c.Result = raw
	}
	return c
}
