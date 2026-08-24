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

import (
	"strings"
	"time"
)

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

// minDailyBudgetUSD and minLifetimeBudgetUSD are LinkedIn's minimum campaign
// budgets. LinkedIn requires at least $10 for a dailyBudget and at least $100
// for a totalBudget (lifetime) campaign; a lower amount is rejected by LinkedIn
// only AFTER the campaign group (a permanent resource) already exists, so the
// client enforces these up front. NOTE: these minimums are USD-specific — the
// client only ever sends currencyCode "USD", so a single pair of constants is
// correct here; a multi-currency client would need per-currency minimums.
const (
	minDailyBudgetUSD    = 10.0
	minLifetimeBudgetUSD = 100.0
)

// oauthTokenURL is LinkedIn's OAuth2 token endpoint, used for the
// refresh_token -> access_token exchange. Per LinkedIn's published contract the
// exchange is a POST with Content-Type x-www-form-urlencoded carrying
// grant_type=refresh_token, refresh_token, client_id and client_secret.
// https://learn.microsoft.com/en-us/linkedin/shared/authentication/programmatic-refresh-tokens
const oauthTokenURL = "https://www.linkedin.com/oauth/v2/accessToken"

// tokenExpiryBuffer refreshes the access token this long before its stated
// expiry so an in-flight request is not made with a just-expired token. Mirrors
// the google-ads and microsoft clients.
const tokenExpiryBuffer = 60 * time.Second

// defaultTokenTTL is the fallback access-token lifetime used when the OAuth
// response omits (or reports a non-positive) expires_in, so a valid-but-
// lifetimeless token still works without caching an already-expired entry.
const defaultTokenTTL = 30 * time.Minute

// refreshTokenExpiryWarning is how far ahead of the REFRESH token's own deadline
// the client starts warning. LinkedIn does NOT reset a refresh token's TTL when
// it is used, so the whole connection hard-stops ~365 days after the member last
// authorized it and only a human re-authorization can restore it. Warning 30 days
// out leaves a full campaign cycle to schedule that reconnect.
const refreshTokenExpiryWarning = 30 * 24 * time.Hour

// serviceErrorCodeExpiredAccessToken is LinkedIn's subcode for an expired access
// token, observed on a 401 as {"serviceErrorCode":65602,"status":401}. It is
// treated as ONE of several expiry signals, never the sole test: LinkedIn's
// published error-handling guide documents expired/revoked/invalid tokens as
// distinct 401 conditions WITHOUT committing to a subcode for each, so matching
// only 65602 would miss revocation and plain invalidity.
const serviceErrorCodeExpiredAccessToken = 65602

// retryMax is the number of times a 429 (rate-limited) request is retried
// before giving up. Mirrors the resilience the Twitter client (#19) applies.
const retryMax = 3

// retryBaseDelay is the base for exponential backoff when the API returns a 429
// without a usable Retry-After header (1s, 2s, 4s, ...).
const retryBaseDelay = 1 * time.Second

// maxRetryWait caps how long a single 429 backoff waits, so an outsized
// Retry-After value can't stall a request past the point of usefulness.
const maxRetryWait = 60 * time.Second

// startTimeBuffer is added to "now" when a campaign starts today/in the past, so
// the campaign group and campaign runSchedule.start isn't already in the past by
// the time LinkedIn receives the POST.
//
// The buffer must exceed a SINGLE lookup+create step's worst-case latency, NOT
// the whole multi-lookup flow: the start is recomputed as (now + startTimeBuffer)
// immediately before EACH mutation — the group start inside findOrCreateCampaignGroup
// just before the group POST (AFTER its own find-existing lookup, NOT reused from
// the validateSchedule preflight value) and the campaign start inside
// createSponsoredCampaign just before the campaign POST (likewise recomputed). The
// validateSchedule preflight only VALIDATES the dates up front; its computed start
// is discarded. A fixed buffer alone could never reliably cover a variable
// multi-lookup flow — the campaign create runs AFTER the group lookup+create AND the
// campaign lookup, and even the group create runs after the group lookup, so a
// once-computed start could slip into the past before either POST, orphaning a
// just-created resource. Recomputing per mutation removes that dependence on total
// elapsed time; the buffer only has to absorb one lookup+create step.
//
// One step's worst case: a single find-existing lookup can span up to
// (retryMax+1)=4 request attempts, each bounded by requestTimeout (30s), plus up
// to retryMax=3 Retry-After waits each capped at maxRetryWait (60s) — i.e. roughly
// 4×30s + 3×60s ≈ 5 minutes. 10 minutes clears that ~5-minute worst case with
// headroom for network/scheduling latency between the recompute and the POST.
const startTimeBuffer = 10 * time.Minute

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
// without a network round-trip. Ported from LINKEDIN_GEO_RESOLVE_MAP, with the
// germany/united kingdom URNs corrected: the shared constant maps germany ->
// urn:li:geo:101165590 (actually the United Kingdom) and united kingdom ->
// urn:li:geo:106693272 (actually Switzerland), which would target paid campaigns
// at the wrong country. Verified against LinkedIn's geo reference: Germany is
// 101282230 and the United Kingdom is 101165590. The upstream shared constant
// carries the same defect and must be fixed separately (tracked: LFXV2-2665) so
// the two do not silently diverge.
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
	"germany":        {Label: "Germany", URN: "urn:li:geo:101282230"},
	"united kingdom": {Label: "United Kingdom", URN: "urn:li:geo:101165590"},
}

