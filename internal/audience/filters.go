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
	ListID    string    `json:"listId,omitempty"`
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

// countryOrBranch wraps a per-country-property filter pair in the OR-of-two-ANDs shape the
// runbook uses everywhere: one AND branch filtering on `country`, a sibling on `ip_country`,
// so a contact qualifies on either. mkFilter receives the property name.
//
// inner, when non-empty, is placed inside EACH AND branch's filterBranches — this is how the
// runbook attaches a UNIFIED_EVENTS qualifier (e.g. "has completed an education enrolment")
// to the country condition. It must be duplicated into both branches, not hoisted: hoisting
// it would change the tree from "(enrolled AND country) OR (enrolled AND ip_country)" to
// "enrolled AND (country OR ip_country)", which HubSpot does not accept at this nesting.
func countryOrBranch(mkFilter func(property string) filter, inner []filterBranch) filterBranch {
	branchFor := func(property string) filterBranch {
		return filterBranch{
			FilterBranchType: "AND",
			FilterBranches:   append([]filterBranch(nil), inner...),
			Filters:          []filter{mkFilter(property)},
		}
	}
	return filterBranch{
		FilterBranchType: "OR",
		FilterBranches:   []filterBranch{branchFor(propCountry), branchFor(propIPCountry)},
		Filters:          []filter{},
	}
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
	branch := countryOrBranch(
		func(property string) filter { return containsValue(property, country) },
		[]filterBranch{educationEnrolled()},
	)
	return json.Marshal(branch)
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
		branches = append(branches, countryOrBranch(
			func(property string) filter { return containsValue(property, country) },
			[]filterBranch{registeredForEvent(name)},
		))
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
		branches = append(branches, countryOrBranch(
			func(property string) filter { return isAnyOf(property, countries) },
			[]filterBranch{registeredForEvent(name)},
		))
	}
	return json.Marshal(filterBranch{
		FilterBranchType: "OR",
		FilterBranches:   branches,
		Filters:          []filter{},
	})
}

// MasterListFilter builds the union of the inclusion lists: a contact qualifies for the master
// if they are in ANY of them.
//
// This is what the email dispatcher actually sends to — it reads only
// `platform_master_list_id`. Without a union, whichever single list was recorded becomes the
// entire send audience and every other group is created in the portal and never emailed, which
// looks like a successful build that silently reaches a fraction of the intended people.
func MasterListFilter(listIDs []string) (json.RawMessage, error) {
	if len(listIDs) == 0 {
		return nil, fmt.Errorf("audience: a master list requires at least one inclusion list")
	}
	filters := make([]filter, 0, len(listIDs))
	for _, id := range listIDs {
		if strings.TrimSpace(id) == "" {
			// A blank id would silently drop one group from the union.
			return nil, fmt.Errorf("audience: a master list cannot be built from a blank list id")
		}
		filters = append(filters, filter{
			FilterType: "LIST_MEMBERSHIP",
			ListID:     id,
			Operation:  operation{Operator: "IN_LIST"},
		})
	}
	// One AND branch holding OR'd membership filters: HubSpot treats sibling filters inside a
	// branch as OR when the operators are membership tests, matching the runbook's master shape.
	return json.Marshal(filterBranch{
		FilterBranchType: "OR",
		FilterBranches: []filterBranch{{
			FilterBranchType: "AND",
			FilterBranches:   []filterBranch{},
			Filters:          filters,
		}},
		Filters: []filter{},
	})
}
