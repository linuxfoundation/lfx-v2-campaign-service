// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package design

import (
	//nolint:staticcheck // ST1001: the recommended way of using the goa DSL package is with the . import
	. "goa.design/goa/v3/dsl"
)

// LinkedIn audience read (get-linkedin-ads-audience, LFXV2-2665): professional demographics for the
// campaigns THIS PROJECT owns on LinkedIn, the sibling of get-meta-ads-audience,
// get-twitter-ads-audience and get-microsoft-ads-audience. Kept in its own file, and its method
// declared by linkedInAdsAudienceMethod() (called from the connections Service in connection.go), so
// the LinkedIn audience contract lives in one place.
//
// The dimensions are LinkedIn's documented Ad Analytics professional-demographic pivots
// (MEMBER_JOB_FUNCTION, MEMBER_SENIORITY, MEMBER_INDUSTRY, MEMBER_COMPANY_SIZE, MEMBER_COUNTRY_V2 —
// learn.microsoft.com/en-us/linkedin/marketing/integrations/ads-reporting/ads-reporting-schema).
// The values are LinkedIn's pivot values VERBATIM (URNs such as urn:li:function:12): resolving them
// to names needs one Standardized Data call per URN type (/functions, /seniorities, /industries,
// /geo), which this read does not make, and a name this service invented would be worse than none.

// linkedInAudienceWindowEnum is the subset of metricsWindowEnum the LinkedIn audience read serves:
// exactly the windows the LinkedIn campaign metrics read maps to an explicit Ad Analytics dateRange
// (linkedin.ValidateMetricsWindow). `yesterday` and `last_14_days` have no mapping there, so the
// decoder refuses them rather than a runtime 400 the design did not declare.
func linkedInAudienceWindowEnum() {
	Enum("today", "last_7_days", "last_30_days", "this_month", "last_month")
}

// linkedInAudienceFunctionExample / linkedInAudienceCountryExample are possible bucket shapes
// (ctr = clicks/impressions), shared by the bucket type's examples and the envelope's so the two
// cannot disagree.
var (
	linkedInAudienceFunctionExample = map[string]any{
		"dimension": "job_function", "value": "urn:li:function:8",
		"impressions": 9000, "clicks": 270, "cost_micros": 2700000, "ctr": 0.03,
	}
	linkedInAudienceCountryExample = map[string]any{
		"dimension": "country", "value": "urn:li:geo:103644278",
		"impressions": 7400, "clicks": 185, "cost_micros": 2100000, "ctr": 0.025,
	}
)

// LinkedInAdsAudienceBucket is one LinkedIn professional-demographic pivot value's counters, summed
// across the project's own campaigns.
var LinkedInAdsAudienceBucket = Type("linkedin-ads-audience-bucket", func() {
	Attribute("dimension", String, "Which LinkedIn Ad Analytics professional-demographic pivot this bucket belongs to: job_function (MEMBER_JOB_FUNCTION), seniority (MEMBER_SENIORITY), industry (MEMBER_INDUSTRY), company_size (MEMBER_COMPANY_SIZE) or country (MEMBER_COUNTRY_V2)", func() {
		Enum("job_function", "seniority", "industry", "company_size", "country")
	})
	Attribute("value", String, "LinkedIn's pivot value for this bucket, VERBATIM — a URN such as urn:li:function:8, urn:li:seniority:5, urn:li:industry:4 or urn:li:geo:103644278 (company_size values are passed through as LinkedIn reports them). NOT resolved to a name: LinkedIn resolves each URN type through its Standardized Data APIs (/functions, /seniorities, /industries, /geo), which this read does not call. A value outside a conservative charset fails the read rather than being returned.", func() {
		Pattern(`^[A-Za-z0-9][A-Za-z0-9:_.\-]{0,127}$`)
		Example("urn:li:function:8")
	})
	Attribute("impressions", Int64, "Impressions over the window — APPROXIMATE (LinkedIn's privacy approximation for professional demographics)", func() { Example(9000) })
	Attribute("clicks", Int64, "Clicks over the window — APPROXIMATE", func() { Example(270) })
	Attribute("cost_micros", Int64, "Spend over the window in micro-units of USD (LinkedIn's costInUsd times 10^6, the same unit the LinkedIn campaign metrics read reports) — APPROXIMATE, and per LinkedIn not adjusted for over-delivery on a demographic pivot.", func() { Example(2700000) })
	Attribute("ctr", Float64, "Clicks/Impressions after summing across campaigns, 0 when Impressions is 0", func() { Example(0.03) })
	Required("dimension", "value", "impressions", "clicks", "cost_micros", "ctr")
	// TYPE-LEVEL examples so the published bucket is one a response can contain.
	Example("job_function bucket", linkedInAudienceFunctionExample)
	Example("country bucket", linkedInAudienceCountryExample)
})

