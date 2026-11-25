// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package audience

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// ---------------------------------------------------------------------------
// Plain-English list descriptions
//
// An operator choosing who an email goes to sees list NAMES, and a name is a claim about a
// list, not a description of it: "KubeCon NA 2025 Registrants" may hold registrants of two
// editions, or exclude a region, or have been edited since it was named. What the list really
// contains is its HubSpot filter tree, so this renders that tree, deterministically, into
// sentences. No model is involved: every sentence says what one filter does, so the
// description can be trusted to the same degree as the filter.
//
// HubSpot's filter schema is a large union that grows without notice (see the read-side note in
// builder_types.go). A rule this code does not model is still listed -- as "a HubSpot
// FORM_SUBMISSION rule" -- and marks the description incomplete, rather than being dropped:
// a description that silently omits a rule overstates who is in the list.
// ---------------------------------------------------------------------------

// Description statuses: which outcome a ListDescription reports.
const (
	DescribeStatusDescribed  = "described"
	DescribeStatusStatic     = "static"
	DescribeStatusNotFound   = "not_found"
	DescribeStatusUnreadable = "unreadable"
)

// How a description's groups combine.
const (
	DescribeMatchAny = "any"
	DescribeMatchAll = "all"
)

// DescribeMaxLists bounds one describe request: one GetList per id plus the lookups their rules
// need. Mirrors the design's MaxLength on list_ids.
const DescribeMaxLists = 25

// describeMaxValues is how many values one condition spells out before summarising the rest.
const describeMaxValues = 4

// ListDescription is a plain-English reading of one contact list.
type ListDescription struct {
	ListID         string
	Name           string
	Size           int // -1 when HubSpot reported no size
	ProcessingType string
	HubSpotURL     string
	Status         string
	Summary        string
	Match          string
	Groups         []RuleGroup
	Complete       bool
}

// RuleGroup is one group of a list's rules; a contact is in the group when every condition holds.
type RuleGroup struct {
	Conditions []string
}

// PropertyLabel is a property's display label and, for an enumeration, its options' labels
// keyed by stored value.
type PropertyLabel struct {
	Label   string
	Options map[string]string
}

// DescribeLabels names what the renderer would otherwise show as an id. Every map may be nil or
// incomplete: a missing entry falls back to the id or a humanised internal name, never to an
// error, because a lookup budget or a failed read must not stop a list being described.
type DescribeLabels struct {
	// Lists maps a list id to its name.
	Lists map[string]string
	// Properties maps a lower-cased CONTACT property name to its label.
	Properties map[string]PropertyLabel
	// Events maps an event type id (e.g. "6-48984571") to its label.
	Events map[string]string
	// Subscriptions maps an email subscription type id to its name.
	Subscriptions map[string]string
}

// DescribeRefs is what a filter tree references that DescribeLabels could name.
type DescribeRefs struct {
	ListIDs         []string
	Properties      []string // contact properties, lower-cased
	EventTypeIDs    []string
	SubscriptionIDs []string
}

// KnownEventLabels names the two portal-wide event types the audience builder writes, so the
// commonest lists need no event-definition lookup at all.
func KnownEventLabels() map[string]string {
	return map[string]string{
		registrationEventTypeID: "event registration",
		educationEventTypeID:    "education enrollment",
	}
}

// describeBranch is the filter tree as the describer reads it: more fields than the classifier's
// ListFilterBranch, because a description has to say what each rule does, not only what kind
// of rule it is. Still loosely typed for the reason builder_types.go gives.
type describeBranch struct {
	FilterBranchType   string           `json:"filterBranchType"`
	FilterBranches     []describeBranch `json:"filterBranches"`
	Filters            []describeFilter `json:"filters"`
	Operator           string           `json:"operator"`
	EventTypeID        string           `json:"eventTypeId"`
	ObjectTypeID       string           `json:"objectTypeId"`
	CoalescingRefineBy json.RawMessage  `json:"coalescingRefineBy"`
	PruningRefineBy    json.RawMessage  `json:"pruningRefineBy"`
}

