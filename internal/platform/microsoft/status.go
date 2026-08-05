// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Campaign run states for UpdateCampaignAndChildrenStatus. Microsoft's Campaign/AdGroup/Ad
// Status enum uses "Active"/"Paused" (Title-case, distinct from the lower-case
// model.CampaignRunActive/CampaignRunPaused the dispatcher maps from).
const (
	StatusActive = "Active"
	StatusPaused = "Paused"
)

// msStatusUpdate is one entity's PUT body: only the Id and Status are sent. The v13
// Update* operations treat an omitted field as "leave unchanged" (unlike Add*, which
// requires every field the entity type needs) — matches how findOrCreateAdGroup/
// findOrCreateResponsiveSearchAd already only ever set the fields being changed.
type msStatusUpdate struct {
	Id     json.Number `json:"Id"`
	Status string      `json:"Status"`
}

type updateCampaignsRequest struct {
	AccountId json.Number      `json:"AccountId"`
	Campaigns []msStatusUpdate `json:"Campaigns"`
}

type updateAdGroupsRequest struct {
	CampaignId json.Number      `json:"CampaignId"`
	AdGroups   []msStatusUpdate `json:"AdGroups"`
}

type updateAdsRequest struct {
	AdGroupId json.Number      `json:"AdGroupId"`
	Ads       []msStatusUpdate `json:"Ads"`
}

// updateResponse is the (subset of the) 200 response shared by UpdateCampaigns/
// UpdateAdGroups/UpdateAds: a per-entity failure is a PartialErrors entry on an
// otherwise-200 response, mirroring the Add* create response this client already
// decodes (see createCampaignsResponse).
type updateResponse struct {
	PartialErrors boundedErrorItems `json:"PartialErrors"`
}

// UpdateCampaignAndChildrenStatus sets Status on the campaign and, when their ids are
// supplied, its child ad group and ad — the platform side of the campaign status toggle.
// CreateCampaign PAUSES all three entities in the hierarchy (campaign, ad group,
// responsive search ad), so toggling only the campaign to Active would leave the ad
// group/ad Paused and the campaign would not serve; cascading keeps the run state
// consistent across the tree.
//
// Ordering is STATUS-DEPENDENT so a PARTIAL cascade never leaves paid delivery running
// unattended (mirrors the Reddit/Meta/LinkedIn fail-closed ordering):
//   - ACTIVATE: lift the CHILDREN first (ad, then ad group) while the campaign is still
//     Paused and gates them, then flip the CAMPAIGN Active LAST. A child failure here
//     happens before the gate opens, so nothing is serving yet — a plain error.
//   - PAUSE: flip the CAMPAIGN (the gate) FIRST so delivery stops immediately, then the
//     children. A child failure after the gate closed still leaves delivery stopped, but
//     the tree is partially applied — reported as a partialCascadeError so the caller
//     does not persist a run state the whole tree does not yet reflect.
//
// An empty adGroupID/adID is skipped (a degraded/partial create that stored no child id
// — already blocked from toggling by the dispatcher's ToggleStatus guard).
func (c *Client) UpdateCampaignAndChildrenStatus(ctx context.Context, campaignID, adGroupID, adID, status string) error {
	if status != StatusActive && status != StatusPaused {
		return fmt.Errorf("microsoft-ads: status must be %q or %q, got %q", StatusActive, StatusPaused, status)
	}
	campaignID = strings.TrimSpace(campaignID)
	if campaignID == "" {
		return fmt.Errorf("microsoft-ads: campaign id is required")
	}
	adGroupID = strings.TrimSpace(adGroupID)
	adID = strings.TrimSpace(adID)

	if status == StatusActive && (adGroupID == "" || adID == "") {
		return fmt.Errorf("microsoft-ads: cannot activate campaign %s: its ad group and ad ids must both be known, so the tree cannot be made servable", campaignID)
	}

	if status == StatusActive {
		if err := c.updateAdStatus(ctx, adGroupID, adID, status); err != nil {
			return err
		}
		if err := c.updateAdGroupStatus(ctx, campaignID, adGroupID, status); err != nil {
			return err
		}
		return c.updateCampaignStatus(ctx, campaignID, status)
	}

	// PAUSE: campaign (gate) FIRST so delivery stops immediately, then the children.
	if err := c.updateCampaignStatus(ctx, campaignID, status); err != nil {
		return err
	}
	if adGroupID != "" {
		if err := c.updateAdGroupStatus(ctx, campaignID, adGroupID, status); err != nil {
			return &partialCascadeError{stage: "ad group", err: err}
		}
	}
	if adID != "" {
		if err := c.updateAdStatus(ctx, adGroupID, adID, status); err != nil {
			return &partialCascadeError{stage: "ad", err: err}
		}
	}
	return nil
}

