// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import "testing"

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
		// Unresolved reconciliation markers — never deletable.
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
