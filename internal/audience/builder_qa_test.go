// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package audience

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// inList builds a membership filter. filterType is "IN_LIST" for BOTH directions
// — which is the trap ReferencedListIDs exists to avoid — so these cases set only
// the operator.
func inList(id, operator string) ListFilter {
	return ListFilter{FilterType: "IN_LIST", ListID: json.Number(id), Operator: operator}
}

// TestParseListRef_AcceptsWhatTheOperatorActuallyHas pins both URL spellings.
// `objectLists` is what the modern editor puts in the address bar; dropping it
// would send every pasted URL down the name-resolution path and then report a
// verdict for whichever list happened to match the URL text.
func TestParseListRef_AcceptsWhatTheOperatorActuallyHas(t *testing.T) {
	cases := []struct {
		ref      string
		wantID   string
		wantName bool
	}{
		{"https://app.hubspot.com/contacts/8112310/objectLists/30781/filters", "30781", false},
		{"https://app.hubspot.com/contacts/8112310/lists/30781", "30781", false},
		{" 30781 ", "30781", false},
		{"26Q1 - CNCF - KubeCon Europe - Master", "", true},
		{"   ", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.ref, func(t *testing.T) {
			id, isName := ParseListRef(tc.ref)
			assert.Equal(t, tc.wantID, id)
			assert.Equal(t, tc.wantName, isName)
		})
	}
}

// TestPickNameMatches_AmbiguityResolvesToNothing pins the refusal. Picking the
// first of several partial matches would run QA on a list the operator never
// meant and then report the verdict under the name they typed — a PASS for the
// wrong asset.
func TestPickNameMatches_AmbiguityResolvesToNothing(t *testing.T) {
	hits := []ListCandidate{
		{ListID: "1", Name: "26Q1 - CNCF - KubeCon Europe - Master"},
		{ListID: "2", Name: "26Q1 - CNCF - KubeCon Europe - Master - Combined Suppression"},
	}

	chosen, ambiguous := PickNameMatches("KubeCon Europe", hits)
	assert.Nil(t, chosen)
	assert.Len(t, ambiguous, 2, "several partial matches must be handed back for the operator to disambiguate")

	chosen, ambiguous = PickNameMatches("  26q1 - cncf - kubecon europe - master  ", hits)
	require.NotNil(t, chosen, "an exact case-insensitive match is unambiguous even when another list contains it")
	assert.Equal(t, "1", chosen.ListID)
	assert.Empty(t, ambiguous)

	chosen, ambiguous = PickNameMatches("nothing like this", nil)
	assert.Nil(t, chosen)
	assert.Empty(t, ambiguous, "no hits is 'not found', which is a different answer from 'ambiguous'")
}

// TestReferencedListIDs_ReadsTheOperatorNotTheType pins the whole reason this
// helper exists. Both directions carry filterType IN_LIST, so a reader that keyed
// off the type would count every exclusion as an inclusion and turn the
// suppression check inside out — reporting suppression as applied on a list that
// includes the opt-out list.
func TestReferencedListIDs_ReadsTheOperatorNotTheType(t *testing.T) {
	filters := []ListFilter{
		inList("111", "IN_LIST"),
		inList("999", "NOT_IN_LIST"),
		inList("111", "IN_LIST"),
		inList("222", "IN_LIST"),
		{FilterType: "PROPERTY", Property: "country", Operator: "IN_LIST"},
	}

	assert.Equal(t, []string{"111", "222"}, ReferencedListIDs(filters, "IN_LIST"),
		"de-duplicated in first-seen order, and a PROPERTY filter is not a list reference")
	assert.Equal(t, []string{"999"}, ReferencedListIDs(filters, "NOT_IN_LIST"))
}

