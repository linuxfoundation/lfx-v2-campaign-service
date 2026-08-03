// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package utm

import "strings"

// Resolution is the outcome of deciding an email's UTM parameters, including WHERE the
// campaign came from. The provenance is recorded because two emails tagged from different
// sources are not comparable in a report, and the difference is otherwise invisible.
type Resolution struct {
	Params Params
	// Source names the provenance: "hubspot_campaign" when the value came from a HubSpot
	// campaign's configured hs_utm, "derived" when it was slugified from the email name, or
	// "fallback" when neither yielded anything usable.
	Source string
}

// Provenance values for Resolution.Source.
const (
	SourceHubSpotCampaign = "hubspot_campaign"
	SourceDerived         = "derived"
	SourceFallback        = "fallback"
)

// Resolve decides the UTM parameters for one email.
//
// campaignUTM is the campaign value configured on the HubSpot campaign the template email
// belongs to, when the caller could resolve one — it WINS, because an operator who configured
// it there did so deliberately and expects the email to roll up to that campaign.
//
// emailName is the deterministic generated name, slugified as the fallback. If that yields
// nothing sluggable, FallbackCampaign is used: a real value is required, since an empty
// utm_campaign is exactly the unattributable state this package exists to prevent.
//
// It never fails. Tagging must not be able to block a send — the email going out untagged is a
// reporting gap, whereas an error here would be a failed campaign.
func Resolve(campaignUTM, emailName string) Resolution {
	p := Params{Source: DefaultSource, Medium: DefaultMedium}

	if c := strings.TrimSpace(campaignUTM); c != "" {
		p.Campaign = c
		return Resolution{Params: p, Source: SourceHubSpotCampaign}
	}
	if slug := Slug(emailName); slug != "" {
		p.Campaign = slug
		return Resolution{Params: p, Source: SourceDerived}
	}
	p.Campaign = FallbackCampaign
	return Resolution{Params: p, Source: SourceFallback}
}
