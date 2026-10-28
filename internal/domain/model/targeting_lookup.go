// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

// GeoTargetQuery is a geo target constant search: a place name or postal code, optionally
// within one country (ISO 3166-1 alpha-2, upper case).
type GeoTargetQuery struct {
	Query       string
	CountryCode string
}

// GeoTargetSuggestion is one geo target constant a platform suggested for a GeoTargetQuery.
// ID is in exactly the form the platform's campaign config accepts as a geo target.
type GeoTargetSuggestion struct {
	ID            string
	Name          string
	CanonicalName string
	CountryCode   string
	TargetType    string
	Status        string
	Reach         *int64
}

// AudienceUserList is one user list (audience) on an ad account. ResourceName is in exactly
// the form the platform's campaign config accepts as an audience segment.
type AudienceUserList struct {
	ResourceName       string
	ID                 string
	Name               string
	Type               string
	MembershipStatus   string
	AccountStatus      string
	SizeForSearch      *int64
	SizeRangeForSearch string
	EligibleForSearch  bool
}

// AudienceUserLists is a user-list read: the customer it was read from, and its lists.
type AudienceUserLists struct {
	AccountID string
	Lists     []AudienceUserList
}
