// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// More than one Display ad group, and audience exclusions (#475)
// ---------------------------------------------------------------------------
//
// A Display campaign used to get exactly one ad group, built from the campaign-level
// DisplayCreative and the four audience lists, and could not exclude anyone.
// CampaignInput.DisplayAdGroups lets a caller name several, each with its own responsive
// display ad and its own audience lists — the shape of a live Display campaign split into
// Retargeting and Prospecting. ExcludedAudienceSegments, on the campaign and on each named
// group, excludes user lists.
//
// What Google accepts here was established by validateOnly probes on 2026-11-03 (v23), and
// nothing beyond them is claimed:
//
//   - two DISPLAY_STANDARD ad groups under one Display campaign, each with its own audience
//     criteria, are accepted;
//   - a negative user list on a Display ad group, {adGroup, userList:{userList},
//     negative:true}, is accepted, and so is one on the campaign, {campaign,
//     userList:{userList}, negative:true}.
//
// Only USER LISTS are excluded: a negative user list is the exclusion the probes covered,
// so a custom audience, custom affinity or user interest named as an exclusion is refused
// rather than sent.
//
// The cascade walks ONE shape either way: without the list, the preflight builds a single
// group from the campaign-level fields, named, targeted and built exactly as before, so a
// caller that omits the list sends the same requests and gets the same result.

// maxDisplayAdGroups caps the list. A SERVICE bound, not Google's, for the reasons
// maxDemandGenAdGroups gives on the Demand Gen list (#460): every group's images are
// fetched and held in memory before the budget mutate, and every group adds sequential
// mutates after the campaign exists. Five covers the audience splits an event campaign uses
// (the reference one has two) with room to spare.
const maxDisplayAdGroups = 5

// DisplayAdGroup is one named Display ad group in CampaignInput.DisplayAdGroups.
//
// Geo, the other campaign criteria and the placement exclusions are NOT per group: Display
// attaches them at the campaign level, so they already cover every group.
type DisplayAdGroup struct {
	// Name labels the group. It is sanitized like every other name part and appended to the
	// campaign's ad group name ("<event> - Display Network | <Name>"), as the Search
	// ad-group list does; two names equal ignoring case are refused, because Google refuses
	// DUPLICATE_ADGROUP_NAME only after the earlier groups exist.
	Name string
	// Creative is this group's responsive display ad, under the rules of
	// CampaignInput.DisplayCreative. Required: a named group with no ad cannot serve, and
	// the list is new, so no caller depends on that shape.
	Creative DisplayCreative
	// AudienceSegments, CustomAudiences, CustomAffinities and UserInterests are this group's
	// audience lists, under the rules of the CampaignInput fields of the same names (#462):
	// existing resources on this campaign's own account, attached as targeting. Optional.
	AudienceSegments []string
	CustomAudiences  []string
	CustomAffinities []string
	UserInterests    []string
	// ExcludedAudienceSegments are user lists excluded from THIS group only, as negative
	// ad-group criteria. Optional. A list excluded from every group belongs in
	// CampaignInput.ExcludedAudienceSegments instead, and naming it in both is refused.
	ExcludedAudienceSegments []string
}

// displayGroupPlan is one validated Display ad group. targeting carries the group's four
// audience lists and its own exclusions; its excludedPlacements is always empty, because
// placements are excluded at the campaign level.
type displayGroupPlan struct {
	name      string
	targeting displayTargetingPlan
	creative  displayCreativePlan
}

