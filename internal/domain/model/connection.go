// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package model holds the campaign service domain entities.
package model

import "time"

// Provider identifies a marketing platform. Each provider maps to its own
// strongly-typed connection table (google_ads_connections, …). Connections are
// singleton per provider per project.
type Provider string

// Supported providers: six PAID ad platforms plus hubspot, the EMAIL channel (see
// ChannelKind). Organic/community channels are added later.
const (
	ProviderGoogleAds    Provider = "google-ads"
	ProviderLinkedInAds  Provider = "linkedin-ads"
	ProviderMetaAds      Provider = "meta-ads"
	ProviderRedditAds    Provider = "reddit-ads"
	ProviderTwitterAds   Provider = "twitter-ads"
	ProviderMicrosoftAds Provider = "microsoft-ads"
	ProviderHubSpot      Provider = "hubspot"
)

// SystemProjectID is the reserved connection scope holding the LF-owned platform
// credentials a project falls back to when it has connected no account of its own.
//
// A reserved scope rather than a new table or LFX_SYS_* environment variables: a
// system account needs exactly what a project account needs (encryption at rest,
// an account id, provider config, a status, an If-Match version, an updated_by
// trail), so bootstrap and account discovery work on it with no new code.
//
// The value is deliberately UNREACHABLE through the API: projectSlugProblem's
// `^[a-z0-9]+(-[a-z0-9]+)*$` cannot match the colon, so no create endpoint can
// plant a row here. Necessary but NOT sufficient — the other paths stay permissive
// on project_id for historical UUID rows and reject this scope explicitly (see
// rejectSystemScope in internal/service).
const SystemProjectID = "system:linuxfoundation"

// providerConfigKeys lists the non-secret, provider-specific config keys each provider
// PERSISTS, in snake_case wire form and in a fixed order. It is exhaustive by construction:
// storage gives each key its own column, so a key that is not here has nowhere to be written.
//
// It lives in the domain rather than beside the SQL because two callers need the SAME answer
// for opposite reasons — the repository builds its column list from it, and any writer that
// accepts operator- or caller-supplied config must REJECT a key outside it. Two copies would
// let the second drift into accepting a key the first silently drops.
//
// The values are compile-time constants, never input, which is what makes interpolating them
// into SQL safe.
var providerConfigKeys = map[Provider][]string{
	ProviderGoogleAds:   {"login_customer_id"},
	ProviderLinkedInAds: {"org_id"},
	ProviderMetaAds:     {"page_id", "app_id"},
	// conversion_pixel_id is stored on the CONNECTION, not per campaign: it identifies the
	// advertiser's pixel, which is a property of the ad account and the same for every
	// campaign created through it. Reddit requires it on EVERY campaign create observed on
	// the LF account (2026-08-13) — including CLICKS/Traffic, not only CONVERSIONS as the
	// docs describe — so a campaign that cannot supply one cannot be created at all.
	ProviderRedditAds:    {"conversion_pixel_id"},
	ProviderTwitterAds:   {"funding_instrument_id"},
	ProviderMicrosoftAds: {"customer_id"},
	ProviderHubSpot:      {"portal_id", "sender_email", "sender_name", "brand_kit"},
}

// ConfigKeys returns the provider's persisted config keys in their fixed order. The slice is a
// copy: callers append to it to build column lists, and appending to the package's own backing
// array would corrupt it for everyone else.
func (p Provider) ConfigKeys() []string {
	keys := providerConfigKeys[p]
	out := make([]string, len(keys))
	copy(out, keys)
	return out
}

// Table returns the Postgres table name backing this provider's connections.
func (p Provider) Table() string {
	switch p {
	case ProviderGoogleAds:
		return "google_ads_connections"
	case ProviderLinkedInAds:
		return "linkedin_ads_connections"
	case ProviderMetaAds:
		return "meta_ads_connections"
	case ProviderRedditAds:
		return "reddit_ads_connections"
	case ProviderTwitterAds:
		return "twitter_ads_connections"
	case ProviderMicrosoftAds:
		return "microsoft_ads_connections"
	case ProviderHubSpot:
		return "hubspot_connections"
	default:
		return ""
	}
}

// Valid reports whether p is a known, CLASSIFIED provider (see the note on Kind() below for
// why classification is the gate rather than Table()).
func (p Provider) Valid() bool { return validFrom(p.Table(), p.Kind()) }

// validFrom is the validity PREDICATE, split out so it is testable on its own. No real
// provider has a table without a kind — that is the invariant Valid() enforces — so a test
// written against the constants can never exercise the table-but-unclassified case, and would
// silently pass even if Valid() stopped consulting Kind(). Feeding the predicate directly is
// the only way to pin the coupling. See TestValidFromRequiresBothTableAndKind.
func validFrom(table string, kind ChannelKind) bool { return table != "" && kind != "" }

