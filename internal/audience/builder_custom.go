// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package audience

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
)

// ---------------------------------------------------------------------------
// Custom audiences
//
// The "build from scratch" path: an operator describes an audience (usually a
// location's mailable contacts, optionally narrowed to speakers or ambassadors),
// a model turns the description into CustomCriteria, the operator edits that, and
// THIS file turns the approved criteria into a HubSpot filter branch.
//
// The split is deliberate. The model proposes; it never writes filter JSON and
// never reaches the create call. Everything that decides who gets mailed is here,
// pure and table-tested, so the shape HubSpot receives is the same for the same
// criteria no matter what the model said on the way.
// ---------------------------------------------------------------------------

// CustomPropertyOperator is an operator an operator can pick for an extra
// property filter. The values are the UI's words, not HubSpot's, so the wire
// contract does not leak HubSpot's operator vocabulary to the client.
type CustomPropertyOperator string

const (
	CustomOpEquals   CustomPropertyOperator = "is_equal_to"
	CustomOpContains CustomPropertyOperator = "contains"
	CustomOpAnyOf    CustomPropertyOperator = "is_any_of"
	CustomOpNotAnyOf CustomPropertyOperator = "is_not_any_of"
	CustomOpKnown    CustomPropertyOperator = "is_known"
	CustomOpUnknown  CustomPropertyOperator = "is_unknown"
)

// hubspotOperator maps an operator to HubSpot's for a property of the given kind,
// and reports whether it takes values at all.
//
// The vocabulary is HubSpot's list-filter reference, which is narrower than it
// looks: MULTISTRING has no IS_ANY_OF (IS_EQUAL_TO with several values means "any
// of"), there is no CONTAINS_TOKEN, and "known"/"unknown" are ALL_PROPERTY
// operations, not HAS_PROPERTY. The first live create built from the earlier
// mapping was refused outright.
func (op CustomPropertyOperator) hubspotOperator(enumeration bool) (string, bool, bool) {
	switch op {
	case CustomOpEquals:
		return "IS_EQUAL_TO", true, true
	case CustomOpContains:
		return "CONTAINS", true, true
	case CustomOpAnyOf:
		if enumeration {
			return "IS_ANY_OF", true, true
		}
		return "IS_EQUAL_TO", true, true
	case CustomOpNotAnyOf:
		return "IS_NOT_EQUAL_TO", true, true
	case CustomOpKnown:
		return "IS_KNOWN", false, true
	case CustomOpUnknown:
		return "IS_NOT_KNOWN", false, true
	}
	return "", false, false
}

// operationTypeFor is the operation type an operator needs: known/unknown are
// ALL_PROPERTY operations whatever the property's own type.
func (op CustomPropertyOperator) operationTypeFor(enumeration bool) string {
	switch {
	case op == CustomOpKnown || op == CustomOpUnknown:
		return "ALL_PROPERTY"
	case enumeration:
		return "ENUMERATION"
	}
	return "MULTISTRING"
}

// CustomPropertyFilter is one extra condition ANDed into every branch.
type CustomPropertyFilter struct {
	Property string
	Operator CustomPropertyOperator
	Values   []string
	// Enumeration is set by the orchestration from the property's definition in
	// the portal, never by the caller or the model: picking MULTISTRING for an
	// enumeration property is accepted by HubSpot and then matches nobody.
	Enumeration bool
}

// CustomSpeakers restricts the audience to event speakers.
type CustomSpeakers struct {
	Scope SpeakerScope
	// ListID is an existing "[Event] Speakers" list. Preferred whenever one exists:
	// a curated list beats an approximation from registration data.
	ListID string
	// EventNames are the exact event_name values to match when there is no list.
	// Exact and complete, including co-located tracks: a keyword fragment matches
	// unrelated events and still misses tracks whose names lack it.
	EventNames []string
	// PropertyTypes maps the registration event's speaker properties to their HubSpot
	// types ("string", "bool", "number", "enumeration"). Set by the orchestration from
	// the portal's event definition, never by the caller: each signal's operation type
	// must match its property's type, and a property the portal lacks is skipped.
	PropertyTypes map[string]string
}

// speakerSignals are the event properties a speaker registration is recorded
// under, with the value that marks one. Events record it under any of them.
var speakerSignals = []struct {
	property string
	marker   string
}{
	{"registration_type", "Speaker"},
	{"is_event_speaker_bool", "true"},
	{"is_event_speaker", "1"},
}

