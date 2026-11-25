// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package audience

import (
	"encoding/json"
	"strings"
	"testing"
)

// The describer must read exactly what this package writes. Each tree below comes from the
// builder's own constructor, so a change to a write shape that the describer cannot follow
// fails here rather than in front of an operator.

func TestDescribeRules_CurrentRegistrantsFilter(t *testing.T) {
	raw, err := CurrentRegistrantsFilter("KubeCon + CloudNativeCon North America 2026")
	if err != nil {
		t.Fatalf("CurrentRegistrantsFilter: %v", err)
	}
	match, groups, complete, err := DescribeRules(raw, DescribeLabels{})
	if err != nil {
		t.Fatalf("DescribeRules: %v", err)
	}
	if match != DescribeMatchAny || !complete {
		t.Errorf("match=%q complete=%v, want any/true", match, complete)
	}
	if len(groups) != 1 || len(groups[0].Conditions) != 1 {
		t.Fatalf("groups = %+v, want one group of one condition", groups)
	}
	want := "Registered for an event where event name is “KubeCon + CloudNativeCon North America 2026”"
	if got := groups[0].Conditions[0]; got != want {
		t.Errorf("condition =\n  %q\nwant\n  %q", got, want)
	}
}

func TestDescribeRules_MasterWithSuppressionsNamesEveryList(t *testing.T) {
	raw, err := MasterListWithSuppressionsFilter([]string{"101", "102"}, []string{"900"})
	if err != nil {
		t.Fatalf("MasterListWithSuppressionsFilter: %v", err)
	}
	labels := DescribeLabels{Lists: map[string]string{"101": "KubeCon NA 2025 Registrants", "900": "LFX Standard Suppressions - tlf"}}
	match, groups, complete, err := DescribeRules(raw, labels)
	if err != nil {
		t.Fatalf("DescribeRules: %v", err)
	}
	if match != DescribeMatchAny || !complete || len(groups) != 2 {
		t.Fatalf("match=%q complete=%v groups=%+v", match, complete, groups)
	}
	first := strings.Join(groups[0].Conditions, " | ")
	if !strings.Contains(first, "Is a member of the list “KubeCon NA 2025 Registrants”") ||
		!strings.Contains(first, "Is not a member of the list “LFX Standard Suppressions - tlf”") {
		t.Errorf("group 1 = %q", first)
	}
	// An unnamed list falls back to its id rather than vanishing.
	if second := strings.Join(groups[1].Conditions, " | "); !strings.Contains(second, "Is a member of list #102") {
		t.Errorf("group 2 = %q", second)
	}
}

func TestDescribeRules_CustomListCoversEveryBranchKind(t *testing.T) {
	raw, err := CustomListFilter(CustomCriteria{
		RegisteredFor: []string{"Open Source Summit Japan 2025"},
		PageViewURLs:  []string{"events.linuxfoundation.org/open-source-summit-japan"},
		Label:         "OSS Japan",
	})
	if err != nil {
		t.Fatalf("CustomListFilter: %v", err)
	}
	_, groups, complete, err := DescribeRules(raw, DescribeLabels{})
	if err != nil {
		t.Fatalf("DescribeRules: %v", err)
	}
	all := flatten(groups)
	if !strings.Contains(all, "Registered for an event where event name is “Open Source Summit Japan 2025”") {
		t.Errorf("registration branch missing: %s", all)
	}
	if !strings.Contains(all, "Viewed a page whose address contains “events.linuxfoundation.org/open-source-summit-japan”") {
		t.Errorf("page-view branch missing: %s", all)
	}
	if !complete {
		t.Errorf("every branch the builder writes must be fully described: %s", all)
	}
}

