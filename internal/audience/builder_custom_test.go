// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package audience

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func decodeBranch(t *testing.T, raw json.RawMessage) filterBranch {
	t.Helper()
	var b filterBranch
	require.NoError(t, json.Unmarshal(raw, &b))
	return b
}

// seoul is the reference pattern: city mailable contacts, contact branches gated,
// event-history branches ungated.
func seoul() CustomCriteria {
	return CustomCriteria{
		Cities:            []string{"Seoul"},
		ContactLocation:   true,
		EventRegistered:   true,
		EducationEnrolled: true,
		GateListID:        "26716",
		Label:             "OSS Korea + MCP Dev Summit Seoul",
	}
}

// TestCustomListFilter_SeoulReferenceShape pins the shape of the live reference
// list "26Q3 - Seoul - Mailable Contacts - …": four AND branches under one OR
// root, contact branches gated, event-history branches qualified inside the
// UNIFIED_EVENTS node.
func TestCustomListFilter_SeoulReferenceShape(t *testing.T) {
	raw, err := CustomListFilter(seoul())
	require.NoError(t, err)
	root := decodeBranch(t, raw)

	assert.Equal(t, "OR", root.FilterBranchType)
	require.Len(t, root.FilterBranches, 4)
	for _, b := range root.FilterBranches {
		assert.Equal(t, "AND", b.FilterBranchType, "children of the root are ANDs; HubSpot rejects nested ORs")
	}

	city := root.FilterBranches[0]
	require.Len(t, city.Filters, 2)
	assert.Equal(t, "city", city.Filters[0].Property)
	assert.Equal(t, []string{"Seoul"}, city.Filters[0].Operation.Values)
	assert.Equal(t, "IN_LIST", city.Filters[1].FilterType)
	assert.Equal(t, "26716", city.Filters[1].ListID)
	assert.Equal(t, "ip_city", root.FilterBranches[1].Filters[0].Property)

	reg := root.FilterBranches[2]
	assert.Empty(t, reg.Filters, "event history is ungated unless GateEventHistory is set")
	require.Len(t, reg.FilterBranches, 1)
	assert.Equal(t, "UNIFIED_EVENTS", reg.FilterBranches[0].FilterBranchType)
	assert.Equal(t, registrationEventTypeID, reg.FilterBranches[0].EventTypeID)
	require.Empty(t, reg.FilterBranches[0].FilterBranches, "HubSpot refuses branches inside UNIFIED_EVENTS")
	inner := reg.FilterBranches[0].Filters[0]
	assert.Equal(t, "event_city", inner.Property)

	edu := root.FilterBranches[3]
	assert.Equal(t, educationEventTypeID, edu.FilterBranches[0].EventTypeID)
	assert.Equal(t, "user_city", edu.FilterBranches[0].Filters[0].Property)
}

// TestCustomListFilter_BroadScopeAndGate covers the "Seoul and South Korea"
// variant plus the stricter mailability answer.
func TestCustomListFilter_BroadScopeAndGate(t *testing.T) {
	c := seoul()
	c.Countries = []string{"South Korea"}
	c.GateEventHistory = true
	raw, err := CustomListFilter(c)
	require.NoError(t, err)
	root := decodeBranch(t, raw)

	require.Len(t, root.FilterBranches, 8, "4 contact branches + 2 per event type")
	country := root.FilterBranches[2]
	assert.Equal(t, "country_dropdown", country.Filters[0].Property)
	assert.Equal(t, "IS_ANY_OF", country.Filters[0].Operation.Operator)
	assert.Equal(t, "ENUMERATION", country.Filters[0].Operation.OperationType)
	for _, b := range root.FilterBranches[4:] {
		require.Len(t, b.Filters, 1, "gated event-history branches carry the master IN_LIST")
		assert.Equal(t, "26716", b.Filters[0].ListID)
	}
}

