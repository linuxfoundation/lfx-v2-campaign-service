// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import (
	"encoding/json"
	"time"
)

// AttributionBasis names HOW a platform counted the conversions on a metric row.
//
// This exists because platforms genuinely disagree, and the disagreement is not
// cosmetic: the same user converting once can be counted by two platforms, and
// Google's fractional data-driven credit does not add to Meta's whole-number
// counting. A cross-platform SUM of conversions is therefore wrong in a way that
// looks entirely plausible — which is worse than showing nothing, because nobody
// checks a number that looks right.
//
// Carrying the basis on every row is what lets a rollup be derived EXPLICITLY and
// labelled, instead of silently adding incomparable numbers.
type AttributionBasis string

// Attribution bases, one per platform reporting contract.
const (
	// AttributionGoogleAdsClickTime: Google Ads attributes a conversion to the DAY OF
	// THE CLICK (not the day of the conversion), using per-conversion-action windows,
	// and may report FRACTIONAL conversions when data-driven attribution splits credit
	// across touchpoints. Two consequences: recent days are RESTATED as conversions
	// mature, and the value is a decimal, not a count.
	AttributionGoogleAdsClickTime AttributionBasis = "google-ads:click-time"

	// AttributionUnknown marks a row whose basis was not recorded. It is deliberately
	// NOT comparable with anything (including another AttributionUnknown row), so it
	// can never be silently folded into a rollup.
	AttributionUnknown AttributionBasis = ""
)

// CampaignMetric is one platform's RAW reported numbers for ONE campaign on ONE day.
//
// "Raw" is the whole point: values are stored exactly as the platform reported them,
// with no normalisation, no FX conversion and no cross-platform reconciliation. Raw
// carries the platform's own response row so a disputed number can be traced back to
// what the platform actually said.
type CampaignMetric struct {
	ID         string
	CampaignID string
	// MetricDate is the PLATFORM's reporting day, in the ad account's timezone. It is
	// a date, not an instant: re-bucketing a platform's day into another timezone would
	// invent precision the platform never reported.
	MetricDate time.Time

	// Delivery counts. Summable across platforms — they double-count a PERSON reached
	// on two platforms, but not an EVENT — unlike Conversions below.
	Impressions int64
	Clicks      int64

	// Spend is money, so it is carried as a DECIMAL STRING, never a float64.
	//
	// The column is NUMERIC(18,6). Scanning it through float64 would reintroduce
	// exactly the representation error the NUMERIC column exists to avoid (summing
	// 0.1 a thousand times through float64 yields 99.9999999999986, verified against
	// a real Postgres). A string round-trips the database's own exact decimal, and
	// Postgres does the summation server-side where it is exact.
	//
	// This deliberately departs from campaigns.budget_amount (*float64). That column
	// holds a single caller-supplied budget that is never summed; these values are
	// summed across days and campaigns, which is precisely where float error compounds.
	Spend string

	// Conversions is also a decimal string, for a second reason on top of exactness:
	// Google Ads reports FRACTIONAL conversions under data-driven attribution, so an
	// integer type would quietly truncate real data.
	Conversions string

	// Currency is the ISO 4217 code Spend is denominated in. The service does NO FX
	// conversion — it has no rate source, and a wrong rate is worse than no rate — so
	// a rollup spanning currencies must omit its spend total rather than add
	// incomparable amounts.
	Currency string

	// AttributionBasis is how THIS row's Conversions were counted. See the type doc.
	AttributionBasis AttributionBasis

	// Platform is denormalised from the owning campaign for convenience on the read
	// path (a caller comparing rows needs to know who reported them). It is not stored
	// on the metrics row itself — the campaign owns it.
	Platform Provider

	// Raw is the platform's own response row, verbatim, for auditability.
	Raw json.RawMessage

	FetchedAt time.Time
}

// MetricsSummary is an EXPLICITLY derived rollup over a set of CampaignMetric rows,
// carrying the caveats that make it honest rather than merely plausible.
//
// The two "…Comparable" flags are the load-bearing part of this type. They are not
// advisory metadata: when a flag is false the corresponding total is left EMPTY, so a
// consumer that ignores the caveat physically cannot read a misleading number. That is
// a deliberate choice over emitting the total with a footnote — footnotes do not
// survive a trip through a dashboard, a spreadsheet, or a slide.
type MetricsSummary struct {
	// Impressions and Clicks are always populated: they are delivery counts, not
	// attributed outcomes, so summing them across platforms is defensible.
	Impressions int64
	Clicks      int64

	// Spend is the exact decimal sum, populated ONLY when CurrencyUniform is true.
	// Empty when the rows span multiple currencies.
	Spend string
	// Currency is the single currency Spend is denominated in; empty when mixed.
	Currency string
	// CurrencyUniform reports whether every row shared one currency. When false,
	// Spend and Currency are empty because no FX rate is available to combine them.
	CurrencyUniform bool

	// Conversions is the exact decimal sum, populated ONLY when
	// ConversionsComparable is true. Empty otherwise.
	Conversions string
	// AttributionBasis is the single basis every row shared; empty when they differ.
	AttributionBasis AttributionBasis
	// ConversionsComparable reports whether every row shared ONE KNOWN attribution
	// basis. When false, Conversions is empty: summing conversions counted under
	// different windows/models produces a number that is wrong in a way that looks
	// right. An unknown basis is never comparable, even with another unknown one.
	ConversionsComparable bool

	// RowCount is how many daily rows the summary covers.
	RowCount int
}

// SummariseMetrics derives a MetricsSummary from rows.
//
// It sums impressions and clicks unconditionally; it sums spend only when every row
// shares one currency, and conversions only when every row shares one KNOWN
// attribution basis. Decimal sums are computed exactly (see addDecimal), never via
// float64.
//
// An empty input yields a zero summary whose comparability flags are FALSE — an empty
// set has no established currency or basis to be comparable against, and reporting
// "comparable" for it would let a caller treat a later non-empty mixed set the same way.
func SummariseMetrics(rows []*CampaignMetric) MetricsSummary {
	s := MetricsSummary{RowCount: len(rows)}
	if len(rows) == 0 {
		return s
	}

	currency := rows[0].Currency
	basis := rows[0].AttributionBasis
	currencyUniform := true
	// An UNKNOWN basis is never comparable — not even with another unknown one. Two
	// rows that both failed to record how they were counted are not thereby counted
	// the same way, so seeding this with "basis is known" fails closed.
	basisUniform := basis != AttributionUnknown

	for _, r := range rows {
		s.Impressions += r.Impressions
		s.Clicks += r.Clicks
		if r.Currency != currency {
			currencyUniform = false
		}
		if r.AttributionBasis != basis || r.AttributionBasis == AttributionUnknown {
			basisUniform = false
		}
	}

	s.CurrencyUniform = currencyUniform
	if currencyUniform {
		s.Currency = currency
		sum := "0"
		for _, r := range rows {
			sum = addDecimal(sum, r.Spend)
		}
		s.Spend = sum
	}

	s.ConversionsComparable = basisUniform
	if basisUniform {
		s.AttributionBasis = basis
		sum := "0"
		for _, r := range rows {
			sum = addDecimal(sum, r.Conversions)
		}
		s.Conversions = sum
	}
	return s
}
