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

// linkedInTypeaheadFacets are the facets search-linkedin-ads-targeting-entities serves: the
// linkedInConfig.targeting facets LinkedIn's typeahead finder supports, plus locations for
// linkedInConfig.geoTargets. Mirrors linkedin.TypeaheadFacetKeys, which a service test pins.
var linkedInTypeaheadFacets = []any{"employers", "groups", "industries", "interests", "locations", "skills", "titles"}

// linkedInTypeaheadQueryMax mirrors linkedin.MaxTypeaheadQueryLen.
const linkedInTypeaheadQueryMax = 100

// LinkedInAdsTargetingEntity is one entity of the targeting typeahead read.
var LinkedInAdsTargetingEntity = Type("linkedin-ads-targeting-entity", func() {
	Attribute("urn", String, "The entity's URN, in exactly the form the facet takes: a value of linkedInConfig.targeting.<facet>, or for locations the urn of a linkedInConfig.geoTargets entry", func() {
		Example("urn:li:skill:17")
	})
	Attribute("name", String, "LinkedIn's display name for the entity (en_US). Not an identifier; for locations, the label of the geoTargets entry.", func() {
		Example("Kubernetes")
	})
	Required("urn", "name")
})

// LinkedInAdsTargetingEntities is the targeting typeahead read.
var LinkedInAdsTargetingEntities = Type("linkedin-ads-targeting-entities", func() {
	Attribute("facet", String, "The facet searched, as requested", func() {
		Enum(linkedInTypeaheadFacets...)
		Example("skills")
	})
	Attribute("entities", ArrayOf(LinkedInAdsTargetingEntity), "The matches, in LinkedIn's relevance order, de-duplicated by urn. Empty when nothing matches.")
	Attribute("entity_count", Int, "How many entities are in `entities`", func() { Example(1) })
	Required("facet", "entities", "entity_count")
})

const linkedInLookupCommon = "Credentials resolve as for a LinkedIn create: the project's own connection, or the shared LF " +
	"system account when it has none (or when the deployment forces the LF account), so the ids are on the account the " +
	"create sends them to. The LF system account is the fallback of every project without its own connection, so " +
	"the read can list resources built for other foundations; it is therefore gated on global_marketing_ops of " +
	"{project_id} (the LF marketing team), not campaign_manager, which also admits a foundation's executive director. " +
	"The read pages LinkedIn's finder 100 at a time and never truncates: more than 1000 entries, a page that repeats " +
	"an earlier one, or an entry of another ad account fails it with 503. A pure read: nothing is persisted."

// linkedInAdsLookupMethods declares the LinkedIn picker reads inside the connections service.
func linkedInAdsLookupMethods() {
	Method("list-linkedin-ads-audiences", func() {
		Description("List the ad segments (matched and retargeting audiences) on the LinkedIn ad account a create " +
			"for this project uses, live from LinkedIn's Ad Segments finder, for picking " +
			"linkedInConfig.targeting audienceMatchingSegments and dynamicSegments (#552). EXPIRED and FAILED " +
			"segments are left out. " + linkedInLookupCommon)
		Payload(func() {
			bearerToken()
			projectIDAttr()
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
			Response(StatusOK)
			Response("NotFound", StatusNotFound)
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})

	Method("search-linkedin-ads-targeting-entities", func() {
		Description("Search LinkedIn's targeting taxonomy by name, live from LinkedIn's Ad Targeting Entities " +
			"typeahead finder (GET /rest/adTargetingEntities?q=typeahead), for picking linkedInConfig.targeting " +
			"values (skills, groups, titles, interests, industries, employers) and linkedInConfig.geoTargets " +
			"(locations) by name instead of by URN (#585). Only URNs of the shape a create accepts for the facet " +
			"are returned. The answer is LinkedIn's own taxonomy, the same for every token, and names nothing any " +
			"ad account holds, so it is gated on campaign_manager of {project_id} like every other project read, " +
			"and a project with no LinkedIn connection of its own reads through the shared LF system credential, " +
			"as a create would. No ad account need be selected. Answers are cached for up to six hours. A pure " +
			"read: nothing is persisted.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Attribute("facet", String, "The targeting facet to search. locations answers linkedInConfig.geoTargets; the rest are linkedInConfig.targeting keys.", func() {
				Enum(linkedInTypeaheadFacets...)
				Example("skills")
			})
			Attribute("query", String, "Text to match against entity names; surrounding space is ignored", func() {
				MinLength(1)
				MaxLength(linkedInTypeaheadQueryMax)
				Pattern(`\S`)
				Example("kubernetes")
			})
			Required("project_id", "facet", "query")
		})
		Result(LinkedInAdsTargetingEntities)
		Error("NotFound", NotFoundError, "Resource not found")
		authErrors()
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			GET("/projects/{project_id}/linkedin-ads/targeting-entities")
			Header("bearer_token:Authorization")
			connectionAuthErrorResponses()
			Param("facet")
			Param("query")
			Response(StatusOK)
			Response("NotFound", StatusNotFound)
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})
}
