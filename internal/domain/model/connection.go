// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package model holds the campaign service domain entities.
package model

import "time"

// Provider identifies a marketing platform. Each provider maps to its own
// strongly-typed connection table (google_ads_connections, …). Connections are
// singleton per provider per project.
type Provider string

// Supported paid providers. Organic/community channels are added later.
const (
	ProviderGoogleAds    Provider = "google-ads"
	ProviderLinkedInAds  Provider = "linkedin-ads"
	ProviderMetaAds      Provider = "meta-ads"
	ProviderRedditAds    Provider = "reddit-ads"
	ProviderTwitterAds   Provider = "twitter-ads"
	ProviderMicrosoftAds Provider = "microsoft-ads"
	ProviderHubSpot      Provider = "hubspot"
)

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

// Valid reports whether p is a known provider.
func (p Provider) Valid() bool { return p.Table() != "" }

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