// TestCheckSignalMapping_DropsExclusionsBeforeJudging pins the subtraction the
// check's comment argues for. A geography-only list that also suppresses opt-outs
// must still FAIL: counting the exclusion's signature as evidence would rescue
// exactly the purchased-list-shaped audience this check exists to catch.
func TestCheckSignalMapping_DropsExclusionsBeforeJudging(t *testing.T) {
	names := map[string]string{"999": "LF Global Opt-Outs"}
	filters := []ListFilter{
		{FilterType: "PROPERTY", Property: "country"},
		inList("999", "NOT_IN_LIST"),
	}

	got := CheckSignalMapping(filters, names)
	assert.Equal(t, VerdictFail, got.Verdict)
	require.Len(t, got.Findings, 1)
	assert.Equal(t, SeverityHigh, got.Findings[0].Severity)
	assert.NotEmpty(t, got.Findings[0].Fix, "a finding an operator cannot act on trains them to ignore the report")
}

// TestCheckSignalMapping_Outcomes covers the three remaining answers. The
// membership case is the one worth naming: "IN_LIST 12345" carries no signal at
// all, and only the referenced list's NAME makes it classifiable.
func TestCheckSignalMapping_Outcomes(t *testing.T) {
	// Both spellings of the same list, because the portal uses both and the hint is
	// a stem: "Registrants" is the commoner name, and a stem that only matched
	// "Registration" would report NEEDS VERIFY on the clearest possible PASS.
	for _, name := range []string{"KubeCon Europe 2026 Registrants", "KubeCon Europe 2026 Registration"} {
		t.Run("engagement via the referenced list name "+name, func(t *testing.T) {
			got := CheckSignalMapping(
				[]ListFilter{inList("111", "IN_LIST")},
				map[string]string{"111": name},
			)
			assert.Equal(t, VerdictPass, got.Verdict)
			assert.Empty(t, got.Findings)
		})
	}

	t.Run("a manual list has nothing to inspect", func(t *testing.T) {
		got := CheckSignalMapping(nil, nil)
		assert.Equal(t, VerdictNeedsVerify, got.Verdict)
		require.Len(t, got.Findings, 1)
		assert.Equal(t, SeverityMedium, got.Findings[0].Severity)
		assert.Contains(t, got.Findings[0].Message, "static/manual")
	})

	t.Run("an unreadable filter is neither a pass nor a fail", func(t *testing.T) {
		got := CheckSignalMapping([]ListFilter{{FilterType: "SOMETHING_NEW"}, {FilterType: "PROPERTY", Property: "hs_lead_score"}}, nil)
		assert.Equal(t, VerdictNeedsVerify, got.Verdict,
			"an unrecognised shape must not FAIL a good list, and must not PASS a bad one")
	})

	t.Run("an all-empty signature set does not pass", func(t *testing.T) {
		got := CheckSignalMapping([]ListFilter{{}, {}}, nil)
		assert.Equal(t, VerdictFail, got.Verdict,
			"filters carrying none of the fields we read count as firmographic-or-unknown, never as engagement")
	})
}

// TestCheckSuppression_EUWithoutGDPRIsTheOnlyFail pins the one CRITICAL in this
// service. It is the single condition that blocks rather than warns, so a
// severity downgrade here converts a compliance stop into a line of advice.
func TestCheckSuppression_EUWithoutGDPRIsTheOnlyFail(t *testing.T) {
	got := CheckSuppression([]string{"LF Global Opt-Outs"}, true, false)
	assert.Equal(t, VerdictFail, got.Verdict)
	assert.False(t, got.AppliedGDPR)
	assert.True(t, got.AppliedOptOut)
	require.NotEmpty(t, got.Findings)
	assert.Equal(t, SeverityCritical, got.Findings[0].Severity)
	assert.Contains(t, got.Findings[0].Message, "GDPR")

	clean := CheckSuppression([]string{"LF Events GDPR Suppression", "LF Global Opt-Outs"}, true, false)
	assert.Equal(t, VerdictPass, clean.Verdict)
	assert.True(t, clean.AppliedGDPR)
	assert.True(t, clean.AppliedOptOut)
	assert.Empty(t, clean.Findings)
}

