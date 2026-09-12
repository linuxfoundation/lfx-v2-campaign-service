// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package audience

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func utc(iso string) time.Time {
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		panic(err)
	}
	return t
}

// TestQuarterCode_BoundariesAndZone pins the two ways a YYQN segment goes wrong
// without erroring: an off-by-one at a quarter edge, and a local-zone reading.
// Either names a real HubSpot list for the wrong quarter, where next quarter's
// search will not find it and an operator builds a duplicate.
func TestQuarterCode_BoundariesAndZone(t *testing.T) {
	cases := map[string]string{
		"2026-01-01T00:00:00Z": "26Q1",
		"2026-03-31T23:59:59Z": "26Q1",
		"2026-04-01T00:00:00Z": "26Q2",
		"2026-06-30T23:59:59Z": "26Q2",
		"2026-07-01T00:00:00Z": "26Q3",
		"2026-09-30T23:59:59Z": "26Q3",
		"2026-10-01T00:00:00Z": "26Q4",
		"2026-12-31T23:59:59Z": "26Q4",
		"2027-01-01T00:00:00Z": "27Q1",
		// A year-end instant expressed in a positive-offset zone is already the
		// next year in UTC, which is the zone the code must read.
		"2026-12-31T23:30:00-05:00": "27Q1",
		"2026-01-01T00:30:00+05:30": "25Q4",
	}
	for iso, want := range cases {
		t.Run(iso, func(t *testing.T) {
			assert.Equal(t, want, QuarterCode(utc(iso)))
		})
	}
}

// TestEventQuarterCode_FirstParseableDateWins covers the skip-and-continue path.
// A page that states one unusable date followed by a good one must still produce
// the event's own quarter, not today's — falling through to `now` for a dated
// event is a silently wrong name, not a visible failure.
func TestEventQuarterCode_FirstParseableDateWins(t *testing.T) {
	now := utc("2026-09-11T12:00:00Z")

	assert.Equal(t, "26Q1", EventQuarterCode([]string{"2026-03-17", "2026-03-20"}, now))
	assert.Equal(t, "26Q2", EventQuarterCode([]string{"March 2026", "2026-05-04"}, now),
		"a prose date is not parseable and must be skipped, not allowed to force the now fallback")
	assert.Equal(t, "26Q3", EventQuarterCode([]string{"2026-02-30"}, now),
		"an ISO-SHAPED but impossible date must fall through to now rather than roll over to March")
	assert.Equal(t, "26Q3", EventQuarterCode(nil, now),
		"an undated event takes the current quarter; a blank segment would sort below every dated list")
}

// TestMasterListName_DropsBlankSegments pins the naming contract an operator
// scans the portal's list index with. A name carrying " -  - " reads as a bug and
// would not be recognised as their own asset.
func TestMasterListName_DropsBlankSegments(t *testing.T) {
	now := utc("2026-09-11T12:00:00Z")
	event := EventIdentity{Name: "KubeCon Europe", BrandShort: "CNCF", Dates: []string{"2026-03-17"}}

	assert.Equal(t, "26Q1 - CNCF - KubeCon Europe - Master", MasterListName(event, "", now),
		"a blank suffix defaults to Master")
	assert.Equal(t, "26Q1 - CNCF - KubeCon Europe - Combined Suppression",
		MasterListName(event, "Combined Suppression", now))
	assert.Equal(t, "26Q1 - KubeCon Europe - Master",
		MasterListName(EventIdentity{Name: "KubeCon Europe", Dates: event.Dates}, "", now))
	assert.Equal(t, "26Q3 - Audience - Master", MasterListName(EventIdentity{}, "", now),
		"with no identity at all the name still has to be a name, dated to the current quarter")
}

// TestCombinedSuppressionName_TracksTheMasterItSuppresses pins the pairing: the
// suppression list must be findable next to the master it belongs to, so it takes
// the master's own name as its base whenever there is one.
func TestCombinedSuppressionName_TracksTheMasterItSuppresses(t *testing.T) {
	now := utc("2026-09-11T12:00:00Z")
	event := EventIdentity{Name: "KubeCon Europe", BrandShort: "CNCF", Dates: []string{"2026-03-17"}}

	assert.Equal(t, "26Q1 - CNCF - KubeCon Europe - Master - Combined Suppression",
		CombinedSuppressionName(MasterListName(event, "", now), event, now))
	assert.Equal(t, "26Q1 - CNCF - KubeCon Europe - Combined Suppression",
		CombinedSuppressionName("   ", event, now),
		"with no master name yet, it falls back to a fully derived name rather than a bare suffix")
}

