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
// Display audiences and placement exclusions (#462).
//
// A Display campaign this client created could say where it serves and when, and
// nothing about WHO: user lists were refused on the channel, custom segments and
// Google's interest categories had no input at all, and there was no way to keep the
// ads off a site. Those are the targeting a human sets by hand on a Display campaign,
// and the reference KubeCon Display campaign uses all of them.
//
// What Google accepts here was established by validateOnly probes on 2026-11-03 (v23),
// and nothing beyond them is claimed:
//
//   - a DISPLAY_STANDARD ad group accepts userList, customAudience, customAffinity and
//     userInterest criteria, each alone and all together;
//   - it REFUSES an `audience` criterion with
//     AUDIENCE_NOT_ALLOWED_TO_ATTACH_WHEN_AUDIENCE_GROUPED_SET_TO_FALSE — Display is not
//     audience-grouped, so it takes segments directly rather than an Audience resource;
//   - a negative `placement` criterion naming a bare domain is accepted at the campaign
//     level (and at the ad group level, which this client does not use);
//   - a DISPLAY_STANDARD ad group created with targetingSetting AGE_RANGE, GENDER,
//     PARENTAL_STATUS and INCOME_RANGE bidOnly true plus AUDIENCE bidOnly false,
//     together with those four criteria, is accepted.
//
// This client creates no audiences. Every audience input is a list of resource names
// the caller already has, checked for shape and for THIS account before the budget
// mutate. A criterion naming another account's list is not expected to be accepted
// (not probed); any refusal would come at the adGroupCriteria:mutate, after the budget,
// the campaign and the ad group are committed.
// ---------------------------------------------------------------------------

// maxExcludedPlacements bounds the placement exclusion list so one
// campaignCriteria:mutate stays a sane size. It is a broker sanity cap, not a Google
// limit, and is sized well above a hand-kept blocklist: a cap the caller's own list
// routinely exceeds refuses a create Google would accept.
const maxExcludedPlacements = 500

// maxPlacementDomainBytes is the DNS limit on a domain name's text form. A longer
// string is not a domain, so it is refused here rather than at the mutate.
const maxPlacementDomainBytes = 253

// displayAudienceCollections maps each audience resource collection this client
// accepts on Display to the CampaignInput field it belongs in. It drives the hint a
// caller gets for a well-formed name in the wrong list — a user interest pasted into
// CustomAudiences is a mistake about the field, not about the name, and the error
// should say which field.
var displayAudienceCollections = map[string]string{
	"userLists":       "AudienceSegments",
	"customAudiences": "CustomAudiences",
	"customInterests": "CustomAffinities",
	"userInterests":   "UserInterests",
}

// customAudienceInfo, customAffinityInfo and userInterestInfo are the criterion
// payloads for the three segment kinds Display adds to the user list. The field names
// are Google's: a custom affinity criterion is `customAffinity` naming a
// customInterests resource, and a user interest criterion carries
// `userInterestCategory`, not `userInterest`.
type customAudienceInfo struct {
	CustomAudience string `json:"customAudience"`
}

type customAffinityInfo struct {
	CustomAffinity string `json:"customAffinity"`
}

type userInterestInfo struct {
	UserInterestCategory string `json:"userInterestCategory"`
}

// displayAudienceCriterionCreate is the create payload for a Display ad group's
// audience criteria. Exactly one of the four arms is set per operation.
//
// It is a separate type from adGroupCriterionCreate (targeting.go) for the reason
// geo.go gives for its own payloads: one type per criterion shape. That type is the
// Search ad group's, carries a keyword arm a Display audience operation must never
// emit, and deliberately has no customAudience field because Search cannot take one.
type displayAudienceCriterionCreate struct {
	AdGroup        string              `json:"adGroup"`
	Status         string              `json:"status"`
	UserList       *userListInfo       `json:"userList,omitempty"`
	CustomAudience *customAudienceInfo `json:"customAudience,omitempty"`
	CustomAffinity *customAffinityInfo `json:"customAffinity,omitempty"`
	UserInterest   *userInterestInfo   `json:"userInterest,omitempty"`
}

