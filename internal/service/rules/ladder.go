// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package rules

// PacingLadder is a set of pacing bands, as percentages of prorated (expected-by-now) spend, and
// Label below is the ONE implementation of placing a percentage on a ladder.
//
// There are two ladders in this service, BriefViewLadder and AccountMonitorLadder, and they used
// to be two separate switches — labelFor in pacing.go and pacingLabelFor in monitor_shared.go —
// that agreed on boundary inclusivity only because each had been written carefully. They now
// differ in their NUMBERS only. Do not write another switch: add a ladder value.
//
// Every boundary is half-open UPWARD — a value sitting exactly on a threshold lands in the
// healthier band. That makes exactly-on-plan mean on plan on the brief view: 100% is a campaign
// spending precisely what the flight expects by now, and labelling it constrained would raise a
// budget item against the only campaign that needs none (an earlier labelFor did, with
// `pct <= Constrained` for constrained). The bands:
//
//	pct <  Underspending                     → underspending
//	Underspending <= pct <= Constrained      → normal
//	Constrained   <  pct <= Overspending     → constrained
//	Overspending  <  pct                     → overspending
//
// Field names are the brief view's original Thresholds names, kept so Thresholds can stay an
// alias of this type. On the account-monitor ladder the same three fields are what
// monitor_shared.go used to call monitorPacingUnderspendingBelow, monitorPacingHealthyTo and
// monitorPacingOverspendingAbove.
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

// BriefViewLadder is the single-campaign brief view's ladder (ComputePacing, Evaluate):
// 50/100/130, so exactly on plan (100) is normal.
//
// Values match `CAMPAIGN_PACING_THRESHOLDS` in lfx-self-serve's shared constants.
//
// D2 IS OPEN: which of this ladder and AccountMonitorLadder is correct is an open product
// decision (Monitor & Optimize brief, D2), not settled here. Switching a caller from one ladder
// to the other moves operator-facing alert bands — do it only as that decision, on its own
// ticket, never as a side effect of a refactor.
var BriefViewLadder = PacingLadder{Underspending: 50, Constrained: 100, Overspending: 130}

// AccountMonitorLadder is the account-scoped /account-monitor ladder, shared by every
// EvaluateXMonitor (pacingLabelFor): 50/90/100, so the healthy band tops out BELOW plan — a
// campaign at 95% of its prorated budget is constrained, and exactly on plan (100) is
// constrained, not overspending.
//
// D2 IS OPEN: which of this ladder and BriefViewLadder is correct is an open product decision
// (Monitor & Optimize brief, D2), not settled here. Switching a caller from one ladder to the
// other moves operator-facing alert bands — do it only as that decision, on its own ticket,
// never as a side effect of a refactor.
var AccountMonitorLadder = PacingLadder{
	Underspending: monitorPacingUnderspendingBelow,
	Constrained:   monitorPacingHealthyTo,
	Overspending:  monitorPacingOverspendingAbove,
}

// AccountMonitorLadder's boundaries as untyped integer constants, because monitor_linkedin.go
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
// a non-finite percentage first. The account-monitor ladder historically placed NaN in normal
// instead, and pacingLabelFor keeps that with its own guard rather than this method changing.
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
