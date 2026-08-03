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
	// NO MinLength here: BriefInput is Reference()d by the Brief RESPONSE type, and goa copies
	// validations through Reference — so constraining it here also constrains every brief
	// response, making an already-persisted empty-slug row undecodable by generated clients
	// (get-brief included). The empty-slug REQUEST is rejected by nonEmptyEventSlug() on the
	// create/update payloads instead, which is where the constraint belongs.
	Attribute("event_slug", String, "Event/course slug (unique within the project)")
	Attribute("url", String, "Event/course page URL")
	Attribute("platforms", ArrayOf(String), "Suggested default platforms (a planning hint; binding selection is on the campaign)")
	Attribute("event_details", Any, "Extracted event/course details")
	Attribute("copy", Any, "Ad copy")
	Attribute("keywords", Any, "Keyword list")
	Attribute("targeting", Any, "Targeting recommendation")
	Required("program_type", "event_slug")
})

// BriefWriteInput is the CREATE/UPDATE payload. It exists solely to carry MinLength(1) on
// event_slug WITHOUT that constraint reaching the response type.
//
// goa's Required() only checks that the JSON key is present, so an explicit "" satisfies it and
// the TEXT NOT NULL column accepts it — a brief with an empty slug was creatable, occupied the
// UNIQUE(project_id, event_slug) index, and could never be recalled through find-brief (whose
// own MinLength(1) rejects the request with a 400 rather than the documented 404/200).
//
// It cannot live on BriefInput: the Brief RESPONSE type Reference()s BriefInput and goa copies
// validations through Reference, so any already-persisted empty-slug row would become
// undecodable by generated clients — breaking reads for exactly the rows this prevents going
// forward. Requests reject empty slugs; responses stay readable.
var BriefWriteInput = Type("brief-write-input", func() {
	Reference(BriefInput)
	Attribute("program_type")
	Attribute("event_slug", String, "Event/course slug (unique within the project)", func() {
		MinLength(1)
	})
	Attribute("url")
	Attribute("platforms")
	Attribute("event_details")
	Attribute("copy")
	Attribute("keywords")
	Attribute("targeting")
	Required("program_type", "event_slug")
})

