// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package design — campaign-audience endpoints.
//
// Hierarchy: Project -> Brief -> Audiences. A built audience (the "B2" resource,
// epic LFXV2-2770) is a POINTER + provenance to an audience that physically lives in
// the platform (a HubSpot master contact list), NOT its contents — so it is a
// first-class, inspectable, reusable, versioned LFX resource. A brief may have
// several audiences (over time / per platform). Every endpoint is gated on
// campaign_manager at the gateway. See docs/api-catalog.md.
package design

import (
	//nolint:staticcheck // ST1001: the recommended way of using the goa DSL package is with the . import
	. "goa.design/goa/v3/dsl"
)

// ─── Audience types ───

// AudienceInput is the mutable audience payload (create/update). The build result
// (platform_master_list_id, suppression_list_ids, status) is set as the platform
// build progresses; inclusion_summary is human-readable provenance.
var AudienceInput = Type("audience-input", func() {
	Attribute("platform", String, "Platform the audience is built on", func() {
		Enum("hubspot")
	})
	Attribute("platform_master_list_id", String, "Pointer to the built master list in the platform (empty until built)")
	Attribute("suppression_list_ids", ArrayOf(String), "Platform suppression list ids applied to the master")
	Attribute("inclusion_summary", String, "Human-readable provenance: how the audience was built (past events, geo, topic)")
	Attribute("status", String, "Build lifecycle status", func() {
		Enum("building", "built", "failed")
	})
	Required("platform")
})

// AudienceUpdateInput is the PATCH payload. Unlike AudienceInput it has NO required
// fields: PATCH is a partial update where every field is optional (a nil field is left
// unchanged, an explicit empty list clears — see applyAudiencePatch). platform is
// deliberately absent: it is immutable, so the update handler never reads it; reusing
// AudienceInput here (where platform is Required) would force callers to resend the
// immutable platform on a status-only or suppression-only patch, breaking the
// "only supplied fields change" contract. It Reference()s AudienceInput so the shared
// mutable attributes inherit their type/validation/description from one source.
var AudienceUpdateInput = Type("audience-update-input", func() {
	Reference(AudienceInput)
	Attribute("platform_master_list_id")
	Attribute("suppression_list_ids")
	Attribute("inclusion_summary")
	Attribute("status")
	// No Required(): every field is optional for a partial update.
})

// Audience is the audience response view. It Reference()s AudienceInput so the
// shared attributes inherit their type/validation/description from one source of
// truth (a later change to the platform Enum or a validation rule flows here), then
// layers on the response-only fields.
var Audience = Type("audience", func() {
	Reference(AudienceInput)
	Attribute("id", String, "Audience UUID")
	Attribute("project_id", String, "Owning project")
	Attribute("brief_id", String, "Owning brief")
	// Inherited from AudienceInput via Reference (name-only Attribute calls).
	Attribute("platform")
	Attribute("platform_master_list_id")
	Attribute("suppression_list_ids")
	Attribute("inclusion_summary")
	Attribute("status")
	// Response-only fields.
	Attribute("version", Int64, "Optimistic-concurrency version")
	Attribute("etag", String, "ETag header value (mirrors version)")
	Required("id", "project_id", "brief_id", "platform", "status", "version")
})

// audienceIDAttr declares the audience path parameter.
func audienceIDAttr() {
	Attribute("audience_id", String, "Audience UUID", func() { Format(FormatUUID) })
}

// ─── Audiences service ───

var _ = Service("lfx-v2-campaign-service-audiences", func() {
	Description("Manage built campaign audiences (a pointer + provenance to a platform-side audience), subordinate to a brief.")

	Security(JWTAuth)

	Method("create-audience", func() {
		Description("Create a built-audience record under a brief.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			briefIDAttr()
			Attribute("audience", AudienceInput)
			Required("project_id", "brief_id", "audience")
		})
		Result(Audience)
		commonBriefErrors(true)
		HTTP(func() {
			POST("/projects/{project_id}/briefs/{brief_id}/audiences")
			Header("bearer_token:Authorization")
			Response(StatusCreated, func() { Header("etag:ETag") })
			briefErrorResponses(true)
		})
	})

	Method("get-audience", func() {
		Description("Get one audience under a brief; returns ETag.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			briefIDAttr()
			audienceIDAttr()
			Required("project_id", "brief_id", "audience_id")
		})
		Result(Audience)
		commonBriefErrors(false)
		HTTP(func() {
			GET("/projects/{project_id}/briefs/{brief_id}/audiences/{audience_id}")
			Header("bearer_token:Authorization")
			Response(StatusOK, func() { Header("etag:ETag") })
			briefErrorResponses(false)
		})
	})

	Method("list-audiences", func() {
		Description("List a brief's audiences (newest first).")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			briefIDAttr()
			Required("project_id", "brief_id")
		})
		Result(func() {
			Attribute("audiences", ArrayOf(Audience))
			Required("audiences")
		})
		commonBriefErrors(false)
		HTTP(func() {
			GET("/projects/{project_id}/briefs/{brief_id}/audiences")
			Header("bearer_token:Authorization")
			Response(StatusOK)
			briefErrorResponses(false)
		})
	})

	Method("update-audience", func() {
		Description("Partially update an audience's build result/status (requires If-Match; only supplied fields change).")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			briefIDAttr()
			audienceIDAttr()
			ifMatchAttr()
			// AudienceUpdateInput (not AudienceInput): a PATCH must not require the
			// immutable platform field, so a status-only/suppression-only patch works.
			Attribute("audience", AudienceUpdateInput)
			Required("project_id", "brief_id", "audience_id", "audience")
		})
		Result(Audience)
		commonBriefErrors(true)
		Error("PreconditionFailed", PreconditionFailedError, "ETag mismatch")
		Error("PreconditionRequired", PreconditionRequiredError, "If-Match header required")
		HTTP(func() {
			PATCH("/projects/{project_id}/briefs/{brief_id}/audiences/{audience_id}")
			Header("bearer_token:Authorization")
			Header("if_match:If-Match")
			Response(StatusOK, func() { Header("etag:ETag") })
			briefErrorResponses(true)
			Response("PreconditionFailed", StatusPreconditionFailed)
			Response("PreconditionRequired", StatusPreconditionRequired)
		})
	})
})