// SpeakerSignalProperties are the registration-event properties a speaker
// restriction may read, for the orchestration to resolve.
func SpeakerSignalProperties() []string {
	out := make([]string, 0, len(speakerSignals))
	for _, s := range speakerSignals {
		out = append(out, s.property)
	}
	return out
}

// speakerSignalFilters builds one filter per speaker property the portal defines,
// with the operation its type requires. HubSpot rejects a mismatch outright —
// the live create of a speakers list failed on is_event_speaker_bool filtered as
// MULTISTRING ("Valid operation types for this property type are: [BOOL, ALL_PROPERTY]").
func speakerSignalFilters(types map[string]string) []filter {
	var out []filter
	for _, s := range speakerSignals {
		typ, ok := types[s.property]
		if !ok {
			continue
		}
		f := filter{FilterType: "PROPERTY", Property: s.property}
		switch strings.ToLower(typ) {
		case "bool", "boolean":
			f.Operation = operation{Operator: "IS_EQUAL_TO", Value: s.marker == "true" || s.marker == "1", OperationType: "BOOL"}
		case "number":
			n := 1
			f.Operation = operation{Operator: "IS_EQUAL_TO", Value: n, OperationType: "NUMBER"}
		case "enumeration":
			f.Operation = operation{Operator: "IS_EQUAL_TO", Values: []string{s.marker}, OperationType: "ENUMERATION"}
		default:
			op := "IS_EQUAL_TO"
			if s.property == "registration_type" {
				// "Speaker" and "Speaker - User" both mark a speaker.
				op = "CONTAINS"
			}
			f.Operation = operation{Operator: op, Values: []string{s.marker}, OperationType: "MULTISTRING"}
		}
		out = append(out, f)
	}
	return out
}

// CustomCriteria is the reviewed definition of a custom audience.
type CustomCriteria struct {
	// Cities are matched on the contact's city and ip_city.
	Cities []string
	// Countries broaden the contact-location branches to country_dropdown and
	// ip_country. Empty means city-only, the narrower default.
	Countries []string
	// ContactLocation includes the contact-property location branches. They are
	// always gated by GateListID: a city filter alone selects everyone who ever
	// typed that city, mailable or not.
	ContactLocation bool
	// EventRegistered / EducationEnrolled add event-history branches: completed a
	// registration (or enrolment) whose own city/country matches.
	EventRegistered   bool
	EducationEnrolled bool
	// RegisteredFor are exact event_name values: contacts who completed a
	// registration for any of those editions. Not location-scoped — this is the
	// "this edition's registrants" signal, which has no city to filter on.
	RegisteredFor []string
	// EducationTopics narrow the education branch to courses whose topic matches
	// (CONTAINS) any of them — the brand fit, so an LF Energy audience does not pick
	// up every Kubernetes course enrolment. With no location they are the whole
	// education signal on their own.
	EducationTopics []string
	// EducationTopicProperty is the education event's course/topic property,
	// resolved by the orchestration from the portal's event definition. A caller's
	// value is honoured only when it is one of the portal's candidates
	// (CourseTopicCandidates); otherwise the best candidate replaces it. The model's
	// choice is never used: a guessed event property matches nobody.
	EducationTopicProperty string
	// PageViewURLs select contacts who viewed a page whose URL contains any of them.
	// Ungated, as in the reference runbook: a page view alone is the signal.
	PageViewURLs []string
	// GateEventHistory also gates the event-history branches by GateListID — the
	// stricter "mailable" reading. The reference lists leave them ungated.
	GateEventHistory bool
	// GateListID is the brand master list every gated branch must be in.
	GateListID string
	// PropertyFilters are ANDed into every branch.
	PropertyFilters []CustomPropertyFilter
	Speakers        *CustomSpeakers
	Ambassadors     bool
	// Label names the event(s) the audience is for; it only feeds the list name.
	Label string
}

// ambassadorProperties are each programme's primary "is an ambassador" property.
// There is no cross-brand flag, so the restriction is one branch per programme.
// Maintained by hand, exactly like the reference it was ported from.
var ambassadorProperties = []string{
	"cncf_ambassador___year_ambassador",
	"lf_energy_ambassador___bio",
	"open_mainframe_ambassador___bio",
}

// customBranchLimit bounds the expanded branch count. Roles multiply branches
// (every base branch is copied once per speaker signal or ambassador programme),
// and HubSpot refuses filter trees far above this with an error naming no limit.
const customBranchLimit = 60

