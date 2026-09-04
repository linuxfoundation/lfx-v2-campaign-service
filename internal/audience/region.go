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
	"unicode"
	"unicode/utf8"
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
//
// The first letter is decoded as a RUNE, not sliced as a byte. Every key in countryToRegion is
// ASCII today, so byte-slicing happened to work — but the map's own comment invites adding
// countries, and the first non-ASCII one (Türkiye, Côte d'Ivoire) would have been split
// mid-rune into mojibake. That failure is silent and expensive: the result is used as an exact
// IS_ANY_OF filter value, so it would build a list matching nobody with no error anywhere.
func titleCase(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		r, size := utf8.DecodeRuneInString(w)
		if r == utf8.RuneError && size <= 1 {
			// Invalid UTF-8: leave it exactly as written rather than mangling it further.
			continue
		}
		words[i] = string(unicode.ToUpper(r)) + w[size:]
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

// iso2ToCountry maps ISO 3166-1 alpha-2 codes to the canonical country names in countryToRegion.
//
// The UI writes `countryCode` and never `country` (CampaignCreateRequest), so without this every
// brief created through the UI failed the audience build with "no country in its details".
//
// Deliberately covers ONLY the countries countryToRegion knows. A code outside that set resolves
// to nothing and the caller keeps failing loudly, which is the property the decoder's comment
// protects: Country reaches HubSpot as an exact CONTAINS/IS_ANY_OF filter value, so an unmapped
// or invented name matches no contact and the build would SUCCEED while storing an empty
// inclusion list. On a list that decides who receives an email, a visible error beats a silent
// wrong answer.
//
// Keep this in step with countryToRegion: a country added there without its code here is
// reachable by name but not by the UI, which is the bug this map exists to close.
var iso2ToCountry = map[string]string{
	// APAC
	"kr": "south korea", "jp": "japan", "cn": "china", "in": "india",
	"sg": "singapore", "au": "australia", "tw": "taiwan", "id": "indonesia",
	"my": "malaysia", "ph": "philippines", "vn": "vietnam", "th": "thailand",
	"nz": "new zealand", "hk": "hong kong",

	// EMEA
	"gb": "united kingdom", "de": "germany", "fr": "france", "nl": "netherlands",
	"es": "spain", "it": "italy", "ie": "ireland", "ch": "switzerland",
	"se": "sweden", "pl": "poland", "za": "south africa", "il": "israel",
	"ae": "uae", "sa": "saudi arabia",

	// NA
	"us": "united states", "ca": "canada", "mx": "mexico",

	// LATAM
	"br": "brazil", "ar": "argentina", "cl": "chile", "co": "colombia", "pe": "peru",
}

// CountryForCode resolves an ISO 3166-1 alpha-2 code to the country name the rest of this package
// expects. The second return is false when the code is unknown, and callers MUST NOT substitute a
// fallback: an unrecognised country must fail visibly rather than build a list matching nobody.
//
// `GB` is the code for the United Kingdom; `UK` is not an ISO code but is accepted because it is
// what people type.
func CountryForCode(code string) (string, bool) {
	key := strings.ToLower(strings.TrimSpace(code))
	if key == "uk" {
		key = "gb"
	}
	name, ok := iso2ToCountry[key]
	return name, ok
}
