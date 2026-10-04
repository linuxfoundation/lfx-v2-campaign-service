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
// Keyword + audience targeting (GA-4): adGroupCriteria:mutate. GA-3 created an
// ad group with zero criteria, which matches no query — this closes that gap
// by attaching positive Search keywords and/or existing audience segments to
// the ad group createAdGroupAndAd just built. This client does not create
// audiences: AudienceSegments are resource names for existing Customer Match
// user lists that the caller has already built elsewhere, supplied the same
// way reddit's dispatcher passes through cfg.Keywords/cfg.Interests.
// ---------------------------------------------------------------------------

const (
	// maxKeywordTextRunes is Google Ads v23's KeywordInfo.text limit (System
	// Limits table: 80 characters).
	maxKeywordTextRunes = 80
	// maxKeywords/maxAudienceSegments bound caller input to keep one
	// adGroupCriteria:mutate call (and its log/error output) a sane size. Not a
	// Google Ads platform limit — a sanity cap on this broker's input; Google's own
	// per-ad-group ceiling is orders of magnitude higher.
	//
	// 20 was the original value and it was NOT generous enough: the product's own AI
	// brief generator routinely emits ~38 keywords, so every default paid create was
	// refused here (observed end-to-end 2026-08-13 — "at most 20 keywords are
	// supported, got 38", zero campaigns created). A cap that the system's own
	// upstream stage exceeds by default is not protecting a caller from a mistake, it
	// is blocking a create the ad platform would have accepted. Raised to 60 to clear
	// the generator's real output with headroom while keeping one mutate call bounded.
	maxKeywords         = 60
	maxAudienceSegments = 20

	// maxNegativeKeywords bounds campaign-level NEGATIVE keyword input for the same
	// reason maxKeywords bounds positive input — one bounded campaignCriteria:mutate
	// call — and is deliberately the SAME number rather than a tighter one. A negative
	// list is routinely longer than its positive counterpart (brand terms, competitor
	// names, "free"/"jobs"/"salary" style exclusions), so a smaller cap here would be
	// the sibling of the 20-keyword cap that blocked every default create on
	// 2026-08-13: a limit the system's own upstream stage exceeds is not protecting a
	// caller from a mistake, it is refusing a create Google would have accepted.
	maxNegativeKeywords = 60

	// MatchTypeExact/Phrase/Broad are the only Search keyword match types.
	MatchTypeExact  = "EXACT"
	MatchTypePhrase = "PHRASE"
	MatchTypeBroad  = "BROAD"
)

// Keyword is a single positive Search keyword criterion. Text and MatchType
// are both required; see validateKeywords for the exact rules.
type Keyword struct {
	Text      string
	MatchType string
}

// keywordInfo is the "keyword" criterion payload in an adGroupCriteria:mutate
// create.
type keywordInfo struct {
	Text      string `json:"text"`
	MatchType string `json:"matchType"`
}

// userListInfo is the "userList" criterion payload — a Customer Match /
// remarketing list the caller already built, referenced by resource name.
type userListInfo struct {
	UserList string `json:"userList"`
}

// adGroupCriterionCreate is the create payload for adGroupCriteria:mutate.
// Exactly one of Keyword/UserList is set per operation.
//
// There is deliberately no customAudience field. Google Ads has a
// customAudience criterion, but validateAudienceSegments rejects every
// customAudiences resource name before any mutate is built — Custom Audiences
// are not supported on SEARCH campaigns, which is the only campaign type this
// client creates. Carrying the field would make the payload advertise a
// targeting shape this client can never populate.
type adGroupCriterionCreate struct {
	AdGroup  string        `json:"adGroup"`
	Status   string        `json:"status"`
	Keyword  *keywordInfo  `json:"keyword,omitempty"`
	UserList *userListInfo `json:"userList,omitempty"`
}

// validateKeywords trims/validates each caller-supplied keyword and
// de-duplicates by (matchType, text) — Google rejects an exact duplicate
// criterion within the same ad group. Returns (nil, nil) for an empty input
// (targeting is optional). An over-limit text or unrecognized match type is a
// hard error: unlike composeAdCopy, there is no deterministic placeholder to
// fall back to for a keyword, so a bad entry must fail loudly rather than be
// silently dropped.
func validateKeywords(keywords []Keyword) ([]Keyword, error) {
	return validateKeywordList("keyword", keywords, maxKeywords)
}

