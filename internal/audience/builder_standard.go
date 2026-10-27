// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package audience

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// StandardSuppressionName is the name of the project's one standing standard suppression
// list: the union of every resolved StandardSuppressionTerms list.
//
// It carries no master name, quarter or selection fingerprint, so every attach and compose
// in the project finds the SAME list by name and reuses it, instead of wrapping the same
// hygiene lists in a new per-audience list each time.
//
// A name too long to fit is cut AND tagged with a short hash of the whole name (fitTagged), as
// CurrentRegistrantsName is: two long project ids sharing a prefix would otherwise share one
// standing list and grow each other's suppressions. A name that fits is unchanged.
func StandardSuppressionName(projectID string) string {
	return fitTagged(standardSuppressionPrefix+strings.TrimSpace(projectID), MaxListNameLength)
}

// fitTagged is s when it fits in room runes; otherwise s cut short enough to append
// " #<8 hex of sha256(s)>", so two long strings sharing a prefix never fit to the same name.
func fitTagged(s string, room int) string {
	if utf8.RuneCountInString(s) <= room {
		return s
	}
	sum := sha256.Sum256([]byte(s))
	tag := " #" + hex.EncodeToString(sum[:])[:8]
	return cutName(s, room-utf8.RuneCountInString(tag)) + tag
}

// standardSuppressionPrefix and currentRegistrantsSuffix are the fixed parts of the two standing
// list names, shared with hasSuppressionHint so discovery never offers either as an inclusion.
const (
	standardSuppressionPrefix = "LFX Standard Suppressions - "
	currentRegistrantsSuffix  = " - Current Registrants"
)

// RegistrantsEditionOK reports whether editionName can select an edition's registrants: the
// registration event records the edition WITH its year ("AGNTCon + MCPCon Japan 2026"), so a
// name without one matches nobody -- an empty suppression that reads as applied.
//
// A year counts when it is a 19xx/20xx run of exactly four digits bounded by non-digits, the
// warehouse resolver's rule, so "Synthetic Summit2026" carries one (a word-boundary regexp
// would not see it) while a six-digit id does not.
func RegistrantsEditionOK(editionName string) bool {
	for i := 0; i+4 <= len(editionName); i++ {
		c := editionName[i : i+4]
		if (strings.HasPrefix(c, "19") || strings.HasPrefix(c, "20")) && isDigits(c) &&
			(i == 0 || !isDigit(editionName[i-1])) && (i+4 == len(editionName) || !isDigit(editionName[i+4])) {
			return true
		}
	}
	return false
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return true
}

// CurrentRegistrantsName is the name of the list holding the current edition's registrants.
// editionName is the event name WITH its year, exactly as the registration event records it,
// so each edition gets its own list and a repeat send for that edition reuses it.
//
// An edition too long to fit is cut AND tagged with a short hash of the whole edition. The
// filter always selects the full edition, so two long editions sharing a prefix would
// otherwise share one name -- and the second would reuse the first's list, suppressing the
// wrong edition's registrants. A name that fits is unchanged.
func CurrentRegistrantsName(editionName string) string {
	const suffix = currentRegistrantsSuffix
	edition := strings.TrimSpace(editionName)
	return fitTagged(edition, MaxListNameLength-utf8.RuneCountInString(suffix)) + suffix
}

// RegistrantsListMatches reports whether a list found under an edition's
// CurrentRegistrantsName really is that edition's registrants list: its filter selects ALL the
// completed registrations of exactly that edition, and nobody else. The name alone proves
// nothing -- list search is case-insensitive and an operator can create or edit a list under
// any name -- and reusing a list that selects a different edition, anything wider, or only a
// slice of the edition would suppress the wrong people or miss some.
//
// Both halves are needed. RegistrationEvidence's exclusive case proves nobody else is in it
// (every path runs through an exact registration branch), but not that the whole edition is:
// it reads only the event_name filters of a registration branch, and an AND path counts as
// registered when any child is, whatever else the AND requires. CoveredEditions proves the
// whole edition is in it: an edition counts only from a registration branch whose every filter
// is the event_name equality, standing alone as an OR alternative -- an AND that adds a
// condition, or an extra filter inside the event branch, covers nothing.
//
// The edition is compared EXACTLY, only its ends trimmed (as CurrentRegistrantsFilter trims
// it): the filter is an exact event_name equality, so "KubeCon  NA 2026" (two spaces) or a
// different case is a different value, and this service cannot establish that HubSpot's
// equality ignores either. A list differing only that way is a conflict an operator resolves,
// never a silent reuse.
func RegistrantsListMatches(filter json.RawMessage, editionName string) bool {
	want := strings.TrimSpace(editionName)
	only := func(editions []string) bool {
		return len(editions) == 1 && strings.TrimSpace(editions[0]) == want
	}
	if includesNoValueSet(filter) || !eventNameValuesAre(filter, want) {
		return false
	}
	editions, exclusive := RegistrationEvidence(filter)
	return exclusive && only(editions) && only(CoveredEditions(filter))
}

