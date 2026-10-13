// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package rules

import (
	"math"
	"testing"
)

// TestStandardLadder_Values pins the one ladder's numbers (D2: 50/90/100 everywhere) and that the
// brief view's DefaultThresholds is that ladder. A change here moves operator-facing alert bands on
// both read paths at once.
func TestStandardLadder_Values(t *testing.T) {
	if want := (PacingLadder{Underspending: 50, Constrained: 90, Overspending: 100}); StandardLadder() != want {
		t.Errorf("StandardLadder = %+v, want %+v", StandardLadder(), want)
	}
	if DefaultThresholds != StandardLadder() {
		t.Errorf("DefaultThresholds = %+v, want StandardLadder %+v", DefaultThresholds, StandardLadder())
	}
}

// TestPacingLadder_Label_Boundaries pins the one implementation's inclusivity on the one ladder.
func TestPacingLadder_Label_Boundaries(t *testing.T) {
	for _, pct := range boundaryPcts {
		if got := StandardLadder().Label(pct); string(got) != unroundedMonitorWant[pct] {
			t.Errorf("StandardLadder().Label(%v) = %q, want %q", pct, got, unroundedMonitorWant[pct])
		}
	}
}

// TestPacingLadder_Label_NaN pins that Label itself places NaN in overspending (the brief
// view's historical behaviour), while pacingLabelFor keeps the account monitor's historical
// normal — see the guard there.
func TestPacingLadder_Label_NaN(t *testing.T) {
	if got := StandardLadder().Label(math.NaN()); got != PacingOverspending {
		t.Errorf("StandardLadder().Label(NaN) = %q, want overspending", got)
	}
}

// TestUnknownPacing pins the brief view's unknown value.
func TestUnknownPacing(t *testing.T) {
	if got := UnknownPacing(); got != (Pacing{Label: PacingUnknown}) || got.Computable || got.Pct != 0 {
		t.Errorf("UnknownPacing() = %+v", got)
	}
}
