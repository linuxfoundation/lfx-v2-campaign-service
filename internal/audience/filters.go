// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package audience

import (
	"encoding/json"
	"fmt"
	"strings"
)

// educationEventTypeID is the PORTAL-WIDE HubSpot event type for education enrolment. It is a
// fixed portal constant, NOT a per-event id — the runbook is explicit that it must not be
// looked up per event, because the lookup returns per-event ids that silently produce an empty
// list. Hardcoding it is the correct behaviour here, not a shortcut.
const educationEventTypeID = "6-58204655"

// registrationEventTypeID is the PORTAL-WIDE HubSpot event type for event registration. Like
// educationEventTypeID it is fixed and must NOT be looked up per event.
const registrationEventTypeID = "6-48984571"

// Contact properties carrying a contact's country. HubSpot stores a self-reported country and
// an IP-derived one, and they disagree often enough that filtering on either alone measurably
// shrinks the audience — so every country filter ORs the two.
const (
	propCountry   = "country"
	propIPCountry = "ip_country"
)

// filterBranch is the HubSpot list filter tree. The field names and nesting are HubSpot's
// contract (see the /crm/v3/lists API), not ours.
type filterBranch struct {
	FilterBranchType string         `json:"filterBranchType"`
	FilterBranches   []filterBranch `json:"filterBranches"`
	Filters          []filter       `json:"filters"`
	// Operator and EventTypeID apply only to UNIFIED_EVENTS branches; omitted elsewhere.
	Operator    string `json:"operator,omitempty"`
	EventTypeID string `json:"eventTypeId,omitempty"`
}

type filter struct {
	FilterType string `json:"filterType"`
	Property   string `json:"property,omitempty"`
	// ListID is set only for LIST_MEMBERSHIP filters (the master list's union members).
	ListID string `json:"listId,omitempty"`
	// Operator is REQUIRED by HubSpot at the FILTER level, in addition to the one inside
	// `operation`. Omitting it fails the whole create with
	// `Some required fields were not set: [operator]` -- a 400 that names no field path, so it
	// reads as a malformed request rather than a missing key. Confirmed against the live API:
	// the identical payload succeeds with it and 400s without.
	//
	// `omitempty` so property filters, which do not set it, keep their existing wire shape.
	Operator  string    `json:"operator,omitempty"`
	Operation operation `json:"operation"`
}

type operation struct {
	Operator                     string   `json:"operator"`
	IncludeObjectsWithNoValueSet bool     `json:"includeObjectsWithNoValueSet"`
	Values                       []string `json:"values,omitempty"`
	OperationType                string   `json:"operationType,omitempty"`
}

// containsValue builds a CONTAINS property operation. MULTISTRING is HubSpot's operation type
// for string values; the runbook uses it for every property filter in the regional-expansion
// groups.
func containsValue(property, value string) filter {
	return filter{
		FilterType: "PROPERTY",
		Property:   property,
		Operation: operation{
			Operator:                     "CONTAINS",
			IncludeObjectsWithNoValueSet: false,
			Values:                       []string{value},
			OperationType:                "MULTISTRING",
		},
	}
}

// isAnyOf builds an IS_ANY_OF property operation over several values — used where one filter
// must match any of a region's countries rather than a single one.
func isAnyOf(property string, values []string) filter {
	return filter{
		FilterType: "PROPERTY",
		Property:   property,
		Operation: operation{
			Operator:                     "IS_ANY_OF",
			IncludeObjectsWithNoValueSet: false,
			Values:                       values,
			OperationType:                "MULTISTRING",
		},
	}
}

// countryAndBranches returns the pair of AND branches a country condition needs: one filtering
// on `country`, one on `ip_country`, so a contact qualifies on either. mkFilter receives the
// property name.
//
// It returns the BRANCHES rather than an OR wrapping them because HubSpot rejects NESTED ORs:
// callers splice these directly into a single OR root. Returning a ready-made OR meant a caller
// building several editions produced OR-of-OR, which HubSpot refuses.
//
// inner, when non-empty, is placed inside EACH AND branch's filterBranches — this is how the
// runbook attaches a UNIFIED_EVENTS qualifier (e.g. "has completed an education enrolment")
// to the country condition. It must be duplicated into both branches, not hoisted: hoisting
// it would change the tree from "(enrolled AND country) OR (enrolled AND ip_country)" to
// "enrolled AND (country OR ip_country)", which HubSpot does not accept at this nesting.
func countryAndBranches(mkFilter func(property string) filter, inner []filterBranch) []filterBranch {
	branchFor := func(property string) filterBranch {
		return filterBranch{
			FilterBranchType: "AND",
			FilterBranches:   append([]filterBranch(nil), inner...),
			Filters:          []filter{mkFilter(property)},
		}
	}
	return []filterBranch{branchFor(propCountry), branchFor(propIPCountry)}
}

// educationEnrolled is the UNIFIED_EVENTS qualifier for "has completed an education enrolment".
func educationEnrolled() filterBranch {
	return filterBranch{
		FilterBranchType: "UNIFIED_EVENTS",
		Operator:         "HAS_COMPLETED",
		EventTypeID:      educationEventTypeID,
		FilterBranches:   []filterBranch{},
		Filters:          []filter{},
	}
}