// Brief is the brief response view. It Reference()s BriefInput so the eight
// shared attributes (program_type, event_slug, url, platforms, event_details,
// copy, keywords, targeting) inherit their type/validation/description from a
// single source of truth — a later change to BriefInput's program_type Enum or a
// validation rule flows here automatically, so the two can't silently drift.
// Brief then layers on its response-only fields.
var Brief = Type("brief", func() {
	Reference(BriefInput)
	Attribute("id", String, "Brief UUID")
	Attribute("project_id", String, "Owning project")
	// Inherited from BriefInput via Reference (name-only Attribute calls).
	Attribute("program_type")
	// event_slug is redeclared here (NOT inherited via Reference) so the CREATE constraint
	// MinLength(1) does not leak into the RESPONSE validator. Reference() copies validations,
	// and the response type is shared by every brief-returning method — so inheriting it would
	// make any already-persisted empty-slug row undecodable by generated clients, breaking even
	// get-brief for exactly the rows the create-side fix is meant to prevent going forward.
	// Requests reject empty slugs; responses stay readable for legacy data.
	Attribute("event_slug", String, "Event/course slug (unique within the project)")
	Attribute("url")
	Attribute("platforms")
	Attribute("event_details")
	Attribute("copy")
	Attribute("keywords")
	Attribute("targeting")
	// Response-only fields.
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
		// Pin a deterministic example that MATCHES the config envelope example below
		// (redditConfig + metaConfig). Without it Goa auto-picks an enum value (e.g.
		// linkedin-ads), producing a published example whose platforms don't match the
		// supplied config — a copyable request that fails asynchronously.
		Example([]string{"reddit-ads", "meta-ads"})
	})
	Attribute("config", Any, "Per-platform campaign configuration", func() {
		// config is an OBJECT ENVELOPE keyed by per-platform config name
		// (redditConfig / linkedInConfig / metaConfig / twitterConfig — plus the
		// top-level hsToken sibling), NOT a string. Goa renders an Any without an
		// example as an empty-string example, which a consumer would copy and then
		// hit "cannot unmarshal string into map" after the 202. Publish a deterministic
		// OBJECT example (matching the reddit-ads + meta-ads platforms example above) so
		// the copyable contract is a valid envelope.
		Example(map[string]any{
			"hsToken": "hs-abc123",
			"redditConfig": map[string]any{
				"budgetUsd": 50, "startDate": "2099-08-01", "endDate": "2099-08-31",
				"objective": "traffic", "subreddits": []string{"kubernetes"},
				"postUrl": "t3_abc123", "variants": []map[string]any{{"headline": "Join us"}},
			},
			"metaConfig": map[string]any{
				"budget": 2500, "startDate": "2099-08-01", "endDate": "2099-08-31",
				"objective": "traffic", "geoTargets": []string{"US"},
				"variants": []map[string]any{{"primaryText": "Join us at KubeCon", "headline": "KubeCon 2099"}},
			},
		})
	})
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
	Attribute("campaign_id", String, "Upstream platform campaign id. Present when ok; also set on the specific failure where the upstream campaign was created but recording it failed, so the orphaned id isn't lost.")
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
			// Slug-only on CREATE: project_id becomes the campaign-name attribution key,
			// so a UUID here would break the slug-based join (projectSlugAttr rejects it).
			projectSlugAttr()
			Attribute("brief", BriefWriteInput)
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

	Method("find-brief", func() {
		Description("Find the saved brief for an event slug. Returns 404 when the event has no brief yet, which is the ordinary first-time-generation case — the caller then generates one and POSTs it to create-brief.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			// NO MaxLength: BriefInput.event_slug is uncapped and the column is unbounded
			// TEXT, so any cap here would make a brief the create contract accepted
			// permanently unrecallable — the caller would get a validation error instead of
			// its saved brief, then collide on re-create.
			//
			// MinLength(1) here is SAFE because BriefInput.event_slug now carries the same
			// constraint. It did not originally: goa's Required() only checks that the JSON
			// key is present, so an explicit "" was creatable and would then have been
			// unrecallable through this endpoint — a 400 instead of the documented 404/200.
			// The two contracts must stay in sync; loosening either one reopens that gap.
			Attribute("event_slug", String, "Event slug derived from the event page URL.", func() {
				Example("kubecon-eu-2026")
				MinLength(1)
			})
			Required("project_id", "event_slug")
		})
		Result(Brief)
		commonBriefErrors(false)
		HTTP(func() {
			// A query param, not a path segment: the slug is caller-derived free text, and
			// a lookup that legitimately MISSES is the common case (404 = generate one).
			GET("/projects/{project_id}/briefs")
			Param("event_slug")
			Header("bearer_token:Authorization")
			Response(StatusOK, func() { Header("etag:ETag") })
			briefErrorResponses(false)
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
			Attribute("brief", BriefWriteInput)
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
			// Slug-only on CREATE: project_id is stamped into the campaign name and is the
			// exact-match key for the dispatch connection lookup — a UUID breaks both.
			projectSlugAttr()
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

	Method("toggle-campaign-status", func() {
		Description("Pause or resume a campaign on its ad platform (ACTIVE↔PAUSED), then persist the new status. Unlike update-campaign (which only writes the DB row), this dispatches the status change to the platform and updates the row only after the platform confirms. Support is per-platform: a campaign whose platform has no status-toggle dispatcher wired returns 400 (Reddit is wired in this change; other platforms follow).")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			briefIDAttr()
			campaignIDAttr()
			ifMatchAttr()
			Attribute("status", String, "Desired run state", func() { Enum("active", "paused") })
			Required("project_id", "brief_id", "campaign_id", "status")
		})
		Result(Campaign)
		commonBriefErrors(true)
		Error("PreconditionFailed", PreconditionFailedError, "ETag mismatch")
		Error("PreconditionRequired", PreconditionRequiredError, "If-Match header required")
		HTTP(func() {
			PATCH("/projects/{project_id}/briefs/{brief_id}/campaigns/{campaign_id}/status")
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
