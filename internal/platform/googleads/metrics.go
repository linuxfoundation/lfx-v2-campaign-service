// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// MetricsWindow is a Google Ads GAQL predefined date-range literal accepted after
// a GAQL query's DURING keyword. Only a fixed allow-list is supported — the value
// is concatenated directly into the GAQL query string (GAQL has no query
// parameters), so an unvalidated caller-supplied window would be a query-injection
// vector the same way an unvalidated campaign id would be.
type MetricsWindow string

// Supported metrics windows. This is a deliberately small subset of GAQL's full
// predefined-date-range literal set (TODAY, YESTERDAY, LAST_7_DAYS, LAST_14_DAYS,
// LAST_30_DAYS, THIS_MONTH, LAST_MONTH, plus several week-aligned variants this
// broker has no current caller for) — extend the allow-list, don't bypass it.
const (
	WindowToday      MetricsWindow = "TODAY"
	WindowYesterday  MetricsWindow = "YESTERDAY"
	WindowLast7Days  MetricsWindow = "LAST_7_DAYS"
	WindowLast14Days MetricsWindow = "LAST_14_DAYS"
	WindowLast30Days MetricsWindow = "LAST_30_DAYS"
	WindowThisMonth  MetricsWindow = "THIS_MONTH"
	WindowLastMonth  MetricsWindow = "LAST_MONTH"

	// defaultMetricsWindow is used when the caller passes an empty MetricsWindow.
	defaultMetricsWindow = WindowLast30Days
)

var validMetricsWindows = map[MetricsWindow]struct{}{
	WindowToday:      {},
	WindowYesterday:  {},
	WindowLast7Days:  {},
	WindowLast14Days: {},
	WindowLast30Days: {},
	WindowThisMonth:  {},
	WindowLastMonth:  {},
}

// ErrUnsupportedWindow is returned for a model.MetricsWindow this client does not map
// to a GAQL literal. Google Ads happens to cover the whole platform-agnostic vocabulary
// today, so this is only reachable for a value outside that closed set — but the mapping
// is still fallible by construction, since a window added to model must be mapped here
// deliberately rather than silently falling through to the default range.
var ErrUnsupportedWindow = errors.New("google ads does not support this metrics window")

// WindowFor translates the platform-agnostic model.MetricsWindow into this client's GAQL
// literal. The mapping lives in the platform package, not the dispatcher, so Google's
// dialect never leaks past this boundary — the same split every other platform client uses.
// The result still goes through validMetricsWindows in GetCampaignMetrics: this function is
// a translation, not a substitute for the injection guard.
func WindowFor(w model.MetricsWindow) (MetricsWindow, error) {
	switch w {
	case model.MetricsWindowToday:
		return WindowToday, nil
	case model.MetricsWindowYesterday:
		return WindowYesterday, nil
	case model.MetricsWindowLast7Days:
		return WindowLast7Days, nil
	case model.MetricsWindowLast14Days:
		return WindowLast14Days, nil
	case model.MetricsWindowLast30Days:
		return WindowLast30Days, nil
	case model.MetricsWindowThisMonth:
		return WindowThisMonth, nil
	case model.MetricsWindowLastMonth:
		return WindowLastMonth, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnsupportedWindow, string(w))
	}
}

