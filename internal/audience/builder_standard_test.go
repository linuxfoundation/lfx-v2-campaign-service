// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package audience

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStandardSuppressionName_IsOnePerProjectAndSearchable(t *testing.T) {
	assert.Equal(t, "LFX Standard Suppressions - tlf", StandardSuppressionName(" tlf "))
	assert.Equal(t, StandardSuppressionName("tlf"), StandardSuppressionName("tlf"), "no date or fingerprint: every audience finds the same list")
	assert.LessOrEqual(t, utf8.RuneCountInString(StandardSuppressionName(strings.Repeat("p", 300))), MaxListNameLength)
	assert.False(t, MatchesStandardSuppression(StandardSuppressionName("tlf"), "LF Events Suppression List"),
		"the standing list is never mistaken for one of its own members")
}

func TestCurrentRegistrantsName(t *testing.T) {
	assert.Equal(t, "KubeCon NA 2026 - Current Registrants", CurrentRegistrantsName(" KubeCon NA 2026 "))
	long := CurrentRegistrantsName(strings.Repeat("k", 300))
	assert.Equal(t, MaxListNameLength, utf8.RuneCountInString(long))
	assert.True(t, strings.HasSuffix(long, " - Current Registrants"))
}

func TestCurrentRegistrantsFilter(t *testing.T) {
	raw, err := CurrentRegistrantsFilter("KubeCon NA 2026")
	require.NoError(t, err)
	s := string(raw)
	assert.True(t, strings.HasPrefix(s, `{"filterBranchType":"OR"`))
	assert.Contains(t, s, `"eventTypeId":"`+RegistrationEventTypeID+`"`)
	assert.Contains(t, s, `"KubeCon NA 2026"`)

	// HubSpot refuses a nested branch under UNIFIED_EVENTS: the event_name filter must sit on
	// the event node itself, as the custom builder's live-verified shape does.
	var root struct {
		FilterBranches []struct {
			FilterBranches []struct {
				FilterBranchType string            `json:"filterBranchType"`
				FilterBranches   []json.RawMessage `json:"filterBranches"`
				Filters          []struct {
					Property string `json:"property"`
				} `json:"filters"`
			} `json:"filterBranches"`
		} `json:"filterBranches"`
	}
	require.NoError(t, json.Unmarshal(raw, &root))
	event := root.FilterBranches[0].FilterBranches[0]
	assert.Equal(t, "UNIFIED_EVENTS", event.FilterBranchType)
	assert.Empty(t, event.FilterBranches)
	require.Len(t, event.Filters, 1)
	assert.Equal(t, "event_name", event.Filters[0].Property)

	_, err = CurrentRegistrantsFilter("  ")
	assert.ErrorIs(t, err, ErrInvalidRequest, "an empty edition would match nobody and read as applied")
	_, err = CurrentRegistrantsFilter("AGNTCon + MCPCon Japan")
	assert.ErrorIs(t, err, ErrInvalidRequest, "the registration event records the edition with its year")
	assert.False(t, RegistrantsEditionOK("AGNTCon + MCPCon Japan"))
	assert.True(t, RegistrantsEditionOK("AGNTCon + MCPCon Japan 2026"))
}

func TestSplitStandardSuppressions(t *testing.T) {
	standard := []string{"1", "2", "3"}

	use, rest := SplitStandardSuppressions([]string{"3", "9", "1", "2", "9"}, standard)
	assert.True(t, use)
	assert.Equal(t, []string{"9"}, rest)

	use, rest = SplitStandardSuppressions([]string{"1", "2", "9"}, standard)
	assert.False(t, use, "a partial selection must not be widened to the whole union")
	assert.Equal(t, []string{"1", "2", "9"}, rest)

	use, rest = SplitStandardSuppressions([]string{"9"}, nil)
	assert.False(t, use, "with nothing resolved there is no standing list")
	assert.Equal(t, []string{"9"}, rest)
}

