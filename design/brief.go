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
	Attribute("campaign_id", String, "Campaign UUID", func() { Example("6f9619ff-8b86-d011-b42d-00c04fc964ff") })
	// The rest of this type's example is forced into an EMAIL-channel shape (see the
	// comment on `impressions` below), so this example has to be one too: a bare-numeric
	// HubSpot marketing-email id, matching what ReadMetrics actually queries by. It is not
	// a claim about the ad platforms' own id formats.
	Attribute("platform_campaign_id", String, "The id the CHANNEL returned when the campaign was created. On an ad platform that is its campaign id; on the email channel it is the HubSpot marketing-email id of the cloned draft, which is what the metrics read queries by.", func() { Example("104670127234") })
	Attribute("window", String, "The reporting window that was REQUESTED. On the ad platforms it is also the period the counters cover. On the email channel it is not: it selects which emails are in scope by their send date, and the counters are then that email's totals to date — see the email object.", metricsWindowEnum)
	// The counters below are examples of an EMAIL-channel response, not an ad-platform one,
	// and that is forced rather than chosen: `email` is optional but Goa emits every attribute
	// into the generated example, so the example always shows an email response. Given that,
	// the numbers have to obey the mapping the adapter enforces — impressions == email.opens,
	// clicks == email.clicks, cost_micros == 0 — or the contract's own example contradicts the
	// descriptions two lines above it. Goa's fabricated integers satisfied none of the three.
	Attribute("impressions", Int64, "Impressions over the window on an ad platform; opens to date on the email channel", func() { Example(1840) })
	Attribute("clicks", Int64, "Clicks over the window on an ad platform; clicks to date on the email channel", func() { Example(212) })
	Attribute("cost_micros", Int64, "Cost over the window, in micro-units of the platform's native currency (platform-dependent: USD for LinkedIn/Reddit, X's billing unit for Twitter, etc.). Always 0 on the email channel, which bills no per-send cost — do not blend that 0 into a cross-channel cost-per-acquisition.", func() { Example(0) })
	Attribute("ctr", Float64, "Clicks/Impressions, 0 when Impressions is 0", func() { Example(0.1152) })
	// The Example(37.0) below is a PER-PROPERTY example only. It deliberately does NOT reach
	// the object-level example, which is pinned explicitly at the bottom of this type and
	// omits `conversions` entirely — see the reasoning there. Left to Goa, the generated
	// object example carried both `conversions` and the mutually exclusive `email` object.
	//
	// OPTIONAL, and deliberately excluded from Required below. Only Google Ads, LinkedIn and
	// Microsoft report a campaign-level conversion count; Meta and X expose conversions only
	// as per-action-type structures with no scalar to read, Reddit's reporting contract is
	// undocumented, and the email channel has no conversion concept at all. On those four the
	// attribute is ABSENT rather than 0 — a 0 would be indistinguishable from a campaign that
	// genuinely converted nobody, which is the same substitution the row `status` field and
	// `pacing.pct` already refuse to make elsewhere in this contract.
	Attribute("conversions", Float64, "Conversions attributed to this campaign over the window. FRACTIONAL: Google Ads and Microsoft both type their conversion metric as a double and credit partial conversions under data-driven, position-based and offline attribution, so a campaign can genuinely hold 0.4 of a conversion — do not round it to a whole number, and in particular do not treat a value below 1 as zero. ABSENT when the channel does not report a campaign-level conversion count (Meta, X, Reddit and the email channel never do), and ALSO absent on Microsoft whenever the ConversionsQualified column is missing from the report or ANY row's conversion cell is blank — that column is only populated for accounts wired for Universal Event Tracking, and a partial column summed as though it were complete would report a campaign's conversions as lower than they are. Absent means \"not measured here\", which is NOT the same as a measured 0, and a consumer must not render it as zero or fold it into a conversion total.", func() { Example(37.0) })
	Attribute("email", EmailMetrics, "Email-channel counters. Present only for the email channel (HubSpot); absent for every ad platform.")
	Required("campaign_id", "platform_campaign_id", "window", "impressions", "clicks", "cost_micros", "ctr")
	// An EXPLICIT object example, because the attribute-level examples above cannot produce a
	// coherent one on their own. Goa emits EVERY attribute into a generated object example,
	// including the two that are mutually exclusive: `email` is present only on the email
	// channel, and the email channel is one of the channels that never reports `conversions`.
	// The generated example therefore advertised `conversions: 37` alongside `email{}` — a
	// response no adapter in this service can produce, contradicting the `conversions`
	// description directly above it. Dropping the attribute-level Example does not fix it
	// either: Goa then fabricates a random double (e.g. 0.2109820735060166), which is a
	// worse claim than a wrong one.
	//
	// This example is the EMAIL shape, matching the attribute examples above (which are
	// themselves forced into an email shape — impressions == opens, clicks == clicks,
	// cost_micros == 0), and it therefore OMITS `conversions` entirely. That omission is the
	// contract's own statement: absent, not zero, is what an email response carries. The
	// attribute-level Example(37.0) is retained because it documents the field's own shape —
	// a fractional-capable count — in the per-property schema, where no `email` sits beside
	// it to contradict it.
	Example(map[string]any{
		"campaign_id":          "6f9619ff-8b86-d011-b42d-00c04fc964ff",
		"platform_campaign_id": "104670127234",
		"window":               "last_30_days",
		"impressions":          1840,
		"clicks":               212,
		"cost_micros":          0,
		"ctr":                  0.1152,
		"email": map[string]any{
			"sent": 9400, "delivered": 9268, "opens": 1840,
			"clicks": 212, "bounces": 95, "unsubscribes": 17,
		},
	})
})