// TestCustomListFilter_RestrictionsReachEveryBranch is the invariant that makes a
// restriction a restriction: one branch without it is an OR'd way around it.
func TestCustomListFilter_RestrictionsReachEveryBranch(t *testing.T) {
	c := seoul()
	c.PropertyFilters = []CustomPropertyFilter{
		{Property: "jobtitle", Operator: CustomOpContains, Values: []string{"director", " Director "}},
		{Property: "hs_language", Operator: CustomOpKnown, Values: []string{"ignored"}},
	}
	raw, err := CustomListFilter(c)
	require.NoError(t, err)
	root := decodeBranch(t, raw)

	for i, b := range root.FilterBranches {
		props := map[string]filter{}
		for _, f := range b.Filters {
			props[f.Property+f.ListID] = f
		}
		job, ok := props["jobtitle"]
		require.True(t, ok, "branch %d lacks the property filter", i)
		assert.Equal(t, "CONTAINS", job.Operation.Operator, "there is no CONTAINS_TOKEN operator")
		assert.Equal(t, []string{"director"}, job.Operation.Values, "values are de-duplicated case-insensitively")
		known := props["hs_language"]
		assert.Equal(t, "IS_KNOWN", known.Operation.Operator)
		assert.Equal(t, "ALL_PROPERTY", known.Operation.OperationType, "known/unknown are ALL_PROPERTY operations")
		assert.Empty(t, known.Operation.Values, "IS_KNOWN takes no values")
		for _, f := range b.Filters {
			assert.NotEqual(t, "NOT_IN_LIST", f.Operator, "suppressions belong to the master, never the building block")
		}
	}
}

func TestCustomListFilter_Speakers(t *testing.T) {
	t.Run("existing list is one IN_LIST per branch", func(t *testing.T) {
		c := seoul()
		c.Speakers = &CustomSpeakers{Scope: SpeakerScopeCurrent, ListID: "4242"}
		raw, err := CustomListFilter(c)
		require.NoError(t, err)
		root := decodeBranch(t, raw)
		require.Len(t, root.FilterBranches, 4)
		for _, b := range root.FilterBranches {
			assert.Equal(t, "4242", b.Filters[len(b.Filters)-1].ListID)
		}
	})

	t.Run("no list fans out over the three speaker signals", func(t *testing.T) {
		c := CustomCriteria{
			Speakers: &CustomSpeakers{Scope: SpeakerScopeCurrent, EventNames: []string{"Open Source Summit Japan 2026", "Embedded Linux Conference Asia 2026"}, PropertyTypes: liveSpeakerTypes},
			Label:    "Open Source Summit Japan",
		}
		raw, err := CustomListFilter(c)
		require.NoError(t, err)
		root := decodeBranch(t, raw)
		require.Len(t, root.FilterBranches, 3)
		signals := map[string]bool{}
		for _, b := range root.FilterBranches {
			require.Len(t, b.FilterBranches, 1)
			ev := b.FilterBranches[0]
			assert.Equal(t, registrationEventTypeID, ev.EventTypeID)
			require.Empty(t, ev.FilterBranches)
			fs := ev.Filters
			require.Len(t, fs, 2)
			assert.Equal(t, "event_name", fs[0].Property)
			assert.Equal(t, "IS_EQUAL_TO", fs[0].Operation.Operator, "event names match exactly (MULTISTRING has no IS_ANY_OF), never by CONTAINS")
			assert.Len(t, fs[0].Operation.Values, 2, "co-located tracks are matched alongside the umbrella")
			signals[fs[1].Property] = true
		}
		assert.Equal(t, map[string]bool{"registration_type": true, "is_event_speaker_bool": true, "is_event_speaker": true}, signals)
	})

	t.Run("branch copies do not alias", func(t *testing.T) {
		c := seoul()
		c.Speakers = &CustomSpeakers{Scope: SpeakerScopePast, EventNames: []string{"KubeCon 2025"}, PropertyTypes: liveSpeakerTypes}
		raw, err := CustomListFilter(c)
		require.NoError(t, err)
		root := decodeBranch(t, raw)
		require.Len(t, root.FilterBranches, 12)
		for _, b := range root.FilterBranches[:2] {
			assert.Len(t, b.FilterBranches, 1, "each contact copy carries exactly its own speaker node")
		}
	})
}

