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

// TestCampaignStatusNeedsReconciliation pins the predicate the DELETE guard uses, over the
// WHOLE status vocabulary rather than the handful of cases that motivated it.
//
// The bug this predicate fixed: DeleteCampaign enumerated only 'pending', so the partial
// orphans 'group_created' and 'unconfirmed' passed the guard. Soft-deleting one overwrites
// status with 'deleted' — erasing the only local record that a half-created campaign may
// exist upstream — and frees the (brief, platform) slot, so the next dispatch creates a
// fresh campaign with no sign of the orphan. Enumerating the full vocabulary here is what
// makes a NEW status a deliberate decision: adding one to the model without classifying it
// fails this test rather than silently defaulting to deletable.
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

	// An unknown status must NOT be treated as needing reconciliation: the guard's job is
	// to name the states it knows are unresolved, not to reject everything unfamiliar and
	// make a campaign permanently undeletable on a status typo.
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
