// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package design

import (
	//nolint:staticcheck // ST1001: the recommended way of using the goa DSL package is with the . import
	. "goa.design/goa/v3/dsl"
)

// Lookups for the Google Ads create form. googleAdsConfig.geoTargets, audienceSegments,
// conversionActions and the audience inputs (customAudiences, userInterests, demandGenAudience,
// performanceMaxAudienceSignal) take raw Google ids; these four reads let a picker offer them
// instead of making an operator type them. None persists anything.

// googleAdsGeoSuggestMax caps the suggestions returned. Google's own suggest answer is short;
// the cap keeps the response a picker's worth whatever it sends.
const googleAdsGeoSuggestMax = 50

// GoogleAdsGeoTargetSuggestion is one geo target constant.
var GoogleAdsGeoTargetSuggestion = Type("google-ads-geo-target-suggestion", func() {
	Attribute("id", String, "The numeric geo target constant id, in exactly the form googleAdsConfig.geoTargets and excludedGeoTargets accept. Send it as-is; the geoTargetConstants/{id} resource name is NOT accepted there.", func() {
		Pattern(`^[1-9][0-9]{0,18}$`)
		Example("1026201")
	})
	Attribute("name", String, "Google's display name for the location", func() { Example("Austin") })
	Attribute("canonical_name", String, "Google's fully qualified name, which disambiguates same-named places", func() { Example("Austin,Texas,United States") })
	Attribute("country_code", String, "ISO 3166-1 alpha-2 country of the location, as Google reports it", func() { Example("US") })
	Attribute("target_type", String, "Google's location type label, verbatim (e.g. Country, State, City, Postal Code, DMA Region)", func() { Example("City") })
	Attribute("status", String, "ENABLED, or REMOVAL_PLANNED for a location Google is retiring", func() { Example("ENABLED") })
	Attribute("reach", Int64, "Google's approximate audience estimate. Absent when Google sent none.", func() { Example(2100000) })
	Required("id", "name", "canonical_name", "country_code", "target_type", "status")
})

// GoogleAdsGeoTargetSuggestions is the geo target suggest response.
var GoogleAdsGeoTargetSuggestions = Type("google-ads-geo-target-suggestions", func() {
	Attribute("suggestions", ArrayOf(GoogleAdsGeoTargetSuggestion), "Matching locations in Google's order, filtered to target_types when given, capped at limit")
	Attribute("suggestion_count", Int, "How many suggestions are in `suggestions`", func() { Example(1) })
	Attribute("truncated", Boolean, "True when more matching locations were available than `limit` allowed", func() { Example(false) })
	Required("suggestions", "suggestion_count", "truncated")
})

// GoogleAdsUserList is one Google Ads user list.
var GoogleAdsUserList = Type("google-ads-user-list", func() {
	Attribute("resource_name", String, "customers/{customerId}/userLists/{id}, in exactly the form googleAdsConfig.audienceSegments accepts (Search and Display). A list targets campaigns on its own customer.", func() {
		Pattern(`^customers/[0-9]+/userLists/[1-9][0-9]{0,18}$`)
		Example("customers/8666746580/userLists/7012345678")
	})
	Attribute("id", String, "The user list id", func() { Example("7012345678") })
	Attribute("name", String, "The list's display name. Not an identifier.", func() { Example("KubeCon NA 2025 registrants") })
	Attribute("type", String, "Google's UserListType, verbatim (e.g. CRM_BASED, RULE_BASED, LOGICAL, SIMILAR)", func() { Example("CRM_BASED") })
	Attribute("membership_status", String, "OPEN (accepting members) or CLOSED, verbatim", func() { Example("OPEN") })
	Attribute("account_status", String, "The list's account_user_list_status (ENABLED or DISABLED), verbatim. Absent when Google sent none.", func() { Example("ENABLED") })
	Attribute("size_for_search", Int64, "Google's estimated Search-network size. Absent when Google sent none, which is not a size of 0.", func() { Example(12000) })
	Attribute("size_range_for_search", String, "Google's bucketed Search-network size, verbatim. Absent when Google sent none.", func() { Example("TEN_THOUSAND_TO_FIFTY_THOUSAND") })
	Attribute("eligible_for_search", Boolean, "Whether Google reports the list usable on Search. An ineligible list will not serve on a Search campaign; this read does not report Display eligibility.", func() { Example(true) })
	Required("resource_name", "id", "name", "type", "membership_status", "eligible_for_search")
})