// ChannelKind classifies what KIND of marketing channel a provider is. The distinction is
// BEHAVIOURAL, not cosmetic: a paid ad channel CREATES a campaign that spends budget and can
// be paused/resumed mid-flight, whereas the email channel STAGES a draft a human sends — it
// has no budget, no delivery this service controls, and nothing to pause. Code that branches
// on "is this an ad platform?" should ask here rather than comparing against ProviderHubSpot,
// so adding a second email provider does not mean hunting down every hardcoded check.
type ChannelKind string

// Channel kinds.
const (
	// ChannelPaidAds is an ad platform: budgeted, dispatchable, and pausable.
	ChannelPaidAds ChannelKind = "paid-ads"
	// ChannelEmail is the email channel: stages a draft, no budget, not pausable.
	ChannelEmail ChannelKind = "email"
)

// Kind reports the channel kind of p. An unknown provider returns "" (mirroring Table()).
//
// This deliberately enumerates each provider rather than defaulting: a NEW provider added to
// the Provider list will return "" here until it is classified, which surfaces the omission
// instead of silently inheriting the wrong behaviour.
func (p Provider) Kind() ChannelKind {
	switch p {
	case ProviderGoogleAds, ProviderLinkedInAds, ProviderMetaAds,
		ProviderRedditAds, ProviderTwitterAds, ProviderMicrosoftAds:
		return ChannelPaidAds
	case ProviderHubSpot:
		return ChannelEmail
	default:
		return ""
	}
}

// Valid is deliberately defined in terms of Kind(), not Table(). Go cannot enumerate a const
// block, so no test can prove a hand-written list is complete — but tying VALIDITY to
// CLASSIFICATION makes the compiler's job unnecessary: a provider that Kind() does not
// classify is not a valid provider at all, so it is rejected by every Valid() check on the
// request path rather than silently taking a default branch deep inside the service.
//
// The practical effect: adding a provider constant and a Table() case without a Kind() case
// yields a provider the API rejects outright — a loud, immediate failure at the boundary
// instead of a subtle misclassification. See TestValidFromRequiresBothTableAndKind.

// IsPaidAds reports whether p is a paid ad channel (budgeted, pausable) rather than email.
func (p Provider) IsPaidAds() bool { return p.Kind() == ChannelPaidAds }

// AllProviders returns every provider this service supports, in a stable order.
//
// This is the ENUMERABLE source of truth that makes exhaustiveness testable: Go has no way to
// iterate a const block, so without it a test can only walk a hand-maintained list — and a new
// provider omitted from both Kind() and that list would pass silently. Tests iterate this and
// assert each entry classifies, so adding a provider here (which you must, to make it usable)
// forces it to be classified too.
//
// Keep in sync with the Provider constants above. TestAllProvidersAreValidAndUnique catches a
// DUPLICATE or a table-less entry here, but it cannot prove completeness: Go cannot enumerate
// a const block, so a provider constant omitted from this list is not detectable by any test.
// Valid() requiring Kind() is the real backstop — an unclassified provider is invalid whether
// or not anyone remembered to list it.
func AllProviders() []Provider {
	return []Provider{
		ProviderGoogleAds,
		ProviderLinkedInAds,
		ProviderMetaAds,
		ProviderRedditAds,
		ProviderTwitterAds,
		ProviderMicrosoftAds,
		ProviderHubSpot,
	}
}

// ConnectionStatus is the lifecycle status of a connection.
type ConnectionStatus string

// Connection statuses.
const (
	StatusActive   ConnectionStatus = "active"
	StatusInactive ConnectionStatus = "inactive"
	StatusError    ConnectionStatus = "error"
	StatusDeleted  ConnectionStatus = "deleted" // soft delete
)

// Actor captures who performed an action, retained inline for attribution
// because connections are not indexed into the Query Service.
type Actor struct {
	Name     string `json:"name,omitempty"`
	Email    string `json:"email,omitempty"`
	Username string `json:"username,omitempty"`
}

// Connection is the common shape shared by every provider's connection table.
// Provider-specific configuration is carried in ProviderConfig (persisted as
// the provider table's typed columns; the repository maps it per provider) and
// write-only credentials are never part of this read model.
//
// The singleton invariant (one connection per provider per project) is enforced
// by a partial unique index on (project_id) WHERE status <> 'deleted', so a
// soft-deleted row no longer blocks re-creating the connection.
type Connection struct {
	ID        string
	ProjectID string
	Provider  Provider
	Label     string
	AccountID string
	// EncryptedCredentials is the AES-256-GCM ciphertext blob. It is never
	// returned to callers; the read model exposes HasCredentials instead.
	EncryptedCredentials []byte
	// ProviderConfig holds the provider-specific, non-credential columns
	// (e.g. org_id for LinkedIn, page_id/app_id for Meta). The repository
	// projects these onto the provider table's real columns.
	ProviderConfig map[string]string
	Status         ConnectionStatus
	// Version is the optimistic-concurrency counter surfaced as the ETag.
	Version   int64
	CreatedBy *Actor
	UpdatedBy *Actor
	CreatedAt time.Time
	UpdatedAt time.Time
}