// validateDisplayAdGroups resolves the groups the Display cascade creates. base is the
// single group the campaign-level fields describe; it is the whole answer when the list is
// absent. campaignExcluded is the validated campaign-level exclusion list. Pure, and run in
// preflightCampaignKind, so every refusal lands before the budget mutate.
func validateDisplayAdGroups(kind, customerID string, in CampaignInput, base displayGroupPlan, campaignExcluded []string) ([]displayGroupPlan, error) {
	if len(in.DisplayAdGroups) == 0 {
		if kind != campaignKindDisplay {
			return nil, nil
		}
		return []displayGroupPlan{base}, nil
	}
	if kind != campaignKindDisplay {
		return nil, fmt.Errorf("google-ads: Display ad groups are not supported on %s; omit DisplayAdGroups (Search takes its own ad-group list, AdGroups)", kind)
	}
	// The campaign-level creative and audience lists describe the ONE group the list
	// replaces; with a list there is no single group for them to go on, and guessing would put
	// an ad or an audience on a group the caller did not name.
	if !in.DisplayCreative.empty() || len(in.AudienceSegments) > 0 || len(in.CustomAudiences) > 0 ||
		len(in.CustomAffinities) > 0 || len(in.UserInterests) > 0 {
		return nil, fmt.Errorf("google-ads: DisplayAdGroups replaces the single ad group, so DisplayCreative, AudienceSegments, CustomAudiences, CustomAffinities and UserInterests must be empty; set the creative and audiences on each ad group instead (ExcludedAudienceSegments and ExcludedPlacements stay campaign-level and cover every group)")
	}
	if len(in.DisplayAdGroups) > maxDisplayAdGroups {
		return nil, fmt.Errorf("google-ads: %d Display ad groups requested, limit %d", len(in.DisplayAdGroups), maxDisplayAdGroups)
	}

	plans := make([]displayGroupPlan, 0, len(in.DisplayAdGroups))
	seen := make(map[string]struct{}, len(in.DisplayAdGroups))
	for i, g := range in.DisplayAdGroups {
		label := sanitizeNamePart(g.Name)
		if label == "" {
			return nil, fmt.Errorf("google-ads: Display ad group %d has no name; each ad group needs a distinct label", i+1)
		}
		name := base.name + " | " + label
		if err := validateEntityName("ad group", name, utf8.RuneCountInString(name), maxAdGroupNameRunes, "characters"); err != nil {
			// By position, and with the label named as the other thing to shorten: the
			// shared message speaks only of EventName and Project.
			return nil, fmt.Errorf("google-ads: Display ad group %d: %w (or shorten this group's name)", i+1, err)
		}
		key := strings.ToLower(name)
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("google-ads: Display ad group name %q is used more than once; Google refuses duplicate ad group names (DUPLICATE_ADGROUP_NAME)", name)
		}
		seen[key] = struct{}{}

		owner := fmt.Sprintf("Display ad group %q", name)
		plan := displayGroupPlan{name: name}
		// The same checks as the campaign-level lists: shape and THIS account's customer id.
		// Not a tenancy check — that is the accepted risk tracked by #468.
		var err error
		for _, l := range []struct {
			field, collection string
			names             []string
			dst               *[]string
		}{
			{"AudienceSegments", "userLists", g.AudienceSegments, &plan.targeting.userLists},
			{"CustomAudiences", "customAudiences", g.CustomAudiences, &plan.targeting.customAudiences},
			{"CustomAffinities", "customInterests", g.CustomAffinities, &plan.targeting.customAffinities},
			{"UserInterests", "userInterests", g.UserInterests, &plan.targeting.userInterests},
		} {
			if *l.dst, err = validateDisplayAudienceList(l.field, l.collection, customerID, l.names); err != nil {
				return nil, fmt.Errorf("google-ads: %s: %w", owner, err)
			}
		}
		if plan.targeting.excludedUserLists, err = validateDisplayExcludedUserLists("ExcludedAudienceSegments", customerID, g.ExcludedAudienceSegments); err != nil {
			return nil, fmt.Errorf("google-ads: %s: %w", owner, err)
		}
		if err := refuseExcludedOverlap(owner+" targets", plan.targeting.userLists, owner+" excludes", plan.targeting.excludedUserLists); err != nil {
			return nil, err
		}
		if err := refuseExcludedOverlap(owner+" targets", plan.targeting.userLists, "the campaign excludes", campaignExcluded); err != nil {
			return nil, err
		}
		// Excluding a list on the campaign AND on a group is refused rather than sent twice:
		// the campaign-level exclusion already covers the group, and the two levels together
		// were not probed.
		if err := refuseExcludedOverlap(owner+" excludes", plan.targeting.excludedUserLists, "the campaign excludes", campaignExcluded); err != nil {
			return nil, err
		}

		if g.Creative.empty() {
			return nil, fmt.Errorf("google-ads: %s has no creative; set its Creative, because an ad group with no ad cannot serve", owner)
		}
		groupIn := in
		groupIn.DisplayCreative = g.Creative
		if plan.creative, err = validateDisplayCreative(kind, groupIn); err != nil {
			return nil, fmt.Errorf("google-ads: %s: %w", owner, err)
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

// validateDisplayExcludedUserLists checks an exclusion list: user lists only, each under
// validateDisplayAudienceList's shape and account rules. Another segment kind is refused
// with its own reason before that validator runs, because its hint ("belongs in
// CustomAudiences") would send the caller to a TARGETING field.
func validateDisplayExcludedUserLists(field, customerID string, names []string) ([]string, error) {
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		parts := strings.Split(name, "/")
		if len(parts) == 4 && parts[0] == "customers" && parts[2] != "userLists" {
			return nil, fmt.Errorf("google-ads: %s entry %q is not a user list (it names the %s collection); only user lists (customers/{customerId}/userLists/{id}) can be excluded — a negative user list is the exclusion this client has verified", field, name, parts[2])
		}
	}
	return validateDisplayAudienceList(field, "userLists", customerID, names)
}

// refuseExcludedOverlap refuses a user list named on both sides. A list both targeted and
// excluded is a contradiction the caller did not mean, and how Google would resolve it was
// not probed; a list excluded at two levels is the same exclusion sent twice. Either way the
// caller is asked to name it once rather than having one side silently dropped.
func refuseExcludedOverlap(leftOwner string, left []string, rightOwner string, right []string) error {
	if len(left) == 0 || len(right) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(right))
	for _, n := range right {
		set[n] = struct{}{}
	}
	for _, n := range left {
		if _, ok := set[n]; ok {
			return fmt.Errorf("google-ads: user list %q is named twice — %s it and %s it; name it once", n, leftOwner, rightOwner)
		}
	}
	return nil
}

