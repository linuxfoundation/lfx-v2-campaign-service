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
}