// TestCheckSuppression_DoesNotDoublePenaliseOneCause pins the finding-count
// discipline the switch encodes. A list that excludes something non-GDPR-named
// gets ONE finding (the opt-out one); putting three findings on one cause buries
// the one that matters.
func TestCheckSuppression_DoesNotDoublePenaliseOneCause(t *testing.T) {
	withOther := CheckSuppression([]string{"Hard Bounces"}, false, false)
	require.Len(t, withOther.Findings, 1)
	assert.Equal(t, SeverityHigh, withOther.Findings[0].Severity)
	assert.Contains(t, withOther.Findings[0].Message, "Opt-Out")

	withNothing := CheckSuppression(nil, false, false)
	require.Len(t, withNothing.Findings, 2,
		"a list that excludes NOTHING is told about both the missing GDPR list and the missing opt-out list")
	assert.Equal(t, VerdictNeedsVerify, withNothing.Verdict,
		"neither of those is a CRITICAL, so the check warns rather than blocks")
}

// TestCheckSuppression_CanadaAsksForConfirmationNotForAList pins the deliberate
// oddity: this portal has no CASL suppression list under any name, so the check
// cannot ask for one to be applied. A remediation the operator cannot perform is
// worse than none.
func TestCheckSuppression_CanadaAsksForConfirmationNotForAList(t *testing.T) {
	got := CheckSuppression(nil, false, true)
	var casl *Finding
	for i := range got.Findings {
		if assert.NotEmpty(t, got.Findings[i].Fix) && strings.Contains(got.Findings[i].Message, "Canada") {
			casl = &got.Findings[i]
		}
	}
	require.NotNil(t, casl, "targeting Canada with no suppression of any kind must be reported")
	assert.Equal(t, SeverityMedium, casl.Severity)
	assert.Contains(t, casl.Fix, "confirm")

	quiet := CheckSuppression([]string{"LF Master Exclusion", "LF Global Opt-Outs"}, false, true)
	for _, f := range quiet.Findings {
		assert.NotContains(t, f.Message, "Canada",
			"a generic suppression exclusion satisfies the Canada check; repeating it would be noise")
	}
}

// TestCheckExclusionCompleteness_ZeroExclusionsFails pins the coarse check. Zero
// exclusions means no unsubscribe, no opt-out and no non-marketable filter, which
// is a send that should not go out whichever specific list is missing.
func TestCheckExclusionCompleteness_ZeroExclusionsFails(t *testing.T) {
	none := CheckExclusionCompleteness([]ListFilter{inList("111", "IN_LIST")})
	assert.Equal(t, VerdictFail, none.Verdict)
	assert.Zero(t, none.ExclusionCount)
	require.Len(t, none.Findings, 1)
	assert.Equal(t, SeverityHigh, none.Findings[0].Severity)

	some := CheckExclusionCompleteness([]ListFilter{
		inList("111", "IN_LIST"),
		inList("999", "NOT_IN_LIST"),
		inList("998", "NOT_IN_LIST"),
	})
	assert.Equal(t, VerdictPass, some.Verdict)
	assert.Equal(t, 2, some.ExclusionCount)
	assert.Empty(t, some.Findings)
}

// TestVerdictFromFindings_OnlyCriticalFails pins the mapping. A HIGH that failed
// the check would block sends on findings the operator is meant to look at, and
// operators route around a gate that blocks everything.
func TestVerdictFromFindings_OnlyCriticalFails(t *testing.T) {
	assert.Equal(t, VerdictPass, VerdictFromFindings(nil))
	assert.Equal(t, VerdictNeedsVerify, VerdictFromFindings([]Finding{{Severity: SeverityHigh}}))
	assert.Equal(t, VerdictNeedsVerify, VerdictFromFindings([]Finding{{Severity: SeverityMedium}}))
	assert.Equal(t, VerdictFail, VerdictFromFindings([]Finding{{Severity: SeverityMedium}, {Severity: SeverityCritical}}),
		"a CRITICAL anywhere in the set fails the check, not only as the first finding")
}