type describeFilter struct {
	FilterType       string             `json:"filterType"`
	Operator         string             `json:"operator"`
	Property         string             `json:"property"`
	PropertyName     string             `json:"propertyName"`
	ListID           json.Number        `json:"listId"`
	PageURL          string             `json:"pageUrl"`
	FormID           string             `json:"formId"`
	EmailID          json.RawMessage    `json:"emailId"`
	SubscriptionIDs  json.RawMessage    `json:"subscriptionIds"`
	AcceptedStatuses []string           `json:"acceptedStatuses"`
	SubscriptionType string             `json:"subscriptionType"`
	Channel          string             `json:"channel"`
	ShouldAccept     *bool              `json:"shouldAccept"`
	PrivacyName      string             `json:"privacyName"`
	Value            json.RawMessage    `json:"value"`
	Values           json.RawMessage    `json:"values"`
	Operation        *describeOperation `json:"operation"`
}

type describeOperation struct {
	OperationType string          `json:"operationType"`
	Operator      string          `json:"operator"`
	Value         json.RawMessage `json:"value"`
	Values        json.RawMessage `json:"values"`
	LowerBound    json.RawMessage `json:"lowerBound"`
	UpperBound    json.RawMessage `json:"upperBound"`
	Timestamp     json.RawMessage `json:"timestamp"`
}

// parseDescribeBranch decodes a raw filterBranch; absent or `{}` is (nil, nil) -- no rules.
func parseDescribeBranch(raw json.RawMessage) (*describeBranch, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" || trimmed == "{}" {
		return nil, nil
	}
	var b describeBranch
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, err
	}
	if strings.TrimSpace(b.FilterBranchType) == "" && len(b.Filters) == 0 && len(b.FilterBranches) == 0 {
		return nil, nil
	}
	return &b, nil
}

// CollectDescribeRefs lists what a filter tree references: list ids, contact properties and
// event types, each once and in first-seen order. An unparseable tree references nothing.
func CollectDescribeRefs(raw json.RawMessage) DescribeRefs {
	root, err := parseDescribeBranch(raw)
	if err != nil || root == nil {
		return DescribeRefs{}
	}
	var refs DescribeRefs
	seenList, seenProp, seenEvent, seenSub := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	var walk func(b *describeBranch, inEvent bool)
	walk = func(b *describeBranch, inEvent bool) {
		branchIsEvent := strings.EqualFold(strings.TrimSpace(b.FilterBranchType), "UNIFIED_EVENTS")
		if branchIsEvent {
			if id := strings.TrimSpace(b.EventTypeID); id != "" && !seenEvent[id] {
				seenEvent[id] = true
				refs.EventTypeIDs = append(refs.EventTypeIDs, id)
			}
		}
		// Properties under an event branch or an association branch belong to that object, not
		// to the contact, so they are not contact-property lookups.
		scoped := inEvent || branchIsEvent || isAssociationBranch(b)
		for _, f := range b.Filters {
			if id := strings.TrimSpace(f.ListID.String()); id != "" && strings.EqualFold(f.FilterType, "IN_LIST") && !seenList[id] {
				seenList[id] = true
				refs.ListIDs = append(refs.ListIDs, id)
			}
			for _, id := range rawStrings(f.SubscriptionIDs) {
				if !seenSub[id] {
					seenSub[id] = true
					refs.SubscriptionIDs = append(refs.SubscriptionIDs, id)
				}
			}
			if scoped || !strings.EqualFold(f.FilterType, "PROPERTY") {
				continue
			}
			if p := propertyKey(f); p != "" && !seenProp[p] {
				seenProp[p] = true
				refs.Properties = append(refs.Properties, p)
			}
		}
		for i := range b.FilterBranches {
			walk(&b.FilterBranches[i], scoped)
		}
	}
	walk(root, false)
	return refs
}