// LinkedInAdsAudience is the project-scoped LinkedIn professional-demographic read: the sibling
// audience reads' envelope (window, buckets, bucket_count) plus `approximate`, because LinkedIn
// documents every professional-demographic metric as approximate and privacy-thresholded.
//
// There is deliberately NO conversions field and NO account_currency: cost is costInUsd (always
// USD), the unit the LinkedIn campaign metrics read already publishes.
var LinkedInAdsAudience = Type("linkedin-ads-audience", func() {
	Attribute("window", String, "The reporting window these counters cover, as UTC calendar days (LinkedIn's Ad Analytics dateRange is UTC) — the same days the LinkedIn campaign metrics read uses for the same window name", linkedInAudienceWindowEnum)
	Attribute("buckets", ArrayOf(LinkedInAdsAudienceBucket), "Every bucket across the five pivots, discriminated by `dimension`. Ordered by dimension (job_function, seniority, industry, company_size, country), then impressions descending, then value. Each dimension covers the same traffic independently, so total within one dimension only — and even then the total is BELOW the campaigns' own totals: LinkedIn drops values with fewer than 3 events, withholds values for ads reaching too few members, and returns only the top 100 values per creative per day.", func() {
		Example([]map[string]any{linkedInAudienceFunctionExample, linkedInAudienceCountryExample})
	})
	Attribute("bucket_count", Int, "How many buckets are in `buckets`, across all dimensions.", func() { Example(2) })
	Attribute("approximate", Boolean, "Always true: LinkedIn documents professional-demographic metrics as APPROXIMATE (privacy protection), privacy-thresholded (values under 3 events dropped, values for ads reaching too few members withheld, top 100 values per creative per day) and delayed 12-24 hours. Present every counter as an estimate, never as an exact measurement, and do not reconcile it against the campaign metrics read.", func() { Example(true) })
	Required("window", "buckets", "bucket_count", "approximate")
	// Type-level so bucket_count equals len(buckets).
	Example(map[string]any{
		"window":       "last_30_days",
		"buckets":      []map[string]any{linkedInAudienceFunctionExample, linkedInAudienceCountryExample},
		"bucket_count": 2,
		"approximate":  true,
	})
})

// linkedInAdsAudienceMethod declares get-linkedin-ads-audience inside the connections Service.
func linkedInAdsAudienceMethod() {
	Method("get-linkedin-ads-audience", func() {
		Description("Read LinkedIn professional demographics — job function, seniority, industry, company " +
			"size and country — for this project's own campaigns, live from LinkedIn's Ad Analytics API. " +
			"Scoped to the campaigns this service holds for the project, NOT to the connected ad account, " +
			"and read from the project's OWN connection only (never the LF system account) and only the " +
			"account that connection is bound to. One Ad Analytics statistics request per pivot " +
			"(pivots CAMPAIGN plus the MEMBER_* pivot, filtered by the project's campaign URNs); every " +
			"returned row must name one of those campaigns or the read fails, because the account is " +
			"shared across foundations. A pure read-through; nothing is persisted. A project with no " +
			"LinkedIn campaigns of its own receives an empty `buckets` array and LinkedIn is not " +
			"contacted. All five pivots must load or the request fails (503). Values are LinkedIn URNs, " +
			"verbatim, not names. Every counter is APPROXIMATE (see `approximate`): LinkedIn drops " +
			"values under 3 events, withholds values for ads reaching too few members, keeps only the " +
			"top 100 values per creative per day, and delays demographics 12-24 hours. Cost is USD " +
			"(costInUsd). No conversions. Not gated: LinkedIn has no metrics feature flag.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Attribute("window", String, "Reporting window; defaults to last_30_days when omitted. yesterday and last_14_days are not available on LinkedIn.", linkedInAudienceWindowEnum)
			Required("project_id")
		})
		Result(LinkedInAdsAudience)
		Error("NotFound", NotFoundError, "Resource not found")
		// authErrors() rather than a hand-listed BadRequest: it also declares Unauthorized,
		// which every bearerToken() method must carry or a refused token encodes as a 500.
		authErrors()
		Error("Conflict", ConflictError, "Conflict")
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			GET("/projects/{project_id}/linkedin-ads/audience")
			Header("bearer_token:Authorization")
			connectionAuthErrorResponses()
			Param("window")
			Response(StatusOK)
			Response("NotFound", StatusNotFound)
			Response("Conflict", StatusConflict)
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})
}
