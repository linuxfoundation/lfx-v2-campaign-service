// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package rules

import (
	"math"
	"testing"
)

// TestLadders_Values pins the two named ladders' numbers. D2 (which ladder is correct) is open;
// a change here moves operator-facing alert bands and is that decision, not a refactor.
func TestLadders_Values(t *testing.T) {
	if want := (PacingLadder{Underspending: 50, Constrained: 100, Overspending: 130}); BriefViewLadder != want {
		t.Errorf("BriefViewLadder = %+v, want %+v", BriefViewLadder, want)
	}
	if want := (PacingLadder{Underspending: 50, Constrained: 90, Overspending: 100}); AccountMonitorLadder != want {
		t.Errorf("AccountMonitorLadder = %+v, want %+v", AccountMonitorLadder, want)
	}
	if DefaultThresholds != BriefViewLadder {
		t.Errorf("DefaultThresholds = %+v, want BriefViewLadder %+v", DefaultThresholds, BriefViewLadder)
	}
}

// TestPacingLadder_Label_Boundaries pins the one implementation's inclusivity on both ladders.
func TestPacingLadder_Label_Boundaries(t *testing.T) {
	for _, c := range []struct {
		name   string
		ladder PacingLadder
		want   map[float64]string
	}{
		{"brief", BriefViewLadder, briefViewWant},
		{"monitor", AccountMonitorLadder, unroundedMonitorWant},
	} {
		for _, pct := range boundaryPcts {
			if got := c.ladder.Label(pct); string(got) != c.want[pct] {
				t.Errorf("%s.Label(%v) = %q, want %q", c.name, pct, got, c.want[pct])
			}
		}
	}
}

// TestPacingLadder_Label_NaN pins that Label itself places NaN in overspending (the brief
// view's historical behaviour), while pacingLabelFor keeps the account monitor's historical
// normal — see the guard there.
func TestPacingLadder_Label_NaN(t *testing.T) {
	if got := BriefViewLadder.Label(math.NaN()); got != PacingOverspending {
		t.Errorf("BriefViewLadder.Label(NaN) = %q, want overspending", got)
	}
	if got := AccountMonitorLadder.Label(math.NaN()); got != PacingOverspending {
		t.Errorf("AccountMonitorLadder.Label(NaN) = %q, want overspending", got)
	}
}

// TestUnknownPacing pins the brief view's unknown value.
func TestUnknownPacing(t *testing.T) {
	if got := UnknownPacing(); got != (Pacing{Label: PacingUnknown}) || got.Computable || got.Pct != 0 {
		t.Errorf("UnknownPacing() = %+v", got)
	}
}
