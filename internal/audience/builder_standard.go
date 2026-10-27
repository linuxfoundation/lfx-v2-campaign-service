// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package audience

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// MaxMasterFilterConditions bounds the filter conditions one composed master may carry.
// MasterListWithSuppressionsFilter repeats every exclusion on every inclusion branch, so a
// master has inclusions × (exclusions + 1) conditions: 200 × 200 would be over 40,000.
//
// HubSpot's real per-list limit is NOT verified (an open live check). 2,500 is chosen to be
// conservative while covering every selection seen in practice (25 lists × 99 exclusions, or
// 200 lists × 11), and it keeps the create request near 250 KB at roughly 100 bytes per
// condition. Raise it only against a measured HubSpot limit.
const MaxMasterFilterConditions = 2500

// CheckMasterExpansion refuses a master whose filter would exceed MaxMasterFilterConditions:
// inclusions branches, each one IN_LIST plus one NOT_IN_LIST per exclusion. exclusions counts
// the RESOLVED excluded lists, the standing standard list as one. The error is a FieldError
// (400) the operator can act on by selecting less.
func CheckMasterExpansion(inclusions, exclusions int) error {
	conditions := inclusions * (exclusions + 1)
	if conditions <= MaxMasterFilterConditions {
		return nil
	}
	return &FieldError{Field: "list_ids and exclude_list_ids", Reason: fmt.Sprintf(
		"%d inclusions × %d exclusions need %d filter conditions (one IN_LIST plus one NOT_IN_LIST per exclusion on every inclusion), over the %d-condition limit for one master list; reduce the selection",
		inclusions, exclusions, conditions, MaxMasterFilterConditions)}
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

// MaxStandingCandidateReads bounds how many of a send's exclusions StandingSendCheck reads to
// find the standing list when the name search did not. Over it the check falls back to the
// search hit alone (Fallback set, for the caller to log); a send is never refused for its size.
const MaxStandingCandidateReads = 50

// MaxSendListFilterReads bounds how many send lists' filters StandingSendCheck reads to see
// through them to their sources. The rest keep the direct id check only (SourcesSkipped set, for
// the caller to log); a send is never refused for its size.
const MaxSendListFilterReads = 50

// ErrListFilterUnreadable is StandingSendCheck's refusal for a send list or standing list read
// back without an explicit filter tree (missing or null filterBranch, see HasFilterTree): what
// it covers or includes is unknown, which is not the same as nothing.
var ErrListFilterUnreadable = errors.New("audience: a list was read back without its filter")

// ListInfo is what StandingSendCheck needs of one list as read back.
type ListInfo struct {
	Name   string
	Filter json.RawMessage
	// ProcessingType is HubSpot's DYNAMIC, MANUAL or SNAPSHOT. Only a DYNAMIC list has a filter;
	// a static list (MANUAL, SNAPSHOT) has none, and no sources to read.
	ProcessingType string
}

// static reports whether l is a static list, which carries no filter. An unknown or empty type
// is treated as dynamic, so a missing filter is still caught.
func (l ListInfo) static() bool {
	t := strings.ToUpper(strings.TrimSpace(l.ProcessingType))
	return t == "MANUAL" || t == "SNAPSHOT"
}

// ListReader reads one list. A failed read is an error.
type ListReader func(listID string) (ListInfo, error)

// StandingSendResult is StandingSendCheck's finding. SendID and CoveredID are set when the
// standing list (StandingID) covers a send list (CoveredID == SendID) or an inclusion source of
// one; all empty when the send is safe or excludes no standing list.
type StandingSendResult struct {
	SendID, CoveredID, StandingID string
	// Fallback reports that the exclusions were too many to read and only the name-search hit
	// was considered.
	Fallback bool
	// SourcesSkipped reports that only the first MaxSendListFilterReads send lists had their
	// sources checked; the rest were checked by id only.
	SourcesSkipped bool
}

// StandingSendCheck decides, at send time, whether a send excludes a standing standard
// suppression list that now covers what it sends to.
//
// The standing list is shared and only grows, so it can come to cover a list an
// already-recorded audience sends to; excluding it then removes that whole inclusion. A send
// list can itself be a master -- a composed audience's one send list, or an existing master
// attached through include_list_ids -- whose own filter includes source lists (IN_LIST) and
// excludes others (NOT_IN_LIST). So the filter of every DYNAMIC send list is read, up to
// MaxSendListFilterReads; a static (MANUAL, SNAPSHOT) send list has no filter and no sources and
// is checked by id only.
//
// The standing list is found among ALL the send's exclusions -- the recorded or supplied
// suppressions plus every NOT_IN_LIST list a read send list excludes itself: the name-search hit
// (searchHit) when that is one of them, otherwise, since search lags a fresh list and misses a
// renamed one, the candidate whose name starts with "LFX Standard Suppressions - " (any
// project's, hash-tagged forms included), reading at most MaxStandingCandidateReads of them;
// over that only the search hit counts.
//
// Whether it is IN EFFECT is decided per OR alternative of each send list (orBranches): the
// email's suppressions apply to every send list and every alternative, but an exclusion inside a
// list's filter constrains only the alternative it sits in -- in (IN X) OR (IN Y AND NOT S), X
// still delivers whatever S covers. With the standing list among the email's suppressions a send
// list it covers is refused by id; within an alternative where it is in effect, a covered IN_LIST
// source refuses. Any covered inclusion path refuses, the send-time match for the compose-time
// overlap 400.
//
// Nothing is refused for its size. Any failed read is an error, and so is a DYNAMIC send list or
// the standing list read back without a filter tree (ErrListFilterUnreadable): the send cannot
// be shown safe.
func StandingSendCheck(searchHit string, sendIDs, suppressionIDs []string, read ListReader) (StandingSendResult, error) {
	var res StandingSendResult
	sends := UniqueIDs(sendIDs)
	global := UniqueIDs(suppressionIDs)
	toRead := sends
	if len(toRead) > MaxSendListFilterReads {
		toRead, res.SourcesSkipped = toRead[:MaxSendListFilterReads], true
	}
	filters := make(map[string]json.RawMessage, len(toRead))
	candidates := append([]string(nil), global...)
	for _, id := range toRead {
		l, err := read(id)
		if err != nil {
			return res, err
		}
		if l.static() {
			continue
		}
		if !HasFilterTree(l.Filter) {
			return res, fmt.Errorf("%w: send list %s", ErrListFilterUnreadable, id)
		}
		filters[id] = l.Filter
		candidates = append(candidates, ExcludedListIDs(l.Filter)...)
	}
	candidates = UniqueIDs(candidates)
	if len(candidates) == 0 {
		return res, nil
	}

	var standingFilter json.RawMessage
	switch {
	case searchHit != "" && slices.Contains(candidates, searchHit):
		l, err := read(searchHit)
		if err != nil {
			return res, err
		}
		res.StandingID, standingFilter = searchHit, l.Filter
	case len(candidates) > MaxStandingCandidateReads:
		res.Fallback = true
		return res, nil
	default:
		prefix := strings.ToLower(standardSuppressionPrefix)
		for _, id := range candidates {
			l, err := read(id)
			if err != nil {
				return res, err
			}
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(l.Name)), prefix) {
				res.StandingID, standingFilter = id, l.Filter
				break
			}
		}
	}
	if res.StandingID == "" {
		return res, nil
	}
	if !HasFilterTree(standingFilter) {
		return res, fmt.Errorf("%w: standing list %s", ErrListFilterUnreadable, res.StandingID)
	}

	covered := CoveredListIDs(standingFilter)
	inGlobal := slices.Contains(global, res.StandingID)
	for _, id := range sends {
		// An email-level suppression applies to the whole send list, its own id included.
		if inGlobal && slices.Contains(covered, id) {
			res.SendID, res.CoveredID = id, id
			return res, nil
		}
		// Each OR alternative of the list's filter: the standing list is in effect there when the
		// email excludes it or that alternative excludes it itself, and then a covered source
		// empties that alternative.
		for _, b := range orBranches(filters[id]) {
			if !inGlobal && !slices.Contains(b.exclusions, res.StandingID) {
				continue
			}
			for _, source := range b.sources {
				if slices.Contains(covered, source) {
					res.SendID, res.CoveredID = id, source
					return res, nil
				}
			}
		}
	}
	res.StandingID = ""
	return res, nil
}

