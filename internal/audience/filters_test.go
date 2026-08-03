// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package audience

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// decode is a small helper so assertions can walk the emitted tree by key.
func decode(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	return m
}

// TestEducationEnrolledFilter_MatchesRunbookShape pins group 4 against the operational runbook
// template. The shape is HubSpot's contract, and a wrong tree does NOT error — HubSpot accepts
// it and builds a list matching nobody, which looks exactly like "this audience is empty".
// That silent failure is why this asserts structure rather than just "no error".
func TestEducationEnrolledFilter_MatchesRunbookShape(t *testing.T) {
	raw, err := EducationEnrolledFilter("Korea")
	require.NoError(t, err)
	root := decode(t, raw)

	assert.Equal(t, "OR", root["filterBranchType"], "root must be an OR of the country/ip_country branches")
	branches, ok := root["filterBranches"].([]any)
	require.True(t, ok)
	require.Len(t, branches, 2, "exactly two branches: one per country property")

	wantProps := []string{"country", "ip_country"}
	for i, b := range branches {
		br := b.(map[string]any)
		assert.Equal(t, "AND", br["filterBranchType"])

		// The UNIFIED_EVENTS qualifier must be duplicated into BOTH branches. Hoisting it
		// would change the tree's meaning and HubSpot does not accept it at this nesting.
		inner, ok := br["filterBranches"].([]any)
		require.True(t, ok)
		require.Len(t, inner, 1, "each branch carries its own education-enrolment qualifier")
		ue := inner[0].(map[string]any)
		assert.Equal(t, "UNIFIED_EVENTS", ue["filterBranchType"])
		assert.Equal(t, "HAS_COMPLETED", ue["operator"])
		assert.Equal(t, educationEventTypeID, ue["eventTypeId"],
			"the education event type is a fixed PORTAL-WIDE id; a per-event id silently yields an empty list")

		filters, ok := br["filters"].([]any)
		require.True(t, ok)
		require.Len(t, filters, 1)
		f := filters[0].(map[string]any)
		assert.Equal(t, "PROPERTY", f["filterType"])
		assert.Equal(t, wantProps[i], f["property"])

		op := f["operation"].(map[string]any)
		assert.Equal(t, "CONTAINS", op["operator"])
		assert.Equal(t, "MULTISTRING", op["operationType"])
		assert.Equal(t, []any{"Korea"}, op["values"])
		assert.Equal(t, false, op["includeObjectsWithNoValueSet"])
	}
}

// TestEventRegisteredFilter_NestsNameInsideUnifiedEvents pins the correction that matters most
// in group 5: the event name is matched INSIDE the UNIFIED_EVENTS node, not as a sibling
// property filter. A sibling filter would test a CONTACT property named event_name — which
// does not exist — so HubSpot would build a list matching nobody without erroring.
func TestEventRegisteredFilter_NestsNameInsideUnifiedEvents(t *testing.T) {
	raw, err := EventRegisteredFilter("Korea", []string{"KubeCon + CloudNativeCon Korea 2025"})
	require.NoError(t, err)
	root := decode(t, raw)

	require.Equal(t, "OR", root["filterBranchType"])
	perEdition := root["filterBranches"].([]any)
	require.Len(t, perEdition, 1, "one OR branch per past edition")

	countryOr := perEdition[0].(map[string]any)
	andBranches := countryOr["filterBranches"].([]any)
	require.Len(t, andBranches, 2)

	ue := andBranches[0].(map[string]any)["filterBranches"].([]any)[0].(map[string]any)
	assert.Equal(t, "UNIFIED_EVENTS", ue["filterBranchType"])
	assert.Equal(t, registrationEventTypeID, ue["eventTypeId"])

	// The name lives one level deeper, inside the UNIFIED_EVENTS node.
	nameBranch := ue["filterBranches"].([]any)
	require.Len(t, nameBranch, 1, "the event-name filter must be INSIDE the UNIFIED_EVENTS node")
	nf := nameBranch[0].(map[string]any)["filters"].([]any)[0].(map[string]any)
	assert.Equal(t, "event_name", nf["property"])
	op := nf["operation"].(map[string]any)
	assert.Equal(t, "IS_EQUAL_TO", op["operator"], "an exact match; CONTAINS would pull in unrelated editions")
	assert.Equal(t, []any{"KubeCon + CloudNativeCon Korea 2025"}, op["values"])

	// The UNIFIED_EVENTS node itself carries no direct filters — they are all nested.
	assert.Empty(t, ue["filters"])
}

// TestEventRegisteredFilter_OneBranchPerEdition pins that multiple past editions each get their
// own OR branch rather than being collapsed into one multi-value filter.
func TestEventRegisteredFilter_OneBranchPerEdition(t *testing.T) {
	names := []string{"Event 2024", "Event 2025"}
	raw, err := EventRegisteredFilter("Japan", names)
	require.NoError(t, err)

	root := decode(t, raw)
	assert.Len(t, root["filterBranches"], len(names), "one OR branch per past edition")

	// Every supplied name must appear verbatim.
	for _, n := range names {
		assert.Contains(t, string(raw), n)
	}
}

// TestFilters_FailClosedOnMissingInputs pins the fail-closed contract. An empty event-name set
// is the dangerous case: HubSpot ACCEPTS a filter with no values and builds a list matching
// everyone who ever registered for anything — a far worse outcome than no list at all, and one
// that would be discovered only after a send.
func TestFilters_FailClosedOnMissingInputs(t *testing.T) {
	t.Run("education filter needs a country", func(t *testing.T) {
		_, err := EducationEnrolledFilter("   ")
		require.Error(t, err)
	})

	t.Run("event filter needs a country", func(t *testing.T) {
		_, err := EventRegisteredFilter("", []string{"Event 2025"})
		require.Error(t, err)
	})

	t.Run("event filter refuses an empty name set", func(t *testing.T) {
		_, err := EventRegisteredFilter("Korea", nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "at least one resolved event name")
	})

	t.Run("event filter refuses a blank name", func(t *testing.T) {
		_, err := EventRegisteredFilter("Korea", []string{"Event 2025", "  "})
		require.Error(t, err, "a blank name would match nobody and silently shrink the audience")
	})

	t.Run("region filter refuses empty inputs", func(t *testing.T) {
		_, err := RegionEventRegistrantsFilter(nil, []string{"Event 2025"})
		require.Error(t, err)
		_, err = RegionEventRegistrantsFilter([]string{"japan"}, nil)
		require.Error(t, err)
	})
}

// TestRegionEventRegistrantsFilter_UsesIsAnyOfForCountries pins group 7: the country condition
// widens to a whole region via IS_ANY_OF rather than one CONTAINS per country.
func TestRegionEventRegistrantsFilter_UsesIsAnyOfForCountries(t *testing.T) {
	countries := CountriesIn(RegionAPAC)
	require.NotEmpty(t, countries)

	raw, err := RegionEventRegistrantsFilter(countries, []string{"Event 2025"})
	require.NoError(t, err)

	root := decode(t, raw)
	countryOr := root["filterBranches"].([]any)[0].(map[string]any)
	f := countryOr["filterBranches"].([]any)[0].(map[string]any)["filters"].([]any)[0].(map[string]any)
	op := f["operation"].(map[string]any)
	assert.Equal(t, "IS_ANY_OF", op["operator"])
	assert.Len(t, op["values"], len(countries), "every country in the region must be listed")

	// Sanity: the emitted JSON is a single object, not an array or a bare value.
	assert.True(t, strings.HasPrefix(string(raw), "{"))
}
