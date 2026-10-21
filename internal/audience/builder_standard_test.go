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
