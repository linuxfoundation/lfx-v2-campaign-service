// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import (
	"math/big"
	"strconv"
	"strings"
	"testing"
)

// TestAddDecimalIsExactWhereFloatIsNot is the regression test for the whole
// decimal-string design: if someone "simplifies" addDecimal back to float64
// arithmetic, this MUST fail.
//
// Choosing the values here took care. The obvious candidate — summing 0.1 a
// thousand times — does NOT discriminate, because addDecimal formats to 6dp on
// every call, and that per-step rounding scrubs the accumulated float error before
// it can ever reach the 6th decimal place. A float64 implementation passes that
// assertion, so it would have been a test that looks meaningful and proves nothing.
//
// What DOES discriminate is magnitude. float64 carries ~15-16 significant decimal
// digits; the NUMERIC(18,6) column allows 12 integer digits plus 6 decimals, i.e.
// up to 18 significant digits. Near the top of the column's range the 6th decimal
// place falls off the end of what float64 can represent at all, so the smallest
// unit is silently swallowed — no rounding step can recover a digit the type never
// held. These are real spend magnitudes for a large account (the column exists to
// hold them), not contrived extremes.
func TestAddDecimalIsExactWhereFloatIsNot(t *testing.T) {
	cases := []struct {
		name, a, b, want string
		// floatWould is what a float64 implementation produces: the value this
		// test exists to reject.
		floatWould string
	}{
		{
			name: "smallest unit onto a large balance is swallowed by float64",
			a:    "100000000000.000000", b: "0.000001",
			want: "100000000000.000001", floatWould: "100000000000.000000",
		},
		{
			name: "carry into a new digit is mis-rounded by float64",
			a:    "999999999999.999998", b: "0.000001",
			want: "999999999999.999999", floatWould: "1000000000000.000000",
		},
		{
			name: "mid-range value loses its last digit under float64",
			a:    "123456789012.345678", b: "0.000001",
			want: "123456789012.345679", floatWould: "123456789012.345673",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := addDecimal(tc.a, tc.b)
			if got != tc.want {
				t.Errorf("addDecimal(%s, %s) = %s, want %s", tc.a, tc.b, got, tc.want)
			}

			// Prove the case actually discriminates: a float64 implementation must
			// produce something DIFFERENT. If this ever stops holding, the case has
			// gone slack and no longer guards the design.
			af, _ := strconv.ParseFloat(tc.a, 64)
			bf, _ := strconv.ParseFloat(tc.b, 64)
			viaFloat := strconv.FormatFloat(af+bf, 'f', 6, 64)
			if viaFloat == tc.want {
				t.Errorf("case does not discriminate: float64 also produces %s, so this assertion would pass against a float64 implementation", viaFloat)
			}
			if viaFloat != tc.floatWould {
				t.Errorf("float64 produced %s, expected %s — the documented failure mode has changed", viaFloat, tc.floatWould)
			}
		})
	}
}

func TestAddDecimalMicrosPrecision(t *testing.T) {
	// 1234567 micros -> 1.234567 currency units, exactly. A float64 round-trip of
	// this value is a classic source of off-by-one-micro drift.
	if got := addDecimal("0", "1.234567"); got != "1.234567" {
		t.Errorf("addDecimal(0, 1.234567) = %s, want 1.234567", got)
	}
	if got := addDecimal("1.234567", "2.765433"); got != "4.000000" {
		t.Errorf("addDecimal(1.234567, 2.765433) = %s, want 4.000000", got)
	}
}

func TestAddDecimalHandlesEmptyAndMalformed(t *testing.T) {
	// A NULL NUMERIC column scans to "" and must behave as zero, not corrupt the sum.
	if got := addDecimal("", "5.5"); got != "5.500000" {
		t.Errorf("addDecimal(empty, 5.5) = %s, want 5.500000", got)
	}
	if got := addDecimal("5.5", ""); got != "5.500000" {
		t.Errorf("addDecimal(5.5, empty) = %s, want 5.500000", got)
	}
	// A malformed value contributes zero rather than panicking or poisoning the sum.
	if got := addDecimal("not-a-number", "3"); got != "3.000000" {
		t.Errorf("addDecimal(malformed, 3) = %s, want 3.000000", got)
	}
}

func TestAddDecimalFixedScale(t *testing.T) {
	// A stable scale means a consumer diffing two summaries never sees a change
	// caused by formatting alone.
	if got := addDecimal("100", "0"); got != "100.000000" {
		t.Errorf("addDecimal(100, 0) = %s, want 100.000000 (fixed scale)", got)
	}
}

