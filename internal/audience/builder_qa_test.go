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

	twoExact := []ListCandidate{
		{ListID: "3", Name: "26Q1 - CNCF - KubeCon Europe - Master"},
		{ListID: "4", Name: "26Q1 - CNCF - KubeCon Europe - Master"},
	}
	chosen, ambiguous = PickNameMatches("26Q1 - CNCF - KubeCon Europe - Master", twoExact)
	assert.Nil(t, chosen, "HubSpot does not enforce unique list names, so two exact matches must not resolve to either one")
	assert.Len(t, ambiguous, 2, "both exact matches must be handed back for the operator to disambiguate")
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

// TestCheckCurrentRegistrants pins the A-02 gap: an event's own registration list offered as
// an INCLUDE, with all three existing checks passing.
//
// Verified on AGNTCon + MCPCon North America (2026-09-30): the edition's registration list,
// 1,346 contacts, was offered as an include, ticking it raised nothing, and QA passed. The
// other three checks read consent suppression and filter shape; none reads the audience's
// own intent.
//
// The table is the whole point. Two earlier versions of this predicate flagged lists that are
// CORRECT to include, and a check that cries wolf on the common case gets switched off:
//   - event tokens alone  -> flagged a PAST edition (the strongest evidence a first edition
//     has) and a SIBLING region (a different event).
//   - tokens + year       -> still flagged the sibling, which shares both.
//   - tokens + year + the FULL token set -> only this edition. The generic tier carries the
//     region, so requiring every token is what separates NA from Japan.
func TestCheckCurrentRegistrants(t *testing.T) {
	const eventName = "AGNTCon + MCPCon North America 2026"

	cases := []struct {
		name     string
		listName string
		wantFail bool
		why      string
	}{
		{
			name:     "this edition's own registration list",
			listName: "26Q2 AGNTCon + MCPCon North America 2026 Event Registration",
			wantFail: true,
			why:      "every invitation would go to someone who already registered",
		},
		{
			name:     "this edition's attendees, named differently",
			listName: "AGNTCon + MCPCon North America 2026 - Attendees",
			wantFail: true,
			why:      "attendee and registrant are the same population for a pre-event send",
		},
		{
			name:     "a PAST edition's registrants",
			listName: "25Q2 AGNTCon North America 2025 Event Registration",
			wantFail: false,
			why:      "the strongest evidence a first-edition send has; flagging it would be wrong far more often than right",
		},
		{
			// The case that binds the YEAR requirement specifically. This past edition carries
			// the IDENTICAL token set -- overlap=4, the same as the current edition -- so the
			// full-token-set rule alone would flag it. The 2025 case below drops a token
			// ("mcpcon") and so is caught by that rule instead, which is why it does not
			// exercise the year at all.
			name:     "a past edition with the identical token set",
			listName: "24Q2 AGNTCon + MCPCon North America 2024 Event Registration",
			wantFail: false,
			why:      "only the year separates this from the current edition",
		},
		{
			name:     "a sibling region's registrants",
			listName: "26Q1 AGNTCon + MCPCon Japan 2026 Event Registration",
			wantFail: false,
			why:      "same series and same year, different event",
		},
		{
			name:     "an unrelated event's registrants",
			listName: "26Q2 PyTorch Conference 2026 Event Registration",
			wantFail: false,
			why:      "a portfolio holds many registration lists",
		},
		{
			name:     "this event's web visitors",
			listName: "26Q2 - AGNTCon North America Web Visitors",
			wantFail: false,
			why:      "not a registration list at all",
		},
		{
			name:     "this event's speakers",
			listName: "26Q2 AGNTCon + MCPCon North America 2026 Speakers",
			wantFail: false,
			why:      "a speaker has not necessarily registered",
		},
		{
			name:     "a portfolio suppression list",
			listName: "LF Global Opt-Outs",
			wantFail: false,
			why:      "carries no registration signal and no event tokens",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckCurrentRegistrants(eventName, []string{tc.listName})

			if tc.wantFail {
				if got.Verdict != VerdictFail {
					t.Fatalf("verdict = %q, want FAIL: %s", got.Verdict, tc.why)
				}
				if len(got.Findings) != 1 {
					t.Fatalf("findings = %d, want 1", len(got.Findings))
				}
				if got.Findings[0].Severity != SeverityCritical {
					t.Errorf("severity = %q, want CRITICAL; a send to people who already hold a ticket is not advisory", got.Findings[0].Severity)
				}
				if !strings.Contains(got.Findings[0].Message, tc.listName) {
					t.Errorf("message does not name the offending list, so the operator cannot act on it: %q", got.Findings[0].Message)
				}
				return
			}
			if got.Verdict != VerdictPass {
				t.Fatalf("verdict = %q, want PASS: %s", got.Verdict, tc.why)
			}
		})
	}
}