// placementInfo is the "placement" criterion payload: a website, by domain.
type placementInfo struct {
	URL string `json:"url"`
}

// campaignNegativePlacementCreate is the create payload for a placement EXCLUSION on
// campaignCriteria:mutate. Negative is sent explicitly (no omitempty), as on
// campaignNegativeKeywordCreate: a placement criterion with negative=false TARGETS the
// site, which is the opposite of what the caller asked, so false must be impossible to
// reach by omission.
type campaignNegativePlacementCreate struct {
	Campaign  string         `json:"campaign"`
	Negative  bool           `json:"negative"`
	Placement *placementInfo `json:"placement"`
}

// displayTargetingPlan is the validated Display audience and placement intent: the
// four segment lists, each already shape- and account-checked and deduplicated, and
// the placement exclusions as the bare domains that will be sent.
type displayTargetingPlan struct {
	userLists          []string
	customAudiences    []string
	customAffinities   []string
	userInterests      []string
	excludedPlacements []string
	// excludedUserLists are the user lists excluded at THIS plan's level (#475): the
	// campaign's on the campaign-level plan, a named ad group's on that group's plan. See
	// display_adgroups.go.
	excludedUserLists []string
}

// audienceCount is how many ad-group audience criteria the plan creates.
func (p displayTargetingPlan) audienceCount() int {
	return len(p.userLists) + len(p.customAudiences) + len(p.customAffinities) + len(p.userInterests)
}

// validateDisplayTargeting resolves the Display audience and placement inputs for the
// given campaign KIND. Called from preflightCampaignKind, so it fails before the budget
// mutate and the adoption path refuses exactly what the create path would.
//
// Off Display the three segment lists Display adds, the placement exclusions and the
// audience exclusions (#475) are REFUSED, never dropped: no other cascade reads them, so
// accepting one would validate it and then create a campaign without it. AudienceSegments
// is not judged here off
// Display — the Search cascade owns it there, and the preflight's own fence refuses it
// on the other channels.
//
// On Display AudienceSegments IS judged here rather than by validateAudienceSegments,
// for two reasons: that validator's messages speak for Search ("custom audiences are
// not supported for SEARCH campaigns"), which is false on this channel; and it does not
// check the account, which every Display list must be — see validateDisplayAudienceList.
func validateDisplayTargeting(kind, customerID string, in CampaignInput) (displayTargetingPlan, error) {
	if kind != campaignKindDisplay {
		for _, f := range []struct {
			field string
			n     int
		}{
			{"CustomAudiences", len(in.CustomAudiences)},
			{"CustomAffinities", len(in.CustomAffinities)},
			{"UserInterests", len(in.UserInterests)},
		} {
			if f.n > 0 {
				return displayTargetingPlan{}, fmt.Errorf("google-ads: %s are not supported on %s (this client attaches custom audiences, custom affinities and user interests only on the Display cascade); omit %s, or create a Display campaign to target them", f.field, kind, f.field)
			}
		}
		if len(in.ExcludedPlacements) > 0 {
			return displayTargetingPlan{}, fmt.Errorf("google-ads: placement exclusions are not supported on %s (this client attaches placement criteria only on the Display cascade); omit ExcludedPlacements, or create a Display campaign to keep its ads off a site", kind)
		}
		if len(in.ExcludedAudienceSegments) > 0 {
			return displayTargetingPlan{}, fmt.Errorf("google-ads: audience exclusions are not supported on %s (this client attaches negative user lists only on the Display cascade); omit ExcludedAudienceSegments, or create a Display campaign to exclude a user list", kind)
		}
		return displayTargetingPlan{}, nil
	}

	var plan displayTargetingPlan
	var err error
	if plan.userLists, err = validateDisplayAudienceList("AudienceSegments", "userLists", customerID, in.AudienceSegments); err != nil {
		return displayTargetingPlan{}, err
	}
	if plan.customAudiences, err = validateDisplayAudienceList("CustomAudiences", "customAudiences", customerID, in.CustomAudiences); err != nil {
		return displayTargetingPlan{}, err
	}
	if plan.customAffinities, err = validateDisplayAudienceList("CustomAffinities", "customInterests", customerID, in.CustomAffinities); err != nil {
		return displayTargetingPlan{}, err
	}
	if plan.userInterests, err = validateDisplayAudienceList("UserInterests", "userInterests", customerID, in.UserInterests); err != nil {
		return displayTargetingPlan{}, err
	}
	if plan.excludedPlacements, err = validateExcludedPlacements(in.ExcludedPlacements); err != nil {
		return displayTargetingPlan{}, err
	}
	// The campaign-level user-list exclusions (#475), under the same account rule. A list
	// both targeted here and excluded is refused; a named ad group's lists are checked
	// against these in validateDisplayAdGroups.
	if plan.excludedUserLists, err = validateDisplayExcludedUserLists("ExcludedAudienceSegments", customerID, in.ExcludedAudienceSegments); err != nil {
		return displayTargetingPlan{}, err
	}
	if err := refuseExcludedOverlap("the ad group targets", plan.userLists, "the campaign excludes", plan.excludedUserLists); err != nil {
		return displayTargetingPlan{}, err
	}
	return plan, nil
}