// metric is a small builder so the summary tests read as data, not setup.
func metric(spend, conv, currency string, basis AttributionBasis, impr, clicks int64) *CampaignMetric {
	return &CampaignMetric{
		Spend: spend, Conversions: conv, Currency: currency,
		AttributionBasis: basis, Impressions: impr, Clicks: clicks,
	}
}

func TestSummariseMetricsUniformBasisAndCurrency(t *testing.T) {
	rows := []*CampaignMetric{
		metric("10.500000", "2.500000", "USD", AttributionGoogleAdsClickTime, 1000, 50),
		metric("20.250000", "1.250000", "USD", AttributionGoogleAdsClickTime, 2000, 75),
	}
	s := SummariseMetrics(rows)

	if !s.CurrencyUniform || !s.ConversionsComparable {
		t.Fatalf("uniform rows should be comparable; got currencyUniform=%v conversionsComparable=%v",
			s.CurrencyUniform, s.ConversionsComparable)
	}
	if s.Spend != "30.750000" {
		t.Errorf("Spend = %s, want 30.750000", s.Spend)
	}
	if s.Conversions != "3.750000" {
		t.Errorf("Conversions = %s, want 3.750000", s.Conversions)
	}
	if s.Impressions != 3000 || s.Clicks != 125 {
		t.Errorf("Impressions/Clicks = %d/%d, want 3000/125", s.Impressions, s.Clicks)
	}
	if s.Currency != "USD" || s.AttributionBasis != AttributionGoogleAdsClickTime {
		t.Errorf("labels = %s/%s, want USD/%s", s.Currency, s.AttributionBasis, AttributionGoogleAdsClickTime)
	}
	if s.RowCount != 2 {
		t.Errorf("RowCount = %d, want 2", s.RowCount)
	}
}

// TestSummariseMetricsMixedCurrencyOmitsSpend is the core safety property for money:
// with no FX rate source, a spend total across currencies must be ABSENT, not summed.
func TestSummariseMetricsMixedCurrencyOmitsSpend(t *testing.T) {
	rows := []*CampaignMetric{
		metric("10.000000", "1.000000", "USD", AttributionGoogleAdsClickTime, 100, 10),
		metric("20.000000", "2.000000", "EUR", AttributionGoogleAdsClickTime, 200, 20),
	}
	s := SummariseMetrics(rows)

	if s.CurrencyUniform {
		t.Error("CurrencyUniform = true for mixed USD/EUR rows, want false")
	}
	if s.Spend != "" {
		t.Errorf("Spend = %q for mixed currencies, want empty: summing USD+EUR without an FX rate produces a number that looks plausible and is wrong", s.Spend)
	}
	if s.Currency != "" {
		t.Errorf("Currency = %q for mixed currencies, want empty", s.Currency)
	}
	// Delivery counts are still summable — they are not currency-denominated.
	if s.Impressions != 300 || s.Clicks != 30 {
		t.Errorf("Impressions/Clicks = %d/%d, want 300/30 (delivery counts stay summable)", s.Impressions, s.Clicks)
	}
	// Conversions share one basis here, so they remain comparable.
	if !s.ConversionsComparable || s.Conversions != "3.000000" {
		t.Errorf("Conversions = %q comparable=%v, want 3.000000/true (basis is uniform even though currency is not)",
			s.Conversions, s.ConversionsComparable)
	}
}

// TestSummariseMetricsMixedBasisOmitsConversions is the core safety property for
// attribution: conversions counted under different windows/models must NOT be summed.
func TestSummariseMetricsMixedBasisOmitsConversions(t *testing.T) {
	rows := []*CampaignMetric{
		metric("10.000000", "1.000000", "USD", AttributionGoogleAdsClickTime, 100, 10),
		metric("20.000000", "2.000000", "USD", AttributionBasis("meta:7d-click-1d-view"), 200, 20),
	}
	s := SummariseMetrics(rows)

	if s.ConversionsComparable {
		t.Error("ConversionsComparable = true across different attribution bases, want false")
	}
	if s.Conversions != "" {
		t.Errorf("Conversions = %q across bases, want empty: a cross-basis sum is wrong in a way that looks plausible", s.Conversions)
	}
	if s.AttributionBasis != "" {
		t.Errorf("AttributionBasis = %q across bases, want empty", s.AttributionBasis)
	}
	// Spend shares one currency, so it stays summable — the two caveats are independent.
	if !s.CurrencyUniform || s.Spend != "30.000000" {
		t.Errorf("Spend = %q uniform=%v, want 30.000000/true (currency is uniform even though basis is not)",
			s.Spend, s.CurrencyUniform)
	}
}

