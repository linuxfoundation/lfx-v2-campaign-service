// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"fmt"
	"net/url"
)

// ---------------------------------------------------------------------------
// Click tracking (#476): the campaign's utmcampaign custom parameter and the
// per-channel utm_medium on every final URL this client tags.
// ---------------------------------------------------------------------------

// The Google Ads account carries an ACCOUNT-level tracking template that every campaign
// inherits. Among HubSpot's hsa_* parameters it sets `utm_campaign={_utmcampaign}`, a
// reference to a custom parameter named `utmcampaign` (Google prefixes the reference with
// an underscore; the parameter itself is defined without one). The campaigns the team
// builds by hand override that template with a literal utm_campaign carrying the
// campaign's name; an LFX campaign defined no `utmcampaign`, so the template's
// utm_campaign came out empty on every click.
//
// Setting the parameter on the campaign fills it. Its value must be the campaign name
// QUERY-ESCAPED, because Google substitutes it into the URL as is. These rules were
// verified by validateOnly probes on 2026-11-03 (v23):
//   - a url.QueryEscape'd campaign name was ACCEPTED on Search, Demand Gen, Display and
//     Performance Max campaign creates; Video was not probed — its creation is refused
//     before any request (CreateVideoCampaign);
//   - the RAW name (spaces, pipes) was REFUSED with stringFormatError ILLEGAL_CHARS on
//     Search, Demand Gen and Display;
//   - the key `_utmcampaign` was REFUSED (ILLEGAL_CHARS) — the key is `utmcampaign`;
//   - a value of 251 characters was REFUSED (TOO_LONG).
const (
	utmCampaignParamKey = "utmcampaign"
	// maxCustomParamValueChars is the longest value this client sends: the probe above saw
	// 251 characters refused. The escaped value is pure ASCII, so its byte length is its
	// character count.
	maxCustomParamValueChars = 250
)

// urlCustomParameter is one Campaign.url_custom_parameters entry, sent on the campaign
// create of every channel. The key is sent without Google's leading underscore.
type urlCustomParameter struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// utmCampaignCustomParameters returns the urlCustomParameters a campaign named
// campaignName is created with, and the step line to record when there are none.
//
// The value is url.QueryEscape(campaignName), which spells a space as `+`, `|` as `%7C`
// and `+` as `%2B` — the encoding the hand-built campaigns already use in their literal
// utm_campaign, so the two kinds of campaign report the same way.
//
// A value over maxCustomParamValueChars is OMITTED rather than refused or cut. Refusing
// would fail a create over reporting metadata, and cutting would hand the pipeline a
// name that matches no campaign. Decided here, from the composed name alone, so the length
// decision is made in the preflight before the budget mutate, and it can never be what
// fails a create after its budget exists.
func utmCampaignCustomParameters(campaignName string) (params []urlCustomParameter, omittedStep string) {
	value := url.QueryEscape(campaignName)
	if n := len(value); n > maxCustomParamValueChars {
		return nil, fmt.Sprintf("tracking template's utm_campaign left empty: the query-escaped campaign name is %d characters, over Google's %d-character limit for a URL custom parameter, so no %s custom parameter was set", n, maxCustomParamValueChars, utmCampaignParamKey)
	}
	return []urlCustomParameter{{Key: utmCampaignParamKey, Value: value}}, ""
}

// The utm_medium each channel's final URLs carry, matching what the team's own
// campaigns use: `paid-search` on Search, `display` on Demand Gen, Display and Video.
// Performance Max keeps `cpc` because one campaign spans search and display inventory,
// so neither of the other two names would be true of its clicks.
const (
	utmMediumSearch  = "paid-search"
	utmMediumDisplay = "display"
	utmMediumCPC     = "cpc"
)

// utmMediumFor maps a campaign kind to the utm_medium its final URLs carry. An unknown
// kind gets `cpc`, the value every channel carried before #476, rather than a guess.
func utmMediumFor(kind string) string {
	switch kind {
	case campaignKindSearch:
		return utmMediumSearch
	case campaignKindDemandGen, campaignKindDisplay, campaignKindVideo:
		return utmMediumDisplay
	case campaignKindPerformanceMax:
		return utmMediumCPC
	default:
		return utmMediumCPC
	}
}
