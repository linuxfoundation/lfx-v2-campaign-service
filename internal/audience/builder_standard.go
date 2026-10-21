// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package audience

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// StandardSuppressionName is the name of the project's one standing standard suppression
// list: the union of every resolved StandardSuppressionTerms list.
//
// It carries no master name, quarter or selection fingerprint, so every attach and compose
// in the project finds the SAME list by name and reuses it, instead of wrapping the same
// hygiene lists in a new per-audience list each time.
func StandardSuppressionName(projectID string) string {
	return cutName("LFX Standard Suppressions - "+strings.TrimSpace(projectID), MaxListNameLength)
}

// RegistrantsEditionOK reports whether editionName can select an edition's registrants: the
// registration event records the edition WITH its year ("AGNTCon + MCPCon Japan 2026"), so a
// name without one matches nobody -- an empty suppression that reads as applied.
func RegistrantsEditionOK(editionName string) bool {
	return eventYearFromName(editionName) != ""
}

// CurrentRegistrantsName is the name of the list holding the current edition's registrants.
// editionName is the event name WITH its year, exactly as the registration event records it,
// so each edition gets its own list and a repeat send for that edition reuses it.
func CurrentRegistrantsName(editionName string) string {
	const suffix = " - Current Registrants"
	return cutName(strings.TrimSpace(editionName), MaxListNameLength-len(suffix)) + suffix
}

// CurrentRegistrantsFilter selects everyone who completed the registration event for
// editionName. It uses the custom builder's live-verified shape (eventWith): the event_name
// filter sits directly on the UNIFIED_EVENTS node. registeredForEvent's nested AND branch under
// that node is refused by HubSpot's list API.
func CurrentRegistrantsFilter(editionName string) (json.RawMessage, error) {
	editionName = strings.TrimSpace(editionName)
	if !RegistrantsEditionOK(editionName) {
		// Fail closed: a name without the edition's year matches nobody, and a suppression
		// that excludes nobody reads as applied.
		return nil, fmt.Errorf("%w: a current-registrants list needs the edition's event name with its year, e.g. \"AGNTCon + MCPCon Japan 2026\"", ErrInvalidRequest)
	}
	return json.Marshal(filterBranch{
		FilterBranchType: "OR",
		FilterBranches: []filterBranch{{
			FilterBranchType: "AND",
			FilterBranches:   []filterBranch{eventWith(registrationEventTypeID, valueFilter("event_name", "IS_EQUAL_TO", []string{editionName}, false))},
			Filters:          []filter{},
		}},
		Filters: []filter{},
	})
}

// SplitStandardSuppressions separates the resolved standard ids from the rest of a
// suppression selection.
//
// useStandard is true only when EVERY resolved standard id was selected: the standing list is
// their union, so substituting it for a partial selection would exclude lists the operator
// unticked. A partial selection keeps its standard ids in rest, attached one by one.
func SplitStandardSuppressions(suppress, standardIDs []string) (useStandard bool, rest []string) {
	standard := UniqueIDs(standardIDs)
	if len(standard) == 0 {
		return false, UniqueIDs(suppress)
	}
	selected := UniqueIDs(suppress)
	for _, id := range standard {
		if !slices.Contains(selected, id) {
			return false, selected
		}
	}
	rest = make([]string, 0, len(selected))
	for _, id := range selected {
		if !slices.Contains(standard, id) {
			rest = append(rest, id)
		}
	}
	return true, rest
}

// SameIDSet reports whether a and b hold the same ids, ignoring order, blanks and repeats.
func SameIDSet(a, b []string) bool {
	x, y := UniqueIDs(a), UniqueIDs(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

// MasterListWithSuppressionsFilter is MasterListWithSuppressionFilter for several exclusions:
// every AND branch carries one NOT_IN_LIST filter per suppression id, so each inclusion path
// excludes all of them.
func MasterListWithSuppressionsFilter(includeIDs, suppressionIDs []string) (json.RawMessage, error) {
	if err := ValidateInclusionIDs(includeIDs); err != nil {
		return nil, err
	}
	suppress := UniqueIDs(suppressionIDs)
	if len(suppress) == 0 {
		return nil, fmt.Errorf("audience: a suppressed master list requires at least one suppression list id")
	}
	branches := make([]filterBranch, 0, len(includeIDs))
	for _, id := range includeIDs {
		filters := make([]filter, 0, 1+len(suppress))
		filters = append(filters, inListFilter(id, "IN_LIST"))
		for _, s := range suppress {
			filters = append(filters, inListFilter(s, "NOT_IN_LIST"))
		}
		branches = append(branches, filterBranch{
			FilterBranchType: "AND",
			FilterBranches:   []filterBranch{},
			Filters:          filters,
		})
	}
	return json.Marshal(filterBranch{
		FilterBranchType: "OR",
		FilterBranches:   branches,
		Filters:          []filter{},
	})
}