// EmailMetrics is the email channel's counter set. It is a separate OPTIONAL object rather
// than extra attributes on campaign-metrics so an ad-platform response does not carry six
// fields that are structurally zero, which a client cannot distinguish from "zero sends".
var EmailMetrics = Type("email-metrics", func() {
	Description("Counters that only an email campaign has. NONE of them is scoped to the requested window: the window selects which emails are in scope by their SEND date, and every counter below is then that email's total to date. Rendering any of them as \"in the last N days\" is therefore wrong. impressions/clicks on the parent object mirror opens/clicks; cost_micros is always 0 for email because the platform bills no per-send cost — that 0 must not be blended into a cross-channel cost-per-acquisition.")
	// Examples chosen to be internally consistent with each other and with the parent
	// object: opens <= delivered, and opens/clicks equal the parent's impressions/clicks.
	// Sent minus Delivered is not Bounces (messages can be dropped or suppressed before
	// delivery is ever attempted), so sent and delivered are both independent data.
	// See the note on campaign-metrics for why the example has to hold up rather than merely exist.
	Attribute("sent", Int64, "Emails handed to the delivery pipeline, to date", func() { Example(9400) })
	Attribute("delivered", Int64, "Emails the receiving server accepted, to date", func() { Example(9268) })
	Attribute("opens", Int64, "Opens to date (mirrors impressions)", func() { Example(1840) })
	Attribute("clicks", Int64, "Clicks to date (mirrors clicks)", func() { Example(212) })
	Attribute("bounces", Int64, "Bounced emails, to date", func() { Example(95) })
	Attribute("unsubscribes", Int64, "Unsubscribes, to date", func() { Example(17) })
	Required("sent", "delivered", "opens", "clicks", "bounces", "unsubscribes")
})

// CampaignPacing is spend measured against what the flight expects BY NOW.
//
// Not against the whole budget: a campaign three days into a thirty-day flight is expected to
// have spent a tenth of it, so the naive comparison reports every healthy campaign as severely
// underspending for most of its life.
var CampaignPacing = Type("campaign-pacing", func() {
	// Absent, not zero, when pacing could not be derived. A zero here is indistinguishable
	// from a campaign that spent nothing, and rendering "0% of plan" for a campaign that has
	// no budget reports the absence of a plan as a failure to follow one.
	Attribute("pct", Float64, "Spend as a percentage of what this campaign should have spent BY NOW. Absent when pacing is not computable — never zero-filled, because 0% is a claim about spend.", func() { Example(94.2) })
	Attribute("label", String, "The band pct falls into. `unknown` means no pacing could be derived, which is NOT the same as being on plan.", func() {
		Enum("underspending", "normal", "constrained", "overspending", "unknown")
	})
	Required("label")
})

