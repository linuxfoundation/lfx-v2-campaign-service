// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"fmt"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// Conversion optimization at create (#564 gap 5): twitterConfig.goal and
// twitterConfig.primaryWebEventTag, sent as the new line item's goal and primary_web_event_tag.
//
// X Ads API v12, POST accounts/:account_id/line_items
// (https://docs.x.com/x-ads-api/campaign-management/reference): goal is "the optimization setting
// to use with this line item", with WEBSITE_CONVERSIONS among its values and a default "based on
// objective"; primary_web_event_tag is "the identifier of the primary web event tag", "required
// when the line item's goal is set to WEBSITE_CONVERSIONS". The objective stays WEBSITE_CLICKS:
// X's objective list has no conversions objective, and goal is the optimization within it.

// FieldGoal and FieldPrimaryWebEventTag are the gap 5 twitterConfig keys (#564).
const (
	FieldGoal               = "goal"
	FieldPrimaryWebEventTag = "primaryWebEventTag"
)

// GoalWebsiteConversions is the only goal this service sends. Every other goal is X's default for
// the WEBSITE_CLICKS objective, which a create without twitterConfig.goal already gets.
const GoalWebsiteConversions = "WEBSITE_CONVERSIONS"

// maxWebEventTagLen bounds a web event tag id. X's ids are short base-36 strings (the measurement
// reference's example is o3bk1); 32 matches the custom audience id bound.
const maxWebEventTagLen = 32

// NormalizeConversionGoal applies the gap 5 rule (#564). goal is trimmed and upper-cased and must be
// WEBSITE_CONVERSIONS; tag is trimmed and lower-cased and must be 1-32 ASCII letters or digits (the
// id of a web event tag, as GET accounts/:account_id/web_event_tags lists it — not the tw-…-…
// event id Events Manager shows). Both empty means no conversion goal: X's default optimization,
// as before. A goal needs a tag (X requires one with WEBSITE_CONVERSIONS), and a tag needs the
// goal, so a tag is never sent where it would change nothing the caller asked for. Pure, so the
// dispatcher's synchronous preflight and CreateCampaign apply the same rule.
func NormalizeConversionGoal(goal, tag string) (string, string, error) {
	g := strings.ToUpper(strings.TrimSpace(goal))
	t := strings.ToLower(strings.TrimSpace(tag))
	if g == "" && t == "" {
		return "", "", nil
	}
	if g == "" {
		return "", "", &TargetingError{Field: FieldPrimaryWebEventTag, Msg: fmt.Sprintf("needs %s %s", FieldGoal, GoalWebsiteConversions)}
	}
	if g != GoalWebsiteConversions {
		return "", "", &TargetingError{Field: FieldGoal, Msg: fmt.Sprintf("%s must be %s", domain.CallerValue(goal), GoalWebsiteConversions)}
	}
	if t == "" {
		return "", "", &TargetingError{Field: FieldPrimaryWebEventTag, Msg: fmt.Sprintf("is required with %s %s", FieldGoal, GoalWebsiteConversions)}
	}
	if !isWebEventTagID(t) {
		return "", "", &TargetingError{Field: FieldPrimaryWebEventTag, Msg: fmt.Sprintf("%s must be 1-%d letters or digits (the web event tag id, e.g. o3bk1)", domain.CallerValue(tag), maxWebEventTagLen)}
	}
	return g, t, nil
}

// isWebEventTagID reports whether s (already trimmed and lower-cased) is 1-maxWebEventTagLen ASCII
// letters or digits.
func isWebEventTagID(s string) bool {
	if s == "" || len(s) > maxWebEventTagLen {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}