// TestNamesRegistrants pins the predicate that replaced a `strings.Contains` hint scan.
//
// `strings.Contains(name, "registered")` could not tell this event's registrants from the people
// who have NOT registered, because "unregistered" contains "registered". Every name in the negated
// group below matched before this, so "KubeCon 2026 - Unregistered Prospects" -- the list a
// registration-push send most obviously SHOULD include -- failed QA as CRITICAL.
//
// Enumerated rather than sampled, in both directions. The plain group carries the names a first
// attempt broke: it looked for a negating word ANYWHERE in the name, which made "Registrants
// (De-duplicated)" and "Registration - No Discount" undecidable. The negation has to sit ON the
// registration term, which is a question about structure; "does any negating spelling appear" is a
// denylist, and a denylist over spellings does not converge.
func TestNamesRegistrants(t *testing.T) {
	negated := []string{
		"KubeCon 2026 - Unregistered Prospects",
		"Not Registered",
		"KubeCon NA 2026 - Non-Registered Leads",
		"Unregistered - KubeCon Europe 2026",
		"KubeCon 2026 - Never Attended",
		"KubeCon 2026 - Not Attendees",
		"KubeCon 2026 - Deregistered",
		"KubeCon 2026 - Pre-registration Interest",
		"KubeCon 2026 - Unattended Sessions",
	}
	plain := []string{
		"26Q1 KubeCon Europe 2026 Event Registration",
		"KubeCon NA 2026 - Registrants",
		"KubeCon NA 2026 - Attendees",
		"KubeCon NA 2026 - Attended",
		"KubeCon 2026 Registered Users",
		// Each of these carries a word a spelling denylist catches, negating something else.
		"KubeCon NA 2026 - Registrants (De-duplicated)",
		"Open Source Summit 2026 Registration - No Discount",
		"KubeCon 2026 Registration - Denver",
		"PyTorch Conference 2026 Registered - Nonprofit Rate",
		"KubeCon 2026 Attendees - Presenters",
		"KubeCon 2026 Registrants - Premium",
		"KubeCon EU 2026 - Event Registration (Unpaid)",
		"KubeCon 2026 Attendees - Denmark",
		// ANY un-negated occurrence decides it, not the first one.
		"Not Registered - Event Registration List",
	}
	none := []string{"KubeCon 2026 - Prospects", "KubeCon 2026", "LF Global Opt-Outs"}

	for _, name := range negated {
		if got := namesRegistrants(name); got != registrantNamingNegated {
			t.Errorf("namesRegistrants(%q) = %v, want negated; the people who have NOT registered are who this send is FOR", name, got)
		}
	}
	for _, name := range plain {
		if got := namesRegistrants(name); got != registrantNamingPlain {
			t.Errorf("namesRegistrants(%q) = %v, want plain; a legitimate registrant list was made undecidable", name, got)
		}
	}
	for _, name := range none {
		if got := namesRegistrants(name); got != registrantNamingNone {
			t.Errorf("namesRegistrants(%q) = %v, want none", name, got)
		}
	}
}