// Contact and event properties the location branches read.
const (
	propCity            = "city"
	propIPCity          = "ip_city"
	propCountryDropdown = "country_dropdown"
	propEventCity       = "event_city"
	propEventCountry    = "event_country"
	propUserCity        = "user_city"
	propUserCountry     = "user_country"
)

// CanonicalCountries maps each country to the form HubSpot stores it in
// (DisplayName), so "USA" filters on "United States" rather than on a literal
// that matches nobody — the fix the brief path's BuildPlan already applies. It is
// applied where criteria enter the service, so preview shows the value actually
// filtered on. Blank values are dropped and aliases of one country collapse to a
// single entry; an unknown country is kept as written. Empty input returns nil.
func CanonicalCountries(countries []string) []string {
	if len(countries) == 0 {
		return nil
	}
	out := make([]string, 0, len(countries))
	for _, c := range countries {
		out = append(out, DisplayName(c))
	}
	return cleanValues(out)
}

// SpeakerListFits reports whether an existing list may stand in for a speaker
// restriction. Once a list id is filled in it REPLACES the scope and event-name
// filters, so it fails closed: a list is only accepted when its name provably
// fits. It must be a speakers list discovery would offer (never a test, staff or
// suppression list); the restriction must name at least one edition, and the
// list must name every one of them (with no editions nothing in the name can be
// checked against the event, so the pick is left to the operator); and its scope
// must match. A list whose name ends in a scope suffix (" - current",
// " - past", " - current + past") must carry exactly the requested scope; a
// current+past restriction is only met by that explicit suffix, since an
// unsuffixed name does not say which editions it covers; and a current-only or
// past-only restriction refuses a name that mentions the other scope. Anything
// else is left for the operator to pick by hand.
func SpeakerListFits(name string, scope SpeakerScope, eventNames []string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if !strings.Contains(lower, "speaker") {
		return false
	}
	if _, hidden := DiscoveryHidden(name, false); hidden {
		return false
	}
	editions := cleanValues(eventNames)
	if len(editions) == 0 {
		return false
	}
	suffixScope, suffixed := speakerScopeFromSuffix(lower)
	if suffixed && suffixScope != scope {
		return false
	}
	tokens := strings.FieldsFunc(lower, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	switch scope {
	case SpeakerScopeCurrent:
		if slices.Contains(tokens, "past") || slices.Contains(tokens, "previous") {
			return false
		}
	case SpeakerScopePast:
		if slices.Contains(tokens, "current") {
			return false
		}
	case SpeakerScopeCurrentPast:
		if !suffixed {
			return false
		}
	default:
		return false
	}
	for _, edition := range editions {
		if !strings.Contains(lower, strings.ToLower(edition)) {
			return false
		}
	}
	return true
}

// speakerScopeFromSuffix returns the scope a speaker list's name states through
// the naming convention's suffix, and whether it states one at all. lower must
// already be lower-cased and trimmed.
func speakerScopeFromSuffix(lower string) (SpeakerScope, bool) {
	for _, s := range speakerScopeSuffixes {
		if strings.HasSuffix(lower, s.suffix) {
			return s.scope, true
		}
	}
	return "", false
}

// CheckFilterKinds refuses a property filter the builder cannot express for the
// property's portal type, so preview fails instead of create sending a filter
// HubSpot rejects or that matches nobody. Every filter is built as a MULTISTRING
// or ENUMERATION operation, so a value comparison on a bool, number or date
// property is refused (is_known / is_unknown are ALL_PROPERTY and work on any
// type), and contains is refused on an enumeration, which has no such operator.
// kinds maps a property name to its portal type; a property absent from it is
// not judged here.
func CheckFilterKinds(filters []CustomPropertyFilter, kinds map[string]string) error {
	for _, f := range filters {
		if f.Operator == CustomOpKnown || f.Operator == CustomOpUnknown {
			continue
		}
		prop := strings.TrimSpace(f.Property)
		switch kind := kinds[prop]; kind {
		case "bool", "boolean", "number", "date", "datetime":
			return fmt.Errorf("%w: property %q is a %s property; only is known / is unknown can filter it here", ErrInvalidRequest, prop, kind)
		case "enumeration":
			if f.Operator == CustomOpContains {
				return fmt.Errorf("%w: property %q is a picklist; use is equal to or is any of, not contains", ErrInvalidRequest, prop)
			}
		}
	}
	return nil
}

// CustomContactProperties are the contact properties a criteria set reads, so
// the orchestration can verify each exists in the portal before anything is
// built. Event properties are not included: they belong to the event type, not
// the contact schema.
func CustomContactProperties(c CustomCriteria) []string {
	var out []string
	if c.ContactLocation {
		if len(cleanValues(c.Cities)) > 0 {
			out = append(out, propCity, propIPCity)
		}
		if len(cleanValues(c.Countries)) > 0 {
			out = append(out, propCountryDropdown, propIPCountry)
		}
	}
	for _, f := range c.PropertyFilters {
		out = append(out, strings.TrimSpace(f.Property))
	}
	if c.Ambassadors {
		out = append(out, ambassadorProperties...)
	}
	return UniqueIDs(out)
}

// ValidateCustomCriteria rejects criteria that would build something other than
// what they appear to describe. Every error wraps ErrInvalidRequest.
func ValidateCustomCriteria(c CustomCriteria) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrInvalidRequest}, args...)...)
	}
	cities, countries := cleanValues(c.Cities), cleanValues(c.Countries)
	hasLocation := len(cities) > 0 || len(countries) > 0
	topics := cleanValues(c.EducationTopics)
	if (c.ContactLocation || c.EventRegistered) && !hasLocation {
		return bad("a location is required for location or event-history branches")
	}
	if c.EducationEnrolled && !hasLocation && len(topics) == 0 {
		// Unscoped, this is every education enrolment the portal has ever recorded.
		return bad("education enrolment needs a location or course topics")
	}
	if len(topics) > 0 && !c.EducationEnrolled {
		return bad("course topics apply only with education enrolment")
	}
	if len(topics) > 0 && strings.TrimSpace(c.EducationTopicProperty) == "" {
		return bad("this portal's education event has no course or topic property, so courses cannot be filtered by topic; remove the topics")
	}
	for _, u := range cleanValues(c.PageViewURLs) {
		if len(u) < 8 || strings.ContainsAny(u, " \t") {
			// A short fragment ("kubecon") matches every page that mentions it.
			return bad("page URL %q is too short to identify one page", u)
		}
	}
	selects := c.ContactLocation || c.EventRegistered || c.EducationEnrolled || len(cleanValues(c.RegisteredFor)) > 0 ||
		len(cleanValues(c.PageViewURLs)) > 0 || c.Speakers != nil || c.Ambassadors || len(c.PropertyFilters) > 0
	if !selects {
		return bad("the audience selects nobody: choose a location, a role or a property filter")
	}
	gate := strings.TrimSpace(c.GateListID)
	if c.ContactLocation && gate == "" {
		return bad("contact-location branches must be gated by a brand master list")
	}
	if c.GateEventHistory && gate == "" {
		return bad("gating event history requires a brand master list")
	}
	for _, f := range c.PropertyFilters {
		if strings.TrimSpace(f.Property) == "" {
			return bad("a property filter needs a property")
		}
		_, takesValues, ok := f.Operator.hubspotOperator(f.Enumeration)
		if !ok {
			return bad("unknown operator %q on property %q", f.Operator, f.Property)
		}
		if takesValues && len(cleanValues(f.Values)) == 0 {
			return bad("property %q needs at least one value", f.Property)
		}
	}
	if s := c.Speakers; s != nil {
		switch s.Scope {
		case SpeakerScopeCurrent, SpeakerScopePast, SpeakerScopeCurrentPast:
		default:
			return bad("unknown speaker scope %q", s.Scope)
		}
		if strings.TrimSpace(s.ListID) == "" && len(cleanValues(s.EventNames)) == 0 {
			// Matching registration_type alone, with no event names, selects every
			// speaker of every event the portal has ever recorded.
			return bad("a speaker restriction needs an existing speakers list or the exact event names")
		}
		if strings.TrimSpace(s.ListID) == "" && len(speakerSignalFilters(s.PropertyTypes)) == 0 {
			// With no signal, the restriction would be "registered for the event" —
			// every attendee, not its speakers.
			return bad("this portal's registration event records no speaker property, so speakers cannot be selected without an existing speakers list")
		}
	}
	return nil
}