// ExcludedListIDs are the lists a filter tree EXCLUDES by membership: every NOT_IN_LIST filter,
// wherever it sits. An unreadable tree excludes nothing.
func ExcludedListIDs(filter json.RawMessage) []string {
	var root qaBranch
	if len(filter) == 0 || json.Unmarshal(filter, &root) != nil {
		return nil
	}
	return membershipIDs(root, "NOT_IN_LIST")
}

// membershipIDs are the list ids a subtree tests with IN_LIST filters of the given operator
// (IN_LIST or NOT_IN_LIST), anywhere in it.
func membershipIDs(root qaBranch, operator string) []string {
	var out []string
	root.walk(func(b qaBranch) {
		for _, f := range b.Filters {
			id := strings.TrimSpace(f.ListID.String())
			if f.FilterType == "IN_LIST" && f.Operator == operator && id != "" {
				out = append(out, id)
			}
		}
	})
	return UniqueIDs(out)
}

// listBranch is one OR alternative of a list's filter: the lists it includes (IN_LIST) and the
// lists it excludes (NOT_IN_LIST), anywhere inside it.
type listBranch struct {
	sources, exclusions []string
}

// orBranches splits a filter into its top-level OR alternatives: each child branch, and each
// filter standing directly in the OR. A filter whose root is not an OR is one alternative. An
// exclusion constrains only the alternative it sits in -- in (IN X) OR (IN Y AND NOT S), X still
// delivers whatever S is -- so StandingSendCheck decides per alternative.
func orBranches(filter json.RawMessage) []listBranch {
	var root qaBranch
	if len(filter) == 0 || json.Unmarshal(filter, &root) != nil {
		return nil
	}
	if !strings.EqualFold(strings.TrimSpace(root.FilterBranchType), "OR") {
		return []listBranch{{sources: membershipIDs(root, "IN_LIST"), exclusions: membershipIDs(root, "NOT_IN_LIST")}}
	}
	out := make([]listBranch, 0, len(root.Filters)+len(root.FilterBranches))
	for _, f := range root.Filters {
		one := qaBranch{FilterBranchType: "AND", Filters: append(root.Filters[:0:0], f)}
		out = append(out, listBranch{sources: membershipIDs(one, "IN_LIST"), exclusions: membershipIDs(one, "NOT_IN_LIST")})
	}
	for _, c := range root.FilterBranches {
		out = append(out, listBranch{sources: membershipIDs(c, "IN_LIST"), exclusions: membershipIDs(c, "NOT_IN_LIST")})
	}
	return out
}

// InclusionListIDs are the lists a filter tree INCLUDES by membership: every IN_LIST filter,
// wherever it sits, never a NOT_IN_LIST one. An unreadable tree includes nothing.
func InclusionListIDs(filter json.RawMessage) []string {
	var root qaBranch
	if len(filter) == 0 || json.Unmarshal(filter, &root) != nil {
		return nil
	}
	return membershipIDs(root, "IN_LIST")
}
