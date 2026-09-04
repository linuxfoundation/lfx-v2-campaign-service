// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import (
	"go/doc"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

func TestJobStatus_Terminal(t *testing.T) {
	terminal := map[JobStatus]bool{
		JobQueued:    false,
		JobRunning:   false,
		JobSucceeded: true,
		JobPartial:   true,
		JobFailed:    true,
	}
	for s, want := range terminal {
		if got := s.Terminal(); got != want {
			t.Errorf("%s.Terminal() = %v, want %v", s, got, want)
		}
	}

	// This expectation table IS the classification, so it must cover the whole vocabulary.
	// Consumers derive their status lists from AllJobStatuses (the retention prune's
	// allow-list among them); a status added to that list without a line above would be
	// unclassified here while every consumer silently started iterating it.
	if len(terminal) != len(AllJobStatuses) {
		t.Errorf("the expectation table covers %d statuses but the vocabulary has %d: %v",
			len(terminal), len(AllJobStatuses), AllJobStatuses)
	}
	for _, s := range AllJobStatuses {
		if _, ok := terminal[s]; !ok {
			t.Errorf("%q is in AllJobStatuses but unclassified here: every status must be "+
				"deliberately declared terminal or non-terminal, because the retention "+
				"prune deletes on the answer", s)
		}
	}
}

// TestDeliveryType_Valid mirrors TestProgramType_Valid for the surface a brief was authored for.
//
// Worth pinning even though nothing in production branches on Valid() today: under 000030 the
// delivery type is part of a brief's unique key, so an accepted-but-wrong value does not merely
// mislabel a row -- it decides WHICH brief that row is. The empty string is explicitly invalid: it
// is the zero value a caller reaches by forgetting the field, and CreateBrief maps that to
// paid-marketing before the write rather than treating it as a surface in its own right.
func TestDeliveryType_Valid(t *testing.T) {
	for _, d := range []DeliveryType{DeliveryPaidMarketing, DeliveryEmail} {
		if !d.Valid() {
			t.Errorf("%s.Valid() = false, want true", d)
		}
	}
	for _, bad := range []DeliveryType{"", "e-mail", "Email", "paid", "paid_marketing"} {
		if bad.Valid() {
			t.Errorf("DeliveryType(%q).Valid() = true, want false", bad)
		}
	}
}

func TestProgramType_Valid(t *testing.T) {
	for _, p := range []ProgramType{ProgramEvents, ProgramEducation, ProgramMembership} {
		if !p.Valid() {
			t.Errorf("%s.Valid() = false, want true", p)
		}
	}
	if ProgramType("webinar").Valid() {
		t.Error(`ProgramType("webinar").Valid() = true, want false`)
	}
}

// TestCampaignStatusNeedsReconciliation pins this named-marker predicate over the WHOLE
// status vocabulary. It is no longer what the DELETE guard calls directly (see
// TestCampaignStatusDeletable below) but the two must stay complementary over every
// KNOWN status, so both are pinned.
func TestCampaignStatusNeedsReconciliation(t *testing.T) {
	needs := map[string]bool{
		// Unresolved — the row and the ad platform may disagree and this string is the
		// only record of how.
		CampaignStatusPending:      true,
		CampaignStatusGroupCreated: true,
		CampaignStatusUnconfirmed:  true,
		// Settled outcomes. created_degraded is deliberately deletable: the campaign WAS
		// fully created upstream, so the row is a complete record and retiring it loses
		// nothing. It is still not TOGGLEABLE — the two predicates differ here on purpose.
		CampaignStatusCreated:         false,
		CampaignStatusCreatedDegraded: false,
		CampaignStatusDeleted:         false,
		CampaignRunActive:             false,
		CampaignRunPaused:             false,
	}
	for status, want := range needs {
		if got := CampaignStatusNeedsReconciliation(status); got != want {
			t.Errorf("CampaignStatusNeedsReconciliation(%q) = %v, want %v", status, got, want)
		}
	}

	// This predicate itself still reads as "not a named unresolved marker" for an unknown
	// status — that is fine for NeedsReconciliation in isolation. It is exactly why the
	// DELETE guard does NOT call this function (or its negation) directly: see
	// TestCampaignStatusDeletable, which pins the opposite default for the same input.
	if CampaignStatusNeedsReconciliation("something_new") {
		t.Error(`CampaignStatusNeedsReconciliation("something_new") = true, want false`)
	}

	// created_degraded is the one status where the two predicates MUST disagree. If a
	// future edit collapses them into one helper, this catches it.
	if CampaignStatusToggleable(CampaignStatusCreatedDegraded) {
		t.Error("created_degraded must not be toggleable (a sub-step still needs reconciliation)")
	}
	if CampaignStatusNeedsReconciliation(CampaignStatusCreatedDegraded) {
		t.Error("created_degraded must be deletable: it was fully created upstream, so the row is a complete record")
	}
}