// CustomListFilter builds the filter branch for approved criteria.
//
// The shape follows the same rules as MasterListFilter: an OR root of AND
// branches, no nested ORs, IN_LIST membership filters. Restrictions (property
// filters, roles) are applied to EVERY branch, because a restriction on only some
// of them is an OR'd escape hatch around it.
//
// There is deliberately no suppression here. A custom list is an inclusion
// building block: suppressions are applied once, by the composed master or at
// send time, and baking them into the block would apply them again wherever the
// block is reused.
func CustomListFilter(c CustomCriteria) (json.RawMessage, error) {
	if err := ValidateCustomCriteria(c); err != nil {
		return nil, err
	}
	branches := customBaseBranches(c)

	extra := make([]filter, 0, len(c.PropertyFilters))
	for _, f := range c.PropertyFilters {
		extra = append(extra, customPropertyFilter(f))
	}
	for i := range branches {
		branches[i].Filters = append(branches[i].Filters, extra...)
	}

	if s := c.Speakers; s != nil {
		branches = withSpeakers(branches, s)
	}
	if c.Ambassadors {
		branches = crossWith(branches, len(ambassadorProperties), func(b *filterBranch, i int) {
			b.Filters = append(b.Filters, customPropertyFilter(CustomPropertyFilter{
				Property: ambassadorProperties[i], Operator: CustomOpKnown,
			}))
		})
	}

	if len(branches) > customBranchLimit {
		return nil, fmt.Errorf("%w: these criteria expand to %d filter branches, more than the %d HubSpot can take; narrow the roles or locations",
			ErrInvalidRequest, len(branches), customBranchLimit)
	}
	for _, b := range branches {
		if len(b.Filters) == 0 && len(b.FilterBranches) == 0 {
			return nil, fmt.Errorf("audience: custom criteria produced an empty branch")
		}
	}
	return json.Marshal(filterBranch{FilterBranchType: "OR", FilterBranches: branches, Filters: []filter{}})
}