// eventNameValuesAre reports whether every event_name value in the tree is exactly want, byte
// for byte, and there is at least one. RegistrationEvidence and CoveredEditions serve the QA
// checks too, so they trim the stored values and intersect several equalities case-insensitively;
// for reuse that would accept a filter on " KubeCon NA 2026 " (padded), or two equalities that
// differ only in case and so may select nobody. Only the requested edition is trimmed, as
// CurrentRegistrantsFilter trims the value it writes.
func eventNameValuesAre(filter json.RawMessage, want string) bool {
	var root qaBranch
	if len(filter) == 0 || json.Unmarshal(filter, &root) != nil {
		return false
	}
	seen, exact := 0, true
	root.walk(func(b qaBranch) {
		for _, f := range b.Filters {
			if !strings.EqualFold(strings.TrimSpace(f.Property), "event_name") {
				continue
			}
			for _, v := range f.Operation.Values {
				seen++
				if v != want {
					exact = false
				}
			}
		}
	})
	return seen > 0 && exact
}

// includesNoValueSet reports whether any filter in the tree sets
// operation.includeObjectsWithNoValueSet: an "event_name IS_EQUAL_TO X" that also admits
// events with NO event_name is not an exact edition, and neither RegistrationEvidence nor
// CoveredEditions reads the flag. An unreadable tree counts as setting it.
func includesNoValueSet(filter json.RawMessage) bool {
	type node struct {
		FilterBranches []node `json:"filterBranches"`
		Filters        []struct {
			Operation struct {
				IncludeObjectsWithNoValueSet bool `json:"includeObjectsWithNoValueSet"`
			} `json:"operation"`
		} `json:"filters"`
	}
	var root node
	if len(filter) == 0 {
		return false
	}
	if json.Unmarshal(filter, &root) != nil {
		return true
	}
	var walk func(n node) bool
	walk = func(n node) bool {
		for _, f := range n.Filters {
			if f.Operation.IncludeObjectsWithNoValueSet {
				return true
			}
		}
		for _, c := range n.FilterBranches {
			if walk(c) {
				return true
			}
		}
		return false
	}
	return walk(root)
}

// StandingFilterIsUnion reports whether a standing list's filter is only a union of list
// memberships -- every condition an IN_LIST filter, each one a whole OR alternative -- which
// is the shape MasterListFilter writes. Only then can the list be rewritten from its
// CoveredListIDs without losing anything; any other condition (an AND of lists, a property
// filter added by hand) would be dropped by the rewrite. An explicitly typed empty tree
// ({"filterBranchType":"OR",...}) is a union of nothing; an untyped node anywhere ("{}") is not; a MISSING or null filter is not a union at all -- the list's real filter is unknown,
// and rewriting it would replace whatever it holds (see HasFilterTree).
func StandingFilterIsUnion(filter json.RawMessage) bool {
	var root qaBranch
	if !HasFilterTree(filter) {
		return false
	}
	if json.Unmarshal(filter, &root) != nil {
		return false
	}
	union := true
	referenced := map[string]struct{}{}
	root.walk(func(b qaBranch) {
		// Every node must say what it is. An untyped node -- "{}" read back from HubSpot, or a
		// nested one -- is not a tree the rewrite can be shown to reproduce.
		switch strings.ToUpper(strings.TrimSpace(b.FilterBranchType)) {
		case "AND", "OR":
		default:
			union = false
		}
		for _, f := range b.Filters {
			id := strings.TrimSpace(f.ListID.String())
			if f.FilterType != "IN_LIST" || f.Operator != "IN_LIST" || id == "" {
				union = false
				continue
			}
			referenced[id] = struct{}{}
		}
	})
	if !union {
		return false
	}
	covered := CoveredListIDs(filter)
	return len(covered) == len(referenced)
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
		return nil, &FieldError{Field: "current_registrants_event",
			Reason: "must be the edition's event name WITH its year, exactly as registrations record it (e.g. \"AGNTCon + MCPCon Japan 2026\"); without the year the list would match nobody"}
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

// HasFilterTree reports whether raw is an explicit filter tree (a JSON object), as opposed to
// a missing or null filterBranch. GetList asks for the filters, so a list read back without one
// says nothing about what the list holds: reading it as "covers nothing" and rewriting it from
// the currently resolved ids would drop every suppression it carried.
func HasFilterTree(raw json.RawMessage) bool {
	t := strings.TrimSpace(string(raw))
	return strings.HasPrefix(t, "{")
}
