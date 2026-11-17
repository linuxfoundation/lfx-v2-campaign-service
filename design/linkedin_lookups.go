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
	"account to list instead: the resolved account itself, or, while LINKEDIN_ADS_ACCOUNT_SELECTION_ENABLED is on, one " +
	"of the LF system account's operator-installed grants (the accounts linkedInConfig.adAccountId may name); any " +
	"other account is a 400. " +
	"A pure read: nothing is persisted."

// linkedInLookupAccountAttr is the optional account_id both pickers take (#581).
func linkedInLookupAccountAttr() {
	Attribute("account_id", String, "The LinkedIn ad account to list. Optional: omitted, the account a create for this project uses; otherwise that account or, while LINKEDIN_ADS_ACCOUNT_SELECTION_ENABLED is on, one of the LF system account's granted accounts.", func() {
		Pattern(`^[0-9]+$`)
		MaxLength(64)
		Example("500928401")
	})
}

// LinkedInAdsAdoptableCampaign is one campaign of the adoption candidate list (#581).
var LinkedInAdsAdoptableCampaign = Type("linkedin-ads-adoptable-campaign", func() {
	Attribute("id", String, "The LinkedIn campaign id, in exactly the form adopt-campaign's platform_campaign_id takes", func() { Example("312345678") })
	Attribute("name", String, "The campaign's name in Campaign Manager. Not an identifier.", func() { Example("KubeCon NA 2026 — Registrations") })
	Attribute("status", String, "LinkedIn's campaign status, verbatim: ACTIVE or PAUSED (only those are listed)", func() { Example("ACTIVE") })
	Attribute("recorded_in_project", Boolean, "true when this project already has a live campaign row for this campaign id (adopted or created here), so its monitor already shows it. false does not mean adoptable: a campaign bound in ANOTHER project is still refused by adopt-campaign (409).", func() { Example(false) })
	Required("id", "name", "status", "recorded_in_project")
})

// LinkedInAdsAdoptableCampaigns is the adoption candidate list (#581).
var LinkedInAdsAdoptableCampaigns = Type("linkedin-ads-adoptable-campaigns", func() {
	Attribute("account_id", String, "The LinkedIn ad account the campaigns were read from", func() { Example("500928401") })
	Attribute("campaigns", ArrayOf(LinkedInAdsAdoptableCampaign), "The account's ACTIVE and PAUSED campaigns, in the order LinkedIn returned them")
	Attribute("adoptable_count", Int, "How many campaigns are in `campaigns`", func() { Example(1) })
	Required("account_id", "campaigns", "adoptable_count")
})

// LinkedInAdsSelectableAccount is one account linkedInConfig.adAccountId may name (#581).
var LinkedInAdsSelectableAccount = Type("linkedin-ads-selectable-account", func() {
	Attribute("account_id", String, "The LinkedIn ad account, in exactly the form linkedInConfig.adAccountId takes", func() { Example("500928401") })
	Attribute("org_id", String, "The organization page a campaign created in this account is authored by: the connection's org for the default account, the grant's org for a grant. Empty when the connection stores none.", func() { Example("12893459") })
	Attribute("label", String, "Display text: the connection's label for the default account, the operator-set grant label otherwise. Not an identifier.", func() { Example("CNCF") })
	Attribute("default", Boolean, "true for the account a create uses when linkedInConfig.adAccountId is omitted", func() { Example(false) })
	Required("account_id", "org_id", "label", "default")
})