func TestDescribeRules_PropertyLabelsAndOptions(t *testing.T) {
	raw := json.RawMessage(`{"filterBranchType":"OR","filters":[],"filterBranches":[{"filterBranchType":"AND","filters":[
		{"filterType":"PROPERTY","property":"lifecyclestage","operation":{"operationType":"ENUMERATION","operator":"IS_ANY_OF","values":["subscriber","lead"]}},
		{"filterType":"PROPERTY","property":"country","operation":{"operationType":"MULTISTRING","operator":"IS_EQUAL_TO","values":["Japan"]}},
		{"filterType":"PROPERTY","property":"is_event_speaker_bool","operation":{"operationType":"BOOL","operator":"IS_EQUAL_TO","value":true}},
		{"filterType":"PROPERTY","property":"hs_email_hard_bounce_reason_enum","operation":{"operationType":"ALL_PROPERTY","operator":"IS_KNOWN"}}
	],"filterBranches":[]}]}`)
	labels := DescribeLabels{Properties: map[string]PropertyLabel{
		"lifecyclestage": {Label: "Lifecycle Stage", Options: map[string]string{"subscriber": "Subscriber", "lead": "Lead"}},
		"country":        {Label: "Country/Region"},
	}}
	_, groups, complete, err := DescribeRules(raw, labels)
	if err != nil {
		t.Fatalf("DescribeRules: %v", err)
	}
	want := []string{
		"Lifecycle Stage is any of “Subscriber” or “Lead”",
		"Country/Region is “Japan”",
		"Is event speaker bool is yes",
		"Hs email hard bounce reason enum is known",
	}
	if len(groups) != 1 || strings.Join(groups[0].Conditions, "\n") != strings.Join(want, "\n") {
		t.Errorf("conditions =\n%s\nwant\n%s", strings.Join(groups[0].Conditions, "\n"), strings.Join(want, "\n"))
	}
	if !complete {
		t.Error("every operator here is modelled")
	}
}

