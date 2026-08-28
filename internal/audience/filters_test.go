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
	// FLAT: two AND branches per edition (one per country property), spliced directly under the
	// single OR root. HubSpot rejects nested ORs, so a per-edition OR wrapper is not allowed.
	andBranches := root["filterBranches"].([]any)
	require.Len(t, andBranches, 2, "one AND branch per country property, flattened under the OR root")
	for _, b := range andBranches {
		assert.Equal(t, "AND", b.(map[string]any)["filterBranchType"],
			"the OR root's children must all be ANDs — a nested OR is rejected by HubSpot")
	}

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
	// Two AND branches per edition (country + ip_country), all flattened under one OR root.
	assert.Len(t, root["filterBranches"], len(names)*2, "two AND branches per past edition")

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
	andBranch := root["filterBranches"].([]any)[0].(map[string]any)
	f := andBranch["filters"].([]any)[0].(map[string]any)
	op := f["operation"].(map[string]any)
	assert.Equal(t, "IS_ANY_OF", op["operator"])
	assert.Len(t, op["values"], len(countries), "every country in the region must be listed")

	// Sanity: the emitted JSON is a single object, not an array or a bare value.
	assert.True(t, strings.HasPrefix(string(raw), "{"))
}

// TestMasterListFilter_IsAUnionNotAnIntersection pins the shape rules the HubSpot client
// documents (internal/platform/hubspot/lists.go): an OR root with AND children, no nested ORs,
// and IN_LIST — not LIST_MEMBERSHIP, which HubSpot rejects.
//
// Each list needs its OWN AND branch. Sibling membership filters inside ONE AND branch mean
// "in list A AND in list B" — an INTERSECTION, typically empty and exactly backwards.
// HubSpot requires `operator` on the FILTER itself, not only inside `operation`. Without it the
// create fails with `Some required fields were not set: [operator]` -- a 400 that names no field
// path, so it reads as a malformed request rather than a missing key, and every master list
// (and therefore every audience) fails while the inclusion lists it unions succeed.
//
// Confirmed against the live API: the identical payload returns 200 with the field and 400
// without it.
func TestMasterListFilter_SetsOperatorAtTheFilterLevel(t *testing.T) {
	raw, err := MasterListFilter([]string{"30781", "30782"})
	if err != nil {
		t.Fatalf("MasterListFilter: %v", err)
	}

	var root struct {
		FilterBranches []struct {
			Filters []map[string]any `json:"filters"`
		} `json:"filterBranches"`
	}
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(root.FilterBranches) != 2 {
		t.Fatalf("want 2 branches, got %d", len(root.FilterBranches))
	}
	for i, b := range root.FilterBranches {
		if len(b.Filters) != 1 {
			t.Fatalf("branch %d: want 1 filter, got %d", i, len(b.Filters))
		}
		// The top-level key is the one HubSpot rejects the request without.
		if got := b.Filters[0]["operator"]; got != "IN_LIST" {
			t.Errorf("branch %d: filter-level operator = %v, want IN_LIST", i, got)
		}
		// The nested one must survive too -- they are both required, not alternatives.
		op, _ := b.Filters[0]["operation"].(map[string]any)
		if op["operator"] != "IN_LIST" {
			t.Errorf("branch %d: operation.operator = %v, want IN_LIST", i, op["operator"])
		}
	}
}

func TestMasterListFilter_IsAUnionNotAnIntersection(t *testing.T) {
	raw, err := MasterListFilter([]string{"111", "222", "333"})
	require.NoError(t, err)

	root := decode(t, raw)
	assert.Equal(t, "OR", root["filterBranchType"])

	branches, ok := root["filterBranches"].([]any)
	require.True(t, ok)
	require.Len(t, branches, 3, "one AND branch PER list: sibling filters in one branch would AND them")

	seen := map[string]bool{}
	for _, b := range branches {
		br := b.(map[string]any)
		assert.Equal(t, "AND", br["filterBranchType"], "no nested ORs — HubSpot rejects them")
		assert.Empty(t, br["filterBranches"], "a membership branch has no sub-branches")

		filters := br["filters"].([]any)
		require.Len(t, filters, 1, "exactly one membership filter per branch, or they would AND")
		f := filters[0].(map[string]any)
		assert.Equal(t, "IN_LIST", f["filterType"],
			"the client contract requires IN_LIST; LIST_MEMBERSHIP is rejected")
		seen[f["listId"].(string)] = true
	}
	assert.Equal(t, map[string]bool{"111": true, "222": true, "333": true}, seen,
		"every inclusion list must appear in the union")
}

// TestFilters_NeverNestORs pins the invariant across every builder. A nested OR is accepted by
// json.Marshal and rejected by HubSpot, so it fails at the platform rather than in review.
func TestFilters_NeverNestORs(t *testing.T) {
	build := map[string]func() (json.RawMessage, error){
		"group4": func() (json.RawMessage, error) { return EducationEnrolledFilter("South Korea") },
		"group5": func() (json.RawMessage, error) {
			return EventRegisteredFilter("South Korea", []string{"E 2024", "E 2025"})
		},
		"group7": func() (json.RawMessage, error) {
			return RegionEventRegistrantsFilter(CountriesIn(RegionAPAC), []string{"E 2024", "E 2025"})
		},
		"master": func() (json.RawMessage, error) { return MasterListFilter([]string{"1", "2"}) },
	}
	for name, fn := range build {
		t.Run(name, func(t *testing.T) {
			raw, err := fn()
			require.NoError(t, err)
			root := decode(t, raw)
			require.Equal(t, "OR", root["filterBranchType"], "the root must be an OR")
			assertNoNestedOR(t, root["filterBranches"].([]any))
		})
	}
}

// assertNoNestedOR walks a branch list and fails on any OR below the root.
func assertNoNestedOR(t *testing.T, branches []any) {
	t.Helper()
	for _, b := range branches {
		br := b.(map[string]any)
		assert.NotEqual(t, "OR", br["filterBranchType"],
			"a nested OR marshals fine and is then rejected by HubSpot")
		if sub, ok := br["filterBranches"].([]any); ok {
			assertNoNestedOR(t, sub)
		}
	}
}