// validateDisplayAudienceList trims, checks and deduplicates one Display audience list.
// Every entry must be exactly customers/{customerId}/{collection}/{id}, both ids
// numeric, and the customer id must be THIS client's account.
//
// The account check is the one validateAudienceSegments does not make. The probes
// established these names on the campaign's own customer; a criterion naming another
// customer's list is not expected to be accepted (not probed), and any refusal would
// come at the adGroupCriteria:mutate, after the budget, the campaign and the ad group
// exist. A resource on another account is refused here, with the account it named.
//
// The connected Google Ads account (same customer id) is the boundary this check enforces.
// That account is shared by every foundation, so create-campaigns also requires
// global_marketing_ops on the project for any account-owned segment
// (service.GoogleAdsMarketingOpsInputs). The dispatch adapter keeps the
// account-owned kinds (user lists, custom audiences, custom affinities) behind a
// per-deployment opt-in that is off by default (GOOGLE_ADS_DISPLAY_AUDIENCES_ENABLED, see
// dispatch.googleAdsDisplayAudiencesEnabled) — the user-list exclusions and every
// DisplayAdGroups entry's lists included (#475). User interests are Google's own
// categories, not account resources, and are not gated.
//
// An Audience resource (customers/{id}/audiences/{id}) is refused with its own reason:
// Display takes segments directly, and Google refuses an audience criterion on a
// Display ad group (AUDIENCE_NOT_ALLOWED_TO_ATTACH_WHEN_AUDIENCE_GROUPED_SET_TO_FALSE,
// verified by a validateOnly probe on 2026-11-03).
//
// Each list is capped at maxAudienceSegments, the cap Search's list already has.
func validateDisplayAudienceList(field, collection, customerID string, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	if len(names) > maxAudienceSegments {
		return nil, fmt.Errorf("google-ads: at most %d %s entries are supported, got %d", maxAudienceSegments, field, len(names))
	}
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			return nil, fmt.Errorf("google-ads: %s entry must not be empty", field)
		}
		parts := strings.Split(name, "/")
		if len(parts) != 4 || parts[0] != "customers" || !numericID(parts[1]) || !numericID(parts[3]) {
			return nil, fmt.Errorf("google-ads: %s entry %q is not a customers/{customerId}/%s/{id} resource name", field, name, collection)
		}
		if parts[2] == "audiences" {
			return nil, fmt.Errorf("google-ads: %s entry %q is an Audience resource, which a Display ad group refuses (it is not audience-grouped and takes segments directly); name the segments themselves — user lists in AudienceSegments, custom audiences in CustomAudiences, custom affinities in CustomAffinities, user interests in UserInterests", field, name)
		}
		if parts[2] != collection {
			if owner, ok := displayAudienceCollections[parts[2]]; ok {
				return nil, fmt.Errorf("google-ads: %s entry %q is a %s resource name; on Display it belongs in %s", field, name, parts[2], owner)
			}
			return nil, fmt.Errorf("google-ads: %s entry %q is not a customers/{customerId}/%s/{id} resource name", field, name, collection)
		}
		if parts[1] != customerID {
			return nil, fmt.Errorf("google-ads: %s entry %q is on customer %q, not this campaign's account %q; name a resource on the campaign's own account", field, name, parts[1], customerID)
		}
		// Digits alone are not the id test: "userLists/011" and "userLists/11" would pass
		// as two names for one list, slipping past this de-dupe and refuseExcludedOverlap.
		// The customer id needs no such check: it must equal customerID exactly, above.
		if canonicalCampaignID(parts[3]) == "" {
			return nil, fmt.Errorf("google-ads: %s entry %q has the id %q, which is not the "+
				"canonical base-10 spelling of a positive id (no leading zeros); write the id "+
				"as Google returns it", field, name, parts[3])
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out, nil
}

