// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package audience

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// kubeCon is the identity most of these cases classify against.
var kubeCon = EventIdentity{
	Name:       "KubeCon Europe 2026",
	BrandShort: "CNCF",
	Dates:      []string{"2026-03-17"},
}

// propFilter builds a PROPERTY leaf carrying one value, the shape every
// subscription and hosted-events case below reads.
func propFilter(property string, values ...string) ListFilter {
	raw, err := json.Marshal(values)
	if err != nil {
		panic(err)
	}
	return ListFilter{FilterType: "PROPERTY", Property: property, ValueList: raw}
}

// TestClassifyList_OneCasePerRule walks the seven rules in their own order. Each
// case is the MINIMAL input that reaches its rule, so a rule that stops firing
// fails exactly one case rather than shifting a block of them.
func TestClassifyList_OneCasePerRule(t *testing.T) {
	cases := []struct {
		name    string
		list    string
		filters []ListFilter
		want    Signal
	}{
		{
			name:    "rule 1 suppression name",
			list:    "26Q1 - CNCF - KubeCon Europe - Suppression",
			filters: []ListFilter{{FilterType: "PAGE_VIEW"}},
			want:    SignalUncertain,
		},
		{
			name:    "rule 2 page view",
			list:    "KubeCon Europe site visitors",
			filters: []ListFilter{{FilterType: "PAGE_VIEW"}},
			want:    SignalPageView,
		},
		{
			name:    "rule 3 education enrollment scoped to the brand",
			list:    "CNCF Kubernetes course enrollees",
			filters: []ListFilter{{FilterType: "UNIFIED_EVENTS", EventTypeID: educationEventTypeID}},
			want:    SignalEducationEnrollment,
		},
		{
			name:    "rule 4 speakers by name",
			list:    "KubeCon Europe 2026 - Speakers - current",
			filters: []ListFilter{{FilterType: "PROPERTY", Property: "email"}},
			want:    SignalEventSpeakers,
		},
		{
			name:    "rule 5 project opt-in by value",
			list:    "Opt-In",
			filters: []ListFilter{propFilter("email_subscription_types", "CNCF Newsletter")},
			want:    SignalProjectOptIn,
		},
		{
			name:    "rule 6 registration by name",
			list:    "KubeCon Europe 2026 all-time registrants",
			filters: []ListFilter{{FilterType: "PROPERTY", Property: "email"}},
			want:    SignalEventRegistration,
		},
		{
			name:    "rule 7 unreadable filter shape",
			list:    "KubeCon Europe misc",
			filters: []ListFilter{{FilterType: "SOMETHING_NEW"}},
			want:    SignalUncertain,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyList(tc.list, tc.filters, kubeCon)
			assert.Equal(t, tc.want, got.Signal)
			assert.NotEmpty(t, got.Reason,
				"every classification must carry its evidence; an operator about to mail thousands judges the bucket by the reason")
		})
	}
}

// TestClassifyList_PrecedenceIsTotal is the half that matters most. Every case
// here satisfies TWO rules at once, and the assertion is that the earlier one
// always wins — a list that could land in two buckets must land in the same one
// every time, or the same portal produces a different audience per run.
func TestClassifyList_PrecedenceIsTotal(t *testing.T) {
	cases := []struct {
		name    string
		list    string
		filters []ListFilter
		want    Signal
		why     string
	}{
		{
			name:    "suppression name beats a registration name",
			list:    "KubeCon Europe registrants - Do Not Email",
			filters: []ListFilter{{FilterType: "UNIFIED_EVENTS"}},
			want:    SignalUncertain,
			why:     "an opt-out asset classified as registration puts people who asked not to be reached into a send",
		},
		{
			name:    "page view beats a speaker name",
			list:    "KubeCon Europe speakers page visitors",
			filters: []ListFilter{{FilterType: "PAGE_VIEW"}},
			want:    SignalPageView,
			why:     "a page-view filter type is structural evidence; the word speakers in the name is not",
		},
		{
			name:    "education event type beats a registration name",
			list:    "CNCF course registrants",
			filters: []ListFilter{{FilterType: "UNIFIED_EVENTS", EventTypeID: educationEventTypeID}},
			want:    SignalEducationEnrollment,
			why:     "the portal-wide education event type is exact; registrant in the name is a hint",
		},
		{
			name: "speakers beats a subscription filter",
			list: "KubeCon Europe 2026 - Speakers",
			filters: []ListFilter{
				propFilter("email_subscription_types", "CNCF Newsletter"),
			},
			want: SignalEventSpeakers,
			why:  "speaker scoping is the narrower audience and must not be merged into an opt-in bucket",
		},
		{
			name:    "subscription value beats a registration name",
			list:    "KubeCon Europe registrants who opted in",
			filters: []ListFilter{propFilter("email_subscription_types", "CNCF Newsletter")},
			want:    SignalProjectOptIn,
			why:     "the subscription filter is what the list actually selects on",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ClassifyList(tc.list, tc.filters, kubeCon).Signal, tc.why)
		})
	}
}

// TestClassifyList_NewsletterValueSplitsTheOptInBuckets pins rule 5's whole
// reason for reading VALUES rather than the list name: the two opt-in buckets
// share one filter shape, and a project list is frequently named only "Opt-In".
// Reading the name would merge a project's own subscribers into the
// portfolio-wide newsletter — a send to people who never asked for it.
func TestClassifyList_NewsletterValueSplitsTheOptInBuckets(t *testing.T) {
	lf := ClassifyList("Opt-In", []ListFilter{
		propFilter("email_subscription_types", "Linux Foundation Newsletter"),
	}, kubeCon)
	assert.Equal(t, SignalLFNewsletterOptIn, lf.Signal)

	project := ClassifyList("Linux Foundation Newsletter", []ListFilter{
		propFilter("email_subscription_types", "CNCF Weekly"),
	}, kubeCon)
	assert.Equal(t, SignalProjectOptIn, project.Signal,
		"the VALUE decides the bucket; a name that says newsletter must not override a project subscription value")
}

