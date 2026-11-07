// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

// AudienceResource is one audience entry on an ad account (#496): a Google Ads custom
// audience, combined audience, Audience resource or user interest. ResourceName is the form the
// platform's campaign config accepts.
type AudienceResource struct {
	ResourceName string
	ID           string
	Name         string
	// Subtype is the platform's own classification, verbatim; empty when it has none.
	Subtype string
	// Status is empty when the platform's resource has no status (Google's user_interest).
	Status      string
	Description string
	// Facet is the create input the entry goes under, where the platform's create takes one of
	// several (LinkedIn: the linkedInConfig.targeting key, #552); empty when not applicable or
	// unknown.
	Facet string
	// MemberCount is the platform's approximate size of the entry; nil when it reported none.
	MemberCount *int64
}

// AudienceResources is an audience read: the account it was read from, the kind that was
// read, and every usable entry of that kind.
type AudienceResources struct {
	AccountID string
	Kind      string
	Audiences []AudienceResource
}