// TestCampaignStatusDeletable pins the predicate the DELETE guard actually calls
// (internal/infrastructure/postgres/campaign_repo.go), over the WHOLE status vocabulary
// plus an unrecognized status.
//
// The bug this predicate fixed: the guard previously called
// CampaignStatusNeedsReconciliation and refused only the statuses that function names.
// campaigns.status is unconstrained TEXT, so any status this repo has never seen — a
// typo, a future addition, upstream drift — fell through NeedsReconciliation as false
// and was silently treated as deletable. CampaignStatusDeletable is instead an explicit
// WHITELIST of settled, complete states, so an unrecognized status fails CLOSED (not
// deletable) rather than open. That is the one assertion this test cannot skip.
func TestCampaignStatusDeletable(t *testing.T) {
	deletable := map[string]bool{
		// Unresolved reconciliation markers — never deletable. The 'pending' row in
		// particular is load-bearing beyond this guard: because a dispatch claim can
		// never be soft-deleted, a re-dispatch after a delete always finds the claim
		// INSERT — not the upsert's INSERT arm — stamping created_by. See
		// claimCampaignDispatchQuery and orchestrator.dispatchPlatform.
		CampaignStatusPending:      false,
		CampaignStatusGroupCreated: false,
		CampaignStatusUnconfirmed:  false,
		// Settled outcomes.
		CampaignStatusCreated:         true,
		CampaignStatusCreatedDegraded: true,
		CampaignRunActive:             true,
		CampaignRunPaused:             true,
		// 'deleted' is handled by an earlier, separate guard in the repo (a second DELETE
		// reads as 404) and is intentionally excluded from this whitelist: the repo never
		// reaches CampaignStatusDeletable for an already-deleted row, so it must not read
		// as re-deletable if that ordering ever changes.
		CampaignStatusDeleted: false,
	}
	for status, want := range deletable {
		if got := CampaignStatusDeletable(status); got != want {
			t.Errorf("CampaignStatusDeletable(%q) = %v, want %v", status, got, want)
		}
	}

	// The whole point of the whitelist: an unrecognized status must fail CLOSED.
	if CampaignStatusDeletable("something_new") {
		t.Error(`CampaignStatusDeletable("something_new") = true, want false — an unknown status must not be treated as deletable`)
	}
}

// TestCompareSettingsField_AbsentIsNeverAMatch is the rule the whole readback rests on: a
// verdict of `match` requires BOTH sides to have been read. Absence on either side is
// `unknown`, because agreement asserted from an observation nobody made is a fabricated
// "they match" — the exact failure this capability exists to prevent.
func TestCompareSettingsField_AbsentIsNeverAMatch(t *testing.T) {
	v := "500.00"
	cases := []struct {
		name     string
		recorded *string
		upstream *string
		want     SettingsComparison
	}{
		{"both absent", nil, nil, SettingsUnknown},
		{"recorded only", &v, nil, SettingsUnknown},
		{"upstream only", nil, &v, SettingsUnknown},
		{"both present and equal", &v, &v, SettingsMatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CompareSettingsField("budget_amount", tc.recorded, tc.upstream)
			if got.Comparison != tc.want {
				t.Errorf("Comparison = %q, want %q", got.Comparison, tc.want)
			}
		})
	}
}