// GoogleAdsUserLists is the user-list read.
var GoogleAdsUserLists = Type("google-ads-user-lists", func() {
	Attribute("account_id", String, "The Google Ads customer the lists were read from", func() { Example("8666746580") })
	Attribute("user_lists", ArrayOf(GoogleAdsUserList), "User lists on the customer, sorted by name, capped at 500 (see `truncated`)")
	Attribute("user_list_count", Int, "How many lists are in `user_lists`", func() { Example(1) })
	Attribute("truncated", Boolean, "True when the customer holds more lists than were returned", func() { Example(false) })
	Required("account_id", "user_lists", "user_list_count", "truncated")
})

// GoogleAdsConversionAction is one enabled conversion action on a Google Ads customer.
var GoogleAdsConversionAction = Type("google-ads-conversion-action", func() {
	Attribute("id", String, "The numeric conversion action id, the short form googleAdsConfig.conversionActions accepts (qualified with the campaign's own account).", func() {
		Example("1111111111")
	})
	Attribute("resource_name", String, "customers/{customerId}/conversionActions/{id}, the full form googleAdsConfig.conversionActions accepts. An action belongs to its own customer and cannot be attached to a campaign on another.", func() {
		Example("customers/1234567890/conversionActions/1111111111")
	})
	Attribute("name", String, "The action's display name. Not an identifier.", func() { Example("Event Registrations") })
	Attribute("status", String, "Always ENABLED: only enabled actions are listed", func() { Example("ENABLED") })
	Attribute("type", String, "Google's ConversionActionType, verbatim: how the conversion is measured (e.g. WEBPAGE, UPLOAD_CLICKS)", func() { Example("WEBPAGE") })
	Attribute("category", String, "Google's ConversionActionCategory, verbatim: what the conversion means (e.g. SIGNUP, PURCHASE, LEAD)", func() { Example("SIGNUP") })
	Attribute("primary_for_goal", Boolean, "Whether the action counts toward the account's Conversions column and so drives bidding by default. A secondary action is still selectable.", func() { Example(true) })
	Required("id", "resource_name", "name", "status", "type", "category", "primary_for_goal")
})

// GoogleAdsConversionActions is the conversion-action read.
var GoogleAdsConversionActions = Type("google-ads-conversion-actions", func() {
	Attribute("account_id", String, "The Google Ads customer the actions were read from: the account a Google create for this project uses", func() { Example("1234567890") })
	Attribute("conversion_actions", ArrayOf(GoogleAdsConversionAction), "The customer's enabled conversion actions, sorted by name")
	Attribute("conversion_action_count", Int, "How many actions are in `conversion_actions`", func() { Example(1) })
	Required("account_id", "conversion_actions", "conversion_action_count")
})

// GoogleAdsAudienceResource is one entry of the audience read (#496).
var GoogleAdsAudienceResource = Type("google-ads-audience-resource", func() {
	Attribute("resource_name", String, "customers/{customerId}/{customAudiences|combinedAudiences|audiences|userInterests}/{id}, in exactly the form the create inputs accept: a custom_audience in googleAdsConfig.customAudiences (Display), an audience in demandGenAudience or performanceMaxAudienceSignal, a user_interest in userInterests (Display). No create input accepts a combined_audience today. The customer is the account a Google create for this project uses.", func() {
		Example("customers/8666746580/customAudiences/123")
	})
	Attribute("id", String, "The entry's numeric id (user_interest_id for a user_interest)", func() { Example("123") })
	Attribute("name", String, "The entry's display name. Not an identifier.", func() { Example("CNCF KubeCon NA") })
	Attribute("type", String, "The kind that was read, echoing the type parameter", func() {
		Enum("custom_audience", "combined_audience", "audience", "user_interest")
		Example("custom_audience")
	})
	Attribute("subtype", String, "Google's own classification, verbatim: custom_audience.type (AUTO, INTEREST, PURCHASE_INTENT, SEARCH) or user_interest.taxonomy_type (AFFINITY, IN_MARKET). Absent for combined_audience and audience, which have none.", func() { Example("SEARCH") })
	Attribute("status", String, "Always ENABLED for custom_audience, combined_audience and audience: only enabled entries are listed. Absent for user_interest, which has no status in Google Ads.", func() { Example("ENABLED") })
	Attribute("description", String, "The entry's description. Absent when Google sent none, and always for user_interest.", func() { Example("People searching for KubeCon") })
	Required("resource_name", "id", "name", "type")
})