func TestSameIDSet(t *testing.T) {
	assert.True(t, SameIDSet([]string{"2", "1", "1"}, []string{"1", " 2"}))
	assert.False(t, SameIDSet([]string{"1"}, []string{"1", "2"}))
}

func TestMasterListWithSuppressionsFilter_ExcludesEveryListOnEveryPath(t *testing.T) {
	raw, err := MasterListWithSuppressionsFilter([]string{"10", "11"}, []string{"20", "21", "20"})
	require.NoError(t, err)
	s := string(raw)
	// Two inclusion paths, each excluding both lists (operator appears twice per filter).
	assert.Equal(t, 2*2*2, strings.Count(s, `"NOT_IN_LIST"`))
	assert.Equal(t, []string{"20", "21"}, ExcludedOnEveryPathIDs(raw))

	_, err = MasterListWithSuppressionsFilter([]string{"10"}, nil)
	assert.Error(t, err)
}

// Two long editions sharing a prefix select different registrants, so they must not share a
// list name: the second would otherwise reuse the first's list.
func TestCurrentRegistrantsName_LongEditionsSharingAPrefixDiffer(t *testing.T) {
	prefix := strings.Repeat("Open Source Summit ", 12)
	a := CurrentRegistrantsName(prefix + "Europe 2026")
	b := CurrentRegistrantsName(prefix + "Japan 2026")
	assert.NotEqual(t, a, b)
	assert.Equal(t, a, CurrentRegistrantsName(" "+prefix+"Europe 2026 "), "stable for the same edition")
	assert.LessOrEqual(t, utf8.RuneCountInString(a), MaxListNameLength)
	assert.True(t, strings.HasSuffix(a, " - Current Registrants"))
}

func TestRegistrantsListMatches(t *testing.T) {
	own, err := CurrentRegistrantsFilter("KubeCon NA 2026")
	require.NoError(t, err)
	assert.True(t, RegistrantsListMatches(own, "KubeCon NA 2026"))
	assert.True(t, RegistrantsListMatches(own, " KubeCon NA 2026 "), "only the ends are trimmed, as the filter's value is")
	assert.False(t, RegistrantsListMatches(own, "KubeCon  NA 2026"), "an internal double space is a different exact event_name")
	assert.False(t, RegistrantsListMatches(own, "kubecon na 2026"), "a different case is not proven to be the same event_name")
	assert.False(t, RegistrantsListMatches(own, "KubeCon NA 2025"))

	wider, err := MasterListFilter([]string{"123"})
	require.NoError(t, err)
	assert.False(t, RegistrantsListMatches(wider, "KubeCon NA 2026"), "a list that is not a registration list")
	assert.False(t, RegistrantsListMatches(nil, "KubeCon NA 2026"))

	// Only a slice of the edition: an AND that adds a contact property beside the registration.
	sliced := json.RawMessage(`{"filterBranchType":"OR","filters":[],"filterBranches":[{"filterBranchType":"AND",` +
		`"filters":[{"filterType":"PROPERTY","property":"country","operation":{"operator":"IS_EQUAL_TO","values":["Japan"]}}],` +
		`"filterBranches":[{"filterBranchType":"UNIFIED_EVENTS","operator":"HAS_COMPLETED","eventTypeId":"` + RegistrationEventTypeID + `","filterBranches":[],` +
		`"filters":[{"filterType":"PROPERTY","property":"event_name","operation":{"operator":"IS_EQUAL_TO","values":["KubeCon NA 2026"]}}]}]}]}`)
	editions, exclusive := RegistrationEvidence(sliced)
	require.True(t, exclusive, "precondition: the exclusive check alone accepts it")
	require.Equal(t, []string{"KubeCon NA 2026"}, editions)
	assert.False(t, RegistrantsListMatches(sliced, "KubeCon NA 2026"), "registrants outside Japan would not be suppressed")

	// Only a slice of the edition: an extra filter inside the registration event branch.
	ticketed := json.RawMessage(`{"filterBranchType":"OR","filters":[],"filterBranches":[{"filterBranchType":"AND","filters":[],` +
		`"filterBranches":[{"filterBranchType":"UNIFIED_EVENTS","operator":"HAS_COMPLETED","eventTypeId":"` + RegistrationEventTypeID + `","filterBranches":[],` +
		`"filters":[{"filterType":"PROPERTY","property":"event_name","operation":{"operator":"IS_EQUAL_TO","values":["KubeCon NA 2026"]}},` +
		`{"filterType":"PROPERTY","property":"ticket_type","operation":{"operator":"IS_EQUAL_TO","values":["Speaker"]}}]}]}]}`)
	_, exclusive = RegistrationEvidence(ticketed)
	require.True(t, exclusive, "precondition: the exclusive check alone accepts it")
	assert.False(t, RegistrantsListMatches(ticketed, "KubeCon NA 2026"), "only speakers would be suppressed")
}

