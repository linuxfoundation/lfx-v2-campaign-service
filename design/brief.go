// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package design — brief and campaign endpoints.
//
// Hierarchy: Project -> Brief -> Campaigns. A brief is the funnel unit
// (carries program_type) and is shared across channels; campaigns are a
// collection subordinate to the brief. Campaign creation is asynchronous:
// POST returns a job to poll. Every endpoint is gated on campaign_manager at
// the gateway. See docs/api-catalog.md.
package design

import (
	//nolint:staticcheck // ST1001: the recommended way of using the goa DSL package is with the . import
	. "goa.design/goa/v3/dsl"
)

// ─── Brief types ───

// BriefInput is the mutable brief payload (create/replace).
var BriefInput = Type("brief-input", func() {
	Attribute("program_type", String, "Funnel context", func() {
		Enum("events", "education", "membership")
	})
	Attribute("event_slug", String, "Event/course slug (unique within the project)")
	Attribute("url", String, "Event/course page URL")
	Attribute("platforms", ArrayOf(String), "Suggested default platforms (a planning hint; binding selection is on the campaign)")
	Attribute("event_details", Any, "Extracted event/course details")
	Attribute("copy", Any, "Ad copy")
	Attribute("keywords", Any, "Keyword list")
	Attribute("targeting", Any, "Targeting recommendation")
	Required("program_type", "event_slug")
})

// Brief is the brief response view.
var Brief = Type("brief", func() {
	Attribute("id", String, "Brief UUID")
	Attribute("project_id", String, "Owning project")
	Attribute("program_type", String, "Funnel context")
	Attribute("event_slug", String, "Event/course slug")
	Attribute("url", String, "Event/course page URL")
	Attribute("platforms", ArrayOf(String), "Suggested default platforms")
	Attribute("event_details", Any, "Extracted event/course details")
	Attribute("copy", Any, "Ad copy")
	Attribute("keywords", Any, "Keyword list")
	Attribute("targeting", Any, "Targeting recommendation")
	Attribute("status", String, "Lifecycle status", func() {
		Enum("draft", "approved", "archived")
	})
	Attribute("version", Int64, "Optimistic-concurrency version")
	Attribute("etag", String, "ETag header value (mirrors version)")
	Required("id", "project_id", "program_type", "event_slug", "status", "version")
})

// ─── Campaign / job types ───

// CampaignCreateInput selects the platforms to launch on and their config.
var CampaignCreateInput = Type("campaign-create-input", func() {
	Attribute("platforms", ArrayOf(String, func() {
		// Constrain to the known providers so OpenAPI clients can discover the
		// valid values and Goa advertises them; the service also revalidates.
		Enum("google-ads", "linkedin-ads", "meta-ads", "reddit-ads", "twitter-ads", "microsoft-ads", "hubspot")
	}), "Platforms to create campaigns on (binding selection)", func() {
		// Reject an empty array in the schema (the handler also rejects it). Note:
		// Goa/OpenAPI can't express uniqueItems, so duplicate rejection stays in
		// the handler.
		MinLength(1)
	})
	Attribute("config", Any, "Per-platform campaign configuration")
	Required("platforms")
})

// JobCreateResponse is returned immediately from POST .../campaigns.
var JobCreateResponse = Type("job-create-response", func() {
	Attribute("job_id", String, "Poll GET /projects/{projectId}/jobs/{jobId}")
	Attribute("status", String, "Initial status (always 'queued' on create)", func() {
		Enum("queued")
	})
	Attribute("platforms", ArrayOf(String), "Platforms this job will create on")
	Required("job_id", "status", "platforms")
})

// PlatformResult is one platform's outcome within a terminal job result. It
// mirrors exactly what the orchestrator emits so the OpenAPI can describe the
// result array instead of an opaque Any.
var PlatformResult = Type("platform-result", func() {
	Attribute("platform", String, "Platform this result is for")
	Attribute("ok", Boolean, "Whether the campaign was created (or reused) successfully")
	Attribute("campaign_id", String, "Upstream platform campaign id (present when ok)")
	Attribute("error", String, "Failure reason (present when not ok)")
	Required("platform", "ok")
})