// GoogleAdsAudienceResources is the audience read.
var GoogleAdsAudienceResources = Type("google-ads-audience-resources", func() {
	Attribute("account_id", String, "The Google Ads customer the entries were read from: the account a Google create for this project uses", func() { Example("8666746580") })
	Attribute("type", String, "The kind that was read", func() {
		Enum("custom_audience", "combined_audience", "audience", "user_interest")
		Example("custom_audience")
	})
	Attribute("audiences", ArrayOf(GoogleAdsAudienceResource), "Matching entries sorted by name (ignoring case), capped at 500 (see `truncated`)")
	Attribute("audience_count", Int, "How many entries are in `audiences`", func() { Example(1) })
	Attribute("truncated", Boolean, "True when more entries matched than were returned; narrow the search with q", func() { Example(false) })
	Required("account_id", "type", "audiences", "audience_count", "truncated")
})

// googleAdsLookupMethods declares the three reads inside the connections service. Called from
// that service's DSL so they share its security scheme and error types.
func googleAdsLookupMethods() {
	Method("suggest-google-ads-geo-targets", func() {
		Description("Search Google Ads geo target constants by place name or postal code, live from Google's " +
			"GeoTargetConstantService.SuggestGeoTargetConstants, for picking googleAdsConfig.geoTargets. " +
			"Returns Google's public location table, not data any ad account holds, so a project with no " +
			"Google Ads connection of its own reads through the shared LF system credential, exactly as " +
			"GET .../connection-google-ads/accounts does. A pure read: nothing is persisted.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Attribute("q", String, "Place name or postal code to search for", func() {
				MinLength(1)
				MaxLength(100)
				Pattern(`\S`)
				Example("Austin")
			})
			Attribute("country_code", String, "Optional ISO 3166-1 alpha-2 country to search within", func() {
				Pattern(`^[A-Za-z]{2}$`)
				Example("US")
			})
			Attribute("target_types", ArrayOf(String, func() { MinLength(1); MaxLength(40); Pattern(`[A-Za-z0-9]`) }), "Optional location types to keep (e.g. City, Postal Code). Matched ignoring case, spaces, underscores and hyphens, so postal_code matches Postal Code.", func() {
				MaxLength(10)
				Example([]string{"Postal Code"})
			})
			// No Default(): with one, Goa makes the field a non-pointer and its generated client
			// always sends limit, so an unset Limit goes out as limit=0 and is refused. The service
			// applies the default instead.
			Attribute("limit", Int, "Maximum suggestions to return (1-50). Defaults to 20 when omitted.", func() {
				Minimum(1)
				Maximum(googleAdsGeoSuggestMax)
				Example(20)
			})
			Required("project_id", "q")
		})
		Result(GoogleAdsGeoTargetSuggestions)
		Error("NotFound", NotFoundError, "Resource not found")
		authErrors()
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			GET("/projects/{project_id}/google-ads/geo-targets")
			Header("bearer_token:Authorization")
			connectionAuthErrorResponses()
			Param("q")
			Param("country_code")
			Param("target_types")
			Param("limit")
			Response(StatusOK)
			Response("NotFound", StatusNotFound)
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})

	Method("list-google-ads-user-lists", func() {
		Description("List the Google Ads user lists (audiences) on a customer, live from GAQL on user_list, for " +
			"picking googleAdsConfig.audienceSegments (Search and Display). account_id names the customer; omitted, " +
			"the customer the project's connection is bound to is read. Like the account monitor, this resolves " +
			"the project's OWN connection only and never falls back to the shared LF system credential: user " +
			"lists are account data on a customer shared across foundations, so a project with no Google Ads " +
			"connection of its own gets a 404, not the LF account's audiences. On a platform whose ad account is " +
			"shared across foundations — Google Ads today — the read is refused with 409 before Google is contacted: " +
			"the read is account-wide, this service records no user list per project, and a project's own " +
			"connection points at the same shared customer, so answering would expose every foundation's audiences. " +
			"Only a per-foundation customer is listed. An account_id the credential cannot reach is refused by " +
			"Google and answered 503, like the account monitor. A pure read: nothing is persisted.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Attribute("account_id", String, "Optional Google Ads customer to read (digits only). Defaults to the connection's account.", func() {
				Pattern(`^[0-9]+$`)
				MaxLength(64)
				Example("8666746580")
			})
			Required("project_id")
		})
		Result(GoogleAdsUserLists)
		Error("NotFound", NotFoundError, "Resource not found")
		Error("Conflict", UserListsConflictError, "User lists cannot be listed per project on an ad account shared across foundations")
		authErrors()
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			GET("/projects/{project_id}/google-ads/user-lists")
			Header("bearer_token:Authorization")
			connectionAuthErrorResponses()
			Param("account_id")
			Response(StatusOK)
			Response("NotFound", StatusNotFound)
			Response("Conflict", StatusConflict)
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})

	Method("list-google-ads-conversion-actions", func() {
		Description("List the ENABLED conversion actions on the Google Ads customer a Google create for this " +
			"project uses, live from GAQL on conversion_action, for picking googleAdsConfig.conversionActions. " +
			"Credentials resolve as for a create: the project's own connection, or the shared LF system " +
			"credential when it has none (or when the deployment forces the LF account). " +
			"The Google Ads customer is shared by every foundation, so the response lists EVERY " +
			"foundation's conversion action names, not only this project's. So, unlike every other " +
			"endpoint, it is gated on global_marketing_ops of {project_id} (the LF marketing team), not " +
			"campaign_manager, which also admits a foundation's executive director. {project_id} selects which " +
			"project's connection supplies the credential. More than 500 enabled actions on the customer fails the read with 503 " +
			"rather than truncating it, and that 503 does not clear on retry: the account itself holds " +
			"more actions than this endpoint publishes. " +
			"A pure read: the only upstream call is googleAds:search, and nothing is persisted.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Required("project_id")
		})
		Result(GoogleAdsConversionActions)
		Error("NotFound", NotFoundError, "Resource not found")
		authErrors()
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			GET("/projects/{project_id}/google-ads/conversion-actions")
			Header("bearer_token:Authorization")
			connectionAuthErrorResponses()
			Response(StatusOK)
			Response("NotFound", StatusNotFound)
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})

	Method("list-google-ads-audiences", func() {
		Description("List the custom audiences, combined audiences, Audience resources or user interests (one type per " +
			"call) on the Google Ads customer a Google create for this project uses, live from GAQL on custom_audience, " +
			"combined_audience, audience or user_interest, for the audience pickers (#496). Only usable entries are " +
			"listed: custom_audience, combined_audience and audience with status ENABLED; audience additionally with " +
			"scope CUSTOMER (an ASSET_GROUP-scoped Audience belongs to one asset group); user_interest restricted to " +
			"the AFFINITY and IN_MARKET taxonomies. q keeps entries whose " +
			"name contains it, ignoring case. Sorted by name, capped at 500 after the q filter, with `truncated` set " +
			"when more matched; the AFFINITY and IN_MARKET taxonomy is about a thousand entries, more than the cap, so a picker should search it " +
			"with q. More than 10000 entries of one type on the customer fails the read with 503. " +
			"Credentials resolve as for a create: the project's own connection, or the shared LF system " +
			"credential when it has none (or when the deployment forces the LF account). The Google Ads customer " +
			"is shared by every foundation, so the response lists EVERY foundation's audiences, not only this " +
			"project's; like list-google-ads-conversion-actions it is therefore gated on global_marketing_ops " +
			"of {project_id} (the LF marketing team), not campaign_manager, which also admits a foundation's " +
			"executive director. A pure read: the only upstream call is googleAds:search, and nothing is persisted.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Attribute("type", String, "Which kind to list", func() {
				Enum("custom_audience", "combined_audience", "audience", "user_interest")
				Example("custom_audience")
			})
			Attribute("q", String, "Optional case-insensitive name substring to keep", func() {
				MinLength(1)
				MaxLength(100)
				Pattern(`\S`)
				Example("KubeCon")
			})
			Required("project_id", "type")
		})
		Result(GoogleAdsAudienceResources)
		Error("NotFound", NotFoundError, "Resource not found")
		authErrors()
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			GET("/projects/{project_id}/google-ads/audiences")
			Header("bearer_token:Authorization")
			connectionAuthErrorResponses()
			Param("type")
			Param("q")
			Response(StatusOK)
			Response("NotFound", StatusNotFound)
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})
}
