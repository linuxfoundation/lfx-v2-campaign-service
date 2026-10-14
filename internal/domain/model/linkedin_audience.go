// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

// LinkedIn audience dimensions (the LinkedIn audience read's own vocabulary; one per LinkedIn Ad
// Analytics professional-demographic pivot — see LinkedInAudienceBucket).
const (
	LinkedInAudienceDimensionJobFunction = "job_function"
	LinkedInAudienceDimensionSeniority   = "seniority"
	LinkedInAudienceDimensionIndustry    = "industry"
	LinkedInAudienceDimensionCompanySize = "company_size"
	LinkedInAudienceDimensionCountry     = "country"
)

// LinkedInAudienceBucket is one LinkedIn professional-demographic pivot value's counters, summed
// across the calling project's own campaigns. Value is LinkedIn's pivot value verbatim (a URN such
// as urn:li:function:8), NOT a resolved name. Every counter is APPROXIMATE: LinkedIn documents
// professional-demographic metrics as privacy-approximated and thresholded. No Conversions field,
// like the other audience reads.
type LinkedInAudienceBucket struct {
	Dimension   string
	Value       string
	Impressions int64
	Clicks      int64
	// CostMicros is LinkedIn's costInUsd in micros of USD; no FX.
	CostMicros int64
	Ctr        float64
}

// LinkedInAudienceInsights is the LinkedIn audience read over the CALLING PROJECT'S OWN CAMPAIGNS
// — not the connected ad account, which is shared across foundations. Each dimension
// independently covers the same traffic, so counters total within a dimension, never across them.
type LinkedInAudienceInsights struct {
	Window  MetricsWindow
	Buckets []LinkedInAudienceBucket
}