func TestCustomListFilter_AmbassadorsOneBranchPerProgramme(t *testing.T) {
	c := CustomCriteria{Ambassadors: true}
	raw, err := CustomListFilter(c)
	require.NoError(t, err)
	root := decodeBranch(t, raw)
	require.Len(t, root.FilterBranches, len(ambassadorProperties))
	for i, b := range root.FilterBranches {
		assert.Equal(t, ambassadorProperties[i], b.Filters[0].Property)
		assert.Equal(t, "IS_KNOWN", b.Filters[0].Operation.Operator)
	}
}

// TestValidateCustomCriteria_FailsClosed lists the criteria that would build a
// list wider than intended if accepted.
func TestValidateCustomCriteria_FailsClosed(t *testing.T) {
	cases := map[string]CustomCriteria{
		"selects nobody":                 {},
		"location branches, no place":    {ContactLocation: true, GateListID: "1"},
		"ungated contact location":       {Cities: []string{"Seoul"}, ContactLocation: true},
		"gate requested without list":    {Cities: []string{"Seoul"}, EventRegistered: true, GateEventHistory: true},
		"unknown operator":               {PropertyFilters: []CustomPropertyFilter{{Property: "x", Operator: "like"}}},
		"value operator without value":   {PropertyFilters: []CustomPropertyFilter{{Property: "x", Operator: CustomOpEquals, Values: []string{"  "}}}},
		"blank property":                 {PropertyFilters: []CustomPropertyFilter{{Operator: CustomOpKnown}}},
		"speakers with nothing to match": {Speakers: &CustomSpeakers{Scope: SpeakerScopeCurrent}},
		"unknown speaker scope":          {Speakers: &CustomSpeakers{Scope: "future", ListID: "1"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := CustomListFilter(c)
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrInvalidRequest))
		})
	}
}

func TestCustomListFilter_BranchLimit(t *testing.T) {
	c := seoul()
	c.Countries = []string{"South Korea"}
	c.Speakers = &CustomSpeakers{Scope: SpeakerScopeCurrent, EventNames: []string{"E"}, PropertyTypes: liveSpeakerTypes}
	c.Ambassadors = true
	_, err := CustomListFilter(c)
	require.Error(t, err, "8 x 3 x 3 = 72 branches is over the limit")
	assert.True(t, errors.Is(err, ErrInvalidRequest))
}

func TestCustomContactProperties(t *testing.T) {
	c := seoul()
	c.Countries = []string{"South Korea"}
	c.PropertyFilters = []CustomPropertyFilter{{Property: "jobtitle", Operator: CustomOpKnown}}
	assert.Equal(t, []string{"city", "ip_city", "country_dropdown", "ip_country", "jobtitle"}, CustomContactProperties(c),
		"event properties are not contact properties and are not listed")
}

func TestCustomListName(t *testing.T) {
	now := utc("2026-08-01T00:00:00Z")
	assert.Equal(t, "26Q3 - Seoul - Mailable Contacts - OSS Korea + MCP Dev Summit Seoul", CustomListName(seoul(), now))

	broad := seoul()
	broad.Countries = []string{"South Korea"}
	assert.Equal(t, "26Q3 - Seoul and South Korea - Mailable Contacts - OSS Korea + MCP Dev Summit Seoul", CustomListName(broad, now))

	speakers := CustomCriteria{Speakers: &CustomSpeakers{Scope: SpeakerScopeCurrentPast, ListID: "1"}, Label: "KubeCon Japan"}
	assert.Equal(t, "KubeCon Japan Speakers - Current + Past", CustomListName(speakers, now))

	assert.Equal(t, "26Q3 - Community Ambassadors", CustomListName(CustomCriteria{Ambassadors: true}, now))
	assert.Equal(t, "26Q3 - Seoul - Mailable Contacts - OSS Korea + MCP Dev Summit Seoul (v2)", VersionedName(CustomListName(seoul(), now), 2))
}

