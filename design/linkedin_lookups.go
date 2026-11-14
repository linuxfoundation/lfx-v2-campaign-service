// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package design

import (
	//nolint:staticcheck // ST1001: the recommended way of using the goa DSL package is with the . import
	. "goa.design/goa/v3/dsl"
)

// LinkedIn picker reads (#552): the ad segments linkedInConfig.targeting names and the conversion
// rules linkedInConfig.conversions names, live from the ad account a LinkedIn create for the
// project uses. Gated on global_marketing_ops by the marketing-ops-api gateway rule, like the
// Google pickers, because the LF system account is the fallback of every project without its own
// LinkedIn connection.

// LinkedInAdsAudienceSegment is one ad segment of the audience picker read.
var LinkedInAdsAudienceSegment = Type("linkedin-ads-audience-segment", func() {
	Attribute("id", String, "The ad segment's numeric id", func() { Example("16182906") })
	Attribute("urn", String, "urn:li:adSegment:{id}, in exactly the form linkedInConfig.targeting takes", func() {
		Example("urn:li:adSegment:16182906")
	})
	Attribute("name", String, "The segment's display name. Not an identifier.", func() { Example("KubeCon site visitors 90d") })
	Attribute("type", String, "LinkedIn's AdSegmentType, verbatim (BULK, RETARGETING, MARKET_AUTOMATION, BUSINESS_OBJECTIVE_BASED_AUDIENCE, or another LinkedIn adds)", func() {
		Example("RETARGETING")
	})
	Attribute("status", String, "LinkedIn's AdSegmentStatus, verbatim. EXPIRED and FAILED segments are not listed; BUILDING, UPDATING and ARCHIVED ones are, and only READY serves today.", func() {
		Example("READY")
	})
	Attribute("facet", String, "The linkedInConfig.targeting key the segment goes under: dynamicSegments for RETARGETING, audienceMatchingSegments for BULK, MARKET_AUTOMATION and BUSINESS_OBJECTIVE_BASED_AUDIENCE. Absent for any other type, which this service does not classify.", func() {
		Enum("audienceMatchingSegments", "dynamicSegments")
		Example("dynamicSegments")
	})
	Attribute("approximate_member_count", Int64, "LinkedIn's approximate member count. Absent when LinkedIn reported none; a campaign's audience can differ widely once other facets apply.", func() {
		Example(48000)
	})
	Required("id", "urn", "name", "type", "status")
})

// LinkedInAdsAudienceSegments is the audience picker read.
var LinkedInAdsAudienceSegments = Type("linkedin-ads-audience-segments", func() {
	Attribute("account_id", String, "The LinkedIn ad account the segments were read from: the account a LinkedIn create for this project uses", func() { Example("512345678") })
	Attribute("audiences", ArrayOf(LinkedInAdsAudienceSegment), "The account's targetable segments, sorted by name (ignoring case)")
	Attribute("audience_count", Int, "How many segments are in `audiences`", func() { Example(1) })
	Attribute("truncated", Boolean, "Always false: the read never truncates (more than 1000 segments fails it with 503). Kept for shape parity with the Google audience picker.", func() { Example(false) })
	Required("account_id", "audiences", "audience_count", "truncated")
})

// LinkedInAdsConversionRule is one conversion rule of the conversion picker read.
var LinkedInAdsConversionRule = Type("linkedin-ads-conversion-rule", func() {
	Attribute("id", String, "The conversion rule's numeric id, in exactly the form linkedInConfig.conversions takes", func() { Example("104012") })
	Attribute("urn", String, "urn:lla:llaPartnerConversion:{id}", func() { Example("urn:lla:llaPartnerConversion:104012") })
	Attribute("name", String, "The rule's display name. Not an identifier.", func() { Example("KubeCon registration") })
	Attribute("type", String, "LinkedIn's conversion type, verbatim (e.g. REGISTRATION, LEAD, PURCHASE); empty when LinkedIn sent none", func() { Example("REGISTRATION") })
	Attribute("enabled", Boolean, "Whether the rule is enabled; false when LinkedIn did not say", func() { Example(true) })
	Required("id", "urn", "name", "type", "enabled")
})