// TestQuarterRank_UndatedSortsLast pins the sentinel. QuarterRank feeds
// NewerFirst, and a missing code that ranked as year 0 quarter 0 would still beat
// nothing — (-1,-1) is what puts an unnamed asset below every dated one.
func TestQuarterRank_UndatedSortsLast(t *testing.T) {
	year, quarter := QuarterRank("26Q1 - CNCF - KubeCon Europe - Master")
	assert.Equal(t, 26, year)
	assert.Equal(t, 1, quarter)

	year, quarter = QuarterRank("KubeCon Europe master list")
	assert.Equal(t, -1, year)
	assert.Equal(t, -1, quarter)

	year, quarter = QuarterRank("26q4 - lowercase code")
	assert.Equal(t, 26, year, "the code is matched case-insensitively; portal names are hand-typed")
	assert.Equal(t, 4, quarter)
}

// TestNewerFirst_OrdersByQuarterThenSize pins the tie-break. Two lists from the
// same quarter are ordered by size, so the operator's default pick is the fuller
// one — and an undated list must never outrank a dated one.
func TestNewerFirst_OrdersByQuarterThenSize(t *testing.T) {
	q1 := ListCandidate{ListID: "1", Name: "26Q1 - Master", Size: 10}
	q2 := ListCandidate{ListID: "2", Name: "26Q2 - Master", Size: 10}
	q1Big := ListCandidate{ListID: "3", Name: "26Q1 - Master", Size: 900}
	undated := ListCandidate{ListID: "4", Name: "Master", Size: 9999}

	assert.True(t, NewerFirst(q2, q1))
	assert.False(t, NewerFirst(q1, q2))
	assert.True(t, NewerFirst(q1Big, q1))
	assert.True(t, NewerFirst(q1, undated),
		"a dated list outranks an undated one however large the undated one is")
}

// TestMasterListWithSuppressionFilter_RepeatsTheExclusionInEveryBranch pins both
// shape rules the function's own comment calls load-bearing. Neither failure
// errors: HubSpot accepts the wrong tree and builds a list that silently ignores
// suppression, which is a send to people who opted out.
func TestMasterListWithSuppressionFilter_RepeatsTheExclusionInEveryBranch(t *testing.T) {
	raw, err := MasterListWithSuppressionFilter([]string{"111", "222"}, "999")
	require.NoError(t, err)
	root := decode(t, raw)

	assert.Equal(t, "OR", root["filterBranchType"],
		"the inclusions are a union; AND at the root would intersect them and match almost nobody")
	assert.Empty(t, root["filters"],
		"a filter at the OR root would be OR'd with the branches and defeat the suppression")

	branches, ok := root["filterBranches"].([]any)
	require.True(t, ok)
	require.Len(t, branches, 2, "one AND branch per inclusion list")

	for i, wantList := range []string{"111", "222"} {
		branch, ok := branches[i].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "AND", branch["filterBranchType"])

		filters, ok := branch["filters"].([]any)
		require.True(t, ok)
		require.Len(t, filters, 2, "each branch pairs its inclusion with the suppression exclusion")

		include, ok := filters[0].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "IN_LIST", include["filterType"])
		assert.Equal(t, "IN_LIST", include["operator"])
		assert.Equal(t, wantList, include["listId"])

		exclude, ok := filters[1].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "IN_LIST", exclude["filterType"],
			"filterType stays IN_LIST; NOT_IN_LIST is not a filter type and HubSpot ignores the whole filter")
		assert.Equal(t, "NOT_IN_LIST", exclude["operator"], "only the operator flips")
		assert.Equal(t, "999", exclude["listId"])

		operation, ok := exclude["operation"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "NOT_IN_LIST", operation["operator"],
			"the nested operation must agree with the filter, or HubSpot reads the two as conflicting")
	}
}

// TestMasterListWithSuppressionFilter_RefusesUnbuildableInput pins the three
// refusals. Each one, if allowed through, creates a REAL list in the production
// portal: an empty union matches nobody, a blank suppression id silently drops
// suppression, and a blank inclusion id builds a branch that matches everyone.
func TestMasterListWithSuppressionFilter_RefusesUnbuildableInput(t *testing.T) {
	_, err := MasterListWithSuppressionFilter(nil, "999")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one inclusion list")

	_, err = MasterListWithSuppressionFilter([]string{"111"}, "  ")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "suppression list id")

	_, err = MasterListWithSuppressionFilter([]string{"111", " "}, "999")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "blank list id")
}

// TestUniqueIDs_PreservesFirstSeenOrder pins that de-duplication does not reorder.
// The order is the operator's own selection order, which is what the review grid
// and the composed list's branches are read in.
func TestUniqueIDs_PreservesFirstSeenOrder(t *testing.T) {
	assert.Equal(t, []string{"222", "111", "333"},
		UniqueIDs([]string{" 222 ", "111", "222", "", "333", "111", "   "}))
	assert.Empty(t, UniqueIDs(nil))
}