// Credentials carries the injected OAuth2 material used for every request. In
// production these are the decrypted values from the stored connection; the
// package never reads them from env, disk, or the database.
//
// Refresh is OPTIONAL and degrades cleanly. LinkedIn issues programmatic refresh
// tokens only to approved Marketing Developer Platform partners
// (https://learn.microsoft.com/en-us/linkedin/shared/authentication/programmatic-refresh-tokens),
// so a connection may legitimately carry an access token alone. When RefreshToken,
// ClientID or ClientSecret is empty the client behaves exactly as before —
// bearer-only, no refresh attempt — and an expired token surfaces as
// ErrCredentialsExpired naming the connection, rather than as an opaque 401.
type Credentials struct {
	// AccessToken is the OAuth2 bearer token (LINKEDIN_ACCESS_TOKEN equivalent).
	// Per LinkedIn's docs this is valid for 60 days.
	AccessToken string

	// AccessTokenExpiresAt is when AccessToken expires, when known. The zero value
	// means "unknown": the client then trusts the token until LinkedIn rejects it,
	// rather than assuming an expiry it was never told.
	AccessTokenExpiresAt time.Time

	// RefreshToken is exchanged for a fresh access token. Empty when the app is not
	// approved for programmatic refresh tokens.
	RefreshToken string

	// RefreshTokenExpiresAt is when RefreshToken itself expires (LinkedIn: ~365
	// days, and the TTL does NOT reset on use). The zero value means unknown.
	RefreshTokenExpiresAt time.Time

	// ClientID and ClientSecret authenticate the refresh exchange. Both are
	// required alongside RefreshToken for refresh to be possible.
	ClientID     string
	ClientSecret string

	// ConnectionName names the stored connection these credentials came from. It is
	// used ONLY to make an expiry error actionable ("which connection do I
	// reconnect?"). It must never carry credential material.
	ConnectionName string

	// ConnectionID is the immutable id of the connection ROW these credentials were
	// decrypted from. It exists solely to give the near-expiry warning a per-connection
	// dedupe identity, and it is never logged and never sent upstream.
	//
	// The three fields that were available before it cannot do that job. ConnectionName is
	// operator-set and OPTIONAL, so every unnamed connection shares one fallback label;
	// ClientID is the OAuth APPLICATION id, shared by every connection on one Marketing
	// Developer Platform app; and the runtime account id is EMPTY on the discovery path and
	// CHANGES for one connection once an account is selected — so it both merges distinct
	// connections and splits a single one. A row id does neither.
	//
	// It is not a secret: it names a row, and reading it grants nothing.
	ConnectionID string
}

// CanRefresh reports whether these credentials carry everything the refresh
// exchange needs. LinkedIn requires grant_type, refresh_token, client_id and
// client_secret; without all three stored values no exchange is possible.
func (c Credentials) CanRefresh() bool {
	return strings.TrimSpace(c.RefreshToken) != "" &&
		strings.TrimSpace(c.ClientID) != "" &&
		strings.TrimSpace(c.ClientSecret) != ""
}

// ConnectionLabel returns a safe, non-empty identifier for the connection in
// error messages. ConnectionName is operator-set metadata, never a secret.
func (c Credentials) ConnectionLabel() string {
	if n := strings.TrimSpace(c.ConnectionName); n != "" {
		return n
	}
	return "the LinkedIn connection"
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
