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
// audiences: AudienceSegments are resource names for a Customer Match "user
// list" or a custom audience the caller already built elsewhere (e.g. via the
// campaign_audiences resource — see docs/api-catalog.md), supplied the same
// way reddit's dispatcher passes through cfg.Keywords/cfg.Interests.
// ---------------------------------------------------------------------------

const (
	// maxKeywordTextRunes is Google Ads v23's KeywordInfo.text limit (System
	// Limits table: 80 characters).
	maxKeywordTextRunes = 80
	// maxKeywords/maxAudienceSegments bound caller input to keep one
	// adGroupCriteria:mutate call (and its log/error output) a sane size. Not a
	// Google Ads platform limit — a generous sanity cap on this broker's input.
	maxKeywords         = 20
	maxAudienceSegments = 20

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

// customAudienceInfo is the "customAudience" criterion payload, the other
// audience-segment shape this client accepts.
type customAudienceInfo struct {
	CustomAudience string `json:"customAudience"`
}

// adGroupCriterionCreate is the create payload for adGroupCriteria:mutate.
// Exactly one of Keyword/UserList/CustomAudience is set per operation.
type adGroupCriterionCreate struct {
	AdGroup        string              `json:"adGroup"`
	Status         string              `json:"status"`
	Keyword        *keywordInfo        `json:"keyword,omitempty"`
	UserList       *userListInfo       `json:"userList,omitempty"`
	CustomAudience *customAudienceInfo `json:"customAudience,omitempty"`
}

// validateKeywords trims/validates each caller-supplied keyword and
// de-duplicates by (matchType, text) — Google rejects an exact duplicate
// criterion within the same ad group. Returns (nil, nil) for an empty input
// (targeting is optional). An over-limit text or unrecognized match type is a
// hard error: unlike composeAdCopy, there is no deterministic placeholder to
// fall back to for a keyword, so a bad entry must fail loudly rather than be
// silently dropped.
func validateKeywords(keywords []Keyword) ([]Keyword, error) {
	if len(keywords) == 0 {
		return nil, nil
	}
	if len(keywords) > maxKeywords {
		return nil, fmt.Errorf("google-ads: at most %d keywords are supported, got %d", maxKeywords, len(keywords))
	}
	seen := map[string]struct{}{}
	out := make([]Keyword, 0, len(keywords))
	for _, kw := range keywords {
		text := strings.TrimSpace(kw.Text)
		if text == "" {
			return nil, fmt.Errorf("google-ads: keyword text must not be empty")
		}
		if utf8.RuneCountInString(text) > maxKeywordTextRunes {
			return nil, fmt.Errorf("google-ads: keyword %q exceeds the %d-character limit", text, maxKeywordTextRunes)
		}
		matchType := strings.ToUpper(strings.TrimSpace(kw.MatchType))
		switch matchType {
		case MatchTypeExact, MatchTypePhrase, MatchTypeBroad:
		default:
			return nil, fmt.Errorf("google-ads: keyword %q has unsupported match type %q (want %s, %s, or %s)",
				text, kw.MatchType, MatchTypeExact, MatchTypePhrase, MatchTypeBroad)
		}
		key := matchType + "\x00" + text
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
// combinedAudience, detailedDemographic, …); this client only recognizes the
// two that match what "a built campaign audience" (docs/api-catalog.md's
// campaign_audiences resource) represents — a Customer Match list or a custom
// audience the caller already built, not a Google-defined category.
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

	// Extract the ID portion after the pattern to validate it's numeric
	// and there's nothing after it.
	parts := strings.Split(resourceName, pattern)
	if len(parts) != 2 {
		return "", false // Multiple occurrences or no occurrence after trim
	}
	id := parts[1]
	if id == "" || !numericID(id) {
		return "", false // Empty or non-numeric ID
	}

	return field_name, true
}

// validateAudienceSegments trims/validates each caller-supplied audience
// resource name and de-duplicates. Returns (nil, nil) for an empty input.
// An unrecognized resource-name shape is a hard error — see
// audienceCriterionField.
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
		if _, ok := audienceCriterionField(s); !ok {
			return nil, fmt.Errorf("google-ads: audience segment %q is not a recognized resource name (want a .../userLists/{id} or .../customAudiences/{id} resource name)", s)
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
// Audience criteria rely on CreateCampaign having already set the campaign's
// targetingSetting to observation-only for the AUDIENCE dimension (see
// targetingSetting's doc comment in campaign.go) — this function does not
// re-check that here, so a caller invoking createAdGroupTargeting outside
// that flow (there is none today) would need to set it itself.
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
		op := adGroupCriterionCreate{AdGroup: adGroupResource, Status: StatusEnabled}
		field, _ := audienceCriterionField(seg) // already validated by validateAudienceSegments
		switch field {
		case "userList":
			op.UserList = &userListInfo{UserList: seg}
		case "customAudience":
			op.CustomAudience = &customAudienceInfo{CustomAudience: seg}
		}
		ops = append(ops, mutateOperation{Create: op})
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
		returnedAdGroupID, critID := compositeResourceID(r.ResourceName)
		if critID == "" || returnedAdGroupID == "" {
			return nil, nil, fmt.Errorf("google-ads keyword/audience targeting UNCONFIRMED (ad group %s; malformed criterion resource name %q at index %d — verify in Google Ads before retrying)", adGroupID, r.ResourceName, i)
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
