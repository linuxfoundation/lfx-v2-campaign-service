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

// BriefData holds the shared brief attributes used by response types. It is Reference()d by
// the Brief RESPONSE type. Do not add constraints here — they reach the response type through
// Reference, breaking already-persisted empty-slug rows. Constraints belong in BriefInput
// (the create/update payload type) only. Keep the two in sync: see BriefInput's doc comment.
var BriefData = Type("brief-data", func() {
	Attribute("program_type", String, "Funnel context", func() {
		Enum("events", "education", "membership")
	})
	// NO MinLength here: BriefData is Reference()d by the Brief RESPONSE type, and goa copies
	// validations through Reference — so constraining it here also constrains every brief
	// response, making an already-persisted empty-slug row undecodable by generated clients
	// (get-brief included). The empty-slug REQUEST is rejected by BriefInput below — the
	// create/update payload type, which carries MinLength(1) — which is where the constraint
	// belongs. Keep the two in sync: see BriefInput's doc comment.
	Attribute("event_slug", String, "Event/course slug (unique within the project)")
	Attribute("url", String, "Event/course page URL")
	Attribute("platforms", ArrayOf(String), "Suggested default platforms (a planning hint; binding selection is on the campaign)")
	Attribute("event_details", Any, "Extracted event/course details")
	Attribute("copy", Any, "Ad copy")
	Attribute("keywords", Any, "Keyword list")
	Attribute("targeting", Any, "Targeting recommendation")
	Required("program_type", "event_slug")
})