// DescribeRules renders a raw filter tree into groups. A tree with no rules returns no groups;
// an unparseable one returns the error so the caller can say the list could not be described.
func DescribeRules(raw json.RawMessage, labels DescribeLabels) (match string, groups []RuleGroup, complete bool, err error) {
	root, perr := parseDescribeBranch(raw)
	if perr != nil {
		return DescribeMatchAny, nil, false, perr
	}
	if root == nil {
		return DescribeMatchAny, []RuleGroup{}, true, nil
	}
	r := &describer{labels: labels, complete: true}
	match = DescribeMatchAny
	switch strings.ToUpper(strings.TrimSpace(root.FilterBranchType)) {
	case "OR":
		// The shape HubSpot requires for every ILS list: an OR root whose children are AND
		// branches. Each child is one group; a bare filter on the root is a group of one.
		for _, f := range root.Filters {
			groups = append(groups, RuleGroup{Conditions: []string{r.filter(f, scopeContact)}})
		}
		for i := range root.FilterBranches {
			if conds := r.conditions(&root.FilterBranches[i], scopeContact); len(conds) > 0 {
				groups = append(groups, RuleGroup{Conditions: conds})
			}
		}
	case "AND", "":
		// HubSpot refuses AND roots on write, but a tree read back is not ours to assume.
		match = DescribeMatchAll
		if conds := r.conditions(root, scopeContact); len(conds) > 0 {
			groups = append(groups, RuleGroup{Conditions: conds})
		}
	default:
		groups = append(groups, RuleGroup{Conditions: []string{r.branch(root, scopeContact)}})
	}
	if groups == nil {
		groups = []RuleGroup{}
	}
	return match, groups, r.complete, nil
}

// DescribeList builds a list's whole description from what GetList returned.
func DescribeList(listID, name string, size int, processingType, hubspotURL string, filterBranch json.RawMessage, labels DescribeLabels) ListDescription {
	d := ListDescription{
		ListID: listID, Name: name, Size: size, ProcessingType: processingType, HubSpotURL: hubspotURL,
		Match: DescribeMatchAny, Groups: []RuleGroup{}, Complete: true,
	}
	pt := strings.ToUpper(strings.TrimSpace(processingType))
	match, groups, complete, err := DescribeRules(filterBranch, labels)
	switch {
	case err != nil:
		d.Status = DescribeStatusUnreadable
		d.Complete = false
		d.Summary = "This list's rules could not be read, so who it contains cannot be described here. Open it in HubSpot to check."
	case len(groups) == 0 && (pt == "MANUAL" || pt == "SNAPSHOT" || pt == ""):
		d.Status = DescribeStatusStatic
		d.Summary = staticSummary(pt)
	case len(groups) == 0:
		// A DYNAMIC list with no rules matches nobody; saying so is the useful answer.
		d.Status = DescribeStatusDescribed
		d.Summary = "An active list with no rules, so it matches no contacts."
	default:
		d.Status = DescribeStatusDescribed
		d.Match, d.Groups, d.Complete = match, groups, complete
		d.Summary = rulesSummary(match, groups)
	}
	return d
}

// UnreadableListDescription is the description of a list that could not be fetched at all.
func UnreadableListDescription(listID string, notFound bool) ListDescription {
	d := ListDescription{ListID: listID, Size: -1, Match: DescribeMatchAny, Groups: []RuleGroup{}, Complete: false}
	if notFound {
		d.Status = DescribeStatusNotFound
		d.Summary = "HubSpot has no list with this id. It may have been deleted."
		return d
	}
	d.Status = DescribeStatusUnreadable
	d.Summary = "This list could not be read from HubSpot just now, so who it contains cannot be described. It may still exist."
	return d
}

func staticSummary(processingType string) string {
	if processingType == "SNAPSHOT" {
		return "A snapshot list: its members were fixed when it was created, so it has no rules to describe."
	}
	return "A static list: contacts were added by hand or by import, so it has no rules to describe."
}

// rulesSummary is one sentence on a described list: the rule when there is only one, otherwise
// how many groups there are and what kinds of rule they use.
func rulesSummary(match string, groups []RuleGroup) string {
	if len(groups) == 1 && len(groups[0].Conditions) == 1 {
		return "Contacts matching one rule: " + lowerFirst(groups[0].Conditions[0]) + "."
	}
	kinds := ruleKinds(groups)
	var lead string
	switch {
	case len(groups) == 1:
		lead = fmt.Sprintf("Contacts matching all %d conditions", len(groups[0].Conditions))
	case match == DescribeMatchAll:
		lead = fmt.Sprintf("Contacts matching every one of %d groups of rules", len(groups))
	default:
		lead = fmt.Sprintf("Contacts matching any of %d groups of rules", len(groups))
	}
	if len(kinds) == 0 {
		return lead + "."
	}
	return lead + ", based on " + joinWords(kinds, "and") + "."
}

