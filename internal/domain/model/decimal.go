// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import (
	"math/big"
	"strings"
)

// metricDecimalScale is the number of decimal places metric money/conversion values
// are carried at. It matches the NUMERIC(18,6) columns in migration 000011 exactly,
// so a value read from the database round-trips through a sum and back without ever
// changing scale.
const metricDecimalScale = 6

// addDecimal returns a + b computed EXACTLY, with both operands and the result as
// decimal strings.
//
// WHY NOT float64 — do not "simplify" this back:
//
// These values are money (spend) and fractional conversion credit, and they are
// SUMMED across days and campaigns. float64 cannot represent most decimal fractions,
// so the error compounds with every addition. Verified against a real Postgres 16:
// summing 0.1 one thousand times yields
//
//	NUMERIC(18,6) -> 100.000000          (exact)
//	double precision -> 99.9999999999986 (wrong, and wrong in a way that looks fine)
//
// A reported spend of 99.9999999999986 is not a rounding curiosity; it is a money
// figure that will not reconcile against the platform's own invoice, and the
// discrepancy grows with the row count. big.Rat is exact for any decimal input, so
// the sum is exact regardless of how many rows are added.
//
// This is a DELIBERATE departure from campaigns.budget_amount (*float64). That column
// holds ONE caller-supplied budget that is never summed, so float error has nowhere to
// accumulate. Metrics are summed, which is precisely where it does.
//
// Inputs are the exact decimal strings Postgres emitted for a NUMERIC column. An empty
// string is treated as zero (a NULL column scans to ""). A value that does not parse as
// a decimal is treated as zero rather than panicking: these strings come from the
// database, so a malformed one is not a caller error, and dropping a corrupt row's
// contribution is safer than aborting a whole summary.
func addDecimal(a, b string) string {
	return formatDecimal(new(big.Rat).Add(parseDecimal(a), parseDecimal(b)))
}

// parseDecimal converts a decimal string to an exact big.Rat, yielding zero for an
// empty or unparseable value (see addDecimal for why this does not error).
func parseDecimal(s string) *big.Rat {
	s = strings.TrimSpace(s)
	if s == "" {
		return new(big.Rat)
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return new(big.Rat)
	}
	return r
}

// formatDecimal renders r as a fixed-point decimal string at metricDecimalScale.
//
// The scale is FIXED rather than trimmed so every emitted value has the same shape as
// the NUMERIC(18,6) column it came from ("100.000000", not "100"). A stable scale means
// a consumer diffing or comparing two summaries never sees a spurious change caused by
// formatting alone.
//
// big.Rat.FloatString rounds half away from zero at the requested precision. At scale 6
// this only ever engages for a value with more than 6 decimal places, which the database
// columns cannot produce — so in practice this is exact, and the rounding is a defensive
// fallback for a hand-constructed value rather than a lossy step in the normal path.
func formatDecimal(r *big.Rat) string {
	return r.FloatString(metricDecimalScale)
}