// validateNegativeKeywords is the NEGATIVE-keyword analogue of validateKeywords:
// the same text/match-type/count rules, applied to the campaign-level exclusion
// list, with "negative keyword" in every message so an operator reading a
// rejection knows which of the two lists was refused.
//
// It is a separate entry point rather than a direct call to validateKeywords
// because that function's error strings all say "keyword", and reusing it would
// report a bad NEGATIVE as a bad positive — sending whoever has to fix it looking
// at the wrong half of the config. The rules themselves are genuinely identical,
// so they live once in validateKeywordList and the two callers differ only in the
// noun and the cap.
//
// Dedupe is PER LIST, deliberately: a term appearing as both a positive and a
// negative is legal upstream (that is how a broad positive is narrowed), the two
// criteria live at different levels — ad group vs campaign — and refusing the
// combination here would reject a create Google accepts.
func validateNegativeKeywords(keywords []Keyword) ([]Keyword, error) {
	return validateKeywordList("negative keyword", keywords, maxNegativeKeywords)
}

// validateKeywordList is the shared implementation behind validateKeywords and
// validateNegativeKeywords. `noun` names the list in every error message; `max`
// is that list's own sanity cap.
func validateKeywordList(noun string, keywords []Keyword, max int) ([]Keyword, error) {
	if len(keywords) == 0 {
		return nil, nil
	}
	if len(keywords) > max {
		return nil, fmt.Errorf("google-ads: at most %d %ss are supported, got %d", max, noun, len(keywords))
	}
	seen := map[string]struct{}{}
	out := make([]Keyword, 0, len(keywords))
	for _, kw := range keywords {
		text := strings.TrimSpace(kw.Text)
		if text == "" {
			return nil, fmt.Errorf("google-ads: %s text must not be empty", noun)
		}
		if utf8.RuneCountInString(text) > maxKeywordTextRunes {
			return nil, fmt.Errorf("google-ads: %s %q exceeds the %d-character limit", noun, text, maxKeywordTextRunes)
		}
		matchType := strings.ToUpper(strings.TrimSpace(kw.MatchType))
		switch matchType {
		case MatchTypeExact, MatchTypePhrase, MatchTypeBroad:
		default:
			return nil, fmt.Errorf("google-ads: %s %q has unsupported match type %q (want %s, %s, or %s)",
				noun, text, kw.MatchType, MatchTypeExact, MatchTypePhrase, MatchTypeBroad)
		}
		// Google Ads treats keyword text as case-insensitive for uniqueness within an
		// ad group, so the dedupe key case-folds text (the stored/sent Text keeps its
		// original casing) — otherwise "Kubernetes" and "kubernetes" both pass this
		// preflight check as distinct, then the whole adGroupCriteria:mutate fails as a
		// definite 4xx after the budget/campaign/ad group/ad already exist.
		key := matchType + "\x00" + strings.ToLower(text)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, Keyword{Text: text, MatchType: matchType})
	}
	return out, nil
}

// audienceCriterionField reports which oneof field a caller-supplied audience
// resource name maps to, inferred from its resource-collection segment.
// Google Ads has several audience-criterion shapes (userInterest,
// combinedAudience, detailedDemographic, …); this client recognizes only the
// two that name a list the caller already built rather than a Google-defined
// category — a Customer Match user list or a custom audience.
//
// Only "userList" is ever actually mutated. "customAudience" is recognized
// solely so validateAudienceSegments can reject it with the reason ("not
// supported for SEARCH campaigns") instead of the generic
// unrecognized-resource-name error, which would send a caller looking for a
// typo in a resource name that is perfectly well formed.
//
// These resource names are Google Ads' own, supplied through this dispatcher's
// configuration. They are unrelated to the campaign_audiences resource in
// docs/api-catalog.md: that resource's platform enum is "hubspot" only
// (design/audience.go), so it holds HubSpot master-list pointers, which can
// never appear here.
//
// Validates that the resource name has the exact shape .../userLists/{id} or
// .../customAudiences/{id}, where {id} is numeric.
func audienceCriterionField(resourceName string) (field string, ok bool) {
	const userListPattern = "/userLists/"
	const customAudiencePattern = "/customAudiences/"

	var field_name string
	var pattern string

	switch {
	case strings.Contains(resourceName, userListPattern):
		field_name = "userList"
		pattern = userListPattern
	case strings.Contains(resourceName, customAudiencePattern):
		field_name = "customAudience"
		pattern = customAudiencePattern
	default:
		return "", false
	}

	// Parse the complete resource name: customers/{numericCustomerId}/{userLists|customAudiences}/{numericId}
	// Extract the prefix before the pattern (should be customers/{id}/) and validate it.
	patternIdx := strings.Index(resourceName, pattern)
	if patternIdx < 0 {
		return "", false // Should not happen given the switch above, but be safe.
	}

	// Verify the prefix is "customers/{numericId}" (the / before pattern is part of pattern)
	prefix := resourceName[:patternIdx]
	if !strings.HasPrefix(prefix, "customers/") {
		return "", false
	}
	custID := prefix[len("customers/"):]
	if custID == "" || !numericID(custID) {
		return "", false
	}

	// Extract and validate the ID portion after the pattern to ensure it's numeric
	// and there's nothing after it.
	idPart := resourceName[patternIdx+len(pattern):]
	if idPart == "" || !numericID(idPart) {
		return "", false // Empty or non-numeric ID
	}

	return field_name, true
}