// validateExcludedPlacements checks the placement exclusion list and returns the
// domains that will be sent.
//
// An entry must be a BARE DOMAIN — example.com, news.example.com — which is the shape
// the 2026-11-03 validateOnly probe established. Anything else is REFUSED rather than
// rewritten, so what reaches Google is exactly what the caller wrote, lower-cased:
//
//   - a scheme (https://example.com) is refused, not stripped;
//   - a path, query, fragment, port or credentials are refused — a site SECTION is a
//     placement Google may accept, but it is not what was probed, and an entry that
//     fails at the campaignCriteria:mutate does so after the campaign exists;
//   - a wildcard, a trailing dot, an underscore or a non-ASCII name is refused (an
//     internationalised domain is written in its xn-- punycode form);
//   - an IPv4 literal (a name whose last label is all digits) is refused: it is not a
//     domain, and no top-level domain is numeric.
//
// "www.example.com" and "example.com" are both sent as written; neither is folded into
// the other (whether Google's exclusion of one covers the other is Google's rule, not
// checked here). The only folding is case, because a domain is case-insensitive and two
// spellings would be one placement sent twice.
//
// The two shape refusals render the entry through redactURLForError: an entry carrying
// a query or credentials may carry a secret, and the error text reaches logs.
func validateExcludedPlacements(placements []string) ([]string, error) {
	if len(placements) == 0 {
		return nil, nil
	}
	if len(placements) > maxExcludedPlacements {
		return nil, fmt.Errorf("google-ads: at most %d excluded placements are supported, got %d", maxExcludedPlacements, len(placements))
	}
	seen := make(map[string]struct{}, len(placements))
	out := make([]string, 0, len(placements))
	for _, raw := range placements {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			return nil, fmt.Errorf("google-ads: excluded placement must not be empty")
		}
		if strings.Contains(entry, "://") {
			return nil, fmt.Errorf("google-ads: excluded placement %q carries a scheme; send the bare domain (example.com) — it is not stripped for you", redactURLForError(entry))
		}
		if strings.ContainsAny(entry, "/?#:@") {
			return nil, fmt.Errorf("google-ads: excluded placement %q must be a bare domain such as example.com, with no path, query, fragment, port or credentials", redactURLForError(entry))
		}
		// ASCII is checked on the entry as sent, before case is folded: strings.ToLower maps
		// some non-ASCII runes to ASCII (KELVIN SIGN U+212A becomes "k"), which would turn the
		// caller's entry into a different domain instead of refusing it.
		for i := 0; i < len(entry); i++ {
			if entry[i] >= utf8.RuneSelf {
				return nil, fmt.Errorf("google-ads: excluded placement %q is not ASCII; write an internationalised domain in its xn-- form", entry)
			}
		}
		domain := strings.ToLower(entry)
		if !placementDomain(domain) {
			return nil, fmt.Errorf("google-ads: excluded placement %q is not a domain name (letters, digits and hyphens in dot-separated labels, at least two labels, a non-numeric last label, no trailing dot; write an internationalised domain in its xn-- form)", entry)
		}
		if _, dup := seen[domain]; dup {
			continue
		}
		seen[domain] = struct{}{}
		out = append(out, domain)
	}
	return out, nil
}