// customBaseBranches are the location and event-history branches, or one empty
// AND branch when the audience is defined by roles or properties alone.
func customBaseBranches(c CustomCriteria) []filterBranch {
	cities, countries := cleanValues(c.Cities), cleanValues(c.Countries)
	gate := strings.TrimSpace(c.GateListID)
	var out []filterBranch

	if c.ContactLocation {
		add := func(property, op string, values []string, enumeration bool) {
			out = append(out, filterBranch{
				FilterBranchType: "AND",
				FilterBranches:   []filterBranch{},
				Filters:          []filter{valueFilter(property, op, values, enumeration), inListFilter(gate, "IN_LIST")},
			})
		}
		if len(cities) > 0 {
			add(propCity, "IS_EQUAL_TO", cities, false)
			add(propIPCity, "IS_EQUAL_TO", cities, false)
		}
		if len(countries) > 0 {
			add(propCountryDropdown, "IS_ANY_OF", countries, true)
			add(propIPCountry, "IS_EQUAL_TO", countries, false)
		}
	}

	topics := cleanValues(c.EducationTopics)
	// eventFilters are the event's own conditions; the education event also carries
	// the topic filter, so the brand fit applies to every education branch alike.
	eventFilters := func(eventTypeID string, location ...filter) []filter {
		out := append([]filter(nil), location...)
		if eventTypeID == educationEventTypeID && len(topics) > 0 {
			out = append(out, valueFilterOp(strings.TrimSpace(c.EducationTopicProperty), "CONTAINS", topics, "MULTISTRING"))
		}
		return out
	}
	history := func(eventTypeID, cityProp, countryProp string) {
		addOne := func(property string, values []string) {
			b := filterBranch{
				FilterBranchType: "AND",
				FilterBranches:   []filterBranch{eventWith(eventTypeID, eventFilters(eventTypeID, valueFilter(property, "IS_EQUAL_TO", values, false))...)},
				Filters:          []filter{},
			}
			if c.GateEventHistory {
				b.Filters = append(b.Filters, inListFilter(gate, "IN_LIST"))
			}
			out = append(out, b)
		}
		if len(cities) > 0 {
			addOne(cityProp, cities)
		}
		if len(countries) > 0 {
			addOne(countryProp, countries)
		}
	}
	if c.EventRegistered {
		history(registrationEventTypeID, propEventCity, propEventCountry)
	}
	if c.EducationEnrolled {
		if len(cities) > 0 || len(countries) > 0 {
			history(educationEventTypeID, propUserCity, propUserCountry)
		} else {
			b := filterBranch{
				FilterBranchType: "AND",
				FilterBranches:   []filterBranch{eventWith(educationEventTypeID, eventFilters(educationEventTypeID)...)},
				Filters:          []filter{},
			}
			if c.GateEventHistory {
				b.Filters = append(b.Filters, inListFilter(gate, "IN_LIST"))
			}
			out = append(out, b)
		}
	}
	noTracking := false
	for _, u := range cleanValues(c.PageViewURLs) {
		out = append(out, filterBranch{
			FilterBranchType: "AND",
			FilterBranches:   []filterBranch{},
			Filters:          []filter{{FilterType: "PAGE_VIEW", Operator: "HAS_PAGEVIEW_CONTAINS", PageURL: u, EnableTracking: &noTracking}},
		})
	}
	if names := cleanValues(c.RegisteredFor); len(names) > 0 {
		out = append(out, filterBranch{
			FilterBranchType: "AND",
			FilterBranches:   []filterBranch{eventWith(registrationEventTypeID, valueFilter("event_name", "IS_EQUAL_TO", names, false))},
			Filters:          []filter{},
		})
	}

	if len(out) == 0 {
		out = append(out, filterBranch{FilterBranchType: "AND", FilterBranches: []filterBranch{}, Filters: []filter{}})
	}
	return out
}

