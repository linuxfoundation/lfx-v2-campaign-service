// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package linkedin is a standalone client for the LinkedIn Marketing API.
//
// It ports the TypeScript linkedin-ads.service.ts client to Go. The package
// creates a full sponsored-content campaign in a single call
// (Client.CreateCampaign): Campaign Group -> Campaign -> Dark Post
// (feedDistribution NONE) -> Creative, mirroring the LinkedIn hierarchy.
//
// Settled architecture: credentials AND runtime configuration are INJECTED by
// the caller. Unlike the TypeScript original, this package performs NO
// os.Getenv calls and NO file reads. In production the OAuth2 access token comes
// from the decrypted stored connection and the RuntimeConfig comes from a
// config source the caller wires up. The package only knows about HTTP.
package linkedin

import "time"

// baseURL is the LinkedIn Marketing API base. Mirrors LINKEDIN_BASE_URL.
const baseURL = "https://api.linkedin.com/rest"

// apiVersion is sent as the LinkedIn-Version header. Ported verbatim from
// LINKEDIN_API_VERSION in the shared constants.
const apiVersion = "202602"

// requestTimeout mirrors LINKEDIN_REQUEST_TIMEOUT_MS (30s).
const requestTimeout = 30 * time.Second

// maxResponseBytes caps how much of a response body is read into memory, far
// above any legitimate LinkedIn API response, to prevent memory exhaustion.
const maxResponseBytes = 10 << 20 // 10 MiB

// maxNameLen is LinkedIn's limit on campaign-group and campaign names.
const maxNameLen = 255

// retryMax is the number of times a 429 (rate-limited) request is retried
// before giving up. Mirrors the resilience the Twitter client (#19) applies.
const retryMax = 3

// retryBaseDelay is the base for exponential backoff when the API returns a 429
// without a usable Retry-After header (1s, 2s, 4s, ...).
const retryBaseDelay = 1 * time.Second

// maxRetryWait caps how long a single 429 backoff waits, so an outsized
// Retry-After value can't stall a request past the point of usefulness.
const maxRetryWait = 60 * time.Second

// jobFunctions are the default job-function facets included in targeting.
// Mirrors JOB_FUNCTIONS.
var jobFunctions = []string{
	"urn:li:function:8",
	"urn:li:function:13",
	"urn:li:function:16",
}

// seniorityExclusions are the default seniority facets excluded from targeting.
// Mirrors SENIORITY_EXCLUSIONS.
var seniorityExclusions = []string{
	"urn:li:seniority:1",
	"urn:li:seniority:3",
}

// skipStatuses are campaign/group statuses treated as "not a live match"
// during idempotent search-by-name. Mirrors SKIP_STATUSES.
var skipStatuses = map[string]struct{}{
	"ARCHIVED":         {},
	"CANCELED":         {},
	"COMPLETED":        {},
	"DRAFT":            {},
	"REMOVED":          {},
	"DELETED":          {},
	"PENDING_DELETION": {}, // terminal: a being-deleted resource is not a live match
}

// GeoTarget is a resolved geo location. Mirrors LinkedInGeoTarget.
type GeoTarget struct {
	Label string `json:"label"`
	URN   string `json:"urn"`
}

// geoResolveMap is the static name->URN lookup used to resolve geo targets
// without a network round-trip. Ported verbatim from LINKEDIN_GEO_RESOLVE_MAP.
// Keys are lowercase, trimmed location names.
var geoResolveMap = map[string]GeoTarget{
	"japan":          {Label: "Japan", URN: "urn:li:geo:101355337"},
	"india":          {Label: "India", URN: "urn:li:geo:102713980"},
	"singapore":      {Label: "Singapore", URN: "urn:li:geo:102454443"},
	"south korea":    {Label: "South Korea", URN: "urn:li:geo:105149562"},
	"australia":      {Label: "Australia", URN: "urn:li:geo:101452733"},
	"taiwan":         {Label: "Taiwan", URN: "urn:li:geo:104441761"},
	"hong kong":      {Label: "Hong Kong", URN: "urn:li:geo:103291313"},
	"united states":  {Label: "United States", URN: "urn:li:geo:103644278"},
	"usa":            {Label: "United States", URN: "urn:li:geo:103644278"},
	"germany":        {Label: "Germany", URN: "urn:li:geo:101165590"},
	"united kingdom": {Label: "United Kingdom", URN: "urn:li:geo:106693272"},
}

// Credentials carries the injected OAuth2 bearer token used for every request.
// In production this is the decrypted access token from the stored connection.
type Credentials struct {
	// AccessToken is the OAuth2 bearer token (LINKEDIN_ACCESS_TOKEN equivalent).
	AccessToken string
}

// Account is one ad-account / organization pairing in the runtime config.
// Mirrors LinkedInAccount.
type Account struct {
	AccountID string `json:"accountId"`
	Label     string `json:"label"`
	OrgID     string `json:"orgId"`
	// Status is optional; when present it is one of ACTIVE or BILLING_HOLD.
	Status string `json:"status,omitempty"`
}

// TargetingProfileConfig is a named targeting profile from the runtime config.
// Mirrors LinkedInTargetingProfileConfig.
type TargetingProfileConfig struct {
	ID     string   `json:"id"`
	Label  string   `json:"label"`
	Skills []string `json:"skills"`
	Groups []string `json:"groups"`
}

// RuntimeConfig is the injected, vendor-specific configuration. Mirrors
// LinkedInRuntimeConfig. It is passed whole to NewClient; the package never
// reads it from disk or env.
type RuntimeConfig struct {
	DefaultAccountID   string                   `json:"defaultAccountId"`
	DefaultOrgID       string                   `json:"defaultOrgId"`
	Accounts           []Account                `json:"accounts"`
	EmployerExclusions []string                 `json:"employerExclusions"`
	TargetingProfiles  []TargetingProfileConfig `json:"targetingProfiles"`
}
