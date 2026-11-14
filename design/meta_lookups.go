// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package design

import (
	//nolint:staticcheck // ST1001: the recommended way of using the goa DSL package is with the . import
	. "goa.design/goa/v3/dsl"
)

// Lookups for the Meta create form (#550). metaConfig.targeting.customAudiences and
// excludedCustomAudiences take raw Meta custom-audience ids; this read lets a picker offer them.
// It persists nothing.

// MetaCustomAudience is one custom audience of the Meta ad account.
var MetaCustomAudience = Type("meta-custom-audience", func() {
	Attribute("id", String, "The audience id, exactly what metaConfig.targeting.customAudiences and excludedCustomAudiences take", func() {
		Example("23851234567890123")
	})
	Attribute("name", String, "The audience's display name. Not an identifier.", func() { Example("KubeCon NA 2025 registrants") })
	Attribute("subtype", String, "Meta's own kind, verbatim (CUSTOM, WEBSITE, LOOKALIKE, ENGAGEMENT, …)", func() { Example("CUSTOM") })
	Attribute("description", String, "The audience's description. Absent when Meta sent none.", func() { Example("HubSpot upload") })
	Attribute("approximate_count_lower_bound", Int64, "Meta's estimated audience size, lower bound. Absent when Meta reported none.", func() { Example(12000) })
	Attribute("approximate_count_upper_bound", Int64, "Meta's estimated audience size, upper bound. Absent when Meta reported none.", func() { Example(14100) })
	Required("id", "name", "subtype")
})

// MetaCustomAudiences is the custom-audience read.
var MetaCustomAudiences = Type("meta-custom-audiences", func() {
	Attribute("account_id", String, "The Meta ad account the audiences were read from: the account a Meta create for this project uses", func() {
		Example("act_193556282970417")
	})
	Attribute("audiences", ArrayOf(MetaCustomAudience), "Matching audiences sorted by name (ignoring case), capped at 500 (see `truncated`)")
	Attribute("audience_count", Int, "How many audiences are in `audiences`", func() { Example(1) })
	Attribute("truncated", Boolean, "True when more audiences matched than were returned; narrow the search with q", func() { Example(false) })
	Required("account_id", "audiences", "audience_count", "truncated")
})

// metaTargetingSearchKinds are the kinds search-meta-targeting-options serves. Mirrors
// meta.TargetingSearchKinds, which a service test pins.
var metaTargetingSearchKinds = []any{"interest", "behavior", "work_position"}

// metaTargetingQueryMin and metaTargetingQueryMax mirror meta.MinTargetingQueryLen and
// meta.MaxTargetingQueryLen. Goa applies both to the raw parameter; the service applies them again
// after trimming.
const (
	metaTargetingQueryMin = 2
	metaTargetingQueryMax = 100
)

// metaTargetingQueryPattern requires a non-space character and refuses any control character
// (Unicode category Cc), so the decoder refuses what meta.NormalizeTargetingQuery would.
const metaTargetingQueryPattern = `^\P{Cc}*[^\s\p{Cc}]\P{Cc}*$`

// MetaTargetingOption is one option of the targeting search.
var MetaTargetingOption = Type("meta-targeting-option", func() {
	Attribute("id", String, "The option id, exactly what metaConfig.targeting.interests, behaviors or workPositions (per the type searched) takes", func() {
		Example("6003107902433")
	})
	Attribute("name", String, "Meta's display name for the option. Not an identifier.", func() { Example("Kubernetes") })
	Attribute("path", ArrayOf(String), "Meta's category path for the option, outermost first. Absent when Meta sent none.")
	Attribute("description", String, "Meta's description of the option (a work position's subtext). Absent when Meta sent none.")
	Attribute("audience_size_lower_bound", Int64, "Meta's estimated audience size, lower bound (interests and behaviors). Absent when Meta reported none.", func() { Example(1200000) })
	Attribute("audience_size_upper_bound", Int64, "Meta's estimated audience size, upper bound (interests and behaviors). Absent when Meta reported none.", func() { Example(1400000) })
	Attribute("coverage", Int64, "Meta's estimated audience size (work positions). Absent when Meta reported none.", func() { Example(56000) })
	Required("id", "name")
})