// CampaignMetrics is the aggregated performance snapshot for one campaign over
// one MetricsWindow. It is a live read-through of Google Ads — this client never
// persists it.
type CampaignMetrics struct {
	CampaignID  string        `json:"campaignId"`
	Window      MetricsWindow `json:"window"`
	Impressions int64         `json:"impressions"`
	Clicks      int64         `json:"clicks"`
	CostMicros  int64         `json:"costMicros"`
	// Ctr is Clicks/Impressions, 0 when Impressions is 0 (never divides by zero).
	Ctr float64 `json:"ctr"`
	// Conversions is metrics.conversions, carried through with its FRACTION intact — the
	// field reference types it DOUBLE because Google credits fractional conversions under
	// data-driven and position-based attribution. It is a POINTER for cross-adapter
	// consistency with model.CampaignMetrics.Conversions, where nil means "this platform
	// cannot measure conversions at all". Google can, so this adapter never leaves it nil:
	// metrics.conversions is always selected, and proto3 JSON omits default values, so an
	// absent member is a measured 0.0 and is reported as a non-nil zero.
	Conversions *float64 `json:"conversions,omitempty"`
	// ConversionValue is metrics.conversions_value: the summed value of those conversions, in
	// WHOLE units of the account's currency — not micros, and deliberately not converted to
	// them. The field reference types it DOUBLE and Google reports it in currency units, so
	// scaling it here would invent a precision the upstream number does not have and would
	// need its own overflow guard to do it. The asymmetry with CostMicros is real and is why
	// Roas below exists: no caller should have to reconcile the two units itself.
	//
	// A POINTER on the same contract as Conversions: nil means no complete measurement. This
	// adapter never leaves it nil — conversions_value is always selected, and proto3 JSON
	// omits default values, so an absent member is a measured 0.0.
	ConversionValue *float64 `json:"conversionValue,omitempty"`
	// Roas is ConversionValue divided by cost in the same currency — conversions_value over
	// cost_micros/1e6 — and is nil whenever that division would not be a measurement.
	//
	// Two distinct cases reach nil and both must, because the alternative in each is a
	// fabricated figure. ConversionValue nil is an unmeasured numerator. CostMicros == 0 is a
	// window with no spend, where the ratio is either 0/0 or a division by zero: a campaign
	// that earned value without spending has an undefined return on ad spend, not an infinite
	// one, and reporting 0 there would say the opposite of what happened.
	//
	// Derived HERE rather than left to each consumer, for the reason the repo keeps relearning
	// on keys: two call sites dividing the same pair of fields will eventually disagree about
	// the zero-cost case, and the disagreement surfaces as a dashboard number nobody can trace.
	Roas *float64 `json:"roas,omitempty"`
}

// gaqlMetricsRow is the shape of one googleAds:search result row for the
// campaign-metrics query below. Google Ads REST returns int64-valued fields
// (resource ids and metrics) as JSON strings to avoid float64 precision loss, so
// the INTEGER fields here are strings despite representing numbers. Conversions is the
// exception and is a bare number: it is declared DOUBLE upstream, so there is no int64
// precision to protect and no string encoding applied.
type gaqlMetricsRow struct {
	Campaign struct {
		ID string `json:"id"`
	} `json:"campaign"`
	Metrics struct {
		Impressions string `json:"impressions"`
		Clicks      string `json:"clicks"`
		CostMicros  string `json:"costMicros"`
		// Conversions is a JSON NUMBER, not a string, and that difference is a type fact
		// rather than an inconsistency worth normalising away. Google Ads REST encodes
		// int64-valued fields as strings to protect them from float64 precision loss;
		// metrics.conversions is declared DOUBLE in the field reference, so it is already a
		// float and is serialized as a bare number. Decoding it into a string field would
		// fail on every response that carries a conversion.
		//
		// The pointer distinguishes an omitted field from a present 0.0.
		Conversions *float64 `json:"conversions"`
		// ConversionsValue is metrics.conversions_value and is a bare JSON number for the same
		// reason Conversions is: the field reference declares it DOUBLE, so REST does not
		// string-encode it. Named for the GAQL column it decodes rather than for the domain
		// field it feeds, because this struct's only job is to match the wire.
		//
		// UNVERIFIED ASSUMPTION (flag before relying on this in production, same convention
		// as the segments.date aggregation note on GetCampaignMetrics): the REST member name
		// is the lowerCamelCase of the GAQL column, i.e. conversions_value -> conversionsValue,
		// as cost_micros -> costMicros already is two fields above. If that is wrong the field
		// never decodes, and because an absent member is deliberately read as a MEASURED 0.0,
		// the failure is silent — every campaign would report conversionValue 0 and roas 0
		// rather than erroring. No fixture can catch it; only a live response can.
		ConversionsValue *float64 `json:"conversionsValue"`
	} `json:"metrics"`
}