// JobPollResponse is returned from GET .../jobs/{jobId}.
var JobPollResponse = Type("job-poll-response", func() {
	Attribute("job_id", String, "Job UUID")
	Attribute("status", String, "Job status", func() {
		Enum("queued", "running", "succeeded", "partial", "failed")
	})
	Attribute("result", ArrayOf(PlatformResult), "Per-platform results, written once when the job reaches a terminal state")
	Attribute("error", String, "Terminal error, if any")
	Required("job_id", "status")
})

// Campaign is a single platform's campaign under a brief.
var Campaign = Type("campaign", func() {
	Attribute("id", String, "Campaign UUID")
	Attribute("project_id", String, "Owning project")
	Attribute("brief_id", String, "Parent brief")
	Attribute("platform", String, "Channel")
	Attribute("platform_campaign_id", String, "ID returned by the ad platform")
	Attribute("campaign_name", String, "Campaign name")
	Attribute("status", String, "Campaign status")
	Attribute("version", Int64, "Optimistic-concurrency version")
	Attribute("etag", String, "ETag header value (mirrors version)")
	Required("id", "project_id", "brief_id", "platform", "campaign_name", "status", "version")
})

// CampaignUpdateInput is the mutable campaign payload (replace).
var CampaignUpdateInput = Type("campaign-update-input", func() {
	Attribute("campaign_name", String, "Campaign name")
	Attribute("status", String, "Campaign status")
	Attribute("config", Any, "Campaign configuration snapshot")
	Required("campaign_name", "status")
})

// ─── Brief + campaign service ───

var _ = Service("lfx-v2-campaign-service-briefs", func() {
	Description("Manage campaign briefs and their subordinate platform campaigns, including async multi-platform creation.")

	Security(JWTAuth)

	Method("create-brief", func() {
		Description("Create a brief.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Attribute("brief", BriefInput)
			Required("project_id", "brief")
		})
		Result(Brief)
		commonBriefErrors(true)
		HTTP(func() {
			POST("/projects/{project_id}/briefs")
			Header("bearer_token:Authorization")
			Response(StatusCreated, func() { Header("etag:ETag") })
			briefErrorResponses(true)
		})
	})

	Method("get-brief", func() {
		Description("Get a brief; returns ETag.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			briefIDAttr()
			Required("project_id", "brief_id")
		})
		Result(Brief)
		commonBriefErrors(false)
		HTTP(func() {
			GET("/projects/{project_id}/briefs/{brief_id}")
			Header("bearer_token:Authorization")
			Response(StatusOK, func() { Header("etag:ETag") })
			briefErrorResponses(false)
		})
	})

	Method("update-brief", func() {
		Description("Replace a brief (requires If-Match).")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			briefIDAttr()
			ifMatchAttr()
			Attribute("brief", BriefInput)
			Required("project_id", "brief_id", "brief")
		})
		Result(Brief)
		commonBriefErrors(true)
		Error("PreconditionFailed", PreconditionFailedError, "ETag mismatch")
		Error("PreconditionRequired", PreconditionRequiredError, "If-Match header required")
		HTTP(func() {
			PUT("/projects/{project_id}/briefs/{brief_id}")
			Header("bearer_token:Authorization")
			Header("if_match:If-Match")
			Response(StatusOK, func() { Header("etag:ETag") })
			briefErrorResponses(true)
			Response("PreconditionFailed", StatusPreconditionFailed)
			Response("PreconditionRequired", StatusPreconditionRequired)
		})
	})

	Method("approve-brief", func() {
		Description("Approve a brief for campaign creation.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			briefIDAttr()
			// Reuse the shared If-Match attribute (carries Example("3")) so Goa
			// generates a valid numeric CLI example instead of a prose placeholder
			// that parseBriefIfMatch would reject.
			ifMatchAttr()
			Required("project_id", "brief_id")
		})
		Result(Brief)
		commonBriefErrors(false)
		Error("PreconditionFailed", PreconditionFailedError, "ETag mismatch")
		Error("PreconditionRequired", PreconditionRequiredError, "If-Match header required")
		HTTP(func() {
			POST("/projects/{project_id}/briefs/{brief_id}/approve")
			Header("bearer_token:Authorization")
			Header("if_match:If-Match")
			Response(StatusOK, func() { Header("etag:ETag") })
			briefErrorResponses(false)
			Response("PreconditionFailed", StatusPreconditionFailed)
			Response("PreconditionRequired", StatusPreconditionRequired)
		})
	})

	Method("delete-brief", func() {
		Description("Archive a brief (soft delete).")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			briefIDAttr()
			Required("project_id", "brief_id")
		})
		commonBriefErrors(false)
		HTTP(func() {
			DELETE("/projects/{project_id}/briefs/{brief_id}")
			Header("bearer_token:Authorization")
			Response(StatusNoContent)
			briefErrorResponses(false)
		})
	})

	Method("create-campaigns", func() {
		Description("Create campaigns across the selected platforms (async -> job).")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			briefIDAttr()
			Attribute("input", CampaignCreateInput)
			Required("project_id", "brief_id", "input")
		})
		Result(JobCreateResponse)
		commonBriefErrors(true)
		HTTP(func() {
			POST("/projects/{project_id}/briefs/{brief_id}/campaigns")
			Header("bearer_token:Authorization")
			Response(StatusAccepted)
			briefErrorResponses(true)
		})
	})

	Method("get-campaign", func() {
		Description("Get one campaign under a brief; returns ETag.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			briefIDAttr()
			campaignIDAttr()
			Required("project_id", "brief_id", "campaign_id")
		})
		Result(Campaign)
		commonBriefErrors(false)
		HTTP(func() {
			GET("/projects/{project_id}/briefs/{brief_id}/campaigns/{campaign_id}")
			Header("bearer_token:Authorization")
			Response(StatusOK, func() { Header("etag:ETag") })
			briefErrorResponses(false)
		})
	})

	Method("update-campaign", func() {
		Description("Replace a campaign (requires If-Match).")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			briefIDAttr()
			campaignIDAttr()
			ifMatchAttr()
			Attribute("campaign", CampaignUpdateInput)
			Required("project_id", "brief_id", "campaign_id", "campaign")
		})
		Result(Campaign)
		commonBriefErrors(true)
		Error("PreconditionFailed", PreconditionFailedError, "ETag mismatch")
		Error("PreconditionRequired", PreconditionRequiredError, "If-Match header required")
		HTTP(func() {
			PUT("/projects/{project_id}/briefs/{brief_id}/campaigns/{campaign_id}")
			Header("bearer_token:Authorization")
			Header("if_match:If-Match")
			Response(StatusOK, func() { Header("etag:ETag") })
			briefErrorResponses(true)
			Response("PreconditionFailed", StatusPreconditionFailed)
			Response("PreconditionRequired", StatusPreconditionRequired)
		})
	})

	Method("get-job", func() {
		Description("Poll campaign-creation job status.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Attribute("job_id", String, "Job UUID", func() { Format(FormatUUID) })
			Required("project_id", "job_id")
		})
		Result(JobPollResponse)
		commonBriefErrors(false)
		HTTP(func() {
			GET("/projects/{project_id}/jobs/{job_id}")
			Header("bearer_token:Authorization")
			Response(StatusOK)
			briefErrorResponses(false)
		})
	})
})