// TestCheckCurrentRegistrants_NegatedRegistrationNaming pins what the check REPORTS for a name it
// cannot read, which is neither a pass nor a CRITICAL.
//
// Flagging it CRITICAL fails QA for the right list; passing it silently hides the inclusion this
// check exists to catch. Undecidable is the honest answer, and it is already how this check reports
// a region it cannot resolve.
func TestCheckCurrentRegistrants_NegatedRegistrationNaming(t *testing.T) {
	const eventName = "AGNTCon + MCPCon North America 2026"

	got := CheckCurrentRegistrants(eventName, []string{"26Q2 AGNTCon + MCPCon North America 2026 - Unregistered Prospects"})

	if got.Verdict != VerdictNeedsVerify {
		t.Fatalf("verdict = %q, want NEEDS VERIFY; a registration-push send's own target list is not a QA failure", got.Verdict)
	}
	if len(got.Findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(got.Findings))
	}
	if got.Findings[0].Severity != SeverityMedium {
		t.Errorf("severity = %q, want MEDIUM; an unreadable name is not the same as a confirmed inclusion", got.Findings[0].Severity)
	}
	if !strings.Contains(got.Findings[0].Message, "Unregistered Prospects") {
		t.Errorf("message does not name the list the operator has to check: %q", got.Findings[0].Message)
	}
	if strings.Contains(got.Findings[0].Message, "names no region") {
		t.Errorf("reported the REGION cause for a naming problem, so the operator would check the wrong thing: %q", got.Findings[0].Message)
	}
}

// TestCheckCurrentRegistrants_MatchesThisServicesOwnNamingConvention pins the false NEGATIVE
// that a raw year substring produced.
//
// `MasterListName` writes a two-digit QUARTER code, not a four-digit year --
// `builder_master_test.go` pins "26Q1 - CNCF - KubeCon Europe - Master". So a list this service
// created itself carries no "2026" anywhere, and the check PASSED on it. Measured before fixing:
// "26Q1 - CNCF - KubeCon Europe - Registrants" returned PASS while the same name with "2026"
// spliced in returned FAIL -- the check was keyed on an accident of one list's name.
//
// This is the dangerous direction. A false positive gets the check switched off; a false negative
// lets the send go out to people who already registered, which is the whole point of check 4.
func TestCheckCurrentRegistrants_MatchesThisServicesOwnNamingConvention(t *testing.T) {
	const eventName = "KubeCon Europe 2026"

	for _, listName := range []string{
		"26Q1 - CNCF - KubeCon Europe - Event Registration",
		"26Q1 - CNCF - KubeCon Europe - Registrants",
		"26Q1 - CNCF - KubeCon Europe 2026 - Registrants",
	} {
		t.Run(listName, func(t *testing.T) {
			if got := CheckCurrentRegistrants(eventName, []string{listName}); got.Verdict != VerdictFail {
				t.Fatalf("verdict = %q, want FAIL; this is a list this service names itself", got.Verdict)
			}
		})
	}

	// And the quarter code is compared on its YEAR digits, not by substring: a PRIOR edition
	// written in the same convention must still pass.
	prior := CheckCurrentRegistrants(eventName, []string{"25Q4 - CNCF - KubeCon Europe - Registrants"})
	if prior.Verdict == VerdictFail {
		t.Error("a prior edition in the same naming convention must not be FAILed")
	}
}

// TestCheckCurrentRegistrants_WithoutAnEventNameDoesNotRun pins the difference between a
// question nobody asked and one this check cannot answer.
//
// No event name means the caller did not ask. Returning NEEDS VERIFY there would flip EVERY
// existing audit's Overall -- today no caller supplies an event name, since the UI is not wired
// -- and append a finding to audits the check has nothing to say about. A check that cannot run
// must not make unrelated results look worse; that is how a check gets removed.
//
// The zero Check is what `currentRegistrantsResult` omits from the wire and what
// `CombineVerdicts` treats as inert.
func TestCheckCurrentRegistrants_WithoutAnEventNameDoesNotRun(t *testing.T) {
	got := CheckCurrentRegistrants("", []string{"26Q2 AGNTCon + MCPCon North America 2026 Event Registration"})

	if got.Verdict != "" {
		t.Fatalf("verdict = %q, want the zero Check; an absent event name is a question nobody asked", got.Verdict)
	}
	if len(got.Findings) != 0 {
		t.Fatalf("findings = %d, want 0; there is nothing to report about a check that did not run", len(got.Findings))
	}

	// And the consequence that made this blocking: an existing caller's roll-up is unchanged.
	if overall := CombineVerdicts(VerdictPass, VerdictPass, VerdictPass, got.Verdict); overall != VerdictPass {
		t.Errorf("Overall = %q, want PASS; check 4 must not change a verdict for callers that do not use it", overall)
	}
}

