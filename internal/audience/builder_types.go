// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package audience

import (
	"encoding/json"
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// Audience Builder — shared vocabulary (LFXV2-2770)
//
// This file and its siblings (builder_discovery.go, builder_master.go,
// builder_qa.go, builder_lastsent.go) hold the EXPLORATORY half of the audience
// feature: the deterministic rules an operator uses to find, judge, and combine
// HubSpot lists BEFORE a brief's audience record exists. The RECORD half — the
// planned build that materializes an audience against a brief — lives in
// plan.go / filters.go and is deliberately separate.
//
// Everything here is pure: no HTTP, no credentials, no clock beyond an injected
// one. That is what makes the classifier and the QA rules testable, and it is
// why the orchestration (which lists to fetch, in what order, under what budget)
// lives in internal/dispatch instead.
// ---------------------------------------------------------------------------

// Signal is the bucket a discovered list is classified into.
//
// `SignalUncertain` is a first-class value, NOT an error state: a list whose
// filters this code does not recognise must still be shown to the operator, in a
// bucket that says "decide for yourself". Silently dropping it is how an audience
// loses a group nobody notices is missing.
type Signal string

// The classified signals, plus the two provenance buckets.
const (
	// SignalLastSent is provenance, not a classification: the list was used in a
	// prior send for this event.
	SignalLastSent Signal = "last_sent"
	// SignalAdded is provenance: the operator added the list by hand via search.
	SignalAdded Signal = "added"

	SignalEventRegistration   Signal = "event_registration"
	SignalEventSpeakers       Signal = "event_speakers"
	SignalProjectOptIn        Signal = "project_opt_in"
	SignalLFNewsletterOptIn   Signal = "lf_newsletter_opt_in"
	SignalEducationEnrollment Signal = "education_enrollment"
	SignalPageView            Signal = "page_view"
	SignalUncertain           Signal = "uncertain"
)

// ClassifiedSignals are the signals the discovery pass actually classifies into,
// in the order the review grid renders them.
//
// `last_sent` and `added` are excluded because they are provenance rather than
// classifications — they can never be "missing". `uncertain` is excluded for the
// opposite reason: it is the fallback, so reporting it as a missing signal would
// tell an operator to go create a list of things we failed to understand.
var ClassifiedSignals = []Signal{
	SignalEventRegistration,
	SignalEventSpeakers,
	SignalProjectOptIn,
	SignalLFNewsletterOptIn,
	SignalEducationEnrollment,
	SignalPageView,
}

// SpeakerScope says which editions' speakers a speaker list covers.
type SpeakerScope string

const (
	SpeakerScopeCurrent     SpeakerScope = "current"
	SpeakerScopePast        SpeakerScope = "past"
	SpeakerScopeCurrentPast SpeakerScope = "current_past"
)

// Caps and budgets. Each bounds a fan-out against a rate-limited API; none is a
// preference, and changing one changes how honest a result can be.
const (
	// UnionExactCap is the combined estimated size above which an exact union
	// count is refused. HubSpot exposes no way to count an arbitrary OR-of-lists,
	// so the only exact answer comes from paginating every selected list's
	// membership and unioning ids in memory. The cap bounds that sweep.
	UnionExactCap = 25000

	// MembershipPageSize is HubSpot's maximum page size for
	// GET /crm/v3/lists/{id}/memberships.
	MembershipPageSize = 250

	// MembershipMaxPages is the hard stop on membership pagination.
	// MembershipPageSize * this == UnionExactCap records.
	MembershipMaxPages = 100

	// PreviewMaxLists bounds how many lists one preview-count may union. The sweep
	// is TWO sequential HubSpot round-trips per id -- GetList for the estimate, then
	// paginated ListMembershipIDs -- so cost grows linearly with the selection and
	// nothing else stops it: the design's list_ids carried MinLength(1) and no upper
	// bound, and the handler had no deadline, so a large array simply ran until the
	// gateway gave up and returned a 504 with no diagnosis.
	//
	// 50 is well clear of any real selection (discovery surfaces at most
	// DiscoveryMaxInspections candidates across 4 signals, and an operator ticks a
	// handful) while keeping the worst case bounded at 50 GetList calls plus, below
	// the cap, at most UnionExactCap membership records.
	PreviewMaxLists = 50

	// DiscoveryMaxInspections is how many candidate lists discovery will fetch in
	// full before it stops inspecting. Every inspection is a
	// GET /crm/v3/lists/{id}?includeFilters=true; a broad event name can return
	// hundreds of candidates across two searches plus one-hop rollup resolution.
	// Candidates past the budget are still listed and classified from their names.
	DiscoveryMaxInspections = 40

	// LastSentEmailSearchLimit is how many marketing emails to pull per search
	// before filtering to published ones.
	LastSentEmailSearchLimit = 30
)

// SuppressionTerm is one portfolio-wide hygiene suppression list, resolved by
// name search rather than by a stored id.
//
// These are recreated every quarter/year, so resolution picks the highest `YYQN`
// code among matches rather than the first result. Brand-scoped opt-out lists
// ("CNCF Global Opt Out") and per-event suppression lists are found separately by
// pattern — they are project-specific and cannot be enumerated here.
type SuppressionTerm struct {
	Key        string
	Label      string
	SearchTerm string
}

// StandardSuppressionTerms is the fixed portfolio-wide set.
var StandardSuppressionTerms = []SuppressionTerm{
	{Key: "lf_global_opt_outs", Label: "LF Global Opt-Outs", SearchTerm: "LF Global Opt-Outs"},
	{Key: "lf_europe_global_opt_outs", Label: "LF Europe Global Opt-Outs", SearchTerm: "LF Europe Global Opt-Outs"},
	{Key: "lf_events_gdpr", Label: "LF Events GDPR Suppression", SearchTerm: "LF Events GDPR Suppression"},
	{Key: "lf_europe_gdpr", Label: "LF Europe GDPR Suppression", SearchTerm: "LF Europe GDPR Suppression"},
	{Key: "lf_master_exclusion", Label: "LF Master Exclusion List", SearchTerm: "LF Master Exclusion"},
	{Key: "lf_events_suppression", Label: "LF Events Suppression List", SearchTerm: "LF Events Suppression List"},
}

// EventIdentity is what discovery extracted from the event page: enough to search
// HubSpot for this event's lists and to name a master list after it.
type EventIdentity struct {
	Name       string
	BrandShort string
	// Dates are ISO `YYYY-MM-DD` strings. Empty when the page stated none — the
	// quarter code then falls back to the current quarter rather than guessing.
	Dates []string
}

// ---------------------------------------------------------------------------
// Read-side filter tree
//
// filters.go's `filterBranch`/`filter` are WRITE shapes: exactly the fields
// HubSpot requires to create a list. Reading is a different problem. HubSpot's
// filter schema is a large discriminated union that varies by `filterType` and
// grows without notice, and this code only inspects a handful of fields to
// classify a list. Restating the full schema would be a contract this repo does
// not own and could not keep current — and an unmodelled member would then fail
// to parse rather than fall through to `uncertain`, which is the honest answer
// for a filter shape we do not recognise.
// ---------------------------------------------------------------------------

// ListFilterBranch is HubSpot's filter tree as read back: nested branches with
// leaf filters at every level.
type ListFilterBranch struct {
	FilterBranchType string             `json:"filterBranchType"`
	FilterBranches   []ListFilterBranch `json:"filterBranches"`
	Filters          []ListFilter       `json:"filters"`
	// Operator and EventTypeID are carried by non-boolean branch types
	// (UNIFIED_EVENTS and friends), where the behavioural predicate lives on the
	// BRANCH rather than on a leaf filter.
	Operator    string `json:"operator"`
	EventTypeID string `json:"eventTypeId"`
}

// ListFilter is one leaf filter, loosely typed on purpose (see the block comment
// above). Values arrive as either a scalar `value` or a `values` array, and the
// property key arrives as either `property` or `propertyName`, depending on
// filter type — both shapes are real, so both are decoded.
type ListFilter struct {
	FilterType   string          `json:"filterType"`
	Operator     string          `json:"operator"`
	ListID       json.Number     `json:"listId"`
	Property     string          `json:"property"`
	PropertyName string          `json:"propertyName"`
	EventTypeID  string          `json:"eventTypeId"`
	Value        json.RawMessage `json:"value"`
	ValueList    json.RawMessage `json:"values"`
	// Operation is the nested operation object write-side filters carry; its
	// values are read as additional classification evidence.
	Operation struct {
		Operator string          `json:"operator"`
		Values   json.RawMessage `json:"values"`
	} `json:"operation"`
}

// ListIDString is the referenced list id as a string, or "" when absent. HubSpot
// returns it as a number on some endpoints and a string on others, which is why
// it is decoded as json.Number.
func (f ListFilter) ListIDString() string {
	return strings.TrimSpace(f.ListID.String())
}

// PropertyKey is the filter's property name, lowercased. `property` wins over
// `propertyName` because HubSpot sets the former on the filter types this code
// classifies on and the latter only on older shapes.
func (f ListFilter) PropertyKey() string {
	if p := strings.TrimSpace(f.Property); p != "" {
		return strings.ToLower(p)
	}
	return strings.ToLower(strings.TrimSpace(f.PropertyName))
}

// StringValues flattens every string value this filter carries, from all three
// places HubSpot puts them: a scalar `value`, a `values` array, and the nested
// `operation.values`. A subscription filter's BUCKET is decided by its value, not
// its property name, so missing one of these shapes silently mis-buckets a list.
func (f ListFilter) StringValues() []string {
	out := make([]string, 0, 4)
	out = appendJSONStrings(out, f.Value)
	out = appendJSONStrings(out, f.ValueList)
	out = appendJSONStrings(out, f.Operation.Values)
	return out
}

// appendJSONStrings decodes a raw JSON value as either a string or an array and
// appends every non-blank string it finds. Anything else (a number, an object) is
// skipped rather than stringified: a stringified object would inject brace noise
// into substring matching and could accidentally satisfy a hint.
func appendJSONStrings(dst []string, raw json.RawMessage) []string {
	if len(raw) == 0 {
		return dst
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		if s := strings.TrimSpace(one); s != "" {
			dst = append(dst, s)
		}
		return dst
	}
	var many []json.RawMessage
	if err := json.Unmarshal(raw, &many); err != nil {
		return dst
	}
	for _, item := range many {
		var s string
		if err := json.Unmarshal(item, &s); err != nil {
			continue
		}
		if s = strings.TrimSpace(s); s != "" {
			dst = append(dst, s)
		}
	}
	return dst
}

// CollectFilters flattens a filter tree into its leaf filters.
//
// Non-boolean branches (UNIFIED_EVENTS and any future sibling) are ALSO emitted
// as a synthetic leaf carrying the branch's own filterType/operator/eventTypeId.
// HubSpot puts a behavioural predicate on the branch, not on a child filter, so a
// flatten that only walked `filters` would lose exactly the evidence that
// identifies an education enrolment or an event registration — and every such
// list would fall through to `uncertain`.
func CollectFilters(branch *ListFilterBranch) []ListFilter {
	if branch == nil {
		return nil
	}
	out := make([]ListFilter, 0, len(branch.Filters)+1)
	if t := strings.ToUpper(strings.TrimSpace(branch.FilterBranchType)); t != "" && t != "AND" && t != "OR" {
		out = append(out, ListFilter{
			FilterType:  branch.FilterBranchType,
			Operator:    branch.Operator,
			EventTypeID: branch.EventTypeID,
		})
	}
	out = append(out, branch.Filters...)
	for i := range branch.FilterBranches {
		out = append(out, CollectFilters(&branch.FilterBranches[i])...)
	}
	return out
}

// ParseFilterBranch decodes a raw filterBranch. A manual list has no filters at
// all and comes back as `{}` or absent; that is not an error, it is a list whose
// membership is static — which the classifier and the QA signal check both need
// to be able to say out loud.
func ParseFilterBranch(raw json.RawMessage) (*ListFilterBranch, error) {
	if len(raw) == 0 {
		return &ListFilterBranch{}, nil
	}
	var branch ListFilterBranch
	if err := json.Unmarshal(raw, &branch); err != nil {
		return nil, err
	}
	return &branch, nil
}

// yearRE matches any four-digit year, which is what makes a list name fail to
// match a different edition of the same event.
var yearRE = regexp.MustCompile(`\b(19|20)\d{2}\b`)

// stopwords are dropped from keyword sets: they overlap between every event name
// and would make an unrelated list look like a plausible candidate.
var stopwords = map[string]struct{}{
	"the": {}, "a": {}, "an": {}, "and": {}, "of": {}, "for": {}, "in": {}, "on": {}, "to": {},
}

var wordRE = regexp.MustCompile(`[a-z0-9]+`)

// EventKeywords is the significant-token set of a piece of text: lowercase
// alphanumeric runs longer than two characters, minus stopwords.
//
// Two-character tokens are dropped along with stopwords because they are almost
// all noise ("22", "eu", "ai") that matches far too much — and a false candidate
// costs an inspection from a budget of DiscoveryMaxInspections.
func EventKeywords(text string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, token := range wordRE.FindAllString(strings.ToLower(text), -1) {
		if len(token) <= 2 {
			continue
		}
		if _, skip := stopwords[token]; skip {
			continue
		}
		out[token] = struct{}{}
	}
	return out
}

// containsAny reports whether haystack contains any of the (already lowercase)
// needles.
func containsAny(haystack string, needles []string) bool {
	haystack = strings.ToLower(haystack)
	for _, needle := range needles {
		if strings.Contains(haystack, needle) {
			return true
		}
	}
	return false
}
