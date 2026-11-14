// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package design

import (
	//nolint:staticcheck // ST1001: the recommended way of using the goa DSL package is with the . import
	. "goa.design/goa/v3/dsl"
)

// Lookups for the Microsoft create form (#563 gap 4 follow-up). microsoftConfig.audienceIds and
// excludedAudienceIds take Microsoft remarketing list ids; this read lets a picker offer them
// instead of making an operator copy them out of Microsoft Advertising. Nothing is persisted.

// MicrosoftAdsRemarketingList is one remarketing list on a Microsoft Advertising account.
var MicrosoftAdsRemarketingList = Type("microsoft-ads-remarketing-list", func() {
	Attribute("id", String, "The remarketing list id, in exactly the form microsoftConfig.audienceIds and excludedAudienceIds accept. Send it as-is.", func() {
		Pattern(`^[1-9][0-9]{0,18}$`)
		Example("8394012")
	})
	Attribute("name", String, "The list's display name. Not an identifier; may be empty.", func() { Example("KubeCon NA site visitors 30d") })
	Attribute("description", String, "The list's description, verbatim; may be empty.", func() { Example("Visitors of the KubeCon NA site in the last 30 days") })
	Attribute("scope", String, "Microsoft's Scope, verbatim (Account or Customer); may be empty.", func() { Example("Account") })
	Attribute("membership_duration", Int, "Microsoft's MembershipDuration, in days. Absent when Microsoft sent none.", func() { Example(30) })
	Attribute("search_size", Int64, "Microsoft's SearchSize: the list members reachable on Search. Absent when Microsoft sent none, which is not a size of 0.", func() { Example(12000) })
	Required("id", "name", "description", "scope")
})

// MicrosoftAdsRemarketingLists is the remarketing-list read.
var MicrosoftAdsRemarketingLists = Type("microsoft-ads-remarketing-lists", func() {
	Attribute("account_id", String, "The Microsoft Advertising account the lists were read from: the account a Microsoft create for this project uses", func() { Example("1234567") })
	Attribute("lists", ArrayOf(MicrosoftAdsRemarketingList), "The account's remarketing lists, sorted by name (ignoring case), then id", func() {
		// Explicit, so the published example has one list matching list_count: 1.
		Example([]map[string]any{{"id": "8394012", "name": "KubeCon NA site visitors 30d", "description": "", "scope": "Account", "membership_duration": 30, "search_size": 12000}})
	})
	Attribute("list_count", Int, "How many lists are in `lists`", func() { Example(1) })
	Required("account_id", "lists", "list_count")
})

// microsoftAdsLookupMethods declares the Microsoft create-form reads inside the connections
// service, next to twitterAdsLookupMethods, so they share its security scheme and error types.
func microsoftAdsLookupMethods() {
	Method("list-microsoft-ads-remarketing-lists", func() {
		Description("List the remarketing lists on the Microsoft Advertising account a Microsoft create for this " +
			"project uses, live from Campaign Management v13 GetAudiencesByIds (POST Audiences/QueryByIds with no " +
			"ids and Type RemarketingList, which returns every list of the type), for picking " +
			"microsoftConfig.audienceIds and excludedAudienceIds (#563). The same call, with ids, is the check a " +
			"create makes before anything is created, so every id listed is one it accepts. System lists (All " +
			"Visitors, All Converters, Smart lists) are not returned by that call and are not listed. Sorted by " +
			"name, ignoring case, then id. Credentials resolve as for a create: the project's own Microsoft " +
			"connection, or the LF system account when the deployment forces it. The Microsoft Advertising account " +
			"is shared by every foundation, so the response lists EVERY foundation's lists; like " +
			"list-twitter-ads-custom-audiences it is therefore gated on global_marketing_ops of {project_id} (the LF " +
			"marketing team), not campaign_manager, which also admits a foundation's executive director. Disabled " +
			"(400 not supported) unless MICROSOFT_REMARKETING_LISTS_ENABLED is \"true\": the read is built from the " +
			"v13 reference and has not been exercised against a live account. An answer this service cannot " +
			"believe (no Audiences array, a null entry, an unusable or repeated id, an entry that is not a " +
			"remarketing list, any partial error) is 503 with nothing partial. A pure read: nothing is persisted.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Required("project_id")
		})
		Result(MicrosoftAdsRemarketingLists)
		Error("NotFound", NotFoundError, "Resource not found")
		authErrors()
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			GET("/projects/{project_id}/microsoft-ads/remarketing-lists")
			Header("bearer_token:Authorization")
			connectionAuthErrorResponses()
			Response(StatusOK)
			Response("NotFound", StatusNotFound)
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})
}