// TestCheckCurrentRegistrants_AnAllGenericEventNameIsNotAPass pins the case no token rule can
// decide.
//
// "Open Source Summit" is entirely portfolio-common words, so its distinctive tier is EMPTY and
// the event name carries no region of its own. Measured: the edition's own registration list and
// "Open Source Summit Japan 2026" BOTH score overlap=3 against the same three generic tokens.
// Nothing separates them.
//
// Flagging would hit a sibling's list, which is correct to include. Passing would miss this
// edition's own, which is the defect the check exists for. Neither is defensible, so it reports
// NEEDS VERIFY and names what the operator has to confirm.
func TestCheckCurrentRegistrants_AnAllGenericEventNameIsNotAPass(t *testing.T) {
	got := CheckCurrentRegistrants("Open Source Summit 2026", []string{"26Q2 Open Source Summit 2026 Event Registration"})

	if got.Verdict != VerdictNeedsVerify {
		t.Fatalf("verdict = %q, want NEEDS VERIFY", got.Verdict)
	}
	if len(got.Findings) == 0 {
		t.Fatal("a NEEDS VERIFY with no finding tells the operator nothing to do")
	}

	// And the sibling that motivated it: the same name must not be FAILed either.
	sibling := CheckCurrentRegistrants("Open Source Summit 2026", []string{"26Q2 Open Source Summit Japan 2026 Event Registration"})
	if sibling.Verdict == VerdictFail {
		t.Error("a sibling region's list must never be FAILed; including it is a legitimate choice")
	}
}

// TestCheckCurrentRegistrants_AnEventNameWithNoYearIsNotAPass is the same reasoning one step
// in: with no year in the event name, this edition cannot be told from an earlier one, and
// substituting the current year would flag a list the operator may have chosen on purpose.
func TestCheckCurrentRegistrants_AnEventNameWithNoYearIsNotAPass(t *testing.T) {
	got := CheckCurrentRegistrants("AGNTCon + MCPCon North America", []string{"26Q2 AGNTCon + MCPCon North America 2026 Event Registration"})

	if got.Verdict != VerdictNeedsVerify {
		t.Fatalf("verdict = %q, want NEEDS VERIFY", got.Verdict)
	}
}

// TestCombineVerdicts_AnEmptyVerdictIsInertAndAnUnknownOneIsNot pins both arms added for
// check 4.
//
// Empty is a check that did NOT RUN -- routine now, since check 4 is skipped whenever no event
// name reaches the audit. It must neither pass nor fail the roll-up, or every existing caller's
// overall verdict would shift.
//
// An UNKNOWN verdict is the opposite: this roll-up feeds a send decision, so a value nobody
// recognises must not resolve to the permissive answer. `severityRank` applies the same rule one
// level down.
func TestCombineVerdicts_AnEmptyVerdictIsInertAndAnUnknownOneIsNot(t *testing.T) {
	cases := []struct {
		name string
		in   []Verdict
		want Verdict
	}{
		{"empty beside passes", []Verdict{VerdictPass, VerdictPass, VerdictPass, ""}, VerdictPass},
		{"empty does not mask a needs-verify", []Verdict{VerdictPass, VerdictNeedsVerify, ""}, VerdictNeedsVerify},
		{"empty does not mask a fail", []Verdict{VerdictPass, VerdictFail, ""}, VerdictFail},
		{"empty alone", []Verdict{""}, VerdictPass},
		{"an unknown verdict is not a pass", []Verdict{VerdictPass, Verdict("WARN")}, VerdictNeedsVerify},
		{"an unknown verdict cannot outrank a fail", []Verdict{VerdictFail, Verdict("WARN")}, VerdictFail},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CombineVerdicts(tc.in...); got != tc.want {
				t.Fatalf("CombineVerdicts(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
