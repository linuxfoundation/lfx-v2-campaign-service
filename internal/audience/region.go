// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package audience builds HubSpot marketing audiences from a campaign brief.
//
// The regional-expansion model is ported from the operational runbook that produced these
// audiences by hand (Email-Automation's hubspot-event-list-builder skill). Only the
// DETERMINISTIC parts are mechanised here — see BuildPlan for what is deliberately left out.
package audience

import (
	"sort"
	"strings"
)

// Region is a marketing region used to widen an audience beyond the event's own country.
type Region string

const (
	RegionAPAC  Region = "APAC"
	RegionEMEA  Region = "EMEA"
	RegionNA    Region = "NA"
	RegionLATAM Region = "LATAM"
)

// countryToRegion maps a country to its marketing region.
//
// This is the runbook's table verbatim, NOT a general-purpose geography: it lists only the
// countries LF events actually draw from, and the groupings are marketing regions rather than
// continents (e.g. Israel/UAE/Saudi Arabia sit in EMEA). Adding a country here widens a real
// audience, so entries should come from the runbook rather than from geographic intuition.
var countryToRegion = map[string]Region{
	// APAC
	"south korea": RegionAPAC, "japan": RegionAPAC, "china": RegionAPAC,
	"india": RegionAPAC, "singapore": RegionAPAC, "australia": RegionAPAC,
	"taiwan": RegionAPAC, "indonesia": RegionAPAC, "malaysia": RegionAPAC,
	"philippines": RegionAPAC, "vietnam": RegionAPAC, "thailand": RegionAPAC,
	"new zealand": RegionAPAC, "hong kong": RegionAPAC,

	// EMEA
	"united kingdom": RegionEMEA, "germany": RegionEMEA, "france": RegionEMEA,
	"netherlands": RegionEMEA, "spain": RegionEMEA, "italy": RegionEMEA,
	"ireland": RegionEMEA, "switzerland": RegionEMEA, "sweden": RegionEMEA,
	"poland": RegionEMEA, "south africa": RegionEMEA, "israel": RegionEMEA,
	"uae": RegionEMEA, "saudi arabia": RegionEMEA,

	// NA
	"united states": RegionNA, "canada": RegionNA, "mexico": RegionNA,

	// LATAM
	"brazil": RegionLATAM, "argentina": RegionLATAM,
	"chile": RegionLATAM, "colombia": RegionLATAM, "peru": RegionLATAM,
}

// countryAliases maps common alternate spellings to the canonical key in countryToRegion.
// Brief event details are free text written by humans, so "USA" and "UK" appear at least as
// often as the canonical names; without this they would silently resolve to no region and
// quietly narrow the audience rather than failing visibly.
var countryAliases = map[string]string{
	"usa": "united states", "u.s.": "united states", "u.s.a.": "united states",
	"us": "united states", "america": "united states",
	"uk": "united kingdom", "u.k.": "united kingdom",
	"great britain": "united kingdom", "britain": "united kingdom", "england": "united kingdom",
	"korea": "south korea", "republic of korea": "south korea",
	"united arab emirates": "uae",
	"holland":              "netherlands",
}

// RegionFor resolves a country name to its marketing region. The second return is false when
// the country is unknown — callers MUST NOT default to a region in that case: silently
// assigning one would widen an audience to a continent the event has no reach in.
func RegionFor(country string) (Region, bool) {
	key := strings.ToLower(strings.TrimSpace(country))
	if key == "" {
		return "", false
	}
	if canonical, ok := countryAliases[key]; ok {
		key = canonical
	}
	r, ok := countryToRegion[key]
	return r, ok
}

// displayNames maps the lowercase lookup keys to the form HubSpot actually stores. The map keys
// are lowercase for case-insensitive LOOKUP, but IS_ANY_OF is an EXACT match — feeding raw keys
// into a filter builds a list matching nobody, with no error. Only the entries whose display
// form differs from a simple title-case need listing.
var displayNames = map[string]string{
	"uae":            "United Arab Emirates",
	"united kingdom": "United Kingdom",
	"united states":  "United States",
	"south korea":    "South Korea",
	"south africa":   "South Africa",
	"new zealand":    "New Zealand",
	"hong kong":      "Hong Kong",
	"saudi arabia":   "Saudi Arabia",
}

// DisplayName returns the country in the form HubSpot stores it, ready to use as an exact
// filter value. Unknown input is returned trimmed but otherwise untouched, so a brief carrying
// a country this package does not know still filters on exactly what the operator wrote.
func DisplayName(country string) string {
	key := strings.ToLower(strings.TrimSpace(country))
	if canonical, ok := countryAliases[key]; ok {
		key = canonical
	}
	if d, ok := displayNames[key]; ok {
		return d
	}
	if _, known := countryToRegion[key]; known {
		return titleCase(key)
	}
	return strings.TrimSpace(country)
}

// titleCase upper-cases the first letter of each word — sufficient for the single-word and
// simple two-word names not covered by displayNames.
func titleCase(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

// CountriesIn returns the countries in a region as HubSpot DISPLAY names, sorted, ready to use
// as exact filter values. Returns nil for an unknown region.
func CountriesIn(r Region) []string {
	var out []string
	for c, cr := range countryToRegion {
		if cr == r {
			out = append(out, DisplayName(c))
		}
	}
	sort.Strings(out)
	return out
}