// LinkedInAdsCreateAccount reports the LinkedIn ad account a create for the project resolves to (#581).
var LinkedInAdsCreateAccount = Type("linkedin-ads-create-account", func() {
	Attribute("account_id", String, "The ad account a LinkedIn create for this project is sent to when linkedInConfig.adAccountId is omitted", func() { Example("538170226") })
	Attribute("org_id", String, "The organization page that create's posts are authored by. Empty when the connection stores none.", func() { Example("208777") })
	Attribute("label", String, "The resolved connection's label. Not an identifier.", func() { Example("Linux Foundation system account") })
	Attribute("source", String, "Which connection a create resolves to: `project` (the project's own LinkedIn connection) or `system` (the LF system connection, the fallback for a project with none, or every project while the deployment forces the system account)", func() {
		Enum("project", "system")
		Example("system")
	})
	Attribute("status", String, "The resolved connection's stored status. A create is refused unless it is `active`.", func() { Example("active") })
	Attribute("account_selection_enabled", Boolean, "Whether LINKEDIN_ADS_ACCOUNT_SELECTION_ENABLED is on, so linkedInConfig.adAccountId may name one of the LF system account's granted accounts", func() { Example(true) })
	Attribute("selectable_accounts", ArrayOf(LinkedInAdsSelectableAccount), "The accounts linkedInConfig.adAccountId may name, the default first. Only the default unless source is `system` and account selection is on, when the system account's grants follow it. While account selection is on, naming any account requires global_marketing_ops of the project.")
	Required("account_id", "org_id", "label", "source", "status", "account_selection_enabled", "selectable_accounts")
})

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

	Method("list-linkedin-ads-adoptable-campaigns", func() {
		Description("List the ACTIVE and PAUSED campaigns in one of the LF system LinkedIn account's addressable ad " +
			"accounts (its default account or an operator-installed grant), as candidates for adopt-campaign with " +
			"account_id (#581), so a campaign built by hand in Campaign Manager can be bound to a brief and shown in " +
			"this project's monitor. Read through the LF system LinkedIn connection, whatever connection the project " +
			"has. Gated on global_marketing_ops of {project_id} by the marketing-ops-api gateway rule, because it lists " +
			"campaigns of every foundation in the account. Answers 400 unless LINKEDIN_ADS_ACCOUNT_SELECTION_ENABLED is " +
			"on, and 400 for an account the system row cannot address (nothing is sent). A missing or unusable LF system " +
			"LinkedIn connection is 500, an operator's fault, and so is an expired or revoked LF system token or a " +
			"rejected LF application credential, or the LF row's credential cannot be decrypted: only an operator can " +
			"repair the LF row. A transient LinkedIn " +
			"failure is 503. The campaign list pages LinkedIn's search finder 100 at a time, up to 50 pages, and fails with 503 beyond " +
			"that or on a page it cannot trust: a repeated key, a campaign without a valid id, a non-blank name or an " +
			"ACTIVE or PAUSED status, or a campaign listed twice. A pure read: nothing is persisted.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Attribute("account_id", String, "The LinkedIn ad account to list: the LF system account's default account or one of its grants", func() {
				Pattern(`^[0-9]+$`)
				MaxLength(64)
				Example("500928401")
			})
			Required("project_id", "account_id")
		})
		Result(LinkedInAdsAdoptableCampaigns)
		Error("NotFound", NotFoundError, "Resource not found")
		authErrors()
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			GET("/projects/{project_id}/linkedin-ads/adoptable-campaigns")
			Header("bearer_token:Authorization")
			connectionAuthErrorResponses()
			Param("account_id")
			Response(StatusOK)
			Response("NotFound", StatusNotFound)
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})

	Method("get-linkedin-ads-create-account", func() {
		Description("Report the LinkedIn ad account and organization a create for {project_id} actually uses, and the " +
			"accounts linkedInConfig.adAccountId may name (#581), so the Implement tab can show the real account instead " +
			"of a list the create ignores. Resolved exactly as a create resolves: the project's own LinkedIn connection, " +
			"else the LF system connection (also for every project while the deployment forces the system account). A " +
			"database read of stored, non-secret ids: nothing is sent to LinkedIn, so the answer says nothing about the " +
			"account's billing or serving state. Gated on campaign_manager of {project_id}, like the connection's own " +
			"accounts read. 404 when neither the project nor the LF system account has a LinkedIn connection, or the " +
			"project disconnected its own; 500 when the LF system connection the project falls back to is unusable. " +
			"A pure read: nothing is persisted.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Required("project_id")
		})
		Result(LinkedInAdsCreateAccount)
		Error("NotFound", NotFoundError, "Resource not found")
		authErrors()
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			GET("/projects/{project_id}/linkedin-ads/create-account")
			Header("bearer_token:Authorization")
			connectionAuthErrorResponses()
			Response(StatusOK)
			Response("NotFound", StatusNotFound)
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})
}