// TestClassifyList_UnscopedEducationIsUncertain pins the one education case that
// is deliberately NOT credited: an enrollment list with no brand or topic scoping
// covers every LFX Education course across the portfolio, so offering it as this
// event's education signal would target learners with no connection to the brand.
func TestClassifyList_UnscopedEducationIsUncertain(t *testing.T) {
	got := ClassifyList("All course enrollments",
		[]ListFilter{{FilterType: "UNIFIED_EVENTS", EventTypeID: educationEventTypeID}}, kubeCon)
	assert.Equal(t, SignalUncertain, got.Signal)
	assert.Contains(t, got.Reason, "all courses",
		"the reason must name the missing scoping, which is what the operator has to fix")
}

// TestClassifyList_SpeakerPropertyIsRecognisedWithoutTheName covers rule 4's
// structural half: a list can filter on a hosted-events speaker value while being
// named nothing in particular, and dropping it would lose an event's speakers.
func TestClassifyList_SpeakerPropertyIsRecognisedWithoutTheName(t *testing.T) {
	got := ClassifyList("26Q1 - CNCF - Europe - VIP", []ListFilter{
		propFilter("hosted_events", "KubeCon Europe 2026 - Speakers"),
	}, kubeCon)
	require.Equal(t, SignalEventSpeakers, got.Signal)
	assert.Equal(t, SpeakerScopeCurrent, got.Scope,
		"the value names the current edition, so the scope is current rather than blank")
}

// TestClassifyList_NoFiltersSaysSo separates the two uncertain reasons. "No
// readable filters" and "filter shape did not match" send an operator to
// different places — the first to the list's definition, the second to the
// classifier — so collapsing them into one message costs a debugging session.
func TestClassifyList_NoFiltersSaysSo(t *testing.T) {
	got := ClassifyList("Mystery list", nil, kubeCon)
	assert.Equal(t, SignalUncertain, got.Signal)
	assert.Contains(t, got.Reason, "No readable filters")
}

// TestDetermineSpeakerScope_LongestSuffixWins pins the trap the suffix table's
// comment names: a shorter pattern matched first would report a current+past list
// as one edition, silently dropping every past speaker from the audience.
func TestDetermineSpeakerScope_LongestSuffixWins(t *testing.T) {
	cases := map[string]SpeakerScope{
		"KubeCon Europe 2026 - Speakers - current + past": SpeakerScopeCurrentPast,
		"KubeCon Europe 2026 - Speakers - past":           SpeakerScopePast,
		"KubeCon Europe 2026 - Speakers - current":        SpeakerScopeCurrent,
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, want, DetermineSpeakerScope(name, nil, "2026"))
		})
	}
}

// TestMissingSignals_ReportsOnlyClassifiedOnes pins two exclusions that are easy
// to get wrong. Provenance buckets (last_sent, added) can never be "missing", and
// `uncertain` must never be reported as missing — that would tell an operator to
// go create a list of things the classifier failed to understand.
func TestMissingSignals_ReportsOnlyClassifiedOnes(t *testing.T) {
	found := map[Signal]struct{}{
		SignalEventRegistration: {},
		SignalPageView:          {},
		SignalLastSent:          {},
		SignalUncertain:         {},
	}
	got := MissingSignals(found)
	assert.Equal(t, []Signal{
		SignalEventSpeakers,
		SignalProjectOptIn,
		SignalLFNewsletterOptIn,
		SignalEducationEnrollment,
	}, got, "missing signals must come back in render order, and never include a provenance or fallback bucket")

	assert.Empty(t, MissingSignals(map[Signal]struct{}{
		SignalEventRegistration:   {},
		SignalEventSpeakers:       {},
		SignalProjectOptIn:        {},
		SignalLFNewsletterOptIn:   {},
		SignalEducationEnrollment: {},
		SignalPageView:            {},
	}))
}

// A PROPERTY filter that happens to use the IN_LIST operator is NOT a rollup.
//
// The guard previously accepted a matching operator alone, so a country-in-list filter
// (a real shape — see builder_qa_test.go) classified as a rollup. Discovery then called
// RollupChildIDs, found no list ids in it, and dropped the candidate entirely rather
// than classifying it — a list the operator could legitimately have mailed, silently
// absent from the results.
func TestIsRollupRequiresTheFilterTypeNotJustTheOperator(t *testing.T) {
	propertyInList := []ListFilter{
		{FilterType: "PROPERTY", Property: "country", Operator: "IN_LIST"},
	}
	if IsRollup(propertyInList) {
		t.Error("a PROPERTY filter using the IN_LIST operator was classified as a rollup; discovery drops such candidates")
	}

	// A genuine rollup must still be recognised, or discovery stops resolving children.
	realRollup := []ListFilter{
		{FilterType: "IN_LIST", Operator: "IN_LIST"},
		{FilterType: "IN_LIST", Operator: "NOT_IN_LIST"},
	}
	if !IsRollup(realRollup) {
		t.Error("a list whose filters are all IN_LIST filters must still be a rollup")
	}

	// Mixed: one real list filter plus a property filter is not a rollup either.
	mixed := []ListFilter{
		{FilterType: "IN_LIST", Operator: "IN_LIST"},
		{FilterType: "PROPERTY", Property: "country", Operator: "IN_LIST"},
	}
	if IsRollup(mixed) {
		t.Error("a rollup must be ALL list-membership filters; one property filter disqualifies it")
	}
}