func TestCustomListFilter_RegisteredFor(t *testing.T) {
	c := CustomCriteria{RegisteredFor: []string{"AGNTCon + MCPCon Japan 2026"}, Label: "AGNTCon + MCPCon Japan"}
	raw, err := CustomListFilter(c)
	require.NoError(t, err)
	root := decodeBranch(t, raw)
	require.Len(t, root.FilterBranches, 1)
	ev := root.FilterBranches[0].FilterBranches[0]
	assert.Equal(t, registrationEventTypeID, ev.EventTypeID)
	f := ev.Filters[0]
	assert.Equal(t, "event_name", f.Property)
	assert.Equal(t, "IS_EQUAL_TO", f.Operation.Operator)
	assert.Equal(t, "26Q3 - AGNTCon + MCPCon Japan - Event Registration", CustomListName(c, utc("2026-08-01T00:00:00Z")))
	assert.False(t, AllBranchesGated(c))
}

func TestAllBranchesGated(t *testing.T) {
	assert.False(t, AllBranchesGated(seoul()), "event history is ungated by default")
	strict := seoul()
	strict.GateEventHistory = true
	assert.True(t, AllBranchesGated(strict))
	contactOnly := CustomCriteria{Cities: []string{"Seoul"}, ContactLocation: true, GateListID: "1"}
	assert.True(t, AllBranchesGated(contactOnly))
	assert.False(t, AllBranchesGated(CustomCriteria{Ambassadors: true}))
}

func TestUnknownPropertiesError(t *testing.T) {
	err := error(&UnknownPropertiesError{Names: []string{"job title", "x"}})
	assert.True(t, errors.Is(err, ErrInvalidRequest))
	assert.Contains(t, err.Error(), "properties job title, x")
	assert.Contains(t, (&UnknownPropertiesError{Names: []string{"x"}}).Error(), "property x")
}

func TestMatchesBrandMaster(t *testing.T) {
	assert.True(t, MatchesBrandMaster("26Q2 - The Linux Foundation - Master-list", "LF"))
	assert.True(t, MatchesBrandMaster("26Q2 - CNCF - Master list", "cncf"))
	assert.False(t, MatchesBrandMaster("26Q3 - CNCF - KubeCon Japan - Master", "CNCF"),
		"a per-event master is one event's audience, not the brand gate")
	assert.False(t, MatchesBrandMaster("26Q2 - Self Service - Master-list", "LF"),
		"a short brand matches only as a whole token")
	assert.False(t, MatchesBrandMaster("26Q2 - CNCF - Master-list Suppression", "CNCF"))
	assert.Equal(t, []string{"LF Master-list", "the linux foundation Master-list", "linux foundation Master-list"}, BrandMasterProbes("LF"))
	assert.Nil(t, BrandMasterProbes(" "))
}

// TestCustomListFilter_PageViewHasNoOperation pins the PAGE_VIEW wire shape: no
// `operation`, the URL under `pageUrl`, ungated.
func TestCustomListFilter_PageViewHasNoOperation(t *testing.T) {
	c := CustomCriteria{PageViewURLs: []string{"events.linuxfoundation.org/agntcon-mcpcon-japan"}, Label: "AGNTCon Japan"}
	raw, err := CustomListFilter(c)
	require.NoError(t, err)

	var generic map[string]any
	require.NoError(t, json.Unmarshal(raw, &generic))
	f := generic["filterBranches"].([]any)[0].(map[string]any)["filters"].([]any)[0].(map[string]any)
	assert.Equal(t, "PAGE_VIEW", f["filterType"])
	assert.Equal(t, "HAS_PAGEVIEW_CONTAINS", f["operator"])
	assert.Equal(t, "events.linuxfoundation.org/agntcon-mcpcon-japan", f["pageUrl"])
	assert.Equal(t, false, f["enableTracking"])
	assert.NotContains(t, f, "operation", "an operation on a PAGE_VIEW filter is a field HubSpot does not define")
	assert.Equal(t, "26Q3 - AGNTCon Japan - Web Visitors", CustomListName(c, utc("2026-08-01T00:00:00Z")))
}

