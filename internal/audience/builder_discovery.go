// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package audience

import (
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// Discovery classifier (LFXV2-2770)
//
// Given a HubSpot list's name and its real filter shape, decide which audience
// signal it represents. Entirely deterministic: no LLM is involved in
// classification, only in extracting the event's identity from its web page. A
// model that guesses which list to email is a model that can silently mail the
// wrong ten thousand people; a fixed rule set is auditable, and `uncertain` is a
// bucket the operator actually sees.
// ---------------------------------------------------------------------------

// Names that mark a list as an exclusion asset, which is never an inclusion signal.
var suppressionNameHints = []string{"suppress", "exclusion", "unsubscribe", "do not email", "opt out", "opt-out", "optout", "bounce"}

// Property names that carry an email-subscription type, whose VALUE decides the
// opt-in bucket.
var subscriptionPropertyHints = []string{"subscription", "opt_in", "optin", "newsletter"}

// The exact subscription value that means the portfolio-wide newsletter rather
// than a project's own.
const lfNewsletterValueHint = "linux foundation newsletter"

// Names and property values that mark all-time registration for an event.
var registrationHints = []string{"registrant", "registration", "registered", "attendee", "attended"}

// behaviouralFilterTypes describe a contact's own behaviour, as opposed to list
// membership. Only these are carried up from a one-hop child: a child's own
// IN_LIST filters would keep the hop going forever.
var behaviouralFilterTypes = map[string]struct{}{
	"PROPERTY": {}, "UNIFIED_EVENTS": {}, "PAGE_VIEW": {}, "CUSTOM_EVENT": {},
	"EVENT": {}, "WEB_ANALYTICS": {}, "FORM_SUBMISSION": {},
}

// speakerScopeSuffix is one suffix the current speaker-list naming convention
// uses to state its own scope.
type speakerScopeSuffix struct {
	suffix string
	scope  SpeakerScope
}

// speakerScopeSuffixes is ordered LONGEST FIRST: " - current" is a suffix of
// nothing, but " - current + past" ends with neither " - current" nor " - past",
// so declaration order matters only for the reader — the real trap is the reverse
// direction, where a shorter pattern checked first would classify a current+past
// list as one edition. Keep longest-first.
var speakerScopeSuffixes = []speakerScopeSuffix{
	{" - current + past", SpeakerScopeCurrentPast},
	{" - past", SpeakerScopePast},
	{" - current", SpeakerScopeCurrent},
}

var speakerNameRE = regexp.MustCompile(`(?i)\bspeakers?\b`)

// EventExtractionSystemPrompt instructs the model to report an event's identity as
// JSON and nothing else.
//
// This is the ONLY use of a model in discovery. It is confined to reading a web
// page the operator pointed at, because "what is this event called" is a reading
// task with a checkable answer, while "which HubSpot list should this email go
// to" is not.
const EventExtractionSystemPrompt = `You read a conference or event web page and report the event identity as JSON.
Respond with ONLY a JSON object, no prose and no code fences, with exactly these keys:
{"eventName": string, "brandShort": string, "eventDates": string[]}

- eventName: the exact, FULL event name as the page states it, including every brand joiner
  it contains (e.g. "KubeCon + CloudNativeCon North America 2026"). Never abbreviate it to an
  acronym and never drop the region or edition.
- brandShort: the owning foundation or project (e.g. "CNCF", "PyTorch Foundation",
  "OpenSearch Software Foundation", "The Linux Foundation").
- eventDates: the event dates as ISO YYYY-MM-DD strings, or [] if the page does not state them.

If a field is genuinely absent from the page, use an empty string or an empty array. Do not
guess a name from the URL and do not invent dates.`

// isoDateRE matches the ISO prefix a date must have to be trusted. MasterListName
// derives a production list's YYQN segment from these, and a half-parsed date
// there names a real list for the wrong quarter.
var isoDateRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}`)

// SanitizeEventDates keeps only well-formed ISO dates, in order.
func SanitizeEventDates(dates []string) []string {
	out := make([]string, 0, len(dates))
	for _, d := range dates {
		if d = strings.TrimSpace(d); isoDateRE.MatchString(d) {
			out = append(out, d)
		}
	}
	return out
}

// EventYear is the four-digit year of the event's first date, or "".
func (id EventIdentity) EventYear() string {
	if len(id.Dates) == 0 || len(id.Dates[0]) < 4 {
		return ""
	}
	return id.Dates[0][:4]
}

// DiscoveryQueries are the HubSpot list searches to run for this event, in order.
//
// The FULL exact event name goes first, verbatim. A bare brand acronym like
// "KubeCon" matches hundreds of lists spanning every year and region the brand has
// ever run, so the 20 result slots fill with old and wrong-region noise and the
// current edition's own lists never appear. The second search appends the edition
// year, because master-list names usually embed it and those lists can rank below
// other results on the year-less query.
func DiscoveryQueries(id EventIdentity) []string {
	name := strings.TrimSpace(id.Name)
	if name == "" {
		return nil
	}
	queries := []string{name}
	if year := id.EventYear(); year != "" && !yearRE.MatchString(name) {
		queries = append(queries, name+" "+year)
	}
	return queries
}

// IsPlausibleCandidate reports whether a name plausibly belongs to this event at
// all.
//
// HubSpot's search matches loosely, so requiring at least one shared keyword is
// what keeps another brand's list out of the grid. Filtering here rather than at
// classification time is deliberate: inspections are the budgeted resource, and
// spending one on obvious noise costs a real list its slot.
func IsPlausibleCandidate(name string, keywords map[string]struct{}) bool {
	words := EventKeywords(name)
	for keyword := range keywords {
		if _, ok := words[keyword]; ok {
			return true
		}
	}
	return false
}

// IsRollup reports whether a list's filters only point at other lists — its real
// shape is one hop away.
//
// An empty filter set is NOT a rollup: a manual list has no filters, and treating
// it as a rollup would send the resolver hunting for children that do not exist.
func IsRollup(filters []ListFilter) bool {
	if len(filters) == 0 {
		return false
	}
	for _, f := range filters {
		if f.FilterType != "IN_LIST" && f.Operator != "IN_LIST" && f.Operator != "NOT_IN_LIST" {
			return false
		}
	}
	return true
}

// RollupChildIDs are the list ids a rollup INCLUDES, de-duplicated in first-seen
// order.
//
// Exclusion references are skipped: hopping into a suppression list would import
// its opt-out filters as classification evidence and could bucket a master list as
// whatever its exclusions happen to filter on.
func RollupChildIDs(filters []ListFilter) []string {
	seen := make(map[string]struct{}, len(filters))
	out := make([]string, 0, len(filters))
	for _, f := range filters {
		if f.Operator == "NOT_IN_LIST" {
			continue
		}
		id := f.ListIDString()
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// KeepBehavioural filters a one-hop child's filters down to the ones that explain
// what it selects.
func KeepBehavioural(filters []ListFilter) []ListFilter {
	out := make([]ListFilter, 0, len(filters))
	for _, f := range filters {
		if _, ok := behaviouralFilterTypes[strings.ToUpper(strings.TrimSpace(f.FilterType))]; ok {
			out = append(out, f)
		}
	}
	return out
}

// DetermineSpeakerScope says which edition(s) a speaker list covers.
//
// The name suffix is authoritative when present — it is the convention the lists
// are built under. Older lists predate it, so the fallback compares the years
// named in the filters against the event's own year. When neither is available the
// answer is `current_past`, the SUPERSET: guessing narrower would silently drop
// speakers from a send, and an operator can always deselect.
func DetermineSpeakerScope(name string, filters []ListFilter, eventYear string) SpeakerScope {
	lower := strings.ToLower(strings.TrimSpace(name))
	for _, s := range speakerScopeSuffixes {
		if strings.HasSuffix(lower, s.suffix) {
			return s.scope
		}
	}
	if eventYear == "" {
		return SpeakerScopeCurrentPast
	}

	years := make(map[string]struct{})
	for _, f := range filters {
		for _, value := range f.StringValues() {
			for _, match := range yearRE.FindAllString(value, -1) {
				years[match] = struct{}{}
			}
		}
	}
	if len(years) == 0 {
		return SpeakerScopeCurrentPast
	}

	_, hasCurrent := years[eventYear]
	hasPrior := false
	for year := range years {
		if year < eventYear {
			hasPrior = true
			break
		}
	}
	switch {
	case hasCurrent && hasPrior:
		return SpeakerScopeCurrentPast
	case hasCurrent:
		return SpeakerScopeCurrent
	case hasPrior:
		return SpeakerScopePast
	default:
		// Only future years were named — not this edition and not a prior one.
		// The superset is the honest answer.
		return SpeakerScopeCurrentPast
	}
}

// Classification is one list's bucket, the reason to show the operator, and — for
// a speaker list — which editions it covers.
type Classification struct {
	Signal Signal
	Reason string
	// Scope is set only for SignalEventSpeakers.
	Scope SpeakerScope
}

// ClassifyList sorts one list into exactly one signal, from its name and its real
// filter shape.
//
// Precedence is FIXED AND TOTAL, so a list that satisfies two rules always lands
// in the same bucket. The order encodes specificity: an exclusion asset is
// disqualified before anything else can claim it, the structurally unambiguous
// filter types come next, and the name-driven rules — the ones most likely to be
// wrong — are consulted last. `uncertain` is a real bucket the operator sees,
// never a silent drop.
func ClassifyList(name string, filters []ListFilter, id EventIdentity) Classification {
	// 1. Suppression. Checked first because these lists routinely ALSO carry a
	//    registration or page-view filter, and bucketing one as an inclusion
	//    signal would put an opt-out audience into a send.
	if containsAny(name, suppressionNameHints) {
		return Classification{Signal: SignalUncertain, Reason: "Suppression or exclusion list, not an inclusion signal"}
	}

	// 2. Page view — a filter type of its own, so there is nothing to infer.
	for _, f := range filters {
		if f.FilterType == "PAGE_VIEW" {
			return Classification{Signal: SignalPageView, Reason: "Page-view filter on this event or project"}
		}
	}

	// 3. Education enrolment, identified by the portal's FIXED education event
	//    type. See educationEventTypeID in filters.go: it is portal-wide, not
	//    per-event, and must not be looked up.
	for _, f := range filters {
		if f.FilterType != "UNIFIED_EVENTS" || f.EventTypeID != educationEventTypeID {
			continue
		}
		keywords := EventKeywords(id.BrandShort + " " + id.Name)
		parts := []string{name}
		for _, other := range filters {
			parts = append(parts, other.StringValues()...)
		}
		// An unscoped education list covers every LFX Education course across the
		// whole portfolio. Offering it as THIS event's education signal would
		// target learners with no connection to the brand, so it goes to
		// `uncertain` with the missing scoping named.
		if !IsPlausibleCandidate(strings.Join(parts, " "), keywords) {
			return Classification{
				Signal: SignalUncertain,
				Reason: "LFX Education enrollment with no brand or topic scoping — covers all courses",
			}
		}
		return Classification{Signal: SignalEducationEnrollment, Reason: "LFX Education enrollment scoped to this brand"}
	}

	// 4. Speakers. Speaker-specific only — a registration list is not one of these
	//    merely because speakers happen to appear in it.
	speakerProperty := false
	for _, f := range filters {
		if f.PropertyKey() != "hosted_events" {
			continue
		}
		for _, value := range f.StringValues() {
			if strings.HasSuffix(strings.ToLower(strings.TrimSpace(value)), " - speakers") {
				speakerProperty = true
				break
			}
		}
		if speakerProperty {
			break
		}
	}
	if speakerNameRE.MatchString(name) || speakerProperty {
		reason := "List name is scoped to this event’s speakers"
		if speakerProperty {
			reason = "Filters on a hosted-events speaker value for this event"
		}
		return Classification{
			Signal: SignalEventSpeakers,
			Reason: reason,
			Scope:  DetermineSpeakerScope(name, filters, id.EventYear()),
		}
	}

	// 5. Subscription opt-in, split BY THE FILTER'S VALUE rather than the list
	//    name. The two buckets share one filter shape, and a project list is
	//    frequently named only "Opt-In" — reading the name would merge a project's
	//    own subscribers into the portfolio newsletter.
	for _, f := range filters {
		property := f.PropertyKey()
		if property == "" || !containsAny(property, subscriptionPropertyHints) {
			continue
		}
		values := f.StringValues()
		for _, value := range values {
			if strings.Contains(strings.ToLower(value), lfNewsletterValueHint) {
				return Classification{Signal: SignalLFNewsletterOptIn, Reason: "Subscribed to the Linux Foundation Newsletter"}
			}
		}
		if len(values) > 0 {
			return Classification{Signal: SignalProjectOptIn, Reason: "Subscribed to “" + strings.ToLower(values[0]) + "”"}
		}
		return Classification{Signal: SignalProjectOptIn, Reason: "Email-subscription filter for this project"}
	}

	// 6. Registration — the broadest inclusion signal, so it is offered only once
	//    the specific ones have declined the list.
	registration := containsAny(name, registrationHints)
	if !registration {
		for _, f := range filters {
			if f.FilterType == "UNIFIED_EVENTS" {
				registration = true
				break
			}
			for _, value := range f.StringValues() {
				if containsAny(value, registrationHints) {
					registration = true
					break
				}
			}
			if registration {
				break
			}
		}
	}
	if registration {
		return Classification{Signal: SignalEventRegistration, Reason: "All-time registrants or attendees for this event"}
	}

	// 7. Everything else, with the reason the operator needs to judge it
	//    themselves.
	if len(filters) > 0 {
		return Classification{Signal: SignalUncertain, Reason: "Filter shape did not match a qualifying signal"}
	}
	return Classification{Signal: SignalUncertain, Reason: "No readable filters — could not determine what this list selects"}
}

// MissingSignals are the classified signals no discovered list landed in.
//
// Derived from ClassifiedSignals rather than from the found set so a newly added
// signal appears in the "not found" section automatically, in render order.
func MissingSignals(found map[Signal]struct{}) []Signal {
	out := make([]Signal, 0, len(ClassifiedSignals))
	for _, signal := range ClassifiedSignals {
		if _, ok := found[signal]; !ok {
			out = append(out, signal)
		}
	}
	return out
}