// TestSummariseMetricsUnknownBasisNeverComparable pins the fail-closed rule: two rows
// that BOTH failed to record how they were counted are not thereby counted the same way.
func TestSummariseMetricsUnknownBasisNeverComparable(t *testing.T) {
	rows := []*CampaignMetric{
		metric("10.000000", "1.000000", "USD", AttributionUnknown, 100, 10),
		metric("20.000000", "2.000000", "USD", AttributionUnknown, 200, 20),
	}
	s := SummariseMetrics(rows)

	if s.ConversionsComparable {
		t.Error("ConversionsComparable = true for two AttributionUnknown rows, want false: an unknown basis is not comparable even with another unknown one")
	}
	if s.Conversions != "" {
		t.Errorf("Conversions = %q for unknown bases, want empty", s.Conversions)
	}

	// A single unknown row must also be non-comparable (guards the seed value).
	if s1 := SummariseMetrics(rows[:1]); s1.ConversionsComparable {
		t.Error("ConversionsComparable = true for ONE AttributionUnknown row, want false")
	}
}

// TestSummariseMetricsEmptyIsNotComparable: an empty set has no established currency
// or basis, so reporting it as comparable would let a caller treat a later mixed set
// the same way.
func TestSummariseMetricsEmptyIsNotComparable(t *testing.T) {
	s := SummariseMetrics(nil)
	if s.CurrencyUniform || s.ConversionsComparable {
		t.Errorf("empty summary comparable flags = %v/%v, want false/false", s.CurrencyUniform, s.ConversionsComparable)
	}
	if s.Spend != "" || s.Conversions != "" {
		t.Errorf("empty summary totals = %q/%q, want empty/empty", s.Spend, s.Conversions)
	}
	if s.RowCount != 0 {
		t.Errorf("RowCount = %d, want 0", s.RowCount)
	}
}

// TestSummariseMetricsExactAcrossManyRows guards the ACCUMULATION path: a year of
// daily rows on a high-spend account, where each row is individually representable
// but the running total crosses out of float64's exact range.
//
// As in TestAddDecimalIsExactWhereFloatIsNot, small values like 0.1 would not
// discriminate here — per-step 6dp rounding hides the drift. The magnitudes below
// are what make the assertion real, and the float64 cross-check at the end proves it.
func TestSummariseMetricsExactAcrossManyRows(t *testing.T) {
	const (
		rowSpend = "27397260.273973" // ~1e7/day: a year of this lands near 1e10
		days     = 365
	)
	rows := make([]*CampaignMetric, 0, days)
	for range days {
		rows = append(rows, metric(rowSpend, "0.333333", "USD", AttributionGoogleAdsClickTime, 1, 1))
	}
	s := SummariseMetrics(rows)

	// Exact expected totals, computed independently with big.Rat.
	wantSpend := new(big.Rat).Mul(ratOf(t, rowSpend), new(big.Rat).SetInt64(days)).FloatString(6)
	wantConv := new(big.Rat).Mul(ratOf(t, "0.333333"), new(big.Rat).SetInt64(days)).FloatString(6)

	if s.Spend != wantSpend {
		t.Errorf("Spend over %d rows = %s, want exactly %s", days, s.Spend, wantSpend)
	}
	if s.Conversions != wantConv {
		t.Errorf("Conversions over %d rows = %s, want exactly %s", days, s.Conversions, wantConv)
	}

	// Prove this actually discriminates: the same accumulation via float64 must
	// diverge, or the test is not guarding anything.
	viaFloat := "0"
	for range days {
		af, _ := strconv.ParseFloat(viaFloat, 64)
		bf, _ := strconv.ParseFloat(rowSpend, 64)
		viaFloat = strconv.FormatFloat(af+bf, 'f', 6, 64)
	}
	if viaFloat == wantSpend {
		t.Errorf("accumulation does not discriminate: float64 also produced %s", viaFloat)
	}
	t.Logf("float64 accumulation would have produced %s instead of %s", viaFloat, wantSpend)

	// The value must remain parseable by any consumer.
	if _, err := strconv.ParseFloat(s.Spend, 64); err != nil {
		t.Errorf("Spend %q is not parseable as a number: %v", s.Spend, err)
	}
	if strings.Count(s.Spend, ".") != 1 {
		t.Errorf("Spend %q should carry exactly one decimal point", s.Spend)
	}
}

// ratOf parses an exact decimal for use as an independent expected value.
func ratOf(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		t.Fatalf("bad test constant %q", s)
	}
	return r
}