// CampaignActionItem is one thing an operator should look at, and what to do about it.
var CampaignActionItem = Type("campaign-action-item", func() {
	// A stable token, so a consumer can group, filter or link on it. The issue text is for
	// humans and may be reworded; keying on prose would break silently when it is.
	Attribute("rule", String, "Which rule fired, as a stable token.", func() {
		Enum("zero_delivery", "underspending", "budget_constrained", "low_ctr", "no_conversions")
		// PINNED. With no explicit example Goa auto-selects an enum member, and which member
		// it picks is a function of the whole design rather than of this attribute — an
		// unrelated edit elsewhere in this file silently moves it. Two constraints bind the
		// choice, and `zero_delivery` is the member that satisfies both:
		//
		//   - It must not be `no_conversions`. The sibling example pins `platform:
		//     reddit-ads`, and Reddit never reports a conversion count, so that member would
		//     advertise a finding this service cannot produce on that platform (the
		//     no_conversions rule is gated on Conversions != nil precisely to avoid it).
		//   - It must agree with the sibling `issue` and `action` examples below, which
		//     describe a campaign that recorded no impressions and no spend and tell the
		//     operator to check targeting and creative approval. That is the zero_delivery
		//     symptom and remedy. `budget_constrained` — previously pinned here — is the
		//     OPPOSITE state: it fires on a campaign spending AHEAD of plan and its remedy is
		//     to raise or accept the budget, so the composed example described two different
		//     findings at once and propagated that incoherence into every
		//     `action_items` array example.
		//
		// zero_delivery fires on paid-ads channels including reddit-ads (it is gated on
		// BillsPerDelivery, which every paid-ads channel sets), so the composition is
		// producible as published. Its priority is HIGH, pinned on the sibling attribute.
		Example("zero_delivery")
	})
	// Pinned for the same reason `rule` is: the composed example must describe ONE finding.
	// zero_delivery is raised at HIGH priority, so an auto-selected MED would contradict the
	// rule pinned above.
	Attribute("priority", String, "How urgently this wants attention.", func() {
		Enum("HIGH", "MED")
		Example("HIGH")
	})
	Attribute("campaign_id", String, "The campaign this concerns", func() { Example("6f9619ff-8b86-d011-b42d-00c04fc964ff") })
	Attribute("platform", String, "The channel that campaign runs on", func() { Example("reddit-ads") })
	Attribute("issue", String, "What is wrong, in operator-facing wording.", func() { Example("No impressions or spend recorded") })
	Attribute("action", String, "What to do about it.", func() { Example("Check targeting and creative approval status") })
	Required("rule", "priority", "campaign_id", "platform", "issue", "action")
})

// BriefMetricsRow is one campaign's slot in the brief-wide metrics read.
//
// Every campaign on the brief gets a row, INCLUDING the ones that could not be read. That is
// the point of the type: a brief spans several platforms, each read can fail independently,
// and a consumer that cannot tell "measured zero" from "could not measure" will render an
// outage as a performance result. The single-campaign endpoint expresses these states as HTTP
// errors, which an aggregate cannot do — one campaign's 409 must not fail the other five.
var BriefMetricsRow = Type("brief-metrics-row", func() {
	Attribute("campaign_id", String, "Campaign UUID", func() { Example("6f9619ff-8b86-d011-b42d-00c04fc964ff") })
	Attribute("platform", String, "The channel this campaign runs on", func() { Example("linkedin-ads") })
	Attribute("status", String, "Whether this row carries a measurement. ONLY `ok` does.", briefMetricsRowStatusEnum)
	// Deliberately optional, and absent on every non-ok status rather than zero-filled. A
	// zeroed row is indistinguishable from a campaign that genuinely served nothing, which is
	// the exact substitution that turned a failed dashboard read into a "pause losing
	// campaigns" recommendation.
	Attribute("metrics", CampaignMetrics, "The measurement. Present if and ONLY if status is `ok`; absent otherwise — never zero-filled, because a zero is a claim.")
	// Safe to render. The service maps each internal failure onto a fixed sentence here
	// rather than forwarding the adapter's error text, which can carry a platform's own
	// response body or operator-supplied account identifiers.
	Attribute("reason", String, "Why this row carries no measurement, in consumer-safe wording. Absent when status is `ok`.", func() {
		Example("this window is not supported for the campaign's platform")
	})
	// Absent on any row that carries no measurement. On an `ok` row it is always PRESENT, and
	// states `unknown` when the arithmetic could not be performed — the absence of a budget is
	// itself worth reporting, and omitting the object would make "no pacing" indistinguishable
	// from an older server that did not send one. Pacing is per-campaign only: cost is reported
	// in each platform's native currency and this service performs no FX, so pacing figures
	// must never be totalled or averaged across rows.
	Attribute("pacing", CampaignPacing, "Spend against the flight-prorated plan. Absent when status is not `ok`. On an `ok` row it is always present: `pct` is absent and `label` is `unknown` when this campaign has no budget or usable flight to pace against, or when the window does not overlap the flight.")
	Required("campaign_id", "platform", "status")
})

