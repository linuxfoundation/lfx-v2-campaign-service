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
	Attribute("email", EmailMetrics, "Email-channel counters. Present only for the email channel (HubSpot); absent for every ad platform.")
	Required("campaign_id", "platform_campaign_id", "window", "impressions", "clicks", "cost_micros", "ctr")
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
		Enum("zero_delivery", "underspending", "budget_constrained", "low_ctr")
	})
	Attribute("priority", String, "How urgently this wants attention.", func() { Enum("HIGH", "MED") })
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

// ─── Google Ads keyword / audience insight types ───

// keywordActionEnum is the closed set of keyword mutations this service brokers.
//
// Deliberately only PAUSE and REMOVE — the two DESTRUCTIVE-toward-delivery directions. There is
// no ENABLE here, and its absence is a decision rather than an omission: re-enabling a keyword
// makes a paused campaign start spending again, and this endpoint's whole justification is that
// it can only ever reduce what serves. A caller that wants to widen delivery goes through the
// create/dispatch path, where budget and flight are validated together.
//
// REMOVE is irreversible in Google Ads: a removed ad-group criterion cannot be re-enabled, only
// re-created as a NEW criterion with a new id. The description says so, because a UI that
// presents it as the symmetric opposite of PAUSE will lose the caller their keyword history.
func keywordActionEnum() {
	Enum("PAUSE", "REMOVE")
}

// GoogleAdsKeyword is one keyword row from the account-wide performance read.
//
// Every counter is scoped to the requested window. `criterion_id` is what the keyword-actions
// endpoint consumes, so the two surfaces are usable together: read here, act there.
var GoogleAdsKeyword = Type("google-ads-keyword", func() {
	Attribute("criterion_id", String, "The ad-group criterion id — the handle keyword-actions takes. Bare numeric, unique only within its ad group, which is why ad_group_id travels with it.", func() { Example("305729261") })
	Attribute("ad_group_id", String, "The ad group this criterion belongs to. Required to address the criterion: a criterion id alone does not identify a keyword.", func() { Example("176216228") })
	Attribute("campaign_id", String, "The Google Ads campaign this keyword serves under", func() { Example("21234567890") })
	Attribute("text", String, "The keyword text as Google stores it", func() { Example("kubernetes training") })
	Attribute("match_type", String, "How broadly the keyword matches queries", func() { Enum("EXACT", "PHRASE", "BROAD") })
	Attribute("status", String, "The criterion's current serving status upstream", func() { Enum("ENABLED", "PAUSED", "REMOVED", "UNKNOWN") })
	Attribute("impressions", Int64, "Impressions over the window", func() { Example(4820) })
	Attribute("clicks", Int64, "Clicks over the window", func() { Example(311) })
	Attribute("cost_micros", Int64, "Cost over the window in micro-units of the account's native currency. This service performs no FX conversion, so do not blend it with another account's figures.", func() { Example(1284000) })
	Attribute("ctr", Float64, "Clicks/Impressions, 0 when Impressions is 0 (never divides by zero)", func() { Example(0.0645) })
	Required("criterion_id", "ad_group_id", "campaign_id", "text", "match_type", "status", "impressions", "clicks", "cost_micros", "ctr")
})

// GoogleAdsKeywords is the account-wide keyword performance read.
//
// This is NOT a list endpoint in the sense api-catalog.md rule 3 forbids. It enumerates nothing
// this service stores: the rows come from Google Ads over the project's connection, and no
// keyword is persisted here in any table. The same reasoning the catalog already applies to
// GET /projects/{projectId}/connection-google-ads/accounts applies unchanged.
var GoogleAdsKeywords = Type("google-ads-keywords", func() {
	Attribute("window", String, "The reporting window these counters cover", metricsWindowEnum)
	Attribute("rows", ArrayOf(GoogleAdsKeyword), "Keyword rows, ordered by impressions descending. Capped — see `truncated`.")
	Attribute("row_count", Int, "How many rows are in `rows`.", func() { Example(50) })
	// A cap without a signal is a silent lie: a caller that receives exactly 50 rows cannot
	// tell a 50-keyword account from a 5000-keyword one, and would present the top slice as
	// the whole account. This flag is the difference between a truncated answer and a wrong one.
	Attribute("truncated", Boolean, "True when the account has more keywords than were returned. The rows are the TOP ones by impressions, not the account's full keyword set — do not total them and present the result as account-wide spend.", func() { Example(true) })
	Required("window", "rows", "row_count", "truncated")
})