// withSpeakers applies the speaker restriction. An existing list is one IN_LIST
// per branch; otherwise each branch is copied once per speaker signal, because
// events record a speaker under any of three properties and HubSpot will not
// accept an OR nested inside the branch to say "any of them".
func withSpeakers(branches []filterBranch, s *CustomSpeakers) []filterBranch {
	if id := strings.TrimSpace(s.ListID); id != "" {
		for i := range branches {
			branches[i].Filters = append(branches[i].Filters, inListFilter(id, "IN_LIST"))
		}
		return branches
	}
	names := cleanValues(s.EventNames)
	signals := speakerSignalFilters(s.PropertyTypes)
	return crossWith(branches, len(signals), func(b *filterBranch, i int) {
		b.FilterBranches = append(b.FilterBranches, eventWith(registrationEventTypeID,
			// IS_EQUAL_TO over several values ("any of"), never CONTAINS: each value is a
			// full edition name, and MULTISTRING has no IS_ANY_OF.
			valueFilter("event_name", "IS_EQUAL_TO", names, false),
			signals[i],
		))
	})
}

// crossWith copies every branch n times, letting apply specialise copy i. Each
// copy gets its own slices so appending to one cannot alias another.
func crossWith(branches []filterBranch, n int, apply func(b *filterBranch, i int)) []filterBranch {
	out := make([]filterBranch, 0, len(branches)*n)
	for _, base := range branches {
		for i := 0; i < n; i++ {
			b := filterBranch{
				FilterBranchType: base.FilterBranchType,
				FilterBranches:   slices.Clone(base.FilterBranches),
				Filters:          slices.Clone(base.Filters),
			}
			apply(&b, i)
			out = append(out, b)
		}
	}
	return out
}

// eventWith is a UNIFIED_EVENTS completion qualified by the event's OWN
// properties. They go in the node's own `filters`: HubSpot's list-filter
// reference says a UNIFIED_EVENTS branch "cannot have any additional filter
// branches", and the reference Seoul lists carry their event_city filter there.
// (A sibling property filter in the parent AND would instead read a CONTACT
// property of that name, which does not exist.)
func eventWith(eventTypeID string, filters ...filter) filterBranch {
	if filters == nil {
		filters = []filter{}
	}
	return filterBranch{
		FilterBranchType: "UNIFIED_EVENTS",
		Operator:         "HAS_COMPLETED",
		EventTypeID:      eventTypeID,
		FilterBranches:   []filterBranch{},
		Filters:          filters,
	}
}

func customPropertyFilter(f CustomPropertyFilter) filter {
	op, takesValues, _ := f.Operator.hubspotOperator(f.Enumeration)
	opType := f.Operator.operationTypeFor(f.Enumeration)
	var values []string
	if takesValues {
		values = cleanValues(f.Values)
	}
	return valueFilterOp(strings.TrimSpace(f.Property), op, values, opType)
}

func valueFilter(property, op string, values []string, enumeration bool) filter {
	opType := "MULTISTRING"
	if enumeration {
		opType = "ENUMERATION"
	}
	return valueFilterOp(property, op, values, opType)
}

func valueFilterOp(property, op string, values []string, opType string) filter {
	return filter{
		FilterType: "PROPERTY",
		Property:   property,
		Operation: operation{
			Operator:                     op,
			IncludeObjectsWithNoValueSet: false,
			Values:                       values,
			OperationType:                opType,
		},
	}
}

// cleanValues trims, drops blanks and de-duplicates case-insensitively.
func cleanValues(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.Join(strings.Fields(v), " ")
		key := strings.ToLower(v)
		if v == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, v)
	}
	return out
}