// placementDomain reports whether s, already lower-cased, is a syntactically valid
// domain name of at least two labels: each label 1..63 bytes of [a-z0-9-], neither
// starting nor ending with a hyphen, the last not all digits, the whole at most
// maxPlacementDomainBytes.
func placementDomain(s string) bool {
	if len(s) > maxPlacementDomainBytes {
		return false
	}
	labels := strings.Split(s, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			ch := label[i]
			if (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') && ch != '-' {
				return false
			}
		}
	}
	// An all-digit last label makes the name an IPv4 literal, not a domain.
	return !numericID(labels[len(labels)-1])
}

// displayAudienceStep summarises the audience criteria for the step list, naming only
// the kinds that were asked for.
func displayAudienceStep(plan displayTargetingPlan) string {
	parts := make([]string, 0, 4)
	if n := len(plan.userLists); n > 0 {
		parts = append(parts, fmt.Sprintf("%d user list(s)", n))
	}
	if n := len(plan.customAudiences); n > 0 {
		parts = append(parts, fmt.Sprintf("%d custom audience(s)", n))
	}
	if n := len(plan.customAffinities); n > 0 {
		parts = append(parts, fmt.Sprintf("%d custom affinity segment(s)", n))
	}
	if n := len(plan.userInterests); n > 0 {
		parts = append(parts, fmt.Sprintf("%d user interest(s)", n))
	}
	return strings.Join(parts, "; ")
}

// displayAdGroupTargetingSetting is the Display ad group's targetingSetting when it carries
// audience criteria, or nil when it does not, so an ad group without them is the pre-#462
// payload exactly. See createDisplayAudienceCriteria for why it is sent.
//
// The four demographic dimensions exactly as Google sets them by default, then AUDIENCE with
// bidOnly false, so the audience criteria target rather than observe. All five entries, in this
// order, rather than AUDIENCE alone: LFX-created Display ad groups (no targetingSetting sent)
// read back exactly AGE_RANGE, GENDER, PARENTAL_STATUS and INCOME_RANGE, each bidOnly true, and
// no AUDIENCE entry. Restating those four keeps Google's default for the demographic dimensions
// instead of relying on how Google merges a partial list. This exact list, with the four
// criteria, was accepted by a validateOnly probe on 2026-11-03.
//
// The demographic dimensions are NEVER sent bidOnly false, whatever DisplayDemographics holds:
// Google refuses that (TARGETING_SETTING_DEMOGRAPHIC_CRITERION_TYPE_GROUPS_MUST_BE_SET_TO_TARGET_ALL,
// validateOnly probe 2026-11-05). Demographic targeting narrows through negative criteria
// instead; see adGroupDemographicTargetingPlan.
func displayAdGroupTargetingSetting(audiences bool) *targetingSetting {
	if !audiences {
		return nil
	}
	return &targetingSetting{TargetRestrictions: []targetRestriction{
		{TargetingDimension: targetingDimensionAgeRange, BidOnly: true},
		{TargetingDimension: targetingDimensionGender, BidOnly: true},
		{TargetingDimension: targetingDimensionParentalStatus, BidOnly: true},
		{TargetingDimension: targetingDimensionIncomeRange, BidOnly: true},
		{TargetingDimension: "AUDIENCE", BidOnly: false},
	}}
}