// ruleKinds names the kinds of rule a description uses, in a fixed order, from phrases this
// file writes -- so the summary cannot drift from the conditions it summarises. A nested
// condition ("None of: ...") can carry several kinds and contributes each one; a condition
// matching none of them is a contact-property rule.
func ruleKinds(groups []RuleGroup) []string {
	order := []struct{ kind, phrase string }{
		{"event registrations", "registered for an event"},
		{"education enrollments", "enrolled in a course"},
		{"custom events", "completed the "},
		{"page views", "viewed the page"},
		{"page views", "viewed a page"},
		{"page views", "viewed a tracked page"},
		{"list membership", "member of "},
		{"form submissions", "submitted form"},
		{"form submissions", "submitted a form"},
		{"email subscriptions", "subscription status"},
		{"email subscriptions", "opted in to"},
		{"email subscriptions", "opted out of"},
		{"email engagement", "marketing email"},
		{"associated records", "has an associated"},
		{"privacy consent", "privacy consent"},
	}
	seen := map[string]bool{}
	var kinds []string
	hasProperty := false
	for _, g := range groups {
		for _, c := range g.Conditions {
			lc := strings.ToLower(c)
			matched := false
			for _, o := range order {
				if !strings.Contains(lc, o.phrase) {
					continue
				}
				matched = true
				if !seen[o.kind] {
					seen[o.kind] = true
					kinds = append(kinds, o.kind)
				}
			}
			if !matched {
				hasProperty = true
			}
		}
	}
	if hasProperty {
		kinds = append(kinds, "contact details")
	}
	return kinds
}

// Scopes a property condition can belong to.
type describeScope int

const (
	scopeContact describeScope = iota
	scopeEvent
	scopeAssociated
)

type describer struct {
	labels   DescribeLabels
	complete bool
}

// conditions renders an AND-like branch into its conditions: one per filter, one per nested
// branch.
func (r *describer) conditions(b *describeBranch, scope describeScope) []string {
	t := strings.ToUpper(strings.TrimSpace(b.FilterBranchType))
	if t != "AND" && t != "" {
		return []string{r.branch(b, scope)}
	}
	out := make([]string, 0, len(b.Filters)+len(b.FilterBranches))
	for _, f := range b.Filters {
		out = append(out, r.filter(f, scope))
	}
	for i := range b.FilterBranches {
		out = append(out, r.branch(&b.FilterBranches[i], scope))
	}
	return out
}

// branch renders a nested branch as ONE condition.
func (r *describer) branch(b *describeBranch, scope describeScope) string {
	switch t := strings.ToUpper(strings.TrimSpace(b.FilterBranchType)); {
	case t == "UNIFIED_EVENTS":
		return r.eventBranch(b)
	case t == "AND" || t == "":
		return joinWords(r.conditions(b, scope), "and")
	case t == "OR":
		return "Any of: " + strings.Join(r.children(b, scope), "; or ")
	case t == "NOT_ANY":
		return "None of: " + strings.Join(r.children(b, scope), "; ")
	case t == "NOT_ALL":
		return "Not all of: " + strings.Join(r.children(b, scope), "; ")
	case isAssociationBranch(b):
		conds := r.conditions(&describeBranch{Filters: b.Filters, FilterBranches: b.FilterBranches}, scopeAssociated)
		object := objectTypeName(b.ObjectTypeID)
		if len(conds) == 0 {
			return "Has an associated " + object
		}
		return "Has an associated " + object + " where " + lowerFirst(joinWords(conds, "and"))
	default:
		r.complete = false
		return "Matches a HubSpot " + humanize(t) + " rule"
	}
}