// MetaTargetingOptions is the targeting search read.
var MetaTargetingOptions = Type("meta-targeting-options", func() {
	Attribute("type", String, "The type searched, as requested", func() {
		Enum(metaTargetingSearchKinds...)
		Example("interest")
	})
	Attribute("options", ArrayOf(MetaTargetingOption), "The matches, de-duplicated by id: Meta's relevance order for interest and work_position, Meta's listing order for behavior. At most 50. Empty when nothing matches.")
	Attribute("option_count", Int, "How many options are in `options`", func() { Example(1) })
	Required("type", "options", "option_count")
})

// metaAdsLookupMethods declares the Meta create-form reads inside the connections service, beside
// googleAdsLookupMethods, so they share its security scheme and error types.
func metaAdsLookupMethods() {
	Method("list-meta-custom-audiences", func() {
		Description("List the custom audiences (lookalikes included) of the Meta ad account a Meta create for " +
			"this project uses, live from the ad account's customaudiences edge, for picking " +
			"metaConfig.targeting.customAudiences and excludedCustomAudiences (#550). Credentials resolve as for a " +
			"create, so every id is one the create accepts. q keeps audiences whose name contains it, ignoring " +
			"case. Sorted by name, capped at 500 after the q filter, with `truncated` set when more matched. More " +
			"than 2000 audiences on the account fails the read with 503 rather than truncating it, and that 503 " +
			"does not clear on retry. The Meta ad account is shared by every foundation, so the response lists " +
			"EVERY foundation's audiences, not only this project's; like list-google-ads-audiences it is " +
			"therefore gated on global_marketing_ops of {project_id} (the LF marketing team), not " +
			"campaign_manager, which also admits a foundation's executive director. A pure read: nothing is " +
			"persisted.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Attribute("q", String, "Optional case-insensitive name substring to keep", func() {
				MinLength(1)
				MaxLength(100)
				Pattern(`\S`)
				Example("KubeCon")
			})
			Required("project_id")
		})
		Result(MetaCustomAudiences)
		Error("NotFound", NotFoundError, "Resource not found")
		authErrors()
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			GET("/projects/{project_id}/meta-ads/custom-audiences")
			Header("bearer_token:Authorization")
			connectionAuthErrorResponses()
			Param("q")
			Response(StatusOK)
			Response("NotFound", StatusNotFound)
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})

	Method("search-meta-targeting-options", func() {
		Description("Search Meta's detailed-targeting taxonomy by name, live from Meta's Targeting Search " +
			"(GET /search: type=adinterest for interest, type=adTargetingCategory&class=behaviors for behavior, " +
			"type=adworkposition for work_position), for picking metaConfig.targeting.interests, behaviors and " +
			"workPositions by name instead of by id (#550). Meta serves behaviors as one browse of the whole " +
			"class, so a behavior search keeps the options whose name contains the query, ignoring case. The answer " +
			"is Meta's own taxonomy, the same for every token, and names nothing any ad account holds, so it is gated " +
			"on campaign_manager of {project_id} like every other project read, and a project with no Meta connection " +
			"of its own reads through the shared LF system credential, as a create would. No ad account need be " +
			"selected. Answers are cached for up to an hour by type and query (behaviors by type), which serves a " +
			"repeated identical search, not a new prefix. A throttle from Meta is not retried and answers 503. Off " +
			"(400) unless the deployment sets META_TARGETING_SEARCH_ENABLED to \"true\". A pure read: nothing is " +
			"persisted.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Attribute("type", String, "What to search: interest (metaConfig.targeting.interests), behavior (behaviors) or work_position (workPositions).", func() {
				Enum(metaTargetingSearchKinds...)
				Example("interest")
			})
			Attribute("query", String, "Text to match against option names: 2 to 100 characters, no control "+
				"characters. The limits apply to the value as sent, and surrounding space is then trimmed, so the "+
				"trimmed text must still be at least 2 characters. Every distinct interest or work_position query "+
				"is a Meta call on a shared rate limit (only a repeated identical search is served from cache), so "+
				"callers should debounce keystrokes rather than search on each one.", func() {
				MinLength(metaTargetingQueryMin)
				MaxLength(metaTargetingQueryMax)
				Pattern(metaTargetingQueryPattern)
				Example("kubernetes")
			})
			Required("project_id", "type", "query")
		})
		Result(MetaTargetingOptions)
		Error("NotFound", NotFoundError, "Resource not found")
		authErrors()
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			GET("/projects/{project_id}/meta-ads/targeting-search")
			Header("bearer_token:Authorization")
			connectionAuthErrorResponses()
			Param("type")
			Param("query")
			Response(StatusOK)
			Response("NotFound", StatusNotFound)
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})
}