// createDisplayAudienceCriteria attaches the plan's audience segments to a
// just-created Display ad group as ONE adGroupCriteria:mutate, in the order user
// lists, custom audiences, custom affinities, user interests. One batch so the set
// shares one atomic outcome: an ad group that got its user lists and not its interest
// categories would be targeted at a narrower audience than the caller named, with
// nothing on it to say so.
//
// TARGETING, not observation — the deliberate difference from Search. On Search,
// AudienceSegments are observation-only: createAdGroupAndAd sets the ad group's
// targetingSetting to bidOnly true for the AUDIENCE dimension so keywords keep
// controlling reach (see targetingSetting in campaign.go). A Display ad group has no
// keywords; its audiences are what the caller means to decide who sees the ad, which is
// how the reference campaign uses them. So when any audience list is supplied,
// CreateDisplayCampaign creates the ad group with a targetingSetting whose AUDIENCE entry
// is bidOnly false (displayAdGroupTargetingSetting, which also restates the four
// demographic defaults) — the AUDIENCE setting the human-built reference campaign carries
// explicitly, and accepted with these criteria by a validateOnly probe on 2026-11-03. It
// is SENT rather than left to a default because an ad group created without it was read
// back with no AUDIENCE restriction at all, so the default is not established.
// Optimized targeting is left at Google's default (optimizedTargetingEnabled is not
// sent) and may widen reach beyond the named segments; that has not been checked.
//
// Every criterion is created ENABLED, as Search's are, for the reason
// createAdGroupTargeting gives. Called after the ad group create and reported by the
// caller as a partial result. Any definite 4xx is a plain failure; an ambiguous one
// (5xx, transport, or a 2xx this client cannot read) is UNCONFIRMED.
func (c *Client) createDisplayAudienceCriteria(ctx context.Context, adGroupResource, adGroupID string, plan displayTargetingPlan) ([]string, error) {
	if plan.audienceCount() == 0 {
		return nil, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("google-ads display audience targeting aborted before any request (context already done; ad group %s has no audience criteria yet): %w", adGroupID, ctxErr)
	}

	ops := make([]mutateOperation, 0, plan.audienceCount())
	for _, name := range plan.userLists {
		ops = append(ops, mutateOperation{Create: displayAudienceCriterionCreate{
			AdGroup: adGroupResource, Status: StatusEnabled, UserList: &userListInfo{UserList: name},
		}})
	}
	for _, name := range plan.customAudiences {
		ops = append(ops, mutateOperation{Create: displayAudienceCriterionCreate{
			AdGroup: adGroupResource, Status: StatusEnabled, CustomAudience: &customAudienceInfo{CustomAudience: name},
		}})
	}
	for _, name := range plan.customAffinities {
		ops = append(ops, mutateOperation{Create: displayAudienceCriterionCreate{
			AdGroup: adGroupResource, Status: StatusEnabled, CustomAffinity: &customAffinityInfo{CustomAffinity: name},
		}})
	}
	for _, name := range plan.userInterests {
		ops = append(ops, mutateOperation{Create: displayAudienceCriterionCreate{
			AdGroup: adGroupResource, Status: StatusEnabled, UserInterest: &userInterestInfo{UserInterestCategory: name},
		}})
	}

	resp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("adGroupCriteria:mutate"), mutateRequest{Operations: ops}, false)
	if err != nil {
		if createOutcomeAmbiguous(err) {
			return nil, fmt.Errorf("google-ads display audience targeting UNCONFIRMED (ad group %s; audience criteria may exist — verify in Google Ads before retrying): %w", adGroupID, err)
		}
		return nil, fmt.Errorf("google-ads display audience targeting failed (ad group %s created; it has NO audience criteria and, if enabled, would serve to everyone its other targeting allows): %w", adGroupID, err)
	}

	var mr mutateResponse
	if uErr := json.Unmarshal(resp, &mr); uErr != nil || len(mr.Results) != len(ops) {
		return nil, fmt.Errorf("google-ads display audience targeting UNCONFIRMED (ad group %s; 2xx with a malformed/short mutate response — criteria may exist — verify in Google Ads before retrying)", adGroupID)
	}
	ids := make([]string, 0, len(ops))
	seen := newResultIDSet(len(mr.Results))
	for i, r := range mr.Results {
		returnedAdGroupID, critID := c.adGroupCriterionID(r.ResourceName)
		if critID == "" || returnedAdGroupID == "" {
			return nil, fmt.Errorf("google-ads display audience targeting UNCONFIRMED (ad group %s; malformed/wrong-kind/wrong-account criterion resource name %q at index %d — verify in Google Ads before retrying)", adGroupID, r.ResourceName, i)
		}
		if returnedAdGroupID != adGroupID {
			return nil, fmt.Errorf("google-ads display audience targeting UNCONFIRMED (ad group %s; adGroupCriterion resource name %q reports a different ad group id %q — verify in Google Ads before retrying)", adGroupID, r.ResourceName, returnedAdGroupID)
		}
		if dup := seen.repeat(critID, i); dup != "" {
			return nil, fmt.Errorf("google-ads display audience targeting UNCONFIRMED "+
				"(ad group %s; %s — verify in Google Ads before retrying)", adGroupID, dup)
		}
		ids = append(ids, critID)
	}
	return ids, nil
}