// children renders every filter and nested branch of b as separate conditions.
func (r *describer) children(b *describeBranch, scope describeScope) []string {
	out := make([]string, 0, len(b.Filters)+len(b.FilterBranches))
	for _, f := range b.Filters {
		out = append(out, r.filter(f, scope))
	}
	for i := range b.FilterBranches {
		out = append(out, r.branch(&b.FilterBranches[i], scope))
	}
	return out
}

// eventBranch renders a UNIFIED_EVENTS branch: an event a contact has (or has not) completed,
// narrowed by the event's own properties.
func (r *describer) eventBranch(b *describeBranch) string {
	negated := strings.Contains(strings.ToUpper(b.Operator), "NOT")
	id := strings.TrimSpace(b.EventTypeID)
	var sentence string
	switch id {
	case registrationEventTypeID:
		sentence = "Registered for an event"
		if negated {
			sentence = "Has not registered for an event"
		}
	case educationEventTypeID:
		sentence = "Enrolled in a course"
		if negated {
			sentence = "Has not enrolled in a course"
		}
	default:
		label := r.labels.Events[id]
		if label == "" {
			label = "custom event " + id
		}
		sentence = "Completed the " + label + " event"
		if negated {
			sentence = "Has not completed the " + label + " event"
		}
	}
	conds := make([]string, 0, len(b.Filters)+len(b.FilterBranches))
	for _, f := range b.Filters {
		conds = append(conds, lowerFirst(r.filter(f, scopeEvent)))
	}
	for i := range b.FilterBranches {
		conds = append(conds, lowerFirst(r.branch(&b.FilterBranches[i], scopeEvent)))
	}
	if len(conds) > 0 {
		sentence += " where " + joinWords(conds, "and")
	}
	if hasRefinement(b.CoalescingRefineBy) || hasRefinement(b.PruningRefineBy) {
		// A count or time window narrows the event. Its shapes vary too much to render
		// faithfully, so it is named rather than guessed, and the description is partial.
		r.complete = false
		sentence += " (limited by a time window or a minimum count)"
	}
	return sentence
}

// filter renders one leaf filter.
func (r *describer) filter(f describeFilter, scope describeScope) string {
	switch t := strings.ToUpper(strings.TrimSpace(f.FilterType)); t {
	case "PROPERTY":
		return r.property(f, scope)
	case "IN_LIST":
		name := r.listName(f.ListID.String())
		if strings.Contains(strings.ToUpper(f.Operator), "NOT") {
			return "Is not a member of " + name
		}
		return "Is a member of " + name
	case "PAGE_VIEW":
		return pageView(f)
	case "CONSTANT":
		if f.ShouldAccept != nil && !*f.ShouldAccept {
			return "No contact (an always-false rule)"
		}
		return "Every contact (an always-true rule)"
	case "FORM_SUBMISSION", "FORM_SUBMISSION_ON_PAGE":
		form := "a form"
		if id := strings.TrimSpace(f.FormID); id != "" {
			form = "form " + id
		}
		if strings.Contains(strings.ToUpper(f.Operator), "NOT") {
			return "Has not submitted " + form
		}
		return "Submitted " + form
	case "EMAIL_SUBSCRIPTION", "COMMUNICATION_SUBSCRIPTION":
		return r.subscription(f)
	case "EMAIL_EVENT":
		action := humanize(f.Operator)
		if action == "" {
			action = "interacted with"
		}
		email := "a marketing email"
		if ids := rawStrings(f.EmailID); len(ids) > 0 {
			email = "marketing email " + ids[0]
		}
		return "Has " + action + " " + email
	case "PRIVACY":
		return "Privacy consent: " + humanize(f.PrivacyName) + " " + humanize(f.Operator)
	default:
		r.complete = false
		if t == "" {
			return "Matches a HubSpot rule this summary cannot read"
		}
		return "Matches a HubSpot " + humanize(t) + " rule"
	}
}