// parseMetricInt parses a metric string value, treating empty string as zero.
// Google Ads omits zero-valued metrics from REST JSON, leaving empty strings
// on unmarshaling; this func treats empty as 0 rather than a parse error.
func parseMetricInt(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.ParseInt(s, 10, 64)
}

// isUsableMetricDouble reports whether v is a magnitude this package is willing to
// report as a measurement. It exists so every double this package reports is held to
// one standard by construction rather than by inline tests a later edit can drift
// apart. Three call sites share it: the campaign conversion COUNT and conversion
// VALUE in GetCampaignMetrics, and the keyword-row conversion count in keywords.go,
// which carried a fourth copy of the same four conditions and a comment saying it
// matched this one. Each caller keeps its own error sentence — the fields fail for
// the same reason but are not the same field.
//
// NaN and ±Inf both survive JSON decoding of a bare number in some encoders, and a
// negative figure is upstream corruption rather than a small number — the same guard
// meta/metrics.go and reddit/metrics.go apply to spend.
//
// The bound is float64(math.MaxInt64) compared with '>=', not '>': MaxInt64 is not
// exactly representable as a float64, so float64(math.MaxInt64) rounds UP to 2^63,
// one more than MaxInt64. A value of exactly 2^63 would pass a '>' guard and then
// wrap to MinInt64 wherever such a value is later narrowed. Same reasoning as
// meta/metrics.go's spend-scaling guard.
func isUsableMetricDouble(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v < float64(math.MaxInt64)
}