// TestExclusionIDs_InclusionWins pins the resolution of a list selected on BOTH
// sides. Suppressing it would exclude the very contacts the operator asked to
// include, and every branch carries the suppression list — so one doubly-selected
// list would empty the whole union.
func TestExclusionIDs_InclusionWins(t *testing.T) {
	assert.Equal(t, []string{"444"},
		ExclusionIDs([]string{"111", "444", "222"}, []string{"111", "222"}))
	assert.Empty(t, ExclusionIDs([]string{"111", "111"}, []string{"111"}))
	assert.Equal(t, []string{"111"}, ExclusionIDs([]string{"111", " 111 "}, nil),
		"the exclusions are de-duplicated even with nothing to clash against")
}

// TestExceedsExactCap_IsStrictlyGreater pins the boundary the comment argues for:
// MembershipPageSize * MembershipMaxPages is EXACTLY UnionExactCap records, so an
// estimate of exactly the cap is countable. Testing it off by one in either
// direction either refuses a countable union or attempts one page too many.
func TestExceedsExactCap_IsStrictlyGreater(t *testing.T) {
	assert.False(t, ExceedsExactCap(UnionExactCap-1))
	assert.False(t, ExceedsExactCap(UnionExactCap))
	assert.True(t, ExceedsExactCap(UnionExactCap+1))
	assert.Equal(t, UnionExactCap, MembershipPageSize*MembershipMaxPages,
		"the cap is derived from the paging budget; changing one without the other makes the boundary a lie")
}

// TestPreviewCount_NeverReportsAnEstimateAsExact pins the invariant the whole type
// exists for: `Exact` false must always carry a reason, and an inexact count must
// never be presented as a live number. The operator decides whether to mail
// thousands of people from this field.
func TestPreviewCount_NeverReportsAnEstimateAsExact(t *testing.T) {
	empty := EmptyPreviewCount()
	assert.True(t, empty.Exact)
	assert.Zero(t, empty.Count)
	assert.Empty(t, empty.Reason)

	exact := ExactPreviewCount(18_400, 25_000)
	assert.True(t, exact.Exact)
	assert.Equal(t, 18_400, exact.Count)
	assert.Equal(t, 25_000, exact.Estimate,
		"the sum is kept beside the union so the UI can show how much overlap was removed")
	assert.Empty(t, exact.Reason)

	over := OverCapPreviewCount(31_500)
	assert.False(t, over.Exact)
	assert.Equal(t, 31_500, over.Count)
	assert.Contains(t, over.Reason, "31,500")
	assert.Contains(t, over.Reason, "25,000",
		"the reason must name the limit; 'too big' alone leaves the operator nothing to act on")

	degraded := DegradedPreviewCount(900)
	assert.False(t, degraded.Exact)
	assert.Equal(t, 900, degraded.Count)
	assert.NotEmpty(t, degraded.Reason,
		"a truncated sweep UNDERCOUNTS, and understating an email's reach must never look exact")
}

// TestWithThousands_GroupsEveryBoundary pins the separator at each digit length.
// This string goes in front of an operator deciding on a send, where 25000 read
// as 2,500 is a real misreading.
func TestWithThousands_GroupsEveryBoundary(t *testing.T) {
	cases := map[int]string{
		0: "0", 999: "999", 1000: "1,000", 25000: "25,000",
		999999: "999,999", 1000000: "1,000,000", -25000: "-25,000",
	}
	for n, want := range cases {
		assert.Equal(t, want, withThousands(n))
	}
}

// An unknown list size must not be summed as zero.
//
// hubspot.List.Size is a plain int, so an OMITTED size and a genuinely empty list are both
// 0 — `sizeOf` returns *int64 precisely to keep them apart. Summing an omitted size makes
// the estimate short by that whole list, and if the membership sweep then fails, that short
// sum is handed back as DegradedPreviewCount's "safe" over-count. Understating reach is the
// one direction with no recovery after a send, so no number is offered at all.
func TestUnknownSizePreviewCountOffersNoNumber(t *testing.T) {
	got := UnknownSizePreviewCount()

	if got.Exact {
		t.Error("an unknown size can never yield an exact count")
	}
	if got.Count != 0 || got.Estimate != 0 {
		t.Errorf("no number may be offered when a size is unknown; got count=%d estimate=%d", got.Count, got.Estimate)
	}
	if !strings.Contains(got.Reason, "did not report a size") {
		t.Errorf("the reason must tell the operator WHY there is no total, so they can check the lists; got %q", got.Reason)
	}
}