// TestCompareSettingsField_DifferentValuesDiverge pins the positive case: two values that
// were both read and differ is the finding an operator acts on.
func TestCompareSettingsField_DifferentValuesDiverge(t *testing.T) {
	a, b := "500.00", "750.00"
	got := CompareSettingsField("budget_amount", &a, &b)
	if got.Comparison != SettingsDiverged {
		t.Errorf("Comparison = %q, want %q", got.Comparison, SettingsDiverged)
	}
	// Both sides must survive into the field: a report that says "diverged" without showing
	// what differs is not actionable.
	if got.Recorded == nil || *got.Recorded != a || got.Upstream == nil || *got.Upstream != b {
		t.Errorf("field = %+v, want both sides preserved", got)
	}
}

// TestSummariseSettings_UnknownIsNotCountedAsDivergedOrMatched pins that the two counts
// stay separate. Folding `unknown` into either would report a number an operator would
// act on as if it had been observed.
func TestSummariseSettings_UnknownIsNotCountedAsDivergedOrMatched(t *testing.T) {
	a, b := "500.00", "750.00"
	rb := &CampaignSettingsReadback{Fields: []CampaignSettingsField{
		CompareSettingsField("budget_amount", &a, &b),  // diverged
		CompareSettingsField("budget_type", &a, &a),    // match
		CompareSettingsField("campaign_name", &a, nil), // unknown
		CompareSettingsField("status", nil, &b),        // unknown
	}}
	rb.SummariseSettings()
	if rb.DivergedCount != 1 {
		t.Errorf("DivergedCount = %d, want 1", rb.DivergedCount)
	}
	if rb.UnknownCount != 2 {
		t.Errorf("UnknownCount = %d, want 2", rb.UnknownCount)
	}
}

// TestSummariseSettings_IsIdempotent: the counts are derived from Fields, so summarising
// twice must not double them. A caller cannot be expected to know it may only be called once.
func TestSummariseSettings_IsIdempotent(t *testing.T) {
	a, b := "1", "2"
	rb := &CampaignSettingsReadback{Fields: []CampaignSettingsField{
		CompareSettingsField("budget_amount", &a, &b),
		CompareSettingsField("campaign_name", nil, nil),
	}}
	rb.SummariseSettings()
	first := [2]int{rb.DivergedCount, rb.UnknownCount}
	rb.SummariseSettings()
	if second := [2]int{rb.DivergedCount, rb.UnknownCount}; first != second {
		t.Errorf("counts changed on a second call: %v then %v", first, second)
	}
}

// Go binds a CONTIGUOUS comment block to whatever declaration it abuts, so inserting a type
// between an existing doc comment and its struct silently re-attaches the whole comment to the
// newcomer — and leaves the original undocumented. That is what happened here: LocalCampaignRef
// was inserted after ProjectCampaignScope's prose, so a security-relevant explanation of the
// shared-customer invariant became LocalCampaignRef's doc comment.
//
// Parsed with go/doc rather than grepped, because the defect is about ATTACHMENT: the text is
// present either way, and only the parser knows which declaration owns it.
func TestDocCommentsAreAttachedToTheirOwnTypes(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	pkg, ok := pkgs["model"]
	if !ok {
		t.Fatal("package model not found")
	}
	docPkg := doc.New(pkg, ".", doc.AllDecls)

	// Each type's own name must appear in its own doc comment. Not a style rule — it is the
	// cheapest signal that the comment belongs to the declaration it sits above, which is
	// exactly what a misplaced insertion breaks.
	for _, want := range []string{"ProjectCampaignScope", "LocalCampaignRef", "PlatformCampaignRef"} {
		var found *doc.Type
		for _, typ := range docPkg.Types {
			if typ.Name == want {
				found = typ
				break
			}
		}
		if found == nil {
			t.Errorf("type %s not found — renamed or removed without updating this guard", want)
			continue
		}
		if strings.TrimSpace(found.Doc) == "" {
			t.Errorf("%s has NO doc comment; a type inserted above it may have absorbed one", want)
			continue
		}
		if !strings.Contains(found.Doc, want) {
			t.Errorf("%s's doc comment does not name it, so it probably belongs to another type:\n%s", want, found.Doc)
		}
	}
}
