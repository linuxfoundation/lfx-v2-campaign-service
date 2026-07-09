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