// LinkedInAdsConversionRules is the conversion picker read.
var LinkedInAdsConversionRules = Type("linkedin-ads-conversion-rules", func() {
	Attribute("account_id", String, "The LinkedIn ad account the rules were read from: the account a LinkedIn create for this project uses", func() { Example("512345678") })
	Attribute("conversions", ArrayOf(LinkedInAdsConversionRule), "The account's conversion rules, enabled or not, sorted by name (ignoring case)")
	Attribute("conversion_count", Int, "How many rules are in `conversions`", func() { Example(1) })
	Required("account_id", "conversions", "conversion_count")
})

const linkedInLookupCommon = "Credentials resolve as for a LinkedIn create: the project's own connection, or the shared LF " +
	"system account when it has none (or when the deployment forces the LF account), so the ids are on the account the " +
	"create sends them to. The LF system account is the fallback of every project without its own connection, so " +
	"the read can list resources built for other foundations; it is therefore gated on global_marketing_ops of " +
	"{project_id} (the LF marketing team), not campaign_manager, which also admits a foundation's executive director. " +
	"The read pages LinkedIn's finder 100 at a time and never truncates: more than 1000 entries, a page that repeats " +
	"an earlier one, or an entry of another ad account fails it with 503. account_id (optional, #581) names the " +
	"account to list instead: the resolved account itself, or one of the LF system account's operator-installed " +
	"grants (the accounts linkedInConfig.adAccountId may name); any other account is a 400. " +
	"A pure read: nothing is persisted."

// linkedInLookupAccountAttr is the optional account_id both pickers take (#581).
func linkedInLookupAccountAttr() {
	Attribute("account_id", String, "The LinkedIn ad account to list. Optional: omitted, the account a create for this project uses; otherwise that account or one of the LF system account's granted accounts.", func() {
		Pattern(`^[0-9]+$`)
		MaxLength(64)
		Example("500928401")
	})
}

// linkedInAdsLookupMethods declares the two reads inside the connections service.
func linkedInAdsLookupMethods() {
	Method("list-linkedin-ads-audiences", func() {
		Description("List the ad segments (matched and retargeting audiences) on the LinkedIn ad account a create " +
			"for this project uses, live from LinkedIn's Ad Segments finder, for picking " +
			"linkedInConfig.targeting audienceMatchingSegments and dynamicSegments (#552). EXPIRED and FAILED " +
			"segments are left out. " + linkedInLookupCommon)
		Payload(func() {
			bearerToken()
			projectIDAttr()
			linkedInLookupAccountAttr()
			Required("project_id")
		})
		Result(LinkedInAdsAudienceSegments)
		Error("NotFound", NotFoundError, "Resource not found")
		authErrors()
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			GET("/projects/{project_id}/linkedin-ads/audiences")
			Header("bearer_token:Authorization")
			connectionAuthErrorResponses()
			Param("account_id")
			Response(StatusOK)
			Response("NotFound", StatusNotFound)
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})

	Method("list-linkedin-ads-conversions", func() {
		Description("List the conversion rules on the LinkedIn ad account a create for this project uses, live from " +
			"LinkedIn's Conversions finder, for picking linkedInConfig.conversions (#552). Disabled rules are " +
			"listed with enabled false. " + linkedInLookupCommon)
		Payload(func() {
			bearerToken()
			projectIDAttr()
			linkedInLookupAccountAttr()
			Required("project_id")
		})
		Result(LinkedInAdsConversionRules)
		Error("NotFound", NotFoundError, "Resource not found")
		authErrors()
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			GET("/projects/{project_id}/linkedin-ads/conversions")
			Header("bearer_token:Authorization")
			connectionAuthErrorResponses()
			Param("account_id")
			Response(StatusOK)
			Response("NotFound", StatusNotFound)
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})
}
