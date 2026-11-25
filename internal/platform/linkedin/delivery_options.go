// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"errors"
	"fmt"
)

// Audience Expansion and the LinkedIn Audience Network at create time (#667, #552 gap 8).
//
// SOURCE. LinkedIn Marketing API, "Create and Manage Campaigns" (li-lms-2026-09), campaign schema,
// consulted 2026-11-25:
//
//   - audienceExpansionEnabled (boolean, optional): "Enable Audience Expansion for the campaign
//     provides query expansion for certain targeting criteria." Omitted, LinkedIn reads false.
//   - offsiteDeliveryEnabled (boolean, optional): "Indicates if this campaign's ads will also be
//     served on the LinkedIn Audience Network". The Audience Network serves Sponsored Content
//     only, so a TEXT_AD campaign cannot opt in.
//
// Both stay off unless the caller turns them on: off is the right default for events, and both
// widen the audience beyond what the targeting says. The create body has always sent
// offsiteDeliveryEnabled false and no audienceExpansionEnabled; with neither option set it still
// does, byte for byte.

// Field names of the two options, as linkedInConfig spells them and as the campaign body does.
const (
	FieldAudienceExpansionEnabled = "audienceExpansionEnabled"
	FieldOffsiteDeliveryEnabled   = "offsiteDeliveryEnabled"
)

// ErrDeliveryOptionInvalid marks a delivery option this client refuses before any request.
var ErrDeliveryOptionInvalid = errors.New("linkedin: delivery option not accepted")

// DeliveryOptions are the caller's Audience Expansion and Audience Network choices. nil means not
// chosen: Audience Expansion is then not sent (LinkedIn's default, off) and the Audience Network
// is sent off.
type DeliveryOptions struct {
	AudienceExpansionEnabled *bool
	OffsiteDeliveryEnabled   *bool
}

// Set reports whether either option was chosen.
func (o DeliveryOptions) Set() bool {
	return o.AudienceExpansionEnabled != nil || o.OffsiteDeliveryEnabled != nil
}

// ValidateDeliveryOptions refuses an option LinkedIn cannot honour for the campaign's format,
// before anything is created: the Audience Network on a Text Ad campaign. format is the raw
// CampaignInput.Format; an unknown format is left to NormalizeFormat's own refusal.
func ValidateDeliveryOptions(o DeliveryOptions, format string) error {
	f, err := NormalizeFormat(format)
	if err != nil {
		return nil
	}
	if f == FormatText && o.OffsiteDeliveryEnabled != nil && *o.OffsiteDeliveryEnabled {
		return fmt.Errorf("%w: %s true is not available with format %q: the LinkedIn Audience Network serves Sponsored Content only", ErrDeliveryOptionInvalid, FieldOffsiteDeliveryEnabled, FormatText)
	}
	return nil
}

// apply writes the options into a campaign create body: offsiteDeliveryEnabled always (false
// unless chosen), audienceExpansionEnabled only when chosen.
func (o DeliveryOptions) apply(body map[string]any) {
	body[FieldOffsiteDeliveryEnabled] = o.OffsiteDeliveryEnabled != nil && *o.OffsiteDeliveryEnabled
	if o.AudienceExpansionEnabled != nil {
		body[FieldAudienceExpansionEnabled] = *o.AudienceExpansionEnabled
	}
}