// ─── shared DSL helpers for briefs ───

func briefIDAttr() {
	Attribute("brief_id", String, "Brief UUID", func() { Format(FormatUUID) })
}

func campaignIDAttr() {
	Attribute("campaign_id", String, "Campaign UUID", func() { Format(FormatUUID) })
}

// commonBriefErrors declares the standard error set for a brief method.
// BadRequest is always declared: JWTAuth can reject any method with a 400
// (malformed/invalid token) regardless of whether the method takes a body, so
// every method's generated encoder must handle it. The withBadRequest parameter
// is retained for call-site readability (true = the method also validates a body)
// but no longer gates BadRequest.
func commonBriefErrors(withBadRequest bool) {
	_ = withBadRequest
	Error("BadRequest", BadRequestError, "Bad request")
	Error("NotFound", NotFoundError, "Resource not found")
	Error("Conflict", ConflictError, "Conflict")
	Error("InternalServerError", InternalServerError, "Internal server error")
	Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
}

// briefErrorResponses maps the standard errors to HTTP responses. BadRequest is
// always mapped to match commonBriefErrors.
func briefErrorResponses(withBadRequest bool) {
	_ = withBadRequest
	Response("BadRequest", StatusBadRequest)
	Response("NotFound", StatusNotFound)
	Response("Conflict", StatusConflict)
	Response("InternalServerError", StatusInternalServerError)
	Response("ServiceUnavailable", StatusServiceUnavailable)
}