func TestCustomListFilter_EducationTopics(t *testing.T) {
	t.Run("topic filter rides inside every education node", func(t *testing.T) {
		c := seoul()
		c.EducationTopics = []string{"Kubernetes", "Cloud Native"}
		c.EducationTopicProperty = "course_name"
		raw, err := CustomListFilter(c)
		require.NoError(t, err)
		root := decodeBranch(t, raw)
		edu := root.FilterBranches[3].FilterBranches[0]
		require.Equal(t, educationEventTypeID, edu.EventTypeID)
		fs := edu.Filters
		require.Len(t, fs, 2)
		assert.Equal(t, "course_name", fs[1].Property)
		assert.Equal(t, "CONTAINS", fs[1].Operation.Operator)
		reg := root.FilterBranches[2].FilterBranches[0].Filters
		assert.Len(t, reg, 1, "the topic filter belongs to the education event only")
	})

	t.Run("topics alone are the education signal", func(t *testing.T) {
		c := CustomCriteria{EducationEnrolled: true, EducationTopics: []string{"PyTorch"}, EducationTopicProperty: "course_name", Label: "PyTorch Conference"}
		raw, err := CustomListFilter(c)
		require.NoError(t, err)
		root := decodeBranch(t, raw)
		require.Len(t, root.FilterBranches, 1)
		assert.Equal(t, "course_name", root.FilterBranches[0].FilterBranches[0].Filters[0].Property)
		assert.Equal(t, "26Q3 - PyTorch Conference - Education Enrolled", CustomListName(c, utc("2026-08-01T00:00:00Z")))
	})

	t.Run("fails closed", func(t *testing.T) {
		cases := map[string]CustomCriteria{
			"education with neither place nor topic": {EducationEnrolled: true},
			"topics without a resolved property":     {EducationEnrolled: true, EducationTopics: []string{"x"}},
			"topics without education":               {Ambassadors: true, EducationTopics: []string{"x"}, EducationTopicProperty: "p"},
			"a page fragment":                        {PageViewURLs: []string{"kubecon"}},
		}
		for name, c := range cases {
			_, err := CustomListFilter(c)
			assert.ErrorIs(t, err, ErrInvalidRequest, name)
		}
	})
}

func TestCourseTopicProperty(t *testing.T) {
	assert.Equal(t, "course_name", CourseTopicProperty(map[string]string{"user_city": "City", "course_name": "Course", "topic_area": "Topic"}),
		"course outranks topic")
	assert.Equal(t, []string{"course_title", "course_area", "course_code"},
		CourseTopicCandidates(map[string]string{"course_code": "Course code", "course_title": "Course", "course_area": "", "user_city": "City"}),
		"a code never outranks words: CONTAINS on LFS258 never matches a topic (the live portal's first pick was course_code)")
	assert.Equal(t, "lf_track", CourseTopicProperty(map[string]string{"lf_track": "Training subject"}), "the label counts too")
	assert.Empty(t, CourseTopicProperty(map[string]string{"user_city": "City"}))
}

