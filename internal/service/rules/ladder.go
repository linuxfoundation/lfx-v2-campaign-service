// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package rules

// PacingLadder is a set of pacing bands, as percentages of prorated (expected-by-now) spend, and
// Label below is the ONE implementation of placing a percentage on a ladder.
//
// There is ONE ladder in this service, StandardLadder (50/90/100), placed by Label for both read
// paths: the single-campaign brief view (ComputePacing, Evaluate) and every account monitor
// (pacingLabelFor). Product decision D2 (2026-10-13) settled it — the brief view used to run its
// own 50/100/130 ladder. Do not write another switch, and do not add a second ladder without a
// product decision to back it.
//
// Every boundary is half-open UPWARD — a value sitting exactly on a threshold lands in the
// healthier band: exactly 50 is normal, exactly 90 is normal, exactly 100 is constrained. The
// bands:
//
//	pct <  Underspending                     → underspending
//	Underspending <= pct <= Constrained      → normal
//	Constrained   <  pct <= Overspending     → constrained
//	Overspending  <  pct                     → overspending
//
// Field names are the brief view's original Thresholds names, kept so Thresholds can stay an
// alias of this type. On StandardLadder they are fed by monitorPacingUnderspendingBelow,
// monitorPacingHealthyTo and monitorPacingOverspendingAbove below.
//
// The ladder places a NUMBER. Whether a campaign has a percentage worth placing — budget, flight,
// rounding — stays each caller's own question, and differs between callers on purpose: Google,
// Microsoft, Meta, Reddit and X round before placing, LinkedIn and the brief view do not.
type PacingLadder struct {
	// Underspending is the floor: below this share of expected spend, the campaign is not
	// delivering the budget it was given. Exclusive: exactly Underspending is normal.
	Underspending float64
	// Constrained is the top of the healthy band, inclusive: exactly Constrained is normal, above
	// it the campaign is constrained.
	Constrained float64
	// Overspending is the top of the constrained band, inclusive: above it the campaign is
	// overspending. ABSOLUTE, not derived from Constrained — deriving it would silently move it
	// whenever Constrained is changed, which is the one thing a change to a different boundary
	// must not do.
	Overspending float64
}

// StandardLadder is the service's one pacing ladder: 50/90/100, so the healthy band tops out
// BELOW plan — a campaign at 95% of its prorated budget is constrained, and exactly on plan (100)
// is constrained, not overspending.
//
// Used by the single-campaign brief view (DefaultThresholds → ComputePacing → Evaluate) and by
// every EvaluateXMonitor (pacingLabelFor). Product decision D2 (Monitor & Optimize brief,
// 2026-10-13): 50/90/100 everywhere. Before it the brief view ran 50/100/130 (BriefViewLadder,
// matching lfx-self-serve's CAMPAIGN_PACING_THRESHOLDS); that ladder was removed, not kept as an
// option. Changing these numbers moves operator-facing alert bands on both read paths at once.
//
// A function, not a var, so no code can reassign the bands at run time and move every platform's
// alerts — or let them drift from the constants monitor_linkedin.go prints into action text.
func StandardLadder() PacingLadder {
	return PacingLadder{
		Underspending: monitorPacingUnderspendingBelow,
		Constrained:   monitorPacingHealthyTo,
		Overspending:  monitorPacingOverspendingAbove,
	}
}

// StandardLadder's boundaries as untyped integer constants, because monitor_linkedin.go
// prints two of them into its action-item text with %d ("pacing below 50%"). They are the
// ladder's only source, so the text cannot drift from the bands. Named for the boundary each one
// IS, not for the band it happens to gate.
const (
	// monitorPacingUnderspendingBelow is the floor: under half the prorated plan, a campaign is
	// not delivering the budget it was given.
	monitorPacingUnderspendingBelow = 50
	// monitorPacingHealthyTo is the top of the healthy band, INCLUSIVE: at exactly 90 a campaign
	// is still normal, above it it is constrained.
	monitorPacingHealthyTo = 90
	// monitorPacingOverspendingAbove is the top of the constrained band, INCLUSIVE: exactly on
	// plan (100) is constrained, not overspending.
	monitorPacingOverspendingAbove = 100
)

// Label places pct on the ladder. It never returns PacingUnknown: "no percentage" is the
// caller's to report, before calling this.
//
// NaN — which compares false against everything — falls through every case to overspending.
// That is the brief view's historical behaviour, unreachable there because ComputePacing refuses
// a non-finite percentage first. The account monitors historically placed NaN in normal instead,
// and pacingLabelFor keeps that with its own guard rather than this method changing.
func (l PacingLadder) Label(pct float64) PacingLabel {
	switch {
	case pct < l.Underspending:
		return PacingUnderspending
	case pct <= l.Constrained:
		return PacingNormal
	case pct <= l.Overspending:
		return PacingConstrained
	default:
		return PacingOverspending
	}
}