// GetCampaignMetrics reads impressions/clicks/cost for one campaign over window
// (defaulting to WindowLast30Days when empty) via a single GAQL search.
//
// UNVERIFIED ASSUMPTION (flag before relying on this in production, same
// convention as the GA-3 AdGroupAd composite-resourceName and GA-4
// observation-targeting flags): a segments.date WHERE-clause filter without
// segments.date in the SELECT list is documented by Google to filter rows
// without segmenting them, so this query returns at most one row — the metrics
// aggregated across the whole window, not one row per day. Verify against a live
// account before trusting the aggregate.
//
// A campaign with no impressions in the window is not an error: Google Ads omits
// it from results entirely, and this method returns a zero-value CampaignMetrics
// rather than surfacing "not found".
func (c *Client) GetCampaignMetrics(ctx context.Context, campaignID string, window MetricsWindow) (*CampaignMetrics, error) {
	// customerIDRE (defined in client.go) enforces digits-only IDs to prevent GAQL
	// injection, since the ID is concatenated directly into the query string below.
	id := strings.TrimSpace(campaignID)
	if !customerIDRE.MatchString(id) {
		return nil, fmt.Errorf("get campaign metrics: campaign id %q must be digits only", campaignID)
	}
	w := window
	if w == "" {
		w = defaultMetricsWindow
	}
	if _, ok := validMetricsWindows[w]; !ok {
		return nil, fmt.Errorf("get campaign metrics: unsupported window %q", window)
	}

	query := fmt.Sprintf(
		"SELECT campaign.id, metrics.impressions, metrics.clicks, metrics.cost_micros, "+
			"metrics.conversions, metrics.conversions_value "+
			"FROM campaign WHERE campaign.id = %s AND segments.date DURING %s",
		id, w,
	)
	rows, err := c.gaqlSearch(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("get campaign metrics: %w", err)
	}
	if len(rows) == 0 {
		// A no-activity window is a MEASUREMENT, not an absence of one. Google Ads omits a
		// campaign from results entirely when it had no impressions, so this branch means the
		// query ran and the answer was zero — not that conversions could not be measured here.
		// Conversions is therefore a non-nil zero, exactly as the row-decoding path below
		// materialises it, keeping this adapter's invariant ("it never leaves the count nil")
		// true on every path. nil is reserved for platforms that cannot report a campaign-level
		// conversion count at all, and this is not one of them.
		//
		// The reason is the invariant itself, NOT any effect on the no_conversions rule. That
		// rule cannot fire on this branch whichever way the pointer goes: it is gated on
		// in.Clicks >= minClicksForConversions (internal/service/rules/actions.go), and a
		// window with no rows has zero clicks. What the honest zero actually buys is that
		// every consumer of this struct — the metrics response, a conversion total, anything
		// reading the pointer — sees "measured, and the answer was none" rather than
		// "unmeasured", which is the true state of a window Google did answer.
		// The same reasoning applies to the conversion VALUE, which is selected on the same
		// query and so was answered by the same no-rows result: measured, and the answer was
		// none. Roas stays nil here and is the one field that must — this window spent nothing,
		// and a return on zero ad spend is undefined, not zero.
		//
		// Two separate variables, not one taken twice: a single &zero would alias both pointers
		// to the same float64, so anything writing through one would silently move the other.
		zeroConversions, zeroValue := 0.0, 0.0
		return &CampaignMetrics{
			CampaignID:      id,
			Window:          w,
			Conversions:     &zeroConversions,
			ConversionValue: &zeroValue,
		}, nil
	}
	// Enforce the single-row assumption documented above rather than trusting it.
	// Reading rows[0] alone is only correct while the query segments by nothing; a
	// future SELECT that reintroduces a segmenting field would silently UNDERREPORT
	// every metric, and an underreport is indistinguishable from a genuinely quiet
	// campaign. Failing loudly is the only outcome a caller can act on: silently
	// summing here would instead paper over a query change that also invalidates
	// CampaignMetrics.Window, since each row would then cover a sub-window.
	if len(rows) > 1 {
		return nil, fmt.Errorf(
			"get campaign metrics: expected at most 1 aggregated row for campaign %s, got %d "+
				"(the GAQL query is segmenting; metrics would be underreported)", id, len(rows))
	}

	var row gaqlMetricsRow
	if err := json.Unmarshal(rows[0], &row); err != nil {
		return nil, &transportError{
			Method: http.MethodPost,
			Path:   c.customerPath("googleAds:search"),
			Err:    fmt.Errorf("decode campaign metrics row: %w", err),
		}
	}

	// Google Ads marks metric fields optional and omits zero-valued metrics from REST JSON,
	// so unmarshaling leaves empty strings. Treat empty as 0, parse non-empty strictly.
	impressions, errImpressions := parseMetricInt(row.Metrics.Impressions)
	clicks, errClicks := parseMetricInt(row.Metrics.Clicks)
	costMicros, errCost := parseMetricInt(row.Metrics.CostMicros)
	if errImpressions != nil || errClicks != nil || errCost != nil {
		// Name the fields that failed, never their values. These strings come straight from
		// the upstream response body, and the service's default failure branch renders this
		// error into a warning log — echoing the raw value would let a malformed metric
		// inject arbitrary attacker-influenced text (including newlines) into the log stream.
		// Which fields failed is what a responder actually needs; the value itself is
		// recoverable from the upstream request if it is ever wanted.
		var bad []string
		if errImpressions != nil {
			bad = append(bad, "impressions")
		}
		if errClicks != nil {
			bad = append(bad, "clicks")
		}
		if errCost != nil {
			bad = append(bad, "costMicros")
		}
		return nil, &transportError{
			Method: http.MethodPost,
			Path:   c.customerPath("googleAds:search"),
			Err:    fmt.Errorf("decode campaign metrics row: non-numeric metric field(s): %s", strings.Join(bad, ", ")),
		}
	}

	m := &CampaignMetrics{
		CampaignID:  id,
		Window:      w,
		Impressions: impressions,
		Clicks:      clicks,
		CostMicros:  costMicros,
	}
	if impressions > 0 {
		m.Ctr = float64(clicks) / float64(impressions)
	}
	// metrics.conversions is ALWAYS in this method's SELECT list, and Google Ads REST encodes
	// responses as proto3 JSON, which OMITS fields holding the default value. An absent
	// conversions member on a row we explicitly selected is therefore the encoding of a
	// measured 0.0 — not "Google could not measure this". Materialising a non-nil zero is the
	// same treatment parseMetricInt already gives an omitted impressions/clicks value.
	//
	// nil is reserved for adapters whose PLATFORM cannot report a campaign-level conversion
	// count at all (Meta, X, Reddit, email), which is a different fact and the reason the
	// domain field is a pointer. Leaving Google nil on a zero would have meant no_conversions
	// never fired for a Google campaign that genuinely converted nobody — the rule's whole
	// purpose.
	conv := 0.0
	if row.Metrics.Conversions != nil {
		conv = *row.Metrics.Conversions
	}
	{
		// Reject malformed magnitudes rather than folding them into a count: each would
		// otherwise become a figure the dashboard renders as a measurement. See
		// isUsableMetricDouble for what the four rejected shapes are and why the bound sits
		// where it does.
		if !isUsableMetricDouble(conv) {
			return nil, &transportError{
				Method: http.MethodPost,
				Path:   c.customerPath("googleAds:search"),
				Err:    fmt.Errorf("decode campaign metrics row: conversions is not a usable count"),
			}
		}
		// Carried through UNROUNDED. metrics.conversions is a DOUBLE because Google credits
		// fractional conversions under data-driven and position-based attribution, so a
		// campaign can genuinely hold 0.4 of a conversion. Rounding that to 0 would report a
		// converting campaign as having produced nothing, and the no_conversions rule reads
		// exactly this number — so the rounding would manufacture the very finding the rule
		// exists to report honestly. Only an upstream value of exactly zero may fire it.
		m.Conversions = &conv
	}
	// metrics.conversions_value rides the same SELECT list and the same proto3-JSON omission
	// rule, so an absent member is again a measured 0.0 rather than an unmeasured one.
	convValue := 0.0
	if row.Metrics.ConversionsValue != nil {
		convValue = *row.Metrics.ConversionsValue
	}
	{
		// The same predicate the conversion COUNT gets, deliberately. A negative total is
		// upstream corruption rather than a small number here too: conversion adjustments
		// (retractions and restatements) subtract from this figure, but they subtract value
		// that was credited, so the aggregate Google reports does not go below zero.
		//
		// The shared ceiling binds this field even though it is never narrowed to an int64 —
		// it stays a float64 all the way out. A conversion value that large is corruption on
		// any currency, and letting it through while rejecting the same magnitude one field
		// above would be an inconsistency a reader has to litigate.
		if !isUsableMetricDouble(convValue) {
			return nil, &transportError{
				Method: http.MethodPost,
				Path:   c.customerPath("googleAds:search"),
				Err:    fmt.Errorf("decode campaign metrics row: conversions value is not a usable amount"),
			}
		}
		m.ConversionValue = &convValue
	}
	// ROAS is derived here, once, so no consumer has to decide what a zero-cost window means.
	// The test is '> 0', which excludes two cases at once. Zero cost is the documented one —
	// see the Roas field comment for why an undefined return must stay nil rather than become
	// 0. Negative cost is the undocumented one: parseMetricInt uses strconv.ParseInt, which
	// accepts a leading minus, so a corrupt upstream cost reaches here intact, and dividing by
	// it would produce a negative ROAS that reads as a real (terrible) result. Withholding the
	// number is the honest answer to a cost that cannot be true.
	if costMicros > 0 {
		roas := convValue / (float64(costMicros) / 1e6)
		m.Roas = &roas
	}
	return m, nil
}