// TestCombineVerdicts_TakesTheWorst pins the roll-up. A list-level PASS is read as
// permission to send, so one failing check must not be averaged away.
func TestCombineVerdicts_TakesTheWorst(t *testing.T) {
	assert.Equal(t, VerdictPass, CombineVerdicts())
	assert.Equal(t, VerdictPass, CombineVerdicts(VerdictPass, VerdictPass))
	assert.Equal(t, VerdictNeedsVerify, CombineVerdicts(VerdictPass, VerdictNeedsVerify))
	assert.Equal(t, VerdictFail, CombineVerdicts(VerdictNeedsVerify, VerdictFail, VerdictPass))
}

// TestOrderFindings_SeverityFirstStableWithin pins that the order is part of the
// answer. The reader acts on the top of the report, so a CRITICAL missing-GDPR
// finding sitting below a MEDIUM "confirm this manually" is a compliance failure
// an operator can plausibly miss — while reordering WITHIN a severity would claim
// a priority the checks never expressed.
func TestOrderFindings_SeverityFirstStableWithin(t *testing.T) {
	in := []Finding{
		{Severity: SeverityMedium, Message: "medium first"},
		{Severity: SeverityHigh, Message: "high first"},
		{Severity: SeverityCritical, Message: "the critical"},
		{Severity: SeverityMedium, Message: "medium second"},
		{Severity: Severity("FUTURE"), Message: "unknown severity"},
		{Severity: SeverityHigh, Message: "high second"},
	}
	got := OrderFindings(in)

	assert.Equal(t, []string{
		"the critical", "high first", "high second", "medium first", "medium second", "unknown severity",
	}, messages(got))
	assert.Equal(t, "medium first", in[0].Message, "the input slice must not be reordered in place")
}

// messages is the findings' messages in order, which is what the ordering
// assertions actually read.
func messages(findings []Finding) []string {
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, f.Message)
	}
	return out
}

// Two lists sharing an exact name must disambiguate, not silently pick one. Before this,
// `len(exact) > 0` suppressed the ambiguity branch, so `matches[0]` resolved to whichever
// order HubSpot happened to return -- QA would then audit an arbitrary list and report a
// verdict under the name the operator typed. Duplicate names are reachable in a real
// portal, especially after an ambiguous create leaves a second list behind.
func TestPickNameMatchesDisambiguatesDuplicateExactNames(t *testing.T) {
	hits := []ListCandidate{
		{ListID: "1", Name: "KubeCon NA 2026 - Registrants"},
		{ListID: "2", Name: "KubeCon NA 2026 - Registrants"},
	}

	chosen, ambiguous := PickNameMatches("KubeCon NA 2026 - Registrants", hits)

	if chosen != nil {
		t.Errorf("two lists share this exact name, so QA picked an arbitrary one (%s) instead of asking", chosen.ListID)
	}
	if len(ambiguous) != 2 {
		t.Fatalf("want both candidates offered for disambiguation, got %d", len(ambiguous))
	}
}

// The single-exact-match case must still resolve, or the fix above would make every
// lookup ambiguous and the QA panel unusable.
func TestPickNameMatchesStillResolvesASingleExactMatch(t *testing.T) {
	hits := []ListCandidate{
		{ListID: "1", Name: "KubeCon NA 2026 - Registrants"},
		{ListID: "2", Name: "KubeCon NA 2026 - Speakers"},
	}

	chosen, ambiguous := PickNameMatches("KubeCon NA 2026 - Registrants", hits)

	if chosen == nil || chosen.ListID != "1" {
		t.Fatalf("a single exact match must resolve; got %v", chosen)
	}
	if len(ambiguous) != 0 {
		t.Errorf("a single exact match must not be ambiguous; got %d candidates", len(ambiguous))
	}
}
