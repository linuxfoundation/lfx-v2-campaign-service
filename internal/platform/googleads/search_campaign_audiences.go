// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

// Search user lists observed at the CAMPAIGN level (#513).
//
// The live KubeCon NA 2026 Search campaign (24188338792) observes its five user lists as
// campaign criteria (campaign_criterion.user_list), with the campaign's targeting_setting
// declaring AUDIENCE bid_only (read-only GAQL, 2026-11-06). AudienceSegments put the same
// lists on each ad group instead. Delivery is the same with one ad group, but an ad group
// added later (in Optimize, or by hand) gets none of them. CampaignAudienceSegments is the
// campaign-level form.
//
// What is sent:
//   - the Search campaigns:mutate create carries targetingSetting.targetRestrictions
//     [{targetingDimension: AUDIENCE, bidOnly: true}], so the lists observe and never
//     restrict delivery (campaignCreate.TargetingSetting);
//   - each list is one positive campaignCriterion.userList, in ONE campaignCriteria:mutate
//     after the language, schedule, device and demographic criteria, before any ad group.
//
// validateOnly probes, 2026-11-06 (v23, account 8666746580, nothing persisted) accepted: a
// budget plus a Search campaign create with that targetingSetting and two USER_LIST campaign
// criteria by temporary id; the same with an ad group whose own targetingSetting declares
// AGE_RANGE, GENDER and INCOME_RANGE bidOnly (and, separately, AUDIENCE too); and, on paused
// LFX Search campaign 24426212391, a campaign targetingSetting update with the criteria and
// such an ad group. They refused a custom audience as a Search campaign criterion
// (OPERATION_NOT_PERMITTED_FOR_CONTEXT), so only user lists are admitted.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// campaignUserListCreate is a positive campaign-level user-list criterion. Status is sent so
// the criterion is ENABLED explicitly, as the probe sent it.
type campaignUserListCreate struct {
	Campaign string        `json:"campaign"`
	Status   string        `json:"status"`
	UserList *userListInfo `json:"userList"`
}

// campaignAudienceTargetingSetting is the Search campaign's targetingSetting when it observes
// campaign-level user lists: AUDIENCE bidOnly, so the lists report and bid without narrowing
// delivery. Without it Google's default for the dimension is restrictive.
func campaignAudienceTargetingSetting() *targetingSetting {
	return &targetingSetting{TargetRestrictions: []targetRestriction{
		{TargetingDimension: "AUDIENCE", BidOnly: true},
	}}
}