// BriefMetrics is every campaign on a brief, read in one request.
var BriefMetrics = Type("brief-metrics", func() {
	Attribute("brief_id", String, "Brief UUID", func() { Example("6f9619ff-8b86-d011-b42d-00c04fc964ff") })
	Attribute("window", String, "The window REQUESTED for this read. Per-platform defaults still apply when it is omitted, so an individual row may have been read over a narrower window than this — X Ads caps queryable ranges at 7 days. Each row's own metrics.window is what that row actually covers.", metricsWindowEnum)
	Attribute("rows", ArrayOf(BriefMetricsRow), "One row per campaign on the brief, in a stable order. Includes rows that could not be read.")
	Attribute("ok_count", Int, "How many rows carry a measurement. Compare against the length of rows before presenting any cross-campaign total — a total over 2 of 6 campaigns is not the brief's performance.", func() { Example(2) })
	// No cross-channel cost total. cost_micros is in micro-units of each platform's OWN
	// native currency and this service performs no FX conversion (see the note on
	// campaign-metrics.cost_micros), so summing LinkedIn USD with X's billing unit would
	// produce a number with no currency and no meaning. Impressions and clicks are unitless
	// and could be summed, but are left to the consumer alongside ok_count rather than
	// presented here as a whole-brief figure the row set may not support.
	// Derived from the rows above, on the service side, so every consumer applies the same
	// thresholds. Four UI implementations previously derived these independently and disagreed
	// three ways on the underspending floor; one of them disagreed with itself, labelling at 50
	// while alerting at 40.
	//
	// Empty is a real answer meaning nothing needs attention. It is NOT a claim that every row
	// was readable — rows that could not be read raise no items, so check ok_count before
	// presenting an empty list as an all-clear.
	Attribute("action_items", ArrayOf(CampaignActionItem), "What an operator should look at, derived from the readable rows. Empty means nothing was flagged among those rows — compare ok_count against the row count before reading that as an all-clear.")
	Required("brief_id", "window", "rows", "ok_count", "action_items")
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

// CreativeAsset is an uploaded image asset a Meta ad creative can reference by id.
// It is insert-only (no version/ETag): the bytes are immutable once stored, and the
// upload is idempotent per (brief, content checksum) — re-uploading identical bytes
// returns the existing asset rather than creating a second row.
var CreativeAsset = Type("creative-asset", func() {
	Attribute("id", String, "Creative asset UUID", func() { Format(FormatUUID) })
	Attribute("project_id", String, "Owning project")
	Attribute("brief_id", String, "Parent brief")
	Attribute("mime_type", String, "Stored image MIME type, as verified from the bytes (not merely the declared header)", func() {
		Enum("image/png", "image/jpeg")
	})
	Attribute("byte_size", Int64, "Size of the stored image in bytes")
	Attribute("checksum", String, "Lowercase-hex SHA-256 digest of the stored bytes; the dedupe key within a brief")
	// created SELECTS the upload response status and is deliberately NOT Required, so it stays
	// out of every other representation of this type (get/list responses never set it).
	//
	// The upload is idempotent on (brief_id, checksum), so a re-upload of identical bytes returns
	// the EXISTING row — fully populated, same id, the FIRST upload's created_at. Nothing in the
	// row distinguishes that from a genuine creation, so an unconditional 201 told a retrying
	// client it had created a resource when nothing was created. This attribute carries the
	// distinction to the transport, which renders it as 201 vs 200.
	Attribute("created", String, "\"true\" when this request stored the asset; \"false\" when an identical upload already existed. Set only on the upload response, where it selects 201 vs 200.", func() {
		Enum("true", "false")
	})
	Required("id", "project_id", "brief_id", "mime_type", "byte_size", "checksum")
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

	Method("upload-creative-asset", func() {
		Description("Upload an image asset for a brief so a Meta ad creative can reference it by id. Synchronous: the image is validated (PNG/JPEG, size limit) and stored, then the asset id is returned. Re-uploading identical bytes to the same brief returns the existing asset (idempotent). This does not touch any ad platform; the account-scoped Meta image_hash is resolved later, at campaign dispatch.")
		Payload(func() {
			bearerToken()
			// Permissive project identifier (UUID or slug), matching the other brief
			// sub-resources: project_id here only scopes ownership/authz. Unlike
			// create-campaigns it is never stamped into a campaign name or used as the
			// connection-lookup key — the asset is bound to a campaign later by its
			// asset id, so the slug-only constraint does not apply.
			projectIDAttr()
			briefIDAttr()
			Attribute("content_type", String, "Declared MIME type of the uploaded bytes. The bytes are re-sniffed server-side and must match; the stored mime_type is the verified one.", func() {
				Enum("image/png", "image/jpeg")
			})
			// Goa Bytes -> []byte in Go, base64-encoded string in the JSON body. This is
			// the transport choice: Goa-native, no multipart machinery. MinLength/MaxLength put the accepted size in the contract and in
			// the OpenAPI document, and the generated validator applies them before the
			// handler runs: MinLength(1) rejects an empty upload; MaxLength is a hard
			// ceiling at Meta's documented single-image maximum.
			//
			// These bound the DECODED image, and the generated validator sees that slice
			// only AFTER goahttp.RequestDecoder's json.Decoder has read the whole request
			// body and base64-decoded it — so MaxLength alone does not bound what the
			// server reads off the wire. The inbound byte cap that does is
			// constants.MaxRequestBodyBytes, applied by middleware.MaxBodyBytes in the
			// server's handler chain; it is sized from this ceiling (see that constant) and
			// must be raised alongside any increase here.
			Attribute("bytes", Bytes, "Raw image bytes, base64-encoded in the JSON request body.", func() {
				MinLength(1)
				// MaxLength is published into the OpenAPI document as `maxLength` on the JSON
				// STRING, not on the decoded bytes — Goa emits this attribute as
				// `type: string, format: binary`. The two are not the same quantity: base64
				// expands by 4/3, so a 30 MiB image is 41,943,040 encoded characters.
				//
				// Declaring the DECODED ceiling here therefore published a constraint that
				// rejects at ~22.5 MiB decoded (31,457,280 chars / 4 * 3) — well inside what
				// this endpoint intends to accept and what the 42 MiB wire cap allows. A
				// standards-compliant validator or a generated client would refuse an upload
				// the server would have honoured, and the server-side generated validator would
				// never disagree because it applies the same number to the decoded slice.
				//
				// So the wire bound is stated on the wire representation, in the unit that
				// representation is measured in: base64 characters. The DECODED 30 MiB ceiling
				// is a different constraint on a different quantity and is enforced in the
				// handler (maxCreativeStoredBytes, the stored-file ceiling, alongside
				// maxCreativeDecodedBytes, the pixel budget — both in internal/service), which
				// is the only layer that sees decoded bytes.
				MaxLength(41943040) // 4/3 * 30 MiB: the ENCODED ceiling, the unit this schema constrains
			})
			Required("project_id", "brief_id", "content_type", "bytes")
		})
		Result(CreativeAsset)
		commonBriefErrors()
		HTTP(func() {
			POST("/projects/{project_id}/briefs/{brief_id}/creative-assets")
			Header("bearer_token:Authorization")
			// TWO success statuses, selected by the `created` Tag, because this upload is
			// idempotent on (brief_id, checksum) and the two outcomes are genuinely different
			// events: 201 when this request stored the asset, 200 when an identical upload
			// already existed and the stored row was returned unchanged. An unconditional 201
			// told a retrying client it had created a resource when nothing was created.
			//
			// The body is the same CreativeAsset in both arms. The `created` attribute IS
			// rendered (as an omitempty field), so it is an ADDITIVE body change, not a
			// breaking one: every field an existing client reads keeps its name, type and
			// meaning, and a client that ignores unknown fields sees no difference. The
			// status is the part that changed, and only for the retry case.
			//
			// No ETag on either: creative assets are insert-only and carry no version, so there
			// is no optimistic-concurrency handle to hand back.
			Response(StatusCreated, func() { Tag("created", "true") })
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

	Method("adopt-campaign", func() {
		Description("Bind a campaign that ALREADY exists on the ad platform to this brief. The platform is read, never written: the campaign must already exist under the project's connection, and nothing is created upstream. Returns 404 when the platform holds no such campaign, 409 when this brief already has a live campaign on that platform / that campaign is already bound to another brief (in any project, since several foundations share one upstream ad account) / the brief lost approval during the read / the project has no ad-platform connection of its own, and 400 when the platform has no adoption capability wired. An adopted campaign supports metrics, delete and pause; activation is refused, because adoption does not verify the targeting the activate guard requires.")
		Payload(func() {
			bearerToken()
			// Slug-only, matching create-campaigns: project_id is the exact-match key for
			// the connection lookup that scopes the platform read to this project's ad
			// account. The lookup's safety property is that it runs under the project's OWN
			// credentials, not a discovery credential that can see every account.
			projectSlugAttr()
			briefIDAttr()
			Attribute("platform", String, "Ad platform the campaign lives on", func() {
				// Hyphenated, matching model.ProviderGoogleAds and the Enum on
				// campaign-create-input. Goa publishes this example straight into the
				// OpenAPI document, so an underscore here hands client authors a value
				// the service rejects.
				Example("google-ads")
			})
			Attribute("platform_campaign_id", String, "The ad platform's own id for the existing campaign", func() {
				// Load-bearing, not hygiene: an empty id degrades the lookup from "this
				// campaign" to "any campaign", and a platform whose filter silently drops
				// an empty operand answers with somebody else's campaign.
				MinLength(1)
				MaxLength(64)
				Example("1234567890")
			})
			Required("project_id", "brief_id", "platform", "platform_campaign_id")
		})
		Result(Campaign)
		commonBriefErrors()
		HTTP(func() {
			// POST to a sub-collection rather than PUT on the campaign: no campaign
			// resource exists yet at any URL. Not idempotent by design — a second adopt of
			// the same pair is a 409, the report an operator needs, not a silent re-bind.
			POST("/projects/{project_id}/briefs/{brief_id}/campaigns/adopt")
			Header("bearer_token:Authorization")
			Response(StatusCreated, func() { Header("etag:ETag") })
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
		Description("Read live performance metrics (impressions, clicks, cost, CTR) for one campaign directly from the platform that runs it — an ad platform, or HubSpot for the email channel, which additionally returns the email object. This is a pure read — never persisted — unlike get-campaign, which returns the stored row. Support is per-platform: a campaign whose platform has no metrics-read dispatcher wired returns 400. Note that the requested window scopes the counters on the ad platforms but NOT on email, where it selects which emails are in scope by send date and the counters are those emails' totals to date.")
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

	Method("get-brief-metrics", func() {
		Description("Read live performance metrics for EVERY campaign on a brief in one request, by calling each campaign's platform directly. A pure read — never persisted. Unlike get-campaign-metrics, a failure on one campaign does not fail the request: each row carries its own status, and a row that could not be read carries NO counters rather than zeroes, so a consumer can distinguish a campaign that served nothing from one that could not be measured. Rows are returned for every campaign on the brief, including unreadable ones. There is no cross-channel cost total: cost is denominated in each platform's own currency and this service performs no FX conversion.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			briefIDAttr()
			Attribute("window", String, "Platform-agnostic reporting window applied to every campaign; defaults to last_30_days when omitted, except on platforms whose API cannot serve that range (e.g. X Ads, capped at 7 days), which fall back to the widest range they support. A row's own metrics.window records what it actually covers.", metricsWindowEnum)
			Required("project_id", "brief_id")
		})
		Result(BriefMetrics)
		commonBriefErrors()
		HTTP(func() {
			GET("/projects/{project_id}/briefs/{brief_id}/metrics")
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

// briefMetricsRowStatusEnum is the per-row outcome vocabulary for a brief-wide metrics read.
//
// The values are DERIVED from the failure modes GetCampaignMetrics already distinguishes as
// separate HTTP responses, collapsed only where the consumer's next action is identical:
//
//   - ok                 — the read succeeded. The only status carrying a measurement.
//   - unsupported        — this platform has no metrics-read dispatcher, or cannot serve the
//     requested window (X Ads caps ranges at 7 days). A 400 on the
//     single-campaign endpoint. Retrying is pointless; narrowing the
//     window may help.
//   - not_ready          — the campaign has no platform campaign id yet, or the platform
//     reported no data for the window. A 409. Common and benign: an
//     email campaign staged as a draft reads this way until a human
//     sends it. NOT an error to surface as a failure.
//   - connection_problem — the connection cannot serve this campaign: no connection row,
//     provenance unknown, a different account than the current connection,
//     undecryptable credentials, or an unusable connection including the
//     LF system fallback. Deliberately does NOT name a single HTTP code —
//     those defects answer 404, 409 and 500 on the single-campaign
//     endpoint. They collapse here because the REMEDY is identical: an
//     operator repairs the connection. Retrying never helps.
//   - failed             — the platform read itself failed. A 5xx on the single-campaign
//     endpoint. Transient; retrying may succeed.
//
// `not_ready` and `failed` are deliberately NOT merged. A staged email draft and an ad-platform
// outage produce the same absence of numbers and want opposite responses — one is the expected
// steady state before a send, the other is an incident.
func briefMetricsRowStatusEnum() {
	Enum("ok", "unsupported", "not_ready", "connection_problem", "failed")
}

// commonBriefErrors declares the standard error set for a brief method.
//
// BadRequest and Unauthorized are both unconditional, and neither takes a parameter
// deciding otherwise. JWTAuth returns this service's own *briefs.UnauthorizedError for any
// refused token — Goa generates one concrete type per service from the shared design type,
// so the connections service's identically-shaped error is a DIFFERENT Go type and naming
// it here would send a reader to the wrong package. Goa builds each method's
// error encoder from its declared list — a method that omits Unauthorized has no case
// for it, so the typed 401 falls out of the generic encoder as a 500 and never appears
// in OpenAPI. That is true of a bodyless GET exactly as much as of a create. An earlier
// revision took a `withBadRequest bool` that had stopped gating anything; it is gone
// rather than retained-and-ignored, because a boolean at 38 call sites that a reader
// must check does nothing is an invitation to make it mean something again.
func commonBriefErrors() {
	Error("BadRequest", BadRequestError, "Bad request")
	// Unauthorized is declared for exactly the reason BadRequest is, and is now what
	// JWTAuth actually returns for a refused token. BadRequest STAYS declared: it is
	// still the status for payload validation, and on the methods that take no body it
	// is still reachable through the generated path/query decoders (a project_id that
	// fails the slug Pattern, a malformed UUID). Dropping it here because auth moved
	// off it would turn those into 500s.
	Error("Unauthorized", UnauthorizedError, "Unauthorized")
	Error("NotFound", NotFoundError, "Resource not found")
	Error("Conflict", ConflictError, "Conflict")
	Error("InternalServerError", InternalServerError, "Internal server error")
	Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
	// PayloadTooLarge is produced by middleware.MaxBodyBytes, which sits outside the mux
	// and so never reaches a Goa encoder — declaring it here is what gives the generated
	// CLIENT a decode case for 413 (otherwise an oversized upload surfaces as
	// ErrInvalidResponse) and what puts the status in the OpenAPI documents. It is on
	// every method because the cap is global, not upload-specific.
	Error("PayloadTooLarge", PayloadTooLargeError, "Payload too large")
}

// briefErrorResponses maps the standard errors to HTTP responses. Every error
// commonBriefErrors declares is mapped here, unconditionally and in the same order:
// a declared error with no Response mapping is the same 500 as an undeclared one, so
// the two functions only work as a pair if neither can be called with a narrower set
// than the other.
func briefErrorResponses() {
	Response("BadRequest", StatusBadRequest)
	// The Header call is what puts the challenge on the wire: it maps the error type's
	// www_authenticate attribute onto the WWW-Authenticate response header. Without it
	// the field would serialize into the JSON body and the 401 would carry no challenge.
	Response("Unauthorized", StatusUnauthorized, func() {
		Header("www_authenticate:WWW-Authenticate")
	})
	Response("NotFound", StatusNotFound)
	Response("Conflict", StatusConflict)
	Response("InternalServerError", StatusInternalServerError)
	Response("ServiceUnavailable", StatusServiceUnavailable)
	Response("PayloadTooLarge", StatusRequestEntityTooLarge)
}