// campaignNegativeKeywordCreate is the create payload for a NEGATIVE keyword
// criterion on campaignCriteria:mutate.
//
// It is a separate type from geo.go's campaignCriterionCreate — which carries the
// `location` arm of the same oneof — for the reason that file already states for
// its own two payloads: one type per criterion SHAPE, named for what it carries,
// so a payload can never emit two arms of the oneof or silently omit the field
// that gives it meaning. Here that field is `negative`, which is emitted
// explicitly (no omitempty) rather than relying on a default: a campaign keyword
// criterion with negative=false is a POSITIVE campaign-level keyword, which is
// not a thing this client ever means to create, so the false case must be
// impossible to reach by omission.
type campaignNegativeKeywordCreate struct {
	Campaign string       `json:"campaign"`
	Negative bool         `json:"negative"`
	Keyword  *keywordInfo `json:"keyword,omitempty"`
}

// validateAudienceSegments trims/validates each caller-supplied audience
// resource name and de-duplicates. Returns (nil, nil) for an empty input.
// An unrecognized resource-name shape is a hard error — see
// audienceCriterionField. This client creates only SEARCH campaigns, which
// do not support Custom Audiences (Google limits them to Display, Demand Gen,
// Gmail, Video, and Performance Max); only userLists are accepted.
func validateAudienceSegments(segments []string) ([]string, error) {
	if len(segments) == 0 {
		return nil, nil
	}
	if len(segments) > maxAudienceSegments {
		return nil, fmt.Errorf("google-ads: at most %d audience segments are supported, got %d", maxAudienceSegments, len(segments))
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(segments))
	for _, s := range segments {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, fmt.Errorf("google-ads: audience segment resource name must not be empty")
		}
		field, ok := audienceCriterionField(s)
		if !ok {
			return nil, fmt.Errorf("google-ads: audience segment %q is not a recognized resource name (want a .../userLists/{id} Customer Match user-list resource name)", s)
		}
		// Reject customAudiences: this client only creates SEARCH campaigns,
		// which do not support Custom Audiences per Google's documentation.
		if field == "customAudience" {
			return nil, fmt.Errorf("google-ads: custom audiences are not supported for SEARCH campaigns; only userLists are accepted")
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out, nil
}

// createAdGroupTargeting attaches keywords and/or audience segments to the
// just-created ad group as a SINGLE adGroupCriteria:mutate call carrying one
// operation per criterion. Batched into one call (not one per criterion) so
// the whole set shares one atomic outcome: partialFailure is left false (the
// package default — see mutateRequest), so this either wholly succeeds or
// wholly fails, same "no partial state" reasoning as every other mutate here,
// just extended to N operations instead of 1.
//
// Every criterion is created ENABLED (not PAUSED, unlike the ad group/ad
// shell): a criterion's own status is one more gate on top of its ancestors
// (ad group, ad, campaign) already being enabled/eligible — Google will not
// serve it while any ancestor is PAUSED, so creating it ENABLED now means the
// campaign is immediately serve-ready the moment a human flips the ad
// group/ad/campaign to ENABLED, with no separate targeting-activation step.
//
// Audience criteria rely on the ad group create having already set its
// targetingSetting to observation-only for the AUDIENCE dimension (see
// targetingSetting's doc comment in campaign.go, and its use in
// adgroup_ad.go) — this function does not re-check that here, so a caller
// invoking createAdGroupTargeting outside that flow (there is none today)
// would need to set it itself.
//
// This is the highest-risk unverified assumption in this slice, mirroring the
// AdGroupAd composite-resourceName flag from GA-3: verify against a live
// account that a Search campaign's audience criteria actually honor
// bidOnly=true as observation-only rather than still narrowing reach, before
// relying on audience segments to expand rather than restrict delivery.
//
// Duplicate-criterion classification is unverified for this resource (unlike
// the budget/campaign/ad-group DUPLICATE_NAME family): any 4xx here —
// including a possible duplicate-criterion rejection on a retry — is reported
// as a straightforward failure, not reconciled by a duplicate predicate.
func (c *Client) createAdGroupTargeting(ctx context.Context, adGroupResource, adGroupID string, keywords []Keyword, audienceSegments []string) (keywordIDs, audienceIDs []string, err error) {
	if len(keywords) == 0 && len(audienceSegments) == 0 {
		return nil, nil, nil
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, nil, fmt.Errorf("google-ads keyword/audience targeting aborted before any request (context already done; ad group %s has no targeting yet): %w", adGroupID, ctxErr)
	}

	ops := make([]mutateOperation, 0, len(keywords)+len(audienceSegments))
	for _, kw := range keywords {
		ops = append(ops, mutateOperation{Create: adGroupCriterionCreate{
			AdGroup: adGroupResource,
			Status:  StatusEnabled,
			Keyword: &keywordInfo{Text: kw.Text, MatchType: kw.MatchType},
		}})
	}
	for _, seg := range audienceSegments {
		// Every segment reaching here is a userList: validateAudienceSegments (the sole
		// producer of this slice) rejects customAudiences resource names outright, and
		// any other shape fails audienceCriterionField. So there is no oneof to branch
		// on. The earlier switch here was dead in its customAudience arm and, worse,
		// silently emitted a criterion with NO oneof set for an unrecognized field —
		// a 4xx after the budget/campaign/ad group/ad already exist.
		ops = append(ops, mutateOperation{Create: adGroupCriterionCreate{
			AdGroup:  adGroupResource,
			Status:   StatusEnabled,
			UserList: &userListInfo{UserList: seg},
		}})
	}

	resp, mErr := c.doRequest(ctx, http.MethodPost, c.customerPath("adGroupCriteria:mutate"), mutateRequest{Operations: ops}, false)
	if mErr != nil {
		if createOutcomeAmbiguous(mErr) {
			return nil, nil, fmt.Errorf("google-ads keyword/audience targeting UNCONFIRMED (ad group %s; criteria may exist — verify in Google Ads before retrying): %w", adGroupID, mErr)
		}
		return nil, nil, fmt.Errorf("google-ads keyword/audience targeting failed (ad group %s created): %w", adGroupID, mErr)
	}

	var mr mutateResponse
	if uErr := json.Unmarshal(resp, &mr); uErr != nil || len(mr.Results) != len(ops) {
		return nil, nil, fmt.Errorf("google-ads keyword/audience targeting UNCONFIRMED (ad group %s; 2xx with a malformed/short mutate response — criteria may exist — verify in Google Ads before retrying)", adGroupID)
	}

	keywordIDs = make([]string, 0, len(keywords))
	audienceIDs = make([]string, 0, len(audienceSegments))
	for i, r := range mr.Results {
		returnedAdGroupID, critID := c.adGroupCriterionID(r.ResourceName)
		if critID == "" || returnedAdGroupID == "" {
			return nil, nil, fmt.Errorf("google-ads keyword/audience targeting UNCONFIRMED (ad group %s; malformed/wrong-kind/wrong-account criterion resource name %q at index %d — verify in Google Ads before retrying)", adGroupID, r.ResourceName, i)
		}
		// The adGroupCriterion resourceName's ad-group-id half must match the ad group this
		// criterion was created under — a mismatch means the response doesn't describe the
		// criterion this call just created (a malformed/substituted resourceName), so the
		// returned criterion ID cannot be trusted enough to persist.
		if returnedAdGroupID != adGroupID {
			return nil, nil, fmt.Errorf("google-ads keyword/audience targeting UNCONFIRMED (ad group %s; adGroupCriterion resource name %q reports a different ad group id %q — verify in Google Ads before retrying)", adGroupID, r.ResourceName, returnedAdGroupID)
		}
		if i < len(keywords) {
			keywordIDs = append(keywordIDs, critID)
		} else {
			audienceIDs = append(audienceIDs, critID)
		}
	}
	return keywordIDs, audienceIDs, nil
}

// createCampaignNegativeKeywords attaches NEGATIVE keyword criteria to a
// just-created SEARCH campaign as a single campaignCriteria:mutate call, one
// operation per negative. Batched into one call so the whole exclusion list
// shares one atomic outcome (partialFailure stays false, as everywhere else in
// this client): a half-applied negative list is worse than none, because an
// operator looking at the campaign sees exclusions in place and has no reason to
// suspect the rest were dropped.
//
// CAMPAIGN level, not ad group, and that is a choice rather than a convenience.
// A negative keyword expresses campaign-wide intent — queries this event should
// never pay for — so it must keep applying when someone adds a second ad group
// later, which an ad-group-level exclusion would not. The Search path already
// owns a campaignCriteria:mutate call for geo, so the level costs nothing new.
//
// It is deliberately NOT folded into that geo call. createCampaignGeoTargeting's
// failure messages name the consequence of ITS criteria going missing ("it has NO
// location criteria and would serve worldwide if enabled"); a shared mutate would
// report that sentence for a dropped negative keyword, which is simply false, and
// would make either list's failure discard the other.
//
// Called AFTER the campaign create and reported as a partial-result failure by
// the caller: the campaign exists and is PAUSED regardless, so a failure here
// must never be a (nil, err) that discards the claim.
//
// Duplicate-criterion classification is unverified for this resource, as it is
// for the ad-group criteria above: any definite 4xx here is reported as a
// straightforward failure rather than reconciled by a duplicate predicate.
func (c *Client) createCampaignNegativeKeywords(ctx context.Context, campaignResource, campaignID string, negatives []Keyword) ([]string, error) {
	if len(negatives) == 0 {
		return nil, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("google-ads negative keywords aborted before any request (context already done; campaign %s has no negative keywords yet): %w", campaignID, ctxErr)
	}

	ops := make([]mutateOperation, 0, len(negatives))
	for _, kw := range negatives {
		ops = append(ops, mutateOperation{Create: campaignNegativeKeywordCreate{
			Campaign: campaignResource,
			Negative: true,
			Keyword:  &keywordInfo{Text: kw.Text, MatchType: kw.MatchType},
		}})
	}

	resp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("campaignCriteria:mutate"), mutateRequest{Operations: ops}, false)
	if err != nil {
		if createOutcomeAmbiguous(err) {
			return nil, fmt.Errorf("google-ads negative keywords UNCONFIRMED (campaign %s; negative keyword criteria may exist — verify in Google Ads before retrying): %w", campaignID, err)
		}
		return nil, fmt.Errorf("google-ads negative keywords failed (campaign %s created; it has NO negative keywords and would be eligible for every query its positive keywords match if enabled): %w", campaignID, err)
	}

	var mr mutateResponse
	if uErr := json.Unmarshal(resp, &mr); uErr != nil || len(mr.Results) != len(ops) {
		return nil, fmt.Errorf("google-ads negative keywords UNCONFIRMED (campaign %s; 2xx with a malformed/short mutate response — negative keyword criteria may exist — verify in Google Ads before retrying)", campaignID)
	}

	ids := make([]string, 0, len(ops))
	for i, r := range mr.Results {
		returnedCampaignID, critID := c.campaignCriterionID(r.ResourceName)
		if critID == "" || returnedCampaignID == "" {
			return nil, fmt.Errorf("google-ads negative keywords UNCONFIRMED (campaign %s; malformed/wrong-kind/wrong-account criterion resource name %q at index %d — verify in Google Ads before retrying)", campaignID, r.ResourceName, i)
		}
		// The campaignCriterion resourceName's campaign-id half must match the campaign
		// these negatives were created under — a mismatch means the response does not
		// describe what this call created, so the ids cannot be trusted enough to persist.
		if returnedCampaignID != campaignID {
			return nil, fmt.Errorf("google-ads negative keywords UNCONFIRMED (campaign %s; campaignCriterion resource name %q reports a different campaign id %q — verify in Google Ads before retrying)", campaignID, r.ResourceName, returnedCampaignID)
		}
		ids = append(ids, critID)
	}
	return ids, nil
}