// EducationEnrolledFilter builds group 4: contacts in `country` who have completed an
// education enrolment. This is the "Education Enrolled [Country]" list.
func EducationEnrolledFilter(country string) (json.RawMessage, error) {
	if strings.TrimSpace(country) == "" {
		return nil, fmt.Errorf("audience: education-enrolled filter requires a country")
	}
	return json.Marshal(filterBranch{
		FilterBranchType: "OR",
		FilterBranches: countryAndBranches(
			func(property string) filter { return containsValue(property, country) },
			[]filterBranch{educationEnrolled()},
		),
		Filters: []filter{},
	})
}

// registeredForEvent is the UNIFIED_EVENTS qualifier for "has registered for this exact past
// edition". The event name is matched INSIDE the UNIFIED_EVENTS node's filterBranches — a
// sibling property filter would test a contact property called event_name, which does not
// exist, and silently match nobody.
//
// name is used VERBATIM and must come from snowflake.ResolvePastEventNames: a guessed or
// remembered name matches nothing, which is indistinguishable from "no such audience exists".
func registeredForEvent(name string) filterBranch {
	return filterBranch{
		FilterBranchType: "UNIFIED_EVENTS",
		Operator:         "HAS_COMPLETED",
		EventTypeID:      registrationEventTypeID,
		FilterBranches: []filterBranch{{
			FilterBranchType: "AND",
			FilterBranches:   []filterBranch{},
			Filters: []filter{{
				FilterType: "PROPERTY",
				Property:   "event_name",
				Operation: operation{
					Operator:                     "IS_EQUAL_TO",
					IncludeObjectsWithNoValueSet: false,
					Values:                       []string{name},
					OperationType:                "MULTISTRING",
				},
			}},
		}},
		Filters: []filter{},
	}
}

// EventRegisteredFilter builds group 5: contacts in `country` who registered for any of the
// given PAST editions. One OR branch per past edition (per the runbook: one per past YEAR of
// this event's own location, never one per other country).
func EventRegisteredFilter(country string, eventNames []string) (json.RawMessage, error) {
	if strings.TrimSpace(country) == "" {
		return nil, fmt.Errorf("audience: event-registered filter requires a country")
	}
	if len(eventNames) == 0 {
		// Fail closed rather than emitting an empty value set: HubSpot would accept it and
		// build a list matching everyone who registered for anything — far worse than no list.
		return nil, fmt.Errorf("audience: event-registered filter requires at least one resolved event name")
	}
	var branches []filterBranch
	for _, name := range eventNames {
		if strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("audience: event-registered filter got a blank event name")
		}
		// FLATTEN into the single OR root — appending a per-edition OR would nest.
		branches = append(branches, countryAndBranches(
			func(property string) filter { return containsValue(property, country) },
			[]filterBranch{registeredForEvent(name)},
		)...)
	}
	return json.Marshal(filterBranch{
		FilterBranchType: "OR",
		FilterBranches:   branches,
		Filters:          []filter{},
	})
}

// RegionEventRegistrantsFilter builds group 7: registrants of the given past editions across a
// whole REGION rather than a single country, to widen reach once country lists exist.
func RegionEventRegistrantsFilter(countries, eventNames []string) (json.RawMessage, error) {
	if len(countries) == 0 {
		return nil, fmt.Errorf("audience: region filter requires at least one country")
	}
	if len(eventNames) == 0 {
		return nil, fmt.Errorf("audience: region filter requires at least one resolved event name")
	}
	var branches []filterBranch
	for _, name := range eventNames {
		if strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("audience: region filter got a blank event name")
		}
		branches = append(branches, countryAndBranches(
			func(property string) filter { return isAnyOf(property, countries) },
			[]filterBranch{registeredForEvent(name)},
		)...)
	}
	return json.Marshal(filterBranch{
		FilterBranchType: "OR",
		FilterBranches:   branches,
		Filters:          []filter{},
	})
}

// MasterListFilter builds the UNION of the inclusion lists: a contact qualifies for the master
// if they are in ANY of them.
//
// This is what the email dispatcher actually sends to — it reads only
// `platform_master_list_id`. Without a union, whichever single list was recorded becomes the
// entire send audience and every other group is created in the portal and never emailed, which
// looks like a successful build that silently reaches a fraction of the intended people.
//
// Shape rules this must obey (documented on the client, `internal/platform/hubspot/lists.go`):
// the root is an OR, its children are ANDs, there are NO nested ORs, and membership filters use
// `IN_LIST` — NOT `LIST_MEMBERSHIP`, which HubSpot rejects.
//
// Each list therefore gets its OWN AND branch. Putting all the membership filters as siblings
// inside ONE AND branch would mean "in list A AND in list B" — an INTERSECTION, typically empty
// and exactly backwards from the intent.
func MasterListFilter(listIDs []string) (json.RawMessage, error) {
	if len(listIDs) == 0 {
		return nil, fmt.Errorf("audience: a master list requires at least one inclusion list")
	}
	branches := make([]filterBranch, 0, len(listIDs))
	for _, id := range listIDs {
		if strings.TrimSpace(id) == "" {
			// A blank id would silently drop one group from the union.
			return nil, fmt.Errorf("audience: a master list cannot be built from a blank list id")
		}
		branches = append(branches, filterBranch{
			FilterBranchType: "AND",
			FilterBranches:   []filterBranch{},
			Filters: []filter{{
				FilterType: "IN_LIST",
				ListID:     id,
				Operator:   "IN_LIST",
				Operation:  operation{Operator: "IN_LIST", OperationType: "MULTISTRING"},
			}},
		})
	}
	return json.Marshal(filterBranch{
		FilterBranchType: "OR",
		FilterBranches:   branches,
		Filters:          []filter{},
	})
}