// GoogleAdsAudienceBucket is one demographic slice's counters.
//
// `dimension` names which breakdown the bucket belongs to, so the three breakdowns can share
// one row type and one array. Splitting them into three typed arrays was considered and
// rejected: a consumer that wants "show me the biggest slice" then has to special-case three
// shapes, and a fourth breakdown (Google exposes several more) would be a breaking change
// rather than a new enum value.
var GoogleAdsAudienceBucket = Type("google-ads-audience-bucket", func() {
	Attribute("dimension", String, "Which breakdown this bucket belongs to", func() { Enum("age", "gender", "device") })
	Attribute("value", String, "The bucket within that breakdown, as Google's own enum literal. UNDETERMINED/UNKNOWN are real Google values, not read failures — a sizeable share of impressions genuinely cannot be attributed to a demographic, and folding them away would make the buckets sum to less than the campaign's traffic with no indication why.", func() { Example("AGE_RANGE_25_34") })
	Attribute("impressions", Int64, "Impressions over the window", func() { Example(12840) })
	Attribute("clicks", Int64, "Clicks over the window", func() { Example(742) })
	Attribute("cost_micros", Int64, "Cost over the window in micro-units of the account's native currency", func() { Example(3120000) })
	Attribute("ctr", Float64, "Clicks/Impressions, 0 when Impressions is 0", func() { Example(0.0578) })
	Required("dimension", "value", "impressions", "clicks", "cost_micros", "ctr")
})

// GoogleAdsAudience is the account-wide demographic read: age, gender and device.
//
// The three breakdowns are read as three separate GAQL queries and returned in one array
// discriminated by `dimension`. They are NOT independently failable in the response: if any one
// query fails the whole request fails, because a partial demographic picture presented as a
// whole one is how a campaign gets re-targeted on the half of the data that happened to load.
var GoogleAdsAudience = Type("google-ads-audience", func() {
	Attribute("window", String, "The reporting window these counters cover", metricsWindowEnum)
	Attribute("buckets", ArrayOf(GoogleAdsAudienceBucket), "Every bucket across all three breakdowns, discriminated by `dimension`. Ordered by dimension then impressions descending.")
	// Impressions are unitless and summable WITHIN one dimension, and each dimension covers the
	// same traffic — so age, gender and device each total to (approximately) the same figure.
	// Summing ACROSS dimensions triple-counts. Said here because the flat array makes that
	// mistake easy to reach.
	Attribute("bucket_count", Int, "How many buckets are in `buckets`, across all dimensions. Each dimension independently covers the same traffic, so summing impressions across dimensions triple-counts it — total within one dimension only.", func() { Example(18) })
	Required("window", "buckets", "bucket_count")
})

// KeywordActionInput is one requested keyword mutation.
//
// Both ids are required and neither is inferable. A criterion id is unique only within its ad
// group, so acting on a criterion id alone would mean guessing the ad group — and a wrong guess
// addresses a DIFFERENT, real keyword rather than failing.
var KeywordActionInput = Type("keyword-action-input", func() {
	// Pattern is digits-only rather than merely non-empty, and that constraint is load-bearing
	// rather than cosmetic: these ids are concatenated into a Google Ads resource name, so the
	// same injection reasoning that governs customerIDRE in the platform client applies here.
	// Declaring it in the design means Goa's request decoder rejects a malformed id before any
	// handler runs, and the client re-validates for non-HTTP callers.
	Attribute("ad_group_id", String, "The ad group the criterion belongs to. Digits only.", func() {
		Pattern(`^[0-9]+$`)
		MaxLength(20)
		Example("176216228")
	})
	Attribute("criterion_id", String, "The keyword's ad-group criterion id, as returned by the keywords read. Digits only.", func() {
		Pattern(`^[0-9]+$`)
		MaxLength(20)
		Example("305729261")
	})
	Attribute("action", String, "What to do to this keyword. REMOVE is IRREVERSIBLE — a removed criterion cannot be re-enabled, only re-created with a new id.", keywordActionEnum)
	Required("ad_group_id", "criterion_id", "action")
})