// CustomListName is the name a custom list is created under.
//
// Speaker-only lists follow the speaker naming ("<Event> Speakers - Current");
// everything else follows the reference pattern
// "<YYQN> - <Location> - Mailable Contacts - <Label>".
func CustomListName(c CustomCriteria, now time.Time) string {
	label := strings.Join(strings.Fields(c.Label), " ")
	hasLocation := len(cleanValues(c.Cities)) > 0 || len(cleanValues(c.Countries)) > 0
	noLocation := !hasLocation || (!c.ContactLocation && !c.EventRegistered && !c.EducationEnrolled)
	if noLocation && label != "" {
		switch {
		case c.Speakers != nil:
			return label + " Speakers - " + speakerScopeLabel(c.Speakers.Scope)
		case len(cleanValues(c.RegisteredFor)) > 0:
			return QuarterCode(now) + " - " + label + " - Event Registration"
		case c.EducationEnrolled:
			return QuarterCode(now) + " - " + label + " - Education Enrolled"
		case len(cleanValues(c.PageViewURLs)) > 0:
			return QuarterCode(now) + " - " + label + " - Web Visitors"
		}
	}
	location := strings.Join(append(cleanValues(c.Cities), cleanValues(c.Countries)...), " and ")
	parts := []string{QuarterCode(now)}
	if location != "" {
		parts = append(parts, location)
	}
	kind := "Mailable Contacts"
	switch {
	case c.Speakers != nil:
		kind = "Speakers - " + speakerScopeLabel(c.Speakers.Scope)
	case c.Ambassadors:
		kind = "Community Ambassadors"
	case noLocation:
		kind = "Custom Audience"
	}
	parts = append(parts, kind)
	if label != "" {
		parts = append(parts, label)
	}
	return strings.Join(parts, " - ")
}

func speakerScopeLabel(s SpeakerScope) string {
	switch s {
	case SpeakerScopeCurrent:
		return "Current"
	case SpeakerScopePast:
		return "Past"
	}
	return "Current + Past"
}

// VersionedName is the n-th version of a name that is already taken: "(v2)",
// "(v3)" and so on. A taken name is never overwritten — matching a name does not
// prove the existing list is the same audience.
func VersionedName(name string, n int) string {
	return fmt.Sprintf("%s (v%d)", strings.TrimSpace(name), n)
}

// ---------------------------------------------------------------------------
// Results
// ---------------------------------------------------------------------------

// CustomInterpretOutcome is a draft plus the portal lists the orchestration
// resolved for it. Gate and Speakers are SUGGESTIONS already filled into the
// draft's criteria; the operator can change either before preview.
type CustomInterpretOutcome struct {
	Draft    CustomDraft
	Gate     *ListRow
	Speakers *ListRow
	// Similar are existing lists that may already be this audience, ranked by
	// the model. Found by deterministic search; the model never adds one.
	Similar []CustomSimilarList
	// Event is the event an event audience was planned from (name, brand, dates),
	// so the caller can QA against it; nil for a plain custom request.
	Event *EventIdentity
}

// CustomPreview is what a create would build, computed without writing.
type CustomPreview struct {
	Name string
	// Criteria are the input with property types resolved from the portal.
	Criteria    CustomCriteria
	BranchCount int
	// EducationTopicCandidates are the education event's properties that could
	// hold a course topic, best first — the operator may pick another.
	EducationTopicCandidates []string
	Gate                     *ListRow
	// UpperBound is the gate list's size when every branch is gated, so the
	// list can only be a subset of it; nil when some branch is ungated.
	UpperBound *int
	Warnings   []string
}

// ErrCustomInterpretUnavailable means this deployment has no model configured, so
// a free-text request cannot be interpreted. The form still works without it.
var ErrCustomInterpretUnavailable = errors.New("audience: custom-audience interpretation is not configured")

// UnknownPropertiesError names every contact property the portal does not define.
type UnknownPropertiesError struct{ Names []string }

func (e *UnknownPropertiesError) Error() string {
	return fmt.Sprintf("%v: the portal has no contact propert%s %s", ErrInvalidRequest,
		map[bool]string{true: "y", false: "ies"}[len(e.Names) == 1], strings.Join(e.Names, ", "))
}

func (e *UnknownPropertiesError) Unwrap() error { return ErrInvalidRequest }

// AllBranchesGated reports whether every branch carries the gate list, so the
// gate's size bounds the audience.
func AllBranchesGated(c CustomCriteria) bool {
	if strings.TrimSpace(c.GateListID) == "" {
		return false
	}
	if len(cleanValues(c.RegisteredFor)) > 0 || len(cleanValues(c.PageViewURLs)) > 0 {
		return false
	}
	if c.EventRegistered || c.EducationEnrolled {
		return c.GateEventHistory
	}
	return c.ContactLocation
}

