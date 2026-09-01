// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package utm

import "strings"

// Resolution is the outcome of deciding an email's UTM parameters, including WHERE the
// campaign came from. The provenance is recorded because two emails tagged from different
// sources are not comparable in a report, and the difference is otherwise invisible.
type Resolution struct {
	Params Params
	// Source names the provenance: "brief_config" when an operator set utmCampaign on the
	// brief's platform config, "derived" when it was slugified from the email name, or
	// "fallback" when neither yielded anything usable.
	//
	// SourceHubSpotCampaign is reserved for a value read from an upstream HubSpot campaign's
	// hs_utm — not emitted today (see SourceHubSpotCampaign for the precise remaining gap). Recording a brief-config
	// override as hubspot_campaign would make the provenance log false, and provenance is the
	// only way to tell two differently-sourced campaigns apart in a report.
	Source string
}

// Provenance values for Resolution.Source.
const (
	// SourceHubSpotCampaign is reserved for an upstream HubSpot campaign's configured hs_utm.
	//
	// Nothing emits it yet, and the reason is narrower than it used to be. The client CAN now
	// reach campaigns — SearchCampaigns finds them BY NAME and CreateCampaign makes one — but
	// resolution here starts from a source EMAIL, and nothing reads the association from an
	// email to the campaign it belongs to. Searching by the email's name would be a guess, not
	// a lookup: campaign and email names need not match, and a wrong hit would attribute this
	// email's traffic to somebody else's campaign, which is worse than emitting no source at
	// all. Closing this needs an association read, not another search endpoint.
	SourceHubSpotCampaign = "hubspot_campaign"
	// SourceBriefConfig is an operator-set utmCampaign on the brief's platform config.
	SourceBriefConfig = "brief_config"
	SourceDerived     = "derived"
	SourceFallback    = "fallback"
)

// Resolve decides the UTM parameters for one email.
//
// campaignUTM is an operator-set override (the brief's utmCampaign platform config). It WINS,
// because someone set it deliberately and expects the email to roll up to that campaign.
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
		return Resolution{Params: p, Source: SourceBriefConfig}
	}
	if slug := Slug(emailName); slug != "" {
		p.Campaign = slug
		return Resolution{Params: p, Source: SourceDerived}
	}
	p.Campaign = FallbackCampaign
	return Resolution{Params: p, Source: SourceFallback}
}
