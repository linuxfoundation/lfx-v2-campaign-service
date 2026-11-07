// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package design

import (
	//nolint:staticcheck // ST1001: the recommended way of using the goa DSL package is with the . import
	. "goa.design/goa/v3/dsl"
)

// Lookups for the X create form (#564 gap 3). twitterConfig.customAudienceIds and
// excludedCustomAudienceIds take X custom audience ids; this read lets a picker offer them
// instead of making an operator copy them out of X Ads Manager. Nothing is persisted.

// TwitterAdsCustomAudience is one custom audience on an X ad account.
var TwitterAdsCustomAudience = Type("twitter-ads-custom-audience", func() {
	Attribute("id", String, "The audience id, in exactly the form twitterConfig.customAudienceIds and excludedCustomAudienceIds accept. Send it as-is.", func() {
		Pattern(`^[A-Za-z0-9]{1,32}$`)
		Example("1nmth")
	})
	Attribute("name", String, "The audience's display name. Not an identifier; may be empty.", func() { Example("KubeCon NA site visitors 30d") })
	Attribute("audience_type", String, "X's audience_type, verbatim (for example WEB, LIST or MOBILE); may be empty.", func() { Example("WEB") })
	Attribute("targetable", Boolean, "X's targetable flag. Only a targetable audience is accepted at create; every audience is listed, so a picker can show why one is not usable yet.", func() { Example(true) })
	Attribute("audience_size", Int64, "X's audience_size. Absent when X sent none, which is not a size of 0.", func() { Example(1470) })
	Attribute("reasons_not_targetable", ArrayOf(String), "X's reasons_not_targetable (for example TOO_SMALL or PROCESSING); only enum-shaped values are passed through. Empty when targetable.", func() {
		Example([]string{})
	})
	Required("id", "name", "audience_type", "targetable", "reasons_not_targetable")
})

// TwitterAdsCustomAudiences is the custom-audience read.
var TwitterAdsCustomAudiences = Type("twitter-ads-custom-audiences", func() {
	Attribute("account_id", String, "The X ad account the audiences were read from: the account an X create for this project uses", func() { Example("18ce54d4x5t") })
	Attribute("audiences", ArrayOf(TwitterAdsCustomAudience), "The account's non-deleted custom audiences, sorted by name (ignoring case), then id", func() {
		// Explicit, so the published example has one audience matching audience_count: 1.
		Example([]map[string]any{{"id": "1nmth", "name": "KubeCon NA site visitors 30d", "audience_type": "WEB", "targetable": true, "audience_size": 1470, "reasons_not_targetable": []string{}}})
	})
	Attribute("audience_count", Int, "How many audiences are in `audiences`", func() { Example(1) })
	Attribute("truncated", Boolean, "True when X still had more pages after the 10 this read follows; the list is then incomplete", func() { Example(false) })
	Required("account_id", "audiences", "audience_count", "truncated")
})

// twitterAdsLookupMethods declares the X create-form reads inside the connections service, next
// to redditAdsLookupMethods, so they share its security scheme and error types.
func twitterAdsLookupMethods() {
	Method("list-twitter-ads-custom-audiences", func() {
		Description("List the custom audiences on the X ad account an X create for this project uses, live from " +
			"X Ads API v12 GET accounts/:account_id/custom_audiences (count=1000), for picking " +
			"twitterConfig.customAudienceIds and excludedCustomAudienceIds (#564). Follows next_cursor for at most 10 " +
			"pages; `truncated` is set when more remained. Deleted audiences are left out; every other audience is " +
			"listed, targetable or not, with X's reasons_not_targetable. Sorted by name, ignoring case, then id. " +
			"Credentials resolve as for a create: the project's own X connection, or the LF system account when the " +
			"deployment forces it. The X ad account is shared by every foundation, so the response lists EVERY " +
			"foundation's audiences; like list-reddit-ads-custom-audiences it is therefore gated on " +
			"global_marketing_ops of {project_id} (the LF marketing team), not campaign_manager, which also admits a " +
			"foundation's executive director. Disabled (400 not supported) unless TWITTER_CUSTOM_AUDIENCES_ENABLED is " +
			"\"true\": the read is built from X's published reference and has not been exercised against a live ad " +
			"account. An answer this service cannot believe (no data list, an unusable or repeated id, a full page " +
			"with no usable cursor, a repeated cursor) is 503 with nothing partial. A pure read: nothing is persisted.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Required("project_id")
		})
		Result(TwitterAdsCustomAudiences)
		Error("NotFound", NotFoundError, "Resource not found")
		authErrors()
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			GET("/projects/{project_id}/twitter-ads/custom-audiences")
			Header("bearer_token:Authorization")
			connectionAuthErrorResponses()
			Response(StatusOK)
			Response("NotFound", StatusNotFound)
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})
}