// fetchDisplayGroupImages fetches every group's images in ONE fetchSlotImages walk, so the
// whole campaign shares one wall-clock budget and one maxCreativeTotalImageBytes total rather
// than one per group — N groups must not multiply what one create may hold in memory or
// spend fetching. Runs before the budget mutate, like every image fetch.
//
// For the single group the walk is exactly the one this cascade made before #475: the same
// four slots and the same URLs.
func (c *Client) fetchDisplayGroupImages(ctx context.Context, groups []displayGroupPlan) ([][]fetchedImage, error) {
	var (
		slots  []imageSlot
		urls   [][]string
		owners []int
		bases  = make([]int, len(groups))
		// Every plan carries the same CampaignInput.ResolvedImages, so any present one's map
		// is the map.
		resolved map[string][]byte
	)
	for gi, g := range groups {
		if !g.creative.present {
			continue
		}
		resolved = g.creative.resolved
		bases[gi] = len(slots)
		for si, s := range displayImageSlots {
			slots = append(slots, s)
			urls = append(urls, g.creative.urls[si])
			owners = append(owners, gi)
		}
	}
	out := make([][]fetchedImage, len(groups))
	if len(slots) == 0 {
		return out, nil
	}
	fetched, err := c.fetchSlotImages(ctx, slots, urls, resolved)
	if err != nil {
		return nil, err
	}
	for _, img := range fetched {
		gi := owners[img.slot]
		// Re-indexed to the slot's position within ITS creative, which is what
		// createDisplayAd's per-slot bucketing reads.
		img.slot -= bases[gi]
		out[gi] = append(out[gi], img)
	}
	return out, nil
}

// displayGroupStep tags a step line with its ad group when the caller named several, so the
// step list says which group each line is about. Empty for the single group, whose step
// lines are unchanged.
func displayGroupStep(multi bool, name string) string {
	if !multi {
		return ""
	}
	return fmt.Sprintf(" [ad group %q]", name)
}

// adGroupNegativeUserListCreate and campaignNegativeUserListCreate are the payloads for a
// user-list EXCLUSION on adGroupCriteria:mutate and campaignCriteria:mutate, in the shapes
// the 2026-11-03 validateOnly probe accepted. Negative is sent explicitly (no omitempty), as
// on campaignNegativePlacementCreate: a user-list criterion with negative=false TARGETS the
// list, the opposite of what was asked. No status is sent, as on the negative placement and
// negative keyword payloads.
type adGroupNegativeUserListCreate struct {
	AdGroup  string        `json:"adGroup"`
	Negative bool          `json:"negative"`
	UserList *userListInfo `json:"userList"`
}

type campaignNegativeUserListCreate struct {
	Campaign string        `json:"campaign"`
	Negative bool          `json:"negative"`
	UserList *userListInfo `json:"userList"`
}