// subscription renders a subscription-status filter in the words an operator uses: opted in to,
// opted out of, or neither, for the subscription types it names.
func (r *describer) subscription(f describeFilter) string {
	ids := rawStrings(f.SubscriptionIDs)
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		if name := strings.TrimSpace(r.labels.Subscriptions[id]); name != "" {
			names = append(names, quote(name))
			continue
		}
		names = append(names, "subscription "+id)
	}
	what := "an email subscription"
	if len(names) > 0 {
		what = joinWords(names, "or")
	}
	statuses := map[string]bool{}
	for _, s := range f.AcceptedStatuses {
		statuses[strings.ToUpper(strings.TrimSpace(s))] = true
	}
	switch {
	case len(statuses) == 0 || (len(statuses) == 1 && statuses["OPT_IN"]):
		return "Is opted in to " + what
	case len(statuses) == 1 && statuses["OPT_OUT"]:
		return "Has opted out of " + what
	case len(statuses) == 1 && statuses["NOT_OPTED"]:
		return "Has neither opted in to nor out of " + what
	default:
		return "Has subscription status " + strings.Join(humanizeAll(f.AcceptedStatuses), " or ") + " for " + what
	}
}

// property renders a property filter: "<label> is “a”, “b”".
func (r *describer) property(f describeFilter, scope describeScope) string {
	key := propertyKey(f)
	label, options := r.propertyLabel(key, scope)

	operator := strings.ToUpper(strings.TrimSpace(f.Operator))
	opType := ""
	var values []string
	var bound struct{ lower, upper string }
	if op := f.Operation; op != nil {
		if o := strings.ToUpper(strings.TrimSpace(op.Operator)); o != "" {
			operator = o
		}
		opType = strings.ToUpper(strings.TrimSpace(op.OperationType))
		values = append(rawStrings(op.Values), rawStrings(op.Value)...)
		bound.lower, bound.upper = firstRawString(op.LowerBound), firstRawString(op.UpperBound)
		if ts := firstRawString(op.Timestamp); ts != "" {
			values = append(values, ts)
		}
	}
	values = append(values, rawStrings(f.Values)...)
	values = append(values, rawStrings(f.Value)...)
	values = mapOptionLabels(values, options)
	if opType == "TIME_POINT" || opType == "TIME_RANGED" || opType == "DATETIME" || opType == "DATE" {
		values = formatTimeValues(values)
		bound.lower, bound.upper = formatTimeValue(bound.lower), formatTimeValue(bound.upper)
	}

	switch operator {
	case "IS_KNOWN", "HAS_PROPERTY":
		return label + " is known"
	case "IS_UNKNOWN", "IS_NOT_KNOWN", "NOT_HAS_PROPERTY":
		return label + " is unknown"
	case "IS_EQUAL_TO", "IS_EXACTLY", "EQ":
		if opType == "BOOL" && len(values) == 1 {
			return label + " is " + boolWord(values[0])
		}
		if len(values) > 1 {
			return label + " is any of " + listValues(values, true)
		}
		return label + " is " + listValues(values, true)
	case "IS_NOT_EQUAL_TO", "IS_NOT_EXACTLY", "NEQ":
		if opType == "BOOL" && len(values) == 1 {
			return label + " is not " + boolWord(values[0])
		}
		if len(values) > 1 {
			return label + " is none of " + listValues(values, true)
		}
		return label + " is not " + listValues(values, true)
	case "IS_ANY_OF", "IS_ANY_OF_VALUES":
		if len(values) == 1 {
			return label + " is " + listValues(values, true)
		}
		return label + " is any of " + listValues(values, true)
	case "IS_NONE_OF", "IS_NOT_ANY_OF":
		if len(values) == 1 {
			return label + " is not " + listValues(values, true)
		}
		return label + " is none of " + listValues(values, true)
	case "CONTAINS", "CONTAINS_ANY":
		if len(values) > 1 {
			return label + " contains any of " + listValues(values, true)
		}
		return label + " contains " + listValues(values, true)
	case "DOES_NOT_CONTAIN", "DOES_NOT_CONTAIN_ANY":
		return label + " does not contain " + listValues(values, true)
	case "CONTAINS_ALL", "IS_ALL_OF":
		return label + " contains all of " + listValues(values, true)
	case "CONTAINS_EXACTLY":
		return label + " contains exactly " + listValues(values, true)
	case "DOES_NOT_CONTAIN_EXACTLY":
		return label + " does not contain exactly " + listValues(values, true)
	case "STARTS_WITH":
		return label + " starts with " + listValues(values, true)
	case "ENDS_WITH":
		return label + " ends with " + listValues(values, true)
	case "HAS_EVER_BEEN_EQUAL_TO", "HAS_EVER_BEEN_ANY_OF", "HAS_EVER_BEEN_EXACTLY":
		return label + " has ever been " + listValues(values, true)
	case "HAS_NEVER_BEEN_EQUAL_TO", "HAS_NEVER_BEEN_ANY_OF", "HAS_NEVER_BEEN_EXACTLY":
		return label + " has never been " + listValues(values, true)
	case "HAS_EVER_CONTAINED":
		return label + " has ever contained " + listValues(values, true)
	case "HAS_NEVER_CONTAINED":
		return label + " has never contained " + listValues(values, true)
	case "IS_GREATER_THAN", "GT":
		return label + " is greater than " + listValues(values, false)
	case "IS_GREATER_THAN_OR_EQUAL_TO", "GTE":
		return label + " is at least " + listValues(values, false)
	case "IS_LESS_THAN", "LT":
		return label + " is less than " + listValues(values, false)
	case "IS_LESS_THAN_OR_EQUAL_TO", "LTE":
		return label + " is at most " + listValues(values, false)
	case "IS_BETWEEN", "IS_NOT_BETWEEN":
		not := ""
		if operator == "IS_NOT_BETWEEN" {
			not = "not "
		}
		if bound.lower != "" && bound.upper != "" {
			return label + " is " + not + "between " + bound.lower + " and " + bound.upper
		}
		r.complete = false
		return label + " is " + not + "within a range"
	case "IS_BEFORE", "IS_BEFORE_DATE", "IS_BEFORE_OR_EQUAL":
		return label + " is before " + listValues(values, false)
	case "IS_AFTER", "IS_AFTER_DATE", "IS_AFTER_OR_EQUAL":
		return label + " is after " + listValues(values, false)
	default:
		r.complete = false
		if operator == "" {
			return label + " has a condition this summary cannot read"
		}
		if len(values) == 0 {
			return label + " " + humanize(operator)
		}
		return label + " " + humanize(operator) + " " + listValues(values, true)
	}
}