func TestDescribeRules_NestedNegationAndUnknownRules(t *testing.T) {
	raw := json.RawMessage(`{"filterBranchType":"OR","filters":[],"filterBranches":[{"filterBranchType":"AND","filters":[
		{"filterType":"IN_LIST","listId":"77","operator":"IN_LIST"}
	],"filterBranches":[
		{"filterBranchType":"NOT_ANY","filters":[{"filterType":"PROPERTY","property":"country","operation":{"operationType":"MULTISTRING","operator":"IS_EQUAL_TO","values":["Russia"]}}],"filterBranches":[]},
		{"filterBranchType":"SOMETHING_NEW","filters":[],"filterBranches":[]}
	]},{"filterBranchType":"AND","filters":[{"filterType":"SURVEY_MONKEY","operator":"HAS_ANSWERED"}],"filterBranches":[]}]}`)
	_, groups, complete, err := DescribeRules(raw, DescribeLabels{Lists: map[string]string{"77": "CNCF Master"}})
	if err != nil {
		t.Fatalf("DescribeRules: %v", err)
	}
	all := flatten(groups)
	for _, want := range []string{
		"Is a member of the list “CNCF Master”",
		"None of: Country is “Russia”",
		"Matches a HubSpot something new rule",
		"Matches a HubSpot survey monkey rule",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
	if complete {
		t.Error("a rule described only generically must mark the description incomplete")
	}
}

func TestDescribeRules_EventRefinementIsNamedNotGuessed(t *testing.T) {
	raw := json.RawMessage(`{"filterBranchType":"OR","filters":[],"filterBranches":[{"filterBranchType":"AND","filters":[],"filterBranches":[
		{"filterBranchType":"UNIFIED_EVENTS","eventTypeId":"6-58204655","operator":"HAS_COMPLETED","filters":[
			{"filterType":"PROPERTY","property":"course_code","operation":{"operationType":"MULTISTRING","operator":"CONTAINS","values":["CKA","CKAD"]}}
		],"filterBranches":[],"coalescingRefineBy":{"type":"NUM_OCCURRENCES","minOccurrences":2}}
	]}]}`)
	_, groups, complete, err := DescribeRules(raw, DescribeLabels{})
	if err != nil {
		t.Fatalf("DescribeRules: %v", err)
	}
	got := flatten(groups)
	want := "Enrolled in a course where course code contains any of “CKA” or “CKAD” (limited by a time window or a minimum count)"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	if complete {
		t.Error("a refinement is named, not rendered, so the description is partial")
	}
}

func TestDescribeList_StaticAndEmpty(t *testing.T) {
	d := DescribeList("5", "Booth scans", 120, "MANUAL", "https://x", nil, DescribeLabels{})
	if d.Status != DescribeStatusStatic || len(d.Groups) != 0 || !strings.Contains(d.Summary, "static list") {
		t.Errorf("manual list: %+v", d)
	}
	d = DescribeList("6", "Broken", 0, "DYNAMIC", "https://x", json.RawMessage(`{"filterBranchType":`), DescribeLabels{})
	if d.Status != DescribeStatusUnreadable || d.Complete {
		t.Errorf("unparseable rules must be unreadable and incomplete: %+v", d)
	}
	nf := UnreadableListDescription("7", true)
	if nf.Status != DescribeStatusNotFound || nf.Size != -1 {
		t.Errorf("not found: %+v", nf)
	}
}

func TestDescribeList_SummaryNamesKinds(t *testing.T) {
	raw, err := MasterListWithSuppressionsFilter([]string{"101"}, []string{"900"})
	if err != nil {
		t.Fatal(err)
	}
	d := DescribeList("1", "Master", 1000, "DYNAMIC", "https://x", raw, DescribeLabels{})
	if d.Summary != "Contacts matching all 2 conditions, based on list membership." {
		t.Errorf("summary = %q", d.Summary)
	}
	raw, err = CurrentRegistrantsFilter("AGNTCon + MCPCon Japan 2026")
	if err != nil {
		t.Fatal(err)
	}
	d = DescribeList("2", "Registrants", 914, "DYNAMIC", "https://x", raw, DescribeLabels{})
	if d.Summary != "Contacts matching one rule: registered for an event where event name is “AGNTCon + MCPCon Japan 2026”." {
		t.Errorf("summary = %q", d.Summary)
	}
}

func TestCollectDescribeRefs(t *testing.T) {
	raw := json.RawMessage(`{"filterBranchType":"OR","filters":[],"filterBranches":[{"filterBranchType":"AND","filters":[
		{"filterType":"IN_LIST","listId":123,"operator":"IN_LIST"},
		{"filterType":"IN_LIST","listId":"456","operator":"NOT_IN_LIST"},
		{"filterType":"PROPERTY","property":"Country","operation":{"operator":"IS_KNOWN"}}
	],"filterBranches":[
		{"filterBranchType":"UNIFIED_EVENTS","eventTypeId":"6-1","operator":"HAS_COMPLETED","filters":[{"filterType":"PROPERTY","property":"event_name","operation":{"operator":"IS_KNOWN"}}],"filterBranches":[]}
	]}]}`)
	refs := CollectDescribeRefs(raw)
	if strings.Join(refs.ListIDs, ",") != "123,456" {
		t.Errorf("list ids = %v", refs.ListIDs)
	}
	// event_name is an EVENT property, so it is not a contact-property lookup.
	if strings.Join(refs.Properties, ",") != "country" {
		t.Errorf("properties = %v", refs.Properties)
	}
	if strings.Join(refs.EventTypeIDs, ",") != "6-1" {
		t.Errorf("event types = %v", refs.EventTypeIDs)
	}
	if got := CollectDescribeRefs(json.RawMessage(`not json`)); len(got.ListIDs)+len(got.Properties)+len(got.EventTypeIDs) != 0 {
		t.Errorf("an unparseable tree references nothing: %+v", got)
	}
}

func flatten(groups []RuleGroup) string {
	parts := make([]string, 0, len(groups))
	for _, g := range groups {
		parts = append(parts, strings.Join(g.Conditions, " | "))
	}
	return strings.Join(parts, " || ")
}

func TestDescribeRules_SubscriptionsReadAsOptInsAndOptOuts(t *testing.T) {
	raw := json.RawMessage(`{"filterBranchType":"OR","filters":[],"filterBranches":[
		{"filterBranchType":"AND","filters":[{"filterType":"EMAIL_SUBSCRIPTION","subscriptionIds":["55904781"],"acceptedStatuses":["OPT_OUT"],"subscriptionType":"EMAIL"}],"filterBranches":[]},
		{"filterBranchType":"AND","filters":[{"filterType":"EMAIL_SUBSCRIPTION","subscriptionIds":[11147681],"acceptedStatuses":["NOT_OPTED"]}],"filterBranches":[]},
		{"filterBranchType":"AND","filters":[{"filterType":"PROPERTY","property":"email","operation":{"operationType":"MULTISTRING","operator":"CONTAINS_EXACTLY","values":["kernel.org"]}}],"filterBranches":[]}
	]}`)
	refs := CollectDescribeRefs(raw)
	if strings.Join(refs.SubscriptionIDs, ",") != "55904781,11147681" {
		t.Fatalf("subscription ids = %v", refs.SubscriptionIDs)
	}
	_, groups, complete, err := DescribeRules(raw, DescribeLabels{Subscriptions: map[string]string{"55904781": "LF Europe Updates"}})
	if err != nil {
		t.Fatalf("DescribeRules: %v", err)
	}
	got := flatten(groups)
	for _, want := range []string{
		"Has opted out of “LF Europe Updates”",
		"Has neither opted in to nor out of subscription 11147681",
		"Email contains exactly “kernel.org”",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	if !complete {
		t.Error("every rule here is modelled, so the description is complete")
	}
	if kinds := ruleKinds(groups); strings.Join(kinds, ",") != "email subscriptions,contact details" {
		t.Errorf("kinds = %v", kinds)
	}
}