// createAdGroupAudienceExclusions excludes a named group's user lists as ONE
// adGroupCriteria:mutate of negative user-list criteria. One batch for the reason the
// placement exclusions are one: a half-applied exclusion list reads as in place while the
// group still serves to the lists that were dropped.
//
// Its own mutate after the group's audience targeting, so each failure sentence names only
// what it left missing. No targetingSetting is added for it: that setting decides whether
// POSITIVE audience criteria target or observe, and the group's own audiences already
// decide whether it is sent (displayAdGroupCreate). Called after the ad group exists, so a
// failure is a partial result; an ambiguous outcome is UNCONFIRMED.
func (c *Client) createAdGroupAudienceExclusions(ctx context.Context, adGroupResource, adGroupID string, userLists []string) ([]string, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("google-ads display ad group audience exclusions aborted before any request (context already done; ad group %s has no audience exclusions yet): %w", adGroupID, ctxErr)
	}
	ops := make([]mutateOperation, 0, len(userLists))
	for _, name := range userLists {
		ops = append(ops, mutateOperation{Create: adGroupNegativeUserListCreate{
			AdGroup: adGroupResource, Negative: true, UserList: &userListInfo{UserList: name},
		}})
	}
	resp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("adGroupCriteria:mutate"), mutateRequest{Operations: ops}, false)
	if err != nil {
		if createOutcomeAmbiguous(err) {
			return nil, fmt.Errorf("google-ads display ad group audience exclusions UNCONFIRMED (ad group %s; exclusions may exist — verify in Google Ads before retrying): %w", adGroupID, err)
		}
		return nil, fmt.Errorf("google-ads display ad group audience exclusions failed (ad group %s created; it has NO audience exclusions and, if enabled, would serve to the user lists it was meant to exclude): %w", adGroupID, err)
	}
	var mr mutateResponse
	if uErr := json.Unmarshal(resp, &mr); uErr != nil || len(mr.Results) != len(ops) {
		return nil, fmt.Errorf("google-ads display ad group audience exclusions UNCONFIRMED (ad group %s; 2xx with a malformed/short mutate response — exclusions may exist — verify in Google Ads before retrying)", adGroupID)
	}
	ids := make([]string, 0, len(ops))
	for i, r := range mr.Results {
		returnedAdGroupID, critID := c.adGroupCriterionID(r.ResourceName)
		if critID == "" || returnedAdGroupID == "" {
			return nil, fmt.Errorf("google-ads display ad group audience exclusions UNCONFIRMED (ad group %s; malformed/wrong-kind/wrong-account criterion resource name %q at index %d — verify in Google Ads before retrying)", adGroupID, r.ResourceName, i)
		}
		if returnedAdGroupID != adGroupID {
			return nil, fmt.Errorf("google-ads display ad group audience exclusions UNCONFIRMED (ad group %s; adGroupCriterion resource name %q reports a different ad group id %q — verify in Google Ads before retrying)", adGroupID, r.ResourceName, returnedAdGroupID)
		}
		ids = append(ids, critID)
	}
	return ids, nil
}

// createCampaignAudienceExclusions excludes the campaign-level user lists as ONE
// campaignCriteria:mutate of negative user-list criteria, covering every ad group,
// including one a human adds later. Its own mutate after the placement exclusions, for the
// reason createCampaignPlacementExclusions gives for keeping each criteria step apart, and
// like it before any ad, so a failure here leaves a paused campaign with no ad. A failure is
// a partial result; an ambiguous outcome is UNCONFIRMED.
func (c *Client) createCampaignAudienceExclusions(ctx context.Context, campaignResource, campaignID string, userLists []string) ([]string, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("google-ads campaign audience exclusions aborted before any request (context already done; campaign %s has no audience exclusions yet): %w", campaignID, ctxErr)
	}
	ops := make([]mutateOperation, 0, len(userLists))
	for _, name := range userLists {
		ops = append(ops, mutateOperation{Create: campaignNegativeUserListCreate{
			Campaign: campaignResource, Negative: true, UserList: &userListInfo{UserList: name},
		}})
	}
	resp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("campaignCriteria:mutate"), mutateRequest{Operations: ops}, false)
	if err != nil {
		if createOutcomeAmbiguous(err) {
			return nil, fmt.Errorf("google-ads campaign audience exclusions UNCONFIRMED (campaign %s; exclusions may exist — verify in Google Ads before retrying): %w", campaignID, err)
		}
		return nil, fmt.Errorf("google-ads campaign audience exclusions failed (campaign %s created; it has NO audience exclusions and, if enabled, would serve to the user lists it was meant to exclude): %w", campaignID, err)
	}
	var mr mutateResponse
	if uErr := json.Unmarshal(resp, &mr); uErr != nil || len(mr.Results) != len(ops) {
		return nil, fmt.Errorf("google-ads campaign audience exclusions UNCONFIRMED (campaign %s; 2xx with a malformed/short mutate response — exclusions may exist — verify in Google Ads before retrying)", campaignID)
	}
	ids := make([]string, 0, len(ops))
	for i, r := range mr.Results {
		returnedCampaignID, critID := c.campaignCriterionID(r.ResourceName)
		if critID == "" || returnedCampaignID == "" {
			return nil, fmt.Errorf("google-ads campaign audience exclusions UNCONFIRMED (campaign %s; malformed/wrong-kind/wrong-account criterion resource name %q at index %d — verify in Google Ads before retrying)", campaignID, r.ResourceName, i)
		}
		if returnedCampaignID != campaignID {
			return nil, fmt.Errorf("google-ads campaign audience exclusions UNCONFIRMED (campaign %s; campaignCriterion resource name %q reports a different campaign id %q — verify in Google Ads before retrying)", campaignID, r.ResourceName, returnedCampaignID)
		}
		ids = append(ids, critID)
	}
	return ids, nil
}