// brandAliases are the long names brand master lists are filed under where they
// differ from the short brand token.
var brandAliases = map[string][]string{
	"lf":         {"the linux foundation", "linux foundation"},
	"opensearch": {"opensearch foundation"},
}

// BrandMasterProbes are the searches for a brand's standing master list, e.g.
// "26Q2 - The Linux Foundation - Master-list".
func BrandMasterProbes(brandShort string) []string {
	brand := strings.TrimSpace(brandShort)
	if brand == "" {
		return nil
	}
	probes := []string{brand + " Master-list"}
	for _, alias := range brandAliases[strings.ToLower(brand)] {
		probes = append(probes, alias+" Master-list")
	}
	return probes
}

// MatchesBrandMaster requires the brand (as a whole token, or a known alias) and
// "master-list"/"master list" in the name, and no suppression word.
//
// "master list" rather than "master" is what separates the brand's standing
// audience from per-event masters ("26Q3 - CNCF - KubeCon Japan - Master"),
// which are a single event's audience and the wrong gate for a location list.
func MatchesBrandMaster(name, brandShort string) bool {
	lower := strings.ToLower(name)
	if !strings.Contains(lower, "master-list") && !strings.Contains(lower, "master list") && !strings.Contains(lower, "masterlist") {
		return false
	}
	if containsAny(name, suppressionNameHints) {
		return false
	}
	brand := strings.ToLower(strings.TrimSpace(brandShort))
	if brand == "" {
		return false
	}
	tokens := strings.FieldsFunc(lower, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	if slices.Contains(tokens, brand) {
		return true
	}
	for _, alias := range brandAliases[brand] {
		if strings.Contains(lower, alias) {
			return true
		}
	}
	return false
}

// courseTopicHints are the words a course/topic property's name or label carries,
// in preference order — the reference runbook's investigation, made deterministic.
var courseTopicHints = []string{"course", "topic", "subject", "program", "curriculum"}

// courseIdentifierHints mark a property that holds an identifier rather than
// words. A CONTAINS on a course CODE ("LFS258") never matches a topic keyword,
// so such a property is a last resort, not a match. Found against the live
// portal: a hint-only pick chose `course_code`.
var courseIdentifierHints = []string{"code", "_id", "id_", "number", "sku", "date", "url"}

// courseWordHints mark a property that holds readable words.
var courseWordHints = []string{"name", "title", "topic", "subject", "label", "description", "category"}

// CourseTopicCandidates ranks the education event properties that may hold a
// course or topic name, best first. names maps property name to label.
//
// Ranking: a word property (name/title/...) beats a plain one, which beats an
// identifier (code/id/...); then the hint order; then the name, so the order is
// stable for one definition.
func CourseTopicCandidates(names map[string]string) []string {
	type scored struct {
		name       string
		kind, hint int
	}
	var out []scored
	for name, label := range names {
		text := strings.ToLower(name + " " + label)
		hint := -1
		for i, h := range courseTopicHints {
			if strings.Contains(text, h) {
				hint = i
				break
			}
		}
		if hint < 0 {
			continue
		}
		kind := 1
		switch {
		case containsAny(strings.ToLower(name), courseIdentifierHints):
			kind = 2
		case containsAny(text, courseWordHints):
			kind = 0
		}
		out = append(out, scored{name, kind, hint})
	}
	slices.SortFunc(out, func(a, b scored) int {
		if a.kind != b.kind {
			return a.kind - b.kind
		}
		if a.hint != b.hint {
			return a.hint - b.hint
		}
		return strings.Compare(a.name, b.name)
	})
	ranked := make([]string, 0, len(out))
	for _, s := range out {
		ranked = append(ranked, s.name)
	}
	return ranked
}

// CourseTopicProperty is the best course/topic property, or "" when none qualifies.
func CourseTopicProperty(names map[string]string) string {
	if c := CourseTopicCandidates(names); len(c) > 0 {
		return c[0]
	}
	return ""
}

// RegistrationEventTypeID exposes the portal-wide registration event type for the
// orchestration's speaker-property lookup.
const RegistrationEventTypeID = registrationEventTypeID

// EducationEventTypeID exposes the portal-wide education event type for the
// orchestration's topic-property lookup.
const EducationEventTypeID = educationEventTypeID