// HasCredentials reports whether an encrypted credential is stored, without
// exposing the credential itself.
func (c *Connection) HasCredentials() bool { return len(c.EncryptedCredentials) > 0 }

// AccessibleAccount represents a minimal view of an accessible ad account
// returned by an account-discovery query. It contains only the identifying
// information needed by the UI to present account options to the user.
type AccessibleAccount struct {
	// ID is the account identifier in the ad platform's namespace (e.g. Google Ads
	// customer ID, LinkedIn member ID). Platform-specific format and semantics.
	ID string
	// Label is a human-readable name or display label for the account.
	Label string
}

// MarketingEmail is a minimal view of one marketing email reachable through a stored
// connection, for a picker that has to choose which email a campaign will CLONE.
//
// Deliberately not an AccessibleAccount. Discovery on the ad platforms answers "which account
// may this credential act as?", and the chosen id is stored on the connection. This answers
// "which email should be cloned?", and the chosen id travels per campaign in the dispatch
// config (hubspotConfig.SourceEmailID) — a different question with a different lifetime, so
// sharing the type would only make two unrelated things look interchangeable.
type MarketingEmail struct {
	// ID is the HubSpot marketing-email id, in the form sourceEmailId expects.
	ID string
	// Name is the internal email name as it appears in the HubSpot email list.
	Name string
	// Subject is the subject line.
	Subject string
	// State is HubSpot's lifecycle state (DRAFT, PUBLISHED, …). Carried so a picker can show
	// that a template is a draft before someone clones it.
	//
	// NOT archived: HubSpot tracks archival as a separate `archived` boolean, and the search
	// does not request archived rows, so they never appear in a result at all. A `state`
	// value cannot express an absence.
	State string
	// UpdatedAt is the last-modified timestamp (ISO-8601). Results arrive ordered
	// most-recently-updated first; this is carried anyway because two templates routinely
	// share a name and the date is what distinguishes them in a list.
	UpdatedAt string
}

// KeywordRow is one keyword's live performance on an ad platform, in the
// platform-agnostic vocabulary. Never persisted — it is a read-through snapshot.
//
// CriterionID and AdGroupID travel together because a criterion id is unique only within
// its ad group: acting on a criterion id alone would mean guessing the ad group, and a
// wrong guess addresses a different, real keyword rather than failing.
type KeywordRow struct {
	CriterionID string
	AdGroupID   string
	CampaignID  string
	Text        string
	MatchType   string
	Status      string
	Impressions int64
	Clicks      int64
	CostMicros  int64
	Ctr         float64
}

// KeywordPerformance is an account-wide keyword read over one window.
type KeywordPerformance struct {
	Window MetricsWindow
	Rows   []KeywordRow
	// Truncated reports that the account holds MORE keywords than Rows carries. A consumer
	// must not total a truncated slice and present it as account-wide spend.
	Truncated bool
}

// Audience breakdown dimensions. A bucket names which breakdown it belongs to so all
// three can share one flat array.
const (
	AudienceDimensionAge    = "age"
	AudienceDimensionGender = "gender"
	AudienceDimensionDevice = "device"
)

// AudienceBucket is one demographic slice's counters over a window.
type AudienceBucket struct {
	Dimension   string
	Value       string
	Impressions int64
	Clicks      int64
	CostMicros  int64
	Ctr         float64
}

// AudienceInsights is an account-wide demographic read across every breakdown.
//
// Each dimension independently covers the SAME traffic, so impressions may be totalled
// within a dimension but never across them — summing age, gender and device triple-counts.
type AudienceInsights struct {
	Window  MetricsWindow
	Buckets []AudienceBucket
}

// KeywordAction is one requested keyword mutation on a live campaign.
type KeywordAction struct {
	AdGroupID   string
	CriterionID string
	// Action is KeywordActionPause or KeywordActionRemove.
	Action string
}

// Keyword actions. There is deliberately no "enable": this surface only ever reduces what
// serves. Widening delivery goes through the create/dispatch path, where budget and flight
// are validated together.
const (
	KeywordActionPause = "PAUSE"
	// KeywordActionRemove is IRREVERSIBLE upstream — a removed criterion cannot be
	// re-enabled, only re-created with a new id.
	KeywordActionRemove = "REMOVE"
)

// KeywordActionOutcome is one applied keyword mutation.
type KeywordActionOutcome struct {
	AdGroupID    string
	CriterionID  string
	Action       string
	ResourceName string
}
