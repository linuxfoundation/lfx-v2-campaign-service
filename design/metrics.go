// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package design — campaign-metrics endpoints.
//
// Hierarchy: Project -> Brief -> Campaign -> Metrics. Metrics are the daily
// performance numbers read back from the ad platform and STORED, so a campaign's
// history survives the platform's own rolling reporting window (and remains readable
// after a campaign ends, when the platform may stop serving it at all).
//
// This is the stored-history view. It complements the live read-through
// GET /projects/{projectId}/{provider}/metrics documented in docs/api-catalog.md:
// that one answers "what is this account doing right now", this one answers "what did
// this campaign do over time". See docs/campaign-metrics.md.
//
// Every endpoint is gated on campaign_manager at the gateway.
package design

import (
	//nolint:staticcheck // ST1001: the recommended way of using the goa DSL package is with the . import
	. "goa.design/goa/v3/dsl"
)

// ─── Metrics types ───

// CampaignMetric is ONE platform's raw reported numbers for ONE campaign on ONE day.
//
// spend and conversions are STRINGS, not numbers. They are exact decimals
// (NUMERIC(18,6) in storage) and are summed across days and campaigns downstream;
// encoding them as JSON floats would reintroduce the representation error the storage
// type exists to prevent, and a client parsing them as IEEE doubles would silently
// drift on large totals. A string carries the platform's exact figure end to end.
var CampaignMetric = Type("campaign-metric", func() {
	Attribute("metric_date", String, "Reporting day in the ad account's timezone (YYYY-MM-DD)")
	Attribute("platform", String, "Platform that reported these numbers")
	Attribute("impressions", Int64, "Impressions delivered")
	Attribute("clicks", Int64, "Clicks recorded")
	Attribute("spend", String, "Spend as an exact decimal string, in `currency`")
	Attribute("conversions", String, "Conversions as an exact decimal string; MAY be fractional under data-driven attribution")
	Attribute("currency", String, "ISO 4217 code that `spend` is denominated in")
	Attribute("attribution_basis", String, "How THIS row's conversions were counted (e.g. google-ads:click-time). Rows with different bases must not have their conversions summed.")
	Attribute("fetched_at", String, "When the service last refreshed this row (RFC3339)")
	Required("metric_date", "platform", "impressions", "clicks", "spend", "conversions")
})

// MetricsSummary is an EXPLICITLY derived rollup over the returned rows, carrying the
// caveats that make it honest.
//
// The two "*_comparable" flags are load-bearing, not advisory. When a flag is false
// the corresponding total is ABSENT from the payload entirely, so a consumer that
// ignores the caveat physically cannot read a misleading number. That is deliberate:
// a footnote does not survive a trip through a dashboard, a spreadsheet or a slide,
// whereas a missing field forces the question.
//
// Platforms disagree on what a conversion is and the window it is counted in (Meta
// defaults to 7-day click / 1-day view; Google counts at click time with per-action
// windows and reports fractional credit), so a cross-platform conversion sum is wrong
// in a way that looks plausible. Spend is safely summable, but only within one
// currency: this service does no FX conversion.
var MetricsSummary = Type("metrics-summary", func() {
	Attribute("impressions", Int64, "Total impressions. Always present: a delivery count, not an attributed outcome.")
	Attribute("clicks", Int64, "Total clicks. Always present, for the same reason.")
	Attribute("spend", String, "Total spend as an exact decimal string. ABSENT when `currency_uniform` is false, because no FX rate is available to combine currencies.")
	Attribute("currency", String, "The single currency `spend` is denominated in; absent when rows span several.")
	Attribute("currency_uniform", Boolean, "True when every row shared one currency. When false, `spend` is omitted rather than being a sum of incomparable amounts.")
	Attribute("conversions", String, "Total conversions as an exact decimal string. ABSENT when `conversions_comparable` is false.")
	Attribute("attribution_basis", String, "The single attribution basis every row shared; absent when they differ.")
	Attribute("conversions_comparable", Boolean, "True when every row shared one KNOWN attribution basis. When false, `conversions` is omitted: summing conversions counted under different windows or models produces a number that is wrong in a way that looks right. An unknown basis is never comparable, not even with another unknown one.")
	Attribute("row_count", Int, "Number of daily rows the summary covers")
	Required("impressions", "clicks", "currency_uniform", "conversions_comparable", "row_count")
})

// ─── Metrics service ───

var _ = Service("lfx-v2-campaign-service-metrics", func() {
	Description("Read stored per-day performance metrics for a campaign, with an explicitly-derived and caveat-labelled summary.")

	Security(JWTAuth)

	Method("get-campaign-metrics", func() {
		Description("Get a campaign's stored daily metrics plus a summary. The window defaults to the trailing 30 days; `from`/`to` (YYYY-MM-DD, inclusive) override it. Conversions and spend totals are omitted from the summary when the rows are not comparable — see `conversions_comparable` and `currency_uniform`.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			briefIDAttr()
			campaignIDAttr()
			Attribute("from", String, "Window start, inclusive (YYYY-MM-DD). Defaults to 30 days before `to`.", func() {
				Pattern(`^\d{4}-\d{2}-\d{2}$`)
			})
			Attribute("to", String, "Window end, inclusive (YYYY-MM-DD). Defaults to today (UTC).", func() {
				Pattern(`^\d{4}-\d{2}-\d{2}$`)
			})
			Required("project_id", "brief_id", "campaign_id")
		})
		Result(func() {
			Attribute("metrics", ArrayOf(CampaignMetric), "Daily rows, ascending by date")
			Attribute("summary", MetricsSummary, "Explicitly-derived rollup with its attribution caveats")
			Required("metrics", "summary")
		})
		commonBriefErrors(false)
		HTTP(func() {
			GET("/projects/{project_id}/briefs/{brief_id}/campaigns/{campaign_id}/metrics")
			Header("bearer_token:Authorization")
			Param("from")
			Param("to")
			// No ETag. Unlike the campaign/audience resources this has no `version`
			// column and no optimistic-concurrency story: metrics are an upsert time
			// series that changes underneath the caller BY DESIGN, because platforms
			// restate recent days as conversions mature. Emitting an ETag would imply a
			// concurrency contract this resource does not have.
			Response(StatusOK)
			briefErrorResponses(false)
		})
	})
})
