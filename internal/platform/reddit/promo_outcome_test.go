// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"strings"
	"testing"
)

func panics(f func()) (did bool) {
	defer func() { did = recover() != nil }()
	f()
	return false
}

// TestPromoOutcomeRow_RenderingPaths pins the table's one-rendering-path rule: a row whose step
// takes non-status arguments carries them in StepFormat and renders only through stepf, and every
// other row's Step and StepWithStatus are what step() emits. Routing a StepFormat row through
// step() (or the reverse) panics, so the misuse fails here rather than emitting a raw verb.
func TestPromoOutcomeRow_RenderingPaths(t *testing.T) {
	for _, row := range PromoOutcomes() {
		if row.StepFormat != "" {
			if !panics(func() { row.step(500, true) }) {
				t.Errorf("%s: step() on a StepFormat row must panic", row.Outcome)
			}
			if row.Step != "" || row.StepWithStatus != "" {
				t.Errorf("%s: a StepFormat row must not also set Step or StepWithStatus", row.Outcome)
			}
			continue
		}
		if !panics(func() { row.stepf("x") }) {
			t.Errorf("%s: stepf() on a row without StepFormat must panic", row.Outcome)
		}
		if strings.Contains(row.Step, "%") {
			t.Errorf("%s: Step is emitted verbatim and must carry no format verb: %q", row.Outcome, row.Step)
		}
		if row.StepWithStatus != "" && strings.Count(row.StepWithStatus, "%d") != 1 {
			t.Errorf("%s: StepWithStatus must format exactly one HTTP status: %q", row.Outcome, row.StepWithStatus)
		}
	}

	if got := promoRow(PromoAdNoID).stepf("t3_abc"); !strings.Contains(got, "post: t3_abc)") {
		t.Errorf("PromoAdNoID step = %q, want the post id rendered", got)
	}
	if got := promoRow(PromoNotRequestedVariants).stepf(3); !strings.HasPrefix(got, "3 ad variant(s) ready") {
		t.Errorf("PromoNotRequestedVariants step = %q, want the variant count rendered", got)
	}
}
