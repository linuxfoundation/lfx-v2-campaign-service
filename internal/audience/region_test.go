// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package audience

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRegionFor_ResolvesRunbookCountries spot-checks the marketing groupings that are NOT
// geographic intuition — the ones most likely to be "corrected" into a bug later.
func TestRegionFor_ResolvesRunbookCountries(t *testing.T) {
	cases := map[string]Region{
		"South Korea": RegionAPAC,
		"Japan":       RegionAPAC,
		// Israel, UAE and Saudi Arabia are EMEA in the marketing model, not APAC.
		"Israel":       RegionEMEA,
		"UAE":          RegionEMEA,
		"Saudi Arabia": RegionEMEA,
		// South Africa is EMEA, not its own region.
		"South Africa": RegionEMEA,
		// Mexico is NA in this model, not LATAM.
		"Mexico": RegionNA,
		"Brazil": RegionLATAM,
	}
	for country, want := range cases {
		t.Run(country, func(t *testing.T) {
			got, ok := RegionFor(country)
			require.True(t, ok, "%q must resolve", country)
			assert.Equal(t, want, got)
		})
	}
}

// TestRegionFor_HandlesAliasesAndCasing pins alias handling. Brief event details are free text,
// so "USA" and "UK" appear at least as often as the canonical names — without aliases those
// would resolve to nothing and silently narrow the audience instead of failing visibly.
func TestRegionFor_HandlesAliasesAndCasing(t *testing.T) {
	for _, in := range []string{"USA", "usa", "  U.S.  ", "America", "United States"} {
		got, ok := RegionFor(in)
		require.True(t, ok, "%q must resolve", in)
		assert.Equal(t, RegionNA, got)
	}
	for _, in := range []string{"UK", "Britain", "England", "united kingdom"} {
		got, ok := RegionFor(in)
		require.True(t, ok, "%q must resolve", in)
		assert.Equal(t, RegionEMEA, got)
	}
	got, ok := RegionFor("Korea")
	require.True(t, ok)
	assert.Equal(t, RegionAPAC, got)
}

// TestRegionFor_UnknownCountryFailsClosed pins the most important property: an unknown country
// must NOT default to a region. Defaulting would widen a real audience to a continent the event
// has no reach in — a silent targeting error that costs money and looks like success.
func TestRegionFor_UnknownCountryFailsClosed(t *testing.T) {
	for _, in := range []string{"", "   ", "Atlantis", "Antarctica", "EMEA"} {
		got, ok := RegionFor(in)
		assert.False(t, ok, "%q must not resolve to a region", in)
		assert.Empty(t, got)
	}
}

// TestCountriesIn_IsStableAndScoped guards the region→countries direction used to build the
// region-wide filters.
func TestCountriesIn_IsStableAndScoped(t *testing.T) {
	apac := CountriesIn(RegionAPAC)
	require.NotEmpty(t, apac)
	// DISPLAY names, not the lowercase lookup keys: these become exact IS_ANY_OF filter values,
	// and HubSpot stores countries title-cased — lowercase would match nobody, silently.
	assert.Contains(t, apac, "Japan")
	assert.Contains(t, apac, "South Korea")
	assert.NotContains(t, apac, "japan")
	assert.NotContains(t, apac, "Germany")

	// Sorted, so the emitted filter (and any snapshot of it) is deterministic.
	assert.IsIncreasing(t, apac)

	// Every country resolves back to the region it was listed under (RegionFor is
	// case-insensitive, so display names still resolve).
	for _, c := range apac {
		r, ok := RegionFor(c)
		require.True(t, ok)
		assert.Equal(t, RegionAPAC, r)
	}

	assert.Nil(t, CountriesIn(Region("NOT-A-REGION")))
}

// TestDisplayName_ProducesExactFilterValues pins the form fed into HubSpot's IS_ANY_OF, which is
// an EXACT match. The map keys are lowercase for case-insensitive lookup; using them raw as
// filter values builds a list matching nobody, with no error to notice.
func TestDisplayName_ProducesExactFilterValues(t *testing.T) {
	cases := map[string]string{
		"USA":                  "United States",
		"usa":                  "United States",
		"uk":                   "United Kingdom",
		"Korea":                "South Korea",
		"south korea":          "South Korea",
		"united arab emirates": "United Arab Emirates",
		"japan":                "Japan",
		"  brazil  ":           "Brazil",
	}
	for in, want := range cases {
		assert.Equal(t, want, DisplayName(in), "input %q", in)
	}

	// An unknown country is passed through as written rather than mangled — a brief may name a
	// country this package does not track, and filtering on the operator's own text is the
	// least-surprising behaviour.
	assert.Equal(t, "Atlantis", DisplayName("Atlantis"))
	assert.Equal(t, "Atlantis", DisplayName("  Atlantis  "))
}