// BriefInput is the CREATE/UPDATE payload. It carries MinLength(1) on event_slug, enforcing
// that the create/update contract rejects empty slugs — without that constraint reaching
// response types (see BriefData).
//
// goa's Required() only checks that the JSON key is present, so an explicit "" satisfies it and
// the TEXT NOT NULL column accepts it — a brief with an empty slug was creatable, occupied the
// UNIQUE(project_id, event_slug) index, and could never be recalled through find-brief (whose
// own MinLength(1) rejects the request with a 400 rather than the documented 404/200).
//
// MinLength(1) cannot live on BriefData: the Brief RESPONSE type Reference()s BriefData and goa
// copies validations through Reference, so any already-persisted empty-slug row would become
// undecodable by generated clients — breaking reads for exactly the rows this input type
// prevents going forward. Requests reject empty slugs; responses stay readable.
var BriefInput = Type("brief-input", func() {
	Reference(BriefData)
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

// Brief is the brief response view. It Reference()s BriefData so the eight
// shared attributes (program_type, event_slug, url, platforms, event_details,
// copy, keywords, targeting) inherit their type/validation/description from a
// single source of truth — a later change to BriefData's program_type Enum or a
// validation rule flows here automatically, so the two can't silently drift.
// Brief then layers on its response-only fields.
var Brief = Type("brief", func() {
	Reference(BriefData)
	Attribute("id", String, "Brief UUID")
	Attribute("project_id", String, "Owning project")
	// Inherited from BriefData via Reference (name-only Attribute calls).
	Attribute("program_type")
	// event_slug is inherited from BriefData via Reference (no constraint): BriefData has
	// no MinLength, so that constraint (which exists in the CREATE payload BriefInput) does
	// not leak into the RESPONSE validator. This keeps any already-persisted empty-slug row
	// decodable by generated clients, preserving reads for exactly the rows the create-side
	// constraint is meant to prevent going forward. Requests reject empty slugs; responses
	// stay readable for legacy data.
	Attribute("event_slug")
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

// CampaignMetrics is the live-read performance snapshot for one campaign over one window.
// It is never persisted — a fresh platform read populates it on every request, unlike
// Campaign (which reflects the stored row plus an ETag).
var CampaignMetrics = Type("campaign-metrics", func() {
	Attribute("campaign_id", String, "Campaign UUID")
	Attribute("platform_campaign_id", String, "ID returned by the ad platform")
	Attribute("window", String, "Platform-agnostic reporting window the metrics were read for", metricsWindowEnum)
	Attribute("impressions", Int64, "Impressions in window")
	Attribute("clicks", Int64, "Clicks in window")
	Attribute("cost_micros", Int64, "Cost in window, in micro-units of the platform's native currency (platform-dependent: USD for LinkedIn/Reddit, X's billing unit for Twitter, etc.)")
	Attribute("ctr", Float64, "Clicks/Impressions, 0 when Impressions is 0")
	Required("campaign_id", "platform_campaign_id", "window", "impressions", "clicks", "cost_micros", "ctr")
})

// EmailCopy holds AI-generated email copy for a campaign brief.
var EmailCopy = Type("email-copy", func() {
	Attribute("subject", String, "Email subject line", func() {
		MaxLength(200)
		Example("Join Us at KubeCon 2026")
	})
	Attribute("preheader", String, "Email preheader text (preview summary)", func() {
		MaxLength(150)
		Example("Register now and shape the future of cloud native computing")
	})
	Attribute("body", String, "Email body HTML (the main content)", func() {
		MaxLength(8000)
		Example("<p>We're excited to invite you...</p>")
	})
	Attribute("cta", String, "Call-to-action button text", func() {
		MaxLength(50)
		Example("Register Now")
	})
	Required("subject", "preheader", "body", "cta")
})

// EventDetailsResult is what fetch-event-url returns: the metadata extracted from an
// event page, plus where in the page it came from.
//
// Deliberately a NAMED type rather than Any. Any renders as `{}` in the generated
// OpenAPI, so every generated client returns an untyped value and no consumer can
// discover or validate the shape — the fields would exist only in prose. The cost of
// naming it is that the shape becomes a contract, which is the point.
//
// Every attribute is OPTIONAL except extracted_from. A page that yields a name and
// nothing else is a normal, useful result — the endpoint's own contract is that a
// missing NAME is the only emptiness worth refusing (400), because a brief with no
// event name is not a draft of anything. Marking the rest Required would turn each
// ordinary omission into a server-side validation failure on a response, which is the
// worst possible place to discover it.
//
// url is the page's own DECLARED landing page (JSON-LD `url` / `og:url`), falling back to
// the URL that was fetched only when the page declares none — see the attribute's own
// description. It is not called registration_url on purpose: the dispatchers treat that as
// the link an ad sends a human to, and an event's landing page is frequently not its
// registration form. A caller that wants them to be the same says so when it creates the
// brief.
var EventDetailsResult = Type("event-details", func() {
	Attribute("event_name", String, "Event name", func() { Example("KubeCon + CloudNativeCon Europe 2026") })
	Attribute("description", String, "Event description")
	Attribute("location", String, "Event location as written on the page — free text, not a resolved place")
	Attribute("start_date", String, "Event start date exactly as the page states it; NOT normalised to RFC 3339, because the source rarely is")
	Attribute("end_date", String, "Event end date, same caveat as start_date")
	Attribute("image", String, "Event image URL")
	Attribute("url", String, "The event's own landing page, as the page declares it (JSON-LD url / og:url), falling back to the URL that was fetched when it declares none. NOT necessarily the fetched URL: a caller commonly pastes a link carrying tracking parameters, and the page's declared canonical is the better destination for an ad")
	Attribute("extracted_from", String, "Which strategy produced this record — the whole record came from exactly one of them", func() {
		Enum("jsonld", "opengraph", "fallback")
	})
	Required("extracted_from")
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
			Attribute("brief", BriefInput)
			Required("project_id", "brief")
		})
		Result(Brief)
		commonBriefErrors()
		HTTP(func() {
			POST("/projects/{project_id}/briefs")
			Header("bearer_token:Authorization")
			Response(StatusCreated, func() { Header("etag:ETag") })
			briefErrorResponses()
		})
	})

	Method("find-brief", func() {
		Description("Find the saved brief for an event slug. Returns 404 when the event has no brief yet, which is the ordinary first-time-generation case — the caller then generates one and POSTs it to create-brief.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			// NO MaxLength: BriefData.event_slug is uncapped and the column is unbounded
			// TEXT, so any cap here would make a brief the create contract accepted
			// permanently unrecallable — the caller would get a validation error instead of
			// its saved brief, then collide on re-create.
			//
			// MinLength(1) here is SAFE because BriefInput.event_slug — the CREATE/UPDATE
			// payload — carries the same constraint. (Not BriefData: it stays unconstrained
			// on purpose, because the Brief RESPONSE type References it and goa copies
			// validations through Reference, which would make an already-persisted
			// empty-slug row undecodable by generated clients. See the note on BriefData.)
			// It did not originally: goa's Required() only checks that the JSON
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
		commonBriefErrors()
		HTTP(func() {
			// A query param, not a path segment: the slug is caller-derived free text, and
			// a lookup that legitimately MISSES is the common case (404 = generate one).
			GET("/projects/{project_id}/briefs")
			Param("event_slug")
			Header("bearer_token:Authorization")
			Response(StatusOK, func() { Header("etag:ETag") })
			briefErrorResponses()
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
		commonBriefErrors()
		HTTP(func() {
			GET("/projects/{project_id}/briefs/{brief_id}")
			Header("bearer_token:Authorization")
			Response(StatusOK, func() { Header("etag:ETag") })
			briefErrorResponses()
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
		commonBriefErrors()
		Error("PreconditionFailed", PreconditionFailedError, "ETag mismatch")
		Error("PreconditionRequired", PreconditionRequiredError, "If-Match header required")
		HTTP(func() {
			PUT("/projects/{project_id}/briefs/{brief_id}")
			Header("bearer_token:Authorization")
			Header("if_match:If-Match")
			Response(StatusOK, func() { Header("etag:ETag") })
			briefErrorResponses()
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
		commonBriefErrors()
		Error("PreconditionFailed", PreconditionFailedError, "ETag mismatch")
		Error("PreconditionRequired", PreconditionRequiredError, "If-Match header required")
		HTTP(func() {
			POST("/projects/{project_id}/briefs/{brief_id}/approve")
			Header("bearer_token:Authorization")
			Header("if_match:If-Match")
			Response(StatusOK, func() { Header("etag:ETag") })
			briefErrorResponses()
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
		commonBriefErrors()
		HTTP(func() {
			DELETE("/projects/{project_id}/briefs/{brief_id}")
			Header("bearer_token:Authorization")
			Response(StatusNoContent)
			briefErrorResponses()
		})
	})

	Method("fetch-event-url", func() {
		Description("Fetch an event page and extract its details, for pre-filling a brief. Does not create anything.")
		Payload(func() {
			bearerToken()
			// Slug-only, matching create-brief: the details returned here go straight
			// into a brief, whose project_id is the campaign-name attribution key, so
			// accepting a UUID here would let a caller fetch under one identifier and
			// create under another.
			projectSlugAttr()
			Attribute("url", String, "Event page URL to fetch. Must be http or https.", func() {
				Example("https://events.linuxfoundation.org/kubecon-cloudnativecon-europe/")
			})
			Required("project_id", "url")
		})
		Result(EventDetailsResult)
		commonBriefErrors()
		HTTP(func() {
			// POST, not GET, though it creates nothing: the URL is a request BODY
			// parameter. As a query parameter it would be written verbatim into access
			// logs, proxy logs and browser history at every hop, and this endpoint makes
			// the service fetch it — so the parameter is the interesting part of the
			// request, not incidental. It is also unbounded in length, which query
			// strings handle badly.
			POST("/projects/{project_id}/fetch-event-url")
			Header("bearer_token:Authorization")
			Response(StatusOK)
			briefErrorResponses()
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
		commonBriefErrors()
		HTTP(func() {
			POST("/projects/{project_id}/briefs/{brief_id}/campaigns")
			Header("bearer_token:Authorization")
			Response(StatusAccepted)
			briefErrorResponses()
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
		commonBriefErrors()
		HTTP(func() {
			GET("/projects/{project_id}/briefs/{brief_id}/campaigns/{campaign_id}")
			Header("bearer_token:Authorization")
			Response(StatusOK, func() { Header("etag:ETag") })
			briefErrorResponses()
		})
	})

	Method("get-campaign-metrics", func() {
		Description("Read live performance metrics (impressions, clicks, cost, CTR) for one campaign directly from its ad platform. This is a pure read — never persisted — unlike get-campaign, which returns the stored row. Support is per-platform: a campaign whose platform has no metrics-read dispatcher wired returns 400.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			briefIDAttr()
			campaignIDAttr()
			Attribute("window", String, "Platform-agnostic reporting window; defaults to last_30_days when omitted, except on platforms whose API cannot serve that range (e.g. X Ads, capped at 7 days), which default to the widest range they support instead", metricsWindowEnum)
			Required("project_id", "brief_id", "campaign_id")
		})
		Result(CampaignMetrics)
		commonBriefErrors()
		HTTP(func() {
			GET("/projects/{project_id}/briefs/{brief_id}/campaigns/{campaign_id}/metrics")
			Header("bearer_token:Authorization")
			Param("window")
			Response(StatusOK)
			briefErrorResponses()
		})
	})

	Method("generate-email-copy", func() {
		Description("Generate AI-written email copy (subject, preheader, body, CTA) for a campaign brief. Returns immediately with generated text; does NOT persist to the brief. The AI model is optional — without it configured this endpoint returns 503.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			briefIDAttr()
			Required("project_id", "brief_id")
		})
		Result(EmailCopy)
		commonBriefErrors()
		HTTP(func() {
			POST("/projects/{project_id}/briefs/{brief_id}/email-copy")
			Header("bearer_token:Authorization")
			Response(StatusOK)
			briefErrorResponses()
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
		commonBriefErrors()
		Error("PreconditionFailed", PreconditionFailedError, "ETag mismatch")
		Error("PreconditionRequired", PreconditionRequiredError, "If-Match header required")
		HTTP(func() {
			PUT("/projects/{project_id}/briefs/{brief_id}/campaigns/{campaign_id}")
			Header("bearer_token:Authorization")
			Header("if_match:If-Match")
			Response(StatusOK, func() { Header("etag:ETag") })
			briefErrorResponses()
			Response("PreconditionFailed", StatusPreconditionFailed)
			Response("PreconditionRequired", StatusPreconditionRequired)
		})
	})

	Method("toggle-campaign-status", func() {
		Description("Pause or resume a campaign on its ad platform (ACTIVE↔PAUSED), then persist the new status. Unlike update-campaign (which only writes the DB row), this dispatches the status change to the platform and updates the row only after the platform confirms. Support is per-platform: a campaign whose platform has no status-toggle dispatcher wired returns 400. Reddit, LinkedIn, Meta, X, Google Ads and Microsoft Ads are wired; HubSpot is not, because an email send has no run state to pause. ONE EXCEPTION to the persist: pausing a campaign in 'created_degraded' pauses it upstream and returns 200 with the status and ETag UNCHANGED. 'created_degraded' records that the campaign's wiring was never verified, and this schema has one status column, so writing 'paused' would spend the reconciliation marker to record a run state the platform already holds authoritatively. Resuming such a campaign is refused outright (409). Read the pause's effect from the ad platform, not from this row.")
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
		commonBriefErrors()
		Error("PreconditionFailed", PreconditionFailedError, "ETag mismatch")
		Error("PreconditionRequired", PreconditionRequiredError, "If-Match header required")
		HTTP(func() {
			PATCH("/projects/{project_id}/briefs/{brief_id}/campaigns/{campaign_id}/status")
			Header("bearer_token:Authorization")
			Header("if_match:If-Match")
			Response(StatusOK, func() { Header("etag:ETag") })
			briefErrorResponses()
			Response("PreconditionFailed", StatusPreconditionFailed)
			Response("PreconditionRequired", StatusPreconditionRequired)
		})
	})

	Method("delete-campaign", func() {
		Description("Delete a campaign (soft delete, requires If-Match). LOCAL ONLY: this removes the campaign from this service and frees its (brief, platform) slot so the brief can be re-dispatched to that platform. It does NOT delete, pause, or otherwise modify the campaign on the ad platform — a campaign already created upstream keeps running and spending until it is stopped there. Use the status-toggle endpoint to pause it first. A campaign that is mid-dispatch returns 409.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			briefIDAttr()
			campaignIDAttr()
			ifMatchAttr()
			Required("project_id", "brief_id", "campaign_id")
		})
		commonBriefErrors()
		Error("PreconditionFailed", PreconditionFailedError, "ETag mismatch")
		Error("PreconditionRequired", PreconditionRequiredError, "If-Match header required")
		HTTP(func() {
			DELETE("/projects/{project_id}/briefs/{brief_id}/campaigns/{campaign_id}")
			Header("bearer_token:Authorization")
			Header("if_match:If-Match")
			Response(StatusNoContent)
			briefErrorResponses()
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
		commonBriefErrors()
		HTTP(func() {
			GET("/projects/{project_id}/jobs/{job_id}")
			Header("bearer_token:Authorization")
			Response(StatusOK)
			briefErrorResponses()
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

// metricsWindowEnum applies the platform-agnostic reporting-window vocabulary
// (model.MetricsWindow's seven values) to the current attribute. Shared between
// get-campaign-metrics' request parameter and CampaignMetrics' result attribute so
// the two stay in lockstep — a window value the request accepts but the result type
// can't represent (or vice versa) would silently diverge otherwise.
func metricsWindowEnum() {
	Enum("today", "yesterday", "last_7_days", "last_14_days", "last_30_days", "this_month", "last_month")
}

// commonBriefErrors declares the standard error set for a brief method.
//
// BadRequest is unconditional, and it takes no parameter deciding otherwise. JWTAuth
// returns this service's own *briefs.BadRequestError for any refused token — Goa generates
// one concrete type per service from the shared design type, so the connections service's
// identically-shaped error is a DIFFERENT Go type and naming it here would send a reader to
// the wrong package. Goa builds each method's
// error encoder from its declared list — a method that omits BadRequest has no case
// for it, so the typed 400 falls out of the generic encoder as a 500 and never appears
// in OpenAPI. That is true of a bodyless GET exactly as much as of a create. An earlier
// revision took a `withBadRequest bool` that had stopped gating anything; it is gone
// rather than retained-and-ignored, because a boolean at 38 call sites that a reader
// must check does nothing is an invitation to make it mean something again.
func commonBriefErrors() {
	Error("BadRequest", BadRequestError, "Bad request")
	Error("NotFound", NotFoundError, "Resource not found")
	Error("Conflict", ConflictError, "Conflict")
	Error("InternalServerError", InternalServerError, "Internal server error")
	Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
}

// briefErrorResponses maps the standard errors to HTTP responses. Every error
// commonBriefErrors declares is mapped here, unconditionally and in the same order:
// a declared error with no Response mapping is the same 500 as an undeclared one, so
// the two functions only work as a pair if neither can be called with a narrower set
// than the other.
func briefErrorResponses() {
	Response("BadRequest", StatusBadRequest)
	Response("NotFound", StatusNotFound)
	Response("Conflict", StatusConflict)
	Response("InternalServerError", StatusInternalServerError)
	Response("ServiceUnavailable", StatusServiceUnavailable)
}
