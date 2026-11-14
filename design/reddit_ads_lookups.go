// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package design

import (
	//nolint:staticcheck // ST1001: the recommended way of using the goa DSL package is with the . import
	. "goa.design/goa/v3/dsl"
)

// Lookups for the Reddit create form (#549). redditConfig.customAudienceIds and
// excludedCustomAudienceIds take Reddit custom audience ids; this read lets a picker offer them
// instead of making an operator copy them out of Reddit Ads Manager. The pixel read (#549 gap 10)
// does the same for redditConfig.conversionPixelId. Nothing is persisted.

// RedditAdsCustomAudience is one custom audience on a Reddit ad account.
var RedditAdsCustomAudience = Type("reddit-ads-custom-audience", func() {
	Attribute("id", String, "The audience id, in exactly the form redditConfig.customAudienceIds and excludedCustomAudienceIds accept. Send it as-is.", func() {
		Pattern(`^[A-Za-z0-9_]{1,64}$`)
		Example("67547345732543")
	})
	Attribute("name", String, "The audience's display name. Not an identifier; may be empty.", func() { Example("KubeCon NA site visitors 30d") })
	Attribute("type", String, "Reddit's audience type, verbatim (CUSTOMER_LIST, LOOKALIKE, ENGAGEMENT_RETARGETING, PIXEL_RETARGETING, THIRD_PARTY or IN_MARKET); may be empty.", func() { Example("PIXEL_RETARGETING") })
	Attribute("status", String, "Reddit's processing status, verbatim (VALID, SUCCESSFUL, PENDING, EXPIRED, NOT_ENOUGH_MATCHES_ERROR and so on); may be empty. Every audience is listed whatever its status, so a picker can show why one is not usable yet.", func() { Example("VALID") })
	Attribute("size_lower", Int64, "Reddit's lower bound on the matched users. Absent when Reddit sent none, which is not a size of 0.", func() { Example(1000) })
	Attribute("size_upper", Int64, "Reddit's upper bound on the matched users. Absent when Reddit sent none.", func() { Example(5000) })
	Required("id", "name", "type", "status")
})

// RedditAdsCustomAudiences is the custom-audience read.
var RedditAdsCustomAudiences = Type("reddit-ads-custom-audiences", func() {
	Attribute("account_id", String, "The Reddit ad account the audiences were read from: the account a Reddit create for this project uses", func() { Example("t2_gv9wtbfa") })
	Attribute("audiences", ArrayOf(RedditAdsCustomAudience), "The account's custom audiences, sorted by name (ignoring case), then id", func() {
		// Explicit, so the published example has one audience matching audience_count: 1 (Goa's
		// generated one repeated the same id four times).
		Example([]map[string]any{{"id": "67547345732543", "name": "KubeCon NA site visitors 30d", "type": "PIXEL_RETARGETING", "status": "VALID", "size_lower": 1000, "size_upper": 5000}})
	})
	Attribute("audience_count", Int, "How many audiences are in `audiences`", func() { Example(1) })
	Attribute("truncated", Boolean, "True when Reddit still had more pages after the 10 this read follows; the list is then incomplete", func() { Example(false) })
	Required("account_id", "audiences", "audience_count", "truncated")
})

// RedditAdsPixel is one conversion pixel on a Reddit ad account (#549 gap 10).
var RedditAdsPixel = Type("reddit-ads-pixel", func() {
	Attribute("id", String, "The pixel id, in the form redditConfig.conversionPixelId and the connection's conversion_pixel_id take. Send it as-is.", func() {
		Pattern(`^[A-Za-z0-9_]{1,64}$`)
		Example("t2_123456")
	})
	Attribute("name", String, "The pixel's display name. Not an identifier; may be empty.", func() { Example("Snoo's Pixel") })
	Attribute("business_id", String, "The Reddit business that owns the pixel, verbatim. Absent when Reddit sent none.", func() { Example("8f2c5b1e-0000-4000-8000-000000000000") })
	Required("id", "name")
})