func (r *describer) propertyLabel(key string, scope describeScope) (string, map[string]string) {
	if key == "" {
		return "A property", nil
	}
	if scope == scopeContact {
		if p, ok := r.labels.Properties[strings.ToLower(key)]; ok && strings.TrimSpace(p.Label) != "" {
			return strings.TrimSpace(p.Label), p.Options
		}
	}
	return upperFirst(humanize(key)), nil
}

func (r *describer) listName(id string) string {
	id = strings.TrimSpace(id)
	if name := strings.TrimSpace(r.labels.Lists[id]); name != "" {
		return "the list “" + name + "”"
	}
	if id == "" {
		return "another list"
	}
	return "list #" + id
}

// pageView renders a PAGE_VIEW filter from its operator.
func pageView(f describeFilter) string {
	url := strings.TrimSpace(f.PageURL)
	op := strings.ToUpper(strings.TrimSpace(f.Operator))
	not := strings.HasPrefix(op, "NOT_")
	verb := "Viewed"
	if not {
		verb = "Has not viewed"
	}
	switch {
	case url == "":
		return verb + " a tracked page"
	case strings.HasSuffix(op, "_EQ"):
		return verb + " the page " + quote(url)
	case strings.HasSuffix(op, "_CONTAINS"):
		return verb + " a page whose address contains " + quote(url)
	case strings.HasSuffix(op, "_MATCHES_REGEX"):
		return verb + " a page matching the pattern " + quote(url)
	default:
		return verb + " a page matching " + quote(url)
	}
}

func isAssociationBranch(b *describeBranch) bool {
	t := strings.ToUpper(strings.TrimSpace(b.FilterBranchType))
	return t == "ASSOCIATION" || t == "PROPERTY_ASSOCIATION"
}

func objectTypeName(id string) string {
	switch strings.TrimSpace(id) {
	case "0-1":
		return "contact"
	case "0-2":
		return "company"
	case "0-3":
		return "deal"
	case "0-5":
		return "ticket"
	case "":
		return "record"
	default:
		return "record (type " + strings.TrimSpace(id) + ")"
	}
}