// KeywordActionResult is one mutation's outcome.
var KeywordActionResult = Type("keyword-action-result", func() {
	Attribute("ad_group_id", String, "The ad group that was addressed", func() { Example("176216228") })
	Attribute("criterion_id", String, "The criterion that was addressed", func() { Example("305729261") })
	Attribute("action", String, "The action that was applied", keywordActionEnum)
	Attribute("resource_name", String, "The criterion resource name Google returned for the applied mutation", func() { Example("customers/1234567890/adGroupCriteria/176216228~305729261") })
	Required("ad_group_id", "criterion_id", "action", "resource_name")
})

// KeywordActions is the outcome of a keyword-actions request.
//
// There is NO partial success. Every action in the request is applied, or none is — the request
// is sent to Google as a single atomic adGroupCriteria:mutate with partial_failure disabled, so
// a rejected operation rolls the whole batch back. That is deliberate for a spend-affecting
// mutation: a caller pausing eight keywords to stop a budget leak, told that five were paused,
// has to work out which three still spend before they can act again. All-or-nothing means the
// remedy is always "fix the input and resend".
//
// This is not the bulk-mutation shape api-catalog.md rule 5 forbids. That rule is about a single
// call cutting across per-target PERMISSION boundaries — bulk status changes over many
// campaigns. Every criterion here belongs to the one campaign named in the path, which is the
// single permission-evaluated target; the batch is one campaign's keywords, not many campaigns.
var KeywordActions = Type("keyword-actions", func() {
	Attribute("campaign_id", String, "The campaign whose keywords were acted on", func() { Example("6f9619ff-8b86-d011-b42d-00c04fc964ff") })
	Attribute("results", ArrayOf(KeywordActionResult), "One entry per requested action, in request order. All applied, or the request failed and none were.")
	Attribute("applied_count", Int, "How many actions were applied. Always equal to the number requested — a partial application is not a possible outcome.", func() { Example(3) })
	Required("campaign_id", "results", "applied_count")
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

	Method("apply-keyword-actions", func() {
		Description("Pause or remove Google Ads keywords on one campaign. " +
			"A MUTATION on a live paid campaign: pausing or removing a keyword changes what serves, so it " +
			"is validated exactly like a create — every action is checked, the campaign's provisioning is " +
			"confirmed, and the campaign's ad account is verified against the project's current connection " +
			"BEFORE Google is contacted at all. " +
			"ALL-OR-NOTHING: the batch is one atomic adGroupCriteria:mutate with partial failure disabled, " +
			"so either every action applied or none did. A caller is never left working out which half of " +
			"a spend-stopping request took effect. " +
			"REMOVE IS IRREVERSIBLE — Google cannot re-enable a removed criterion, only create a new one " +
			"with a new id. " +
			"Google Ads only: a campaign on any other platform is refused with 400, since no other adapter " +
			"models keywords as addressable criteria. " +
			"**409** when the change is refused before Google is contacted: the campaign is unprovisioned " +
			"(no platform campaign id, or no ad group), the campaign belongs to a different ad account than " +
			"the project's connection now resolves to, or the connection row itself is unusable. Those are " +
			"non-retryable, which is why none of them is a 503.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			briefIDAttr()
			campaignIDAttr()
			// MinLength(1) rather than merely Required: an empty array is a request to mutate
			// nothing, and answering it 200 would tell a caller their keywords were paused when
			// no request was ever made. MaxLength bounds one mutate call, matching the platform
			// client's own maxKeywords sanity cap.
			Attribute("actions", ArrayOf(KeywordActionInput), "The keyword mutations to apply, all-or-nothing.", func() {
				MinLength(1)
				MaxLength(60)
			})
			Required("project_id", "brief_id", "campaign_id", "actions")
		})
		Result(KeywordActions)
		commonBriefErrors()
		HTTP(func() {
			POST("/projects/{project_id}/briefs/{brief_id}/campaigns/{campaign_id}/keyword-actions")
			Header("bearer_token:Authorization")
			Response(StatusOK)
			briefErrorResponses()
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