// validateCampaignAudienceSegments checks CampaignInput.CampaignAudienceSegments and returns
// the lists to send. Search only. Each entry must be a user list,
// customers/<this account>/userLists/<id>, with canonical ids; at most maxAudienceSegments,
// deduplicated. A list is observed at one level only, so the field is refused beside
// AudienceSegments or any AdGroups entry's AudienceSegments.
//
// The connected account is shared by every foundation, so create-campaigns also requires
// global_marketing_ops on the project for this field (service.GoogleAdsMarketingOpsInputs).
func validateCampaignAudienceSegments(kind, customerID string, in CampaignInput) ([]string, error) {
	const field = "CampaignAudienceSegments"
	if len(in.CampaignAudienceSegments) == 0 {
		return nil, nil
	}
	if kind != campaignKindSearch {
		return nil, fmt.Errorf("google-ads: campaign-level audience segments are not supported on %s (only a Search campaign observes user lists at the campaign level); omit %s, or create a Search campaign", kind, field)
	}
	if len(in.AudienceSegments) > 0 {
		return nil, fmt.Errorf("google-ads: %s and AudienceSegments cannot both be set (a user list is observed at one level); keep the lists in one of them", field)
	}
	for i, g := range in.AdGroups {
		if len(g.AudienceSegments) > 0 {
			return nil, fmt.Errorf("google-ads: %s and AdGroups entry %d AudienceSegments cannot both be set (a user list is observed at one level); keep the lists in one of them", field, i+1)
		}
	}
	if len(in.CampaignAudienceSegments) > maxAudienceSegments {
		return nil, fmt.Errorf("google-ads: at most %d %s entries are supported, got %d", maxAudienceSegments, field, len(in.CampaignAudienceSegments))
	}
	seen := make(map[string]struct{}, len(in.CampaignAudienceSegments))
	out := make([]string, 0, len(in.CampaignAudienceSegments))
	for _, raw := range in.CampaignAudienceSegments {
		name := strings.TrimSpace(raw)
		if name == "" {
			return nil, fmt.Errorf("google-ads: %s entry must not be empty", field)
		}
		parts := strings.Split(name, "/")
		if len(parts) != 4 || parts[0] != "customers" || !numericID(parts[1]) || !numericID(parts[3]) {
			return nil, fmt.Errorf("google-ads: %s entry %q is not a customers/{customerId}/userLists/{id} user list resource name", field, capForError(name))
		}
		if parts[2] != "userLists" {
			return nil, fmt.Errorf("google-ads: %s entry %q is not a user list; a Search campaign observes user lists only, customers/{customerId}/userLists/{id} (Google refuses a custom audience as a Search campaign criterion)", field, capForError(name))
		}
		if parts[1] != customerID {
			return nil, fmt.Errorf("google-ads: %s entry %q is on customer %q, not this campaign's account %s; name a user list on the campaign's own account", field, capForError(name), parts[1], customerID)
		}
		if canonicalCampaignID(parts[3]) == "" {
			return nil, fmt.Errorf("google-ads: %s entry %q has the id %q, which is not the canonical base-10 spelling of a positive id (no leading zeros); write the id as Google returns it", field, capForError(name), parts[3])
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out, nil
}

// createCampaignAudienceObservation attaches the campaign-level observed user lists as ONE
// campaignCriteria:mutate of positive user-list criteria. Its own mutate, after the other
// campaign criteria, so each failure sentence names only what it left missing. Called after
// the campaign exists, so a failure is a partial result; an ambiguous outcome is UNCONFIRMED.
func (c *Client) createCampaignAudienceObservation(ctx context.Context, campaignResource, campaignID string, userLists []string) ([]string, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("google-ads campaign audience observation aborted before any request (context already done; campaign %s observes no user lists yet): %w", campaignID, ctxErr)
	}
	ops := make([]mutateOperation, 0, len(userLists))
	for _, name := range userLists {
		ops = append(ops, mutateOperation{Create: campaignUserListCreate{
			Campaign: campaignResource, Status: "ENABLED", UserList: &userListInfo{UserList: name},
		}})
	}
	resp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("campaignCriteria:mutate"), mutateRequest{Operations: ops}, false)
	if err != nil {
		if createOutcomeAmbiguous(err) {
			return nil, fmt.Errorf("google-ads campaign audience observation UNCONFIRMED (campaign %s; user-list criteria may exist — verify in Google Ads before retrying): %w", campaignID, err)
		}
		return nil, fmt.Errorf("google-ads campaign audience observation failed (campaign %s created; it observes NONE of its %d user list(s), which only report and never restrict, so it would serve to the same people without those reports): %w", campaignID, len(userLists), err)
	}
	var mr mutateResponse
	if uErr := json.Unmarshal(resp, &mr); uErr != nil || len(mr.Results) != len(ops) {
		return nil, fmt.Errorf("google-ads campaign audience observation UNCONFIRMED (campaign %s; 2xx with a malformed/short mutate response — user-list criteria may exist — verify in Google Ads before retrying)", campaignID)
	}
	ids := make([]string, 0, len(ops))
	seen := newResultIDSet(len(mr.Results))
	for i, r := range mr.Results {
		returnedCampaignID, critID := c.campaignCriterionID(r.ResourceName)
		if critID == "" || returnedCampaignID == "" {
			return nil, fmt.Errorf("google-ads campaign audience observation UNCONFIRMED (campaign %s; malformed/wrong-kind/wrong-account criterion resource name %q at index %d — verify in Google Ads before retrying)", campaignID, r.ResourceName, i)
		}
		if returnedCampaignID != campaignID {
			return nil, fmt.Errorf("google-ads campaign audience observation UNCONFIRMED (campaign %s; campaignCriterion resource name %q reports a different campaign id %q — verify in Google Ads before retrying)", campaignID, r.ResourceName, returnedCampaignID)
		}
		if dup := seen.repeat(critID, i); dup != "" {
			return nil, fmt.Errorf("google-ads campaign audience observation UNCONFIRMED "+
				"(campaign %s; %s — verify in Google Ads before retrying)", campaignID, dup)
		}
		ids = append(ids, critID)
	}
	return ids, nil
}