// RedditAdsPixels is the pixel read.
var RedditAdsPixels = Type("reddit-ads-pixels", func() {
	Attribute("account_id", String, "The Reddit ad account the pixels were read from: the account a Reddit create for this project uses", func() { Example("t2_gv9wtbfa") })
	Attribute("pixels", ArrayOf(RedditAdsPixel), "The account's pixels, sorted by name (ignoring case), then id", func() {
		Example([]map[string]any{{"id": "t2_123456", "name": "Snoo's Pixel"}})
	})
	Attribute("pixel_count", Int, "How many pixels are in `pixels`", func() { Example(1) })
	Attribute("truncated", Boolean, "True when Reddit still had more pages after the 10 this read follows; the list is then incomplete", func() { Example(false) })
	Required("account_id", "pixels", "pixel_count", "truncated")
})

// redditAdsLookupMethods declares the Reddit create-form reads inside the connections service,
// next to googleAdsLookupMethods, so they share its security scheme and error types.
func redditAdsLookupMethods() {
	Method("list-reddit-ads-custom-audiences", func() {
		Description("List the custom audiences on the Reddit ad account a Reddit create for this project uses, live " +
			"from Reddit Ads API v3 GET /ad_accounts/{ad_account_id}/custom_audiences, for picking " +
			"redditConfig.customAudienceIds and excludedCustomAudienceIds (#549). Follows Reddit's pagination for at " +
			"most 10 pages; `truncated` is set when more remained. Sorted by name, ignoring case. Credentials resolve as " +
			"for a create: the project's own Reddit connection, or the LF system account when the deployment forces it. " +
			"The Reddit ad account is shared by every foundation, so the response lists EVERY foundation's audiences; " +
			"like list-google-ads-audiences it is therefore gated on global_marketing_ops of {project_id} (the LF " +
			"marketing team), not campaign_manager, which also admits a foundation's executive director. Disabled " +
			"(400 not supported) unless REDDIT_CUSTOM_AUDIENCES_ENABLED is \"true\": the read is built from Reddit's " +
			"public OpenAPI document and has not been exercised against a live ad account. An answer this service " +
			"cannot believe (no data list, an unusable or repeated id) is 503 with nothing partial. A pure read: " +
			"nothing is persisted.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Required("project_id")
		})
		Result(RedditAdsCustomAudiences)
		Error("NotFound", NotFoundError, "Resource not found")
		authErrors()
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			GET("/projects/{project_id}/reddit-ads/custom-audiences")
			Header("bearer_token:Authorization")
			connectionAuthErrorResponses()
			Response(StatusOK)
			Response("NotFound", StatusNotFound)
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})

	Method("list-reddit-ads-pixels", func() {
		Description("List the conversion pixels on the Reddit ad account a Reddit create for this project uses, live " +
			"from Reddit Ads API v3 GET /ad_accounts/{ad_account_id}/pixels (list_pixels_by_ad_account), for picking " +
			"redditConfig.conversionPixelId (#549 gap 10). Follows Reddit's pagination for at most 10 pages; `truncated` " +
			"is set when more remained. Sorted by name, ignoring case, then id. Credentials resolve as for a create: the " +
			"project's own Reddit connection, or the LF system account when the deployment forces it. The Reddit ad " +
			"account is shared by every foundation, so the response lists EVERY foundation's pixels; like " +
			"list-reddit-ads-custom-audiences it is therefore gated on global_marketing_ops of {project_id}, not " +
			"campaign_manager. Disabled (400 not supported) unless REDDIT_PIXEL_LIST_ENABLED is \"true\": the read is " +
			"built from Reddit's public OpenAPI document and has not been exercised against a live ad account. An " +
			"answer this service cannot believe (no data list, an unusable or repeated id) is 503 with nothing partial. " +
			"A pure read: nothing is persisted.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Required("project_id")
		})
		Result(RedditAdsPixels)
		Error("NotFound", NotFoundError, "Resource not found")
		authErrors()
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			GET("/projects/{project_id}/reddit-ads/pixels")
			Header("bearer_token:Authorization")
			connectionAuthErrorResponses()
			Response(StatusOK)
			Response("NotFound", StatusNotFound)
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})
}