// createCampaignPlacementExclusions attaches the placement exclusions to a
// just-created Display campaign as ONE campaignCriteria:mutate, one negative
// placement criterion per domain.
//
// CAMPAIGN level, not ad group — both were accepted by the 2026-11-03 validateOnly
// probe, and the campaign level is how an operator thinks of a blocklist: a site the
// campaign must never appear on, including under an ad group a human adds later. One
// batch for the reason the negative keywords are one: a half-applied blocklist reads
// as in place while the campaign still serves on the sites that were dropped.
//
// Its own mutate rather than a share of the location or the language/schedule/device
// criteria batch, so neither failure discards the other and each failure sentence
// names only what it left missing. Called after the campaign exists, so a failure is a
// partial result; an ambiguous outcome is UNCONFIRMED. Every criteria step runs before
// the ad, the only step that makes the campaign servable, so ordering this after the ad
// group leaves a paused campaign with no ad on any failure here.
func (c *Client) createCampaignPlacementExclusions(ctx context.Context, campaignResource, campaignID string, placements []string) ([]string, error) {
	if len(placements) == 0 {
		return nil, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("google-ads placement exclusions aborted before any request (context already done; campaign %s has no placement exclusions yet): %w", campaignID, ctxErr)
	}

	ops := make([]mutateOperation, 0, len(placements))
	for _, domain := range placements {
		ops = append(ops, mutateOperation{Create: campaignNegativePlacementCreate{
			Campaign:  campaignResource,
			Negative:  true,
			Placement: &placementInfo{URL: domain},
		}})
	}

	resp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("campaignCriteria:mutate"), mutateRequest{Operations: ops}, false)
	if err != nil {
		if createOutcomeAmbiguous(err) {
			return nil, fmt.Errorf("google-ads placement exclusions UNCONFIRMED (campaign %s; exclusions may exist — verify in Google Ads before retrying): %w", campaignID, err)
		}
		return nil, fmt.Errorf("google-ads placement exclusions failed (campaign %s created; it has NO placement exclusions and, if enabled, would be eligible to serve on every site the list named): %w", campaignID, err)
	}

	var mr mutateResponse
	if uErr := json.Unmarshal(resp, &mr); uErr != nil || len(mr.Results) != len(ops) {
		return nil, fmt.Errorf("google-ads placement exclusions UNCONFIRMED (campaign %s; 2xx with a malformed/short mutate response — exclusions may exist — verify in Google Ads before retrying)", campaignID)
	}
	ids := make([]string, 0, len(ops))
	seen := newResultIDSet(len(mr.Results))
	for i, r := range mr.Results {
		returnedCampaignID, critID := c.campaignCriterionID(r.ResourceName)
		if critID == "" || returnedCampaignID == "" {
			return nil, fmt.Errorf("google-ads placement exclusions UNCONFIRMED (campaign %s; malformed/wrong-kind/wrong-account criterion resource name %q at index %d — verify in Google Ads before retrying)", campaignID, r.ResourceName, i)
		}
		if returnedCampaignID != campaignID {
			return nil, fmt.Errorf("google-ads placement exclusions UNCONFIRMED (campaign %s; campaignCriterion resource name %q reports a different campaign id %q — verify in Google Ads before retrying)", campaignID, r.ResourceName, returnedCampaignID)
		}
		if dup := seen.repeat(critID, i); dup != "" {
			return nil, fmt.Errorf("google-ads placement exclusions UNCONFIRMED "+
				"(campaign %s; %s — verify in Google Ads before retrying)", campaignID, dup)
		}
		ids = append(ids, critID)
	}
	return ids, nil
}