func TestStandingFilterIsUnion(t *testing.T) {
	union, err := MasterListFilter([]string{"1", "2"})
	require.NoError(t, err)
	assert.True(t, StandingFilterIsUnion(union))
	assert.False(t, StandingFilterIsUnion(nil), "a missing filter is unknown, not a union of nothing")
	assert.False(t, StandingFilterIsUnion(json.RawMessage(`null`)), "nor is a null one")
	assert.True(t, StandingFilterIsUnion(json.RawMessage(`{"filterBranchType":"OR","filterBranches":[],"filters":[]}`)),
		"an explicit empty tree is a union of nothing")

	narrowed, err := MasterListWithSuppressionFilter([]string{"1"}, "2")
	require.NoError(t, err)
	assert.False(t, StandingFilterIsUnion(narrowed), "a NOT_IN_LIST condition a rewrite would drop")

	registrants, err := CurrentRegistrantsFilter("KubeCon NA 2026")
	require.NoError(t, err)
	assert.False(t, StandingFilterIsUnion(registrants), "an event condition a rewrite would drop")

	both := json.RawMessage(`{"filterBranchType":"OR","filters":[],"filterBranches":[{"filterBranchType":"AND","filterBranches":[],"filters":[` +
		`{"filterType":"IN_LIST","operator":"IN_LIST","listId":"1"},{"filterType":"IN_LIST","operator":"IN_LIST","listId":"2"}]}]}`)
	assert.False(t, StandingFilterIsUnion(both), "an AND of two lists covers neither")
	assert.False(t, StandingFilterIsUnion(json.RawMessage(`{`)))
}

// "event_name IS_EQUAL_TO X" that also admits events with no event_name is not an exact
// edition: it would suppress registrants of events that never recorded one.
func TestRegistrantsListMatches_RejectsIncludeObjectsWithNoValueSet(t *testing.T) {
	own, err := CurrentRegistrantsFilter("KubeCon NA 2026")
	require.NoError(t, err)
	require.True(t, RegistrantsListMatches(own, "KubeCon NA 2026"))
	loose := json.RawMessage(strings.Replace(string(own), `"includeObjectsWithNoValueSet":false`, `"includeObjectsWithNoValueSet":true`, 1))
	require.NotEqual(t, string(own), string(loose), "precondition: the flag is present to flip")
	assert.False(t, RegistrantsListMatches(loose, "KubeCon NA 2026"))
}

func TestCurrentRegistrantsFilter_YearlessEditionNamesTheField(t *testing.T) {
	_, err := CurrentRegistrantsFilter("AGNTCon + MCPCon Japan")
	var field *FieldError
	require.ErrorAs(t, err, &field)
	assert.Equal(t, "current_registrants_event", field.Field)
	assert.ErrorIs(t, err, ErrInvalidRequest)
	assert.Contains(t, err.Error(), "year")
}