// partialCascadeError marks a cascade that changed the campaign upstream but then failed
// on a child entity: the run state is PARTIALLY applied. Its Unconfirmed() reports true
// so the caller (via the shared IsOutcomeUnconfirmed classifier) treats it as "may be
// applied — verify before retrying" rather than "not modified"; a retry re-runs the
// idempotent cascade. Mirrors the reddit client's partialCascadeError.
type partialCascadeError struct {
	stage string
	err   error
}

func (e *partialCascadeError) Error() string {
	return "microsoft-ads: campaign status changed but the " + e.stage + " update failed (partially applied): " + e.err.Error()
}
func (e *partialCascadeError) Unwrap() error     { return e.err }
func (e *partialCascadeError) Unconfirmed() bool { return true }

// IsOutcomeUnconfirmed reports whether err represents a status change whose platform
// outcome cannot be confirmed absent (a definite rejection) or applied (a confirmed
// success) — either a transport/5xx/3xx-mutating ambiguity from createOutcomeAmbiguous,
// or a partialCascadeError. Mirrors the reddit client's exported classifier so dispatch
// can report "verify before retry" instead of a flat failure.
func IsOutcomeUnconfirmed(err error) bool {
	if err == nil {
		return false
	}
	var pc *partialCascadeError
	if errors.As(err, &pc) {
		return pc.Unconfirmed()
	}
	return createOutcomeAmbiguous(err)
}

func (c *Client) updateCampaignStatus(ctx context.Context, campaignID, status string) error {
	id, err := parseEntityID("campaign", campaignID)
	if err != nil {
		return err
	}
	req := updateCampaignsRequest{
		AccountId: json.Number(c.account.AccountID),
		Campaigns: []msStatusUpdate{{Id: id, Status: status}},
	}
	return c.putEntityStatus(ctx, "Campaigns", req, "campaign", campaignID, status)
}

func (c *Client) updateAdGroupStatus(ctx context.Context, campaignID, adGroupID, status string) error {
	cid, err := parseEntityID("campaign", campaignID)
	if err != nil {
		return err
	}
	id, err := parseEntityID("ad group", adGroupID)
	if err != nil {
		return err
	}
	req := updateAdGroupsRequest{
		CampaignId: cid,
		AdGroups:   []msStatusUpdate{{Id: id, Status: status}},
	}
	return c.putEntityStatus(ctx, "AdGroups", req, "ad group", adGroupID, status)
}

func (c *Client) updateAdStatus(ctx context.Context, adGroupID, adID, status string) error {
	gid, err := parseEntityID("ad group", adGroupID)
	if err != nil {
		return err
	}
	id, err := parseEntityID("ad", adID)
	if err != nil {
		return err
	}
	req := updateAdsRequest{
		AdGroupId: gid,
		Ads:       []msStatusUpdate{{Id: id, Status: status}},
	}
	return c.putEntityStatus(ctx, "Ads", req, "ad", adID, status)
}

// putEntityStatus issues the PUT and decodes the shared PartialErrors envelope. A
// PartialErrors entry on an otherwise-200 response is a definite per-entity rejection
// (mirrors the Add* create response this client already decodes), NOT retried.
func (c *Client) putEntityStatus(ctx context.Context, path string, req any, entity, id, status string) error {
	respBody, err := c.doRequest(ctx, http.MethodPut, path, req, false)
	if err != nil {
		return fmt.Errorf("microsoft-ads: update %s %s status to %s: %w", entity, id, status, err)
	}
	var resp updateResponse
	if len(respBody) > 0 {
		if uerr := json.Unmarshal(respBody, &resp); uerr != nil {
			// The status may have committed (2xx) but the confirming body is unreadable —
			// ambiguous, not a clean failure.
			return &transportError{Method: http.MethodPut, Path: path, err: fmt.Errorf("decode update response: %w", uerr)}
		}
	}
	if partialErrorsHaveAny(resp.PartialErrors) {
		return fmt.Errorf("microsoft-ads: update %s %s status to %s rejected: %s", entity, id, status, partialErrorCodes(resp.PartialErrors))
	}
	return nil
}

// parseEntityID converts a caller-supplied id string to the json.Number the v13 REST
// body expects. Microsoft entity ids are int64 on the wire; a non-numeric string here
// means the persisted CampaignResult blob is corrupt, not a valid retryable failure.
func parseEntityID(kind, id string) (json.Number, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", fmt.Errorf("microsoft-ads: %s id is required", kind)
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return "", fmt.Errorf("microsoft-ads: invalid %s id %q: must be numeric", kind, id)
		}
	}
	return json.Number(id), nil
}