// TestCustomListFilter_OnlyDocumentedOperators pins the operator vocabulary to
// HubSpot's list-filter reference: the first live create built from undocumented
// operators (CONTAINS_TOKEN, HAS_PROPERTY, MULTISTRING IS_ANY_OF) and nested
// event branches was refused.
func TestCustomListFilter_OnlyDocumentedOperators(t *testing.T) {
	documented := map[string]map[string]bool{
		"MULTISTRING":  {"IS_EQUAL_TO": true, "IS_NOT_EQUAL_TO": true, "CONTAINS": true, "DOES_NOT_CONTAIN": true, "STARTS_WITH": true, "ENDS_WITH": true},
		"ENUMERATION":  {"IS_EQUAL_TO": true, "IS_NOT_EQUAL_TO": true, "IS_ANY_OF": true},
		"ALL_PROPERTY": {"IS_KNOWN": true, "IS_NOT_KNOWN": true},
		"BOOL":         {"IS_EQUAL_TO": true, "IS_NOT_EQUAL_TO": true},
		"NUMBER":       {"IS_EQUAL_TO": true, "IS_NOT_EQUAL_TO": true},
	}
	c := seoul()
	c.Countries = []string{"South Korea"}
	c.GateListID = "26716"
	c.EducationTopics = []string{"Linux"}
	c.EducationTopicProperty = "course_name"
	c.PropertyFilters = []CustomPropertyFilter{
		{Property: "a", Operator: CustomOpEquals, Values: []string{"x"}},
		{Property: "b", Operator: CustomOpContains, Values: []string{"x"}},
		{Property: "c", Operator: CustomOpAnyOf, Values: []string{"x", "y"}},
		{Property: "d", Operator: CustomOpNotAnyOf, Values: []string{"x"}},
		{Property: "e", Operator: CustomOpKnown},
		{Property: "f", Operator: CustomOpUnknown},
		{Property: "g", Operator: CustomOpAnyOf, Values: []string{"x"}, Enumeration: true},
	}
	c.Speakers = &CustomSpeakers{Scope: SpeakerScopeCurrent, EventNames: []string{"E 2026"}, PropertyTypes: liveSpeakerTypes}
	raw, err := CustomListFilter(c)
	require.NoError(t, err)

	var walk func(b filterBranch)
	walk = func(b filterBranch) {
		if b.FilterBranchType == "UNIFIED_EVENTS" {
			assert.Empty(t, b.FilterBranches, "UNIFIED_EVENTS cannot have filter branches")
		}
		for _, f := range b.Filters {
			if f.FilterType != "PROPERTY" {
				continue
			}
			ops, ok := documented[f.Operation.OperationType]
			require.True(t, ok, "undocumented operation type %q on %s", f.Operation.OperationType, f.Property)
			assert.True(t, ops[f.Operation.Operator], "undocumented operator %s/%s on %s", f.Operation.OperationType, f.Operation.Operator, f.Property)
		}
		for _, child := range b.FilterBranches {
			walk(child)
		}
	}
	walk(decodeBranch(t, raw))
}

// liveSpeakerTypes are the registration event's speaker property types as the tlf
// portal reports them; is_event_speaker_bool is a bool, which is what refused the
// first live speakers create.
var liveSpeakerTypes = map[string]string{"registration_type": "string", "is_event_speaker_bool": "bool", "is_event_speaker": "number"}

func TestSpeakerSignals_TypedByThePortal(t *testing.T) {
	c := CustomCriteria{Speakers: &CustomSpeakers{Scope: SpeakerScopeCurrent, EventNames: []string{"KubeCon + CloudNativeCon Europe"}, PropertyTypes: liveSpeakerTypes}, Label: "KubeCon"}
	raw, err := CustomListFilter(c)
	require.NoError(t, err)

	var generic map[string]any
	require.NoError(t, json.Unmarshal(raw, &generic))
	ops := map[string]map[string]any{}
	for _, b := range generic["filterBranches"].([]any) {
		ev := b.(map[string]any)["filterBranches"].([]any)[0].(map[string]any)
		f := ev["filters"].([]any)[1].(map[string]any)
		ops[f["property"].(string)] = f["operation"].(map[string]any)
	}
	assert.Equal(t, "BOOL", ops["is_event_speaker_bool"]["operationType"])
	assert.Equal(t, true, ops["is_event_speaker_bool"]["value"], "a bool takes a single boolean value")
	assert.NotContains(t, ops["is_event_speaker_bool"], "values")
	assert.Equal(t, "NUMBER", ops["is_event_speaker"]["operationType"])
	assert.Equal(t, float64(1), ops["is_event_speaker"]["value"])
	assert.Equal(t, "MULTISTRING", ops["registration_type"]["operationType"])
	assert.Equal(t, "CONTAINS", ops["registration_type"]["operator"])
}

func TestSpeakerSignals_SkipWhatThePortalLacks(t *testing.T) {
	c := CustomCriteria{Speakers: &CustomSpeakers{Scope: SpeakerScopeCurrent, EventNames: []string{"E"}, PropertyTypes: map[string]string{"registration_type": "string"}}}
	raw, err := CustomListFilter(c)
	require.NoError(t, err)
	assert.Len(t, decodeBranch(t, raw).FilterBranches, 1, "only the signal the portal defines")

	c.Speakers.PropertyTypes = nil
	_, err = CustomListFilter(c)
	assert.ErrorIs(t, err, ErrInvalidRequest, "no speaker property means no way to tell speakers from attendees")

	c.Speakers.ListID = "4242"
	_, err = CustomListFilter(c)
	assert.NoError(t, err, "an existing speakers list needs no event properties")
}