func propertyKey(f describeFilter) string {
	if p := strings.TrimSpace(f.Property); p != "" {
		return strings.ToLower(p)
	}
	return strings.ToLower(strings.TrimSpace(f.PropertyName))
}

func hasRefinement(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null" && s != "{}"
}

// rawStrings reads a JSON scalar or array as strings. Numbers and booleans are kept as their
// literal text (a subscription id or a bool property's value is still a value worth showing);
// objects are skipped rather than stringified.
func rawStrings(raw json.RawMessage) []string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil
	}
	if strings.HasPrefix(s, "[") {
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil
		}
		out := make([]string, 0, len(items))
		for _, it := range items {
			out = append(out, rawStrings(it)...)
		}
		return out
	}
	if strings.HasPrefix(s, "{") {
		return nil
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		if str = strings.TrimSpace(str); str != "" {
			return []string{str}
		}
		return nil
	}
	return []string{s}
}

func firstRawString(raw json.RawMessage) string {
	if v := rawStrings(raw); len(v) > 0 {
		return v[0]
	}
	return ""
}

func mapOptionLabels(values []string, options map[string]string) []string {
	if len(options) == 0 {
		return values
	}
	out := make([]string, len(values))
	for i, v := range values {
		if l := strings.TrimSpace(options[v]); l != "" {
			out[i] = l
			continue
		}
		out[i] = v
	}
	return out
}

func formatTimeValues(values []string) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = formatTimeValue(v)
	}
	return out
}

// formatTimeValue turns an epoch-milliseconds or RFC 3339 value into a calendar date; anything
// else is returned as it came.
func formatTimeValue(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if ms, err := strconv.ParseInt(v, 10, 64); err == nil && ms > 0 {
		return time.UnixMilli(ms).UTC().Format("2 Jan 2006")
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC().Format("2 Jan 2006")
	}
	return v
}

func boolWord(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "yes", "1":
		return "yes"
	case "false", "no", "0":
		return "no"
	default:
		return quote(v)
	}
}

// listValues spells out up to describeMaxValues values and summarises the rest. quoted wraps
// each value in typographic quotes, which is how text values read; numbers and dates read
// better bare.
func listValues(values []string, quoted bool) string {
	clean := make([]string, 0, len(values))
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			if quoted {
				v = quote(v)
			}
			clean = append(clean, v)
		}
	}
	if len(clean) == 0 {
		return "(no value)"
	}
	if len(clean) > describeMaxValues {
		rest := len(clean) - describeMaxValues
		return strings.Join(clean[:describeMaxValues], ", ") + fmt.Sprintf(" and %d more", rest)
	}
	return joinWords(clean, "or")
}

func quote(v string) string { return "“" + v + "”" }

// joinWords joins a, b and c as "a, b and c" (or "or").
func joinWords(parts []string, conj string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	case 2:
		return parts[0] + " " + conj + " " + parts[1]
	default:
		return strings.Join(parts[:len(parts)-1], ", ") + " " + conj + " " + parts[len(parts)-1]
	}
}

// humanize turns an internal name or an enum into words: "IS_NOT_KNOWN" -> "is not known",
// "hs_email_optout" -> "hs email optout".
func humanize(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return strings.ToLower(strings.Join(strings.FieldsFunc(s, func(r rune) bool { return r == '_' || r == '-' || unicode.IsSpace(r) }), " "))
}

func humanizeAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if h := humanize(s); h != "" {
			out = append(out, h)
		}
	}
	return out
}

func upperFirst(s string) string {
	for i, r := range s {
		return string(unicode.ToUpper(r)) + s[i+len(string(r)):]
	}
	return s
}

// lowerFirst lower-cases a sentence's first letter unless the first word looks like a name or an
// acronym (its second letter is upper-case too, as in "IP Country" or "HubSpot").
func lowerFirst(s string) string {
	runes := []rune(s)
	if len(runes) == 0 {
		return s
	}
	if len(runes) > 1 && unicode.IsUpper(runes[1]) {
		return s
	}
	runes[0] = unicode.ToLower(runes[0])
	return string(runes)
}
