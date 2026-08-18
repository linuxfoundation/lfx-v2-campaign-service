// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// GetCampaignMetrics reads live campaign performance metrics (impressions, clicks,
// spend, derived CTR) for a single campaign from the Reddit Ads v3 reporting endpoint.
//
// # CONTRACT SOURCE: Reddit's official public OpenAPI spec (verified LFXV2-3282)
//
// The request and response shapes below are taken from Reddit's published OpenAPI
// document at https://ads-api.reddit.com/api/v3/openapi.json (linked as "Download Specs"
// from https://ads-api.reddit.com/docs/v3/), operation POST
// /ad_accounts/{ad_account_id}/reports. Reddit's own introduction states the Ads API "is
// open to all developers and does not require allowlisting or approval" — the docs are
// public and require no portal access.
//
// This supersedes the prior BLOCKED finding on LFXV2-2995, which recorded that Reddit
// published no public documentation for v3 reporting and that the shape here was an
// unverified guess inferred from this client's own conventions. That is no longer true.
// Reading the spec falsified four of the five guesses; see the notes at each site below
// and docs/knowledge/log/2026-08-18-LFXV2-3282-reddit-reporting-contract.md.
//
// STILL NOT VERIFIED: no request has been made against a live Reddit ad account, because
// this repository has no Reddit credentials. The shapes match Reddit's published schema,
// but behaviour that a schema cannot express — whether a campaign with no activity is
// omitted or returned as an explicit zero row, and whether the account's configured
// attribution window shifts the numbers — remains unconfirmed. Guards below that depend
// on such behaviour say so at their site.
func (c *Client) GetCampaignMetrics(ctx context.Context, campaignID string, window model.MetricsWindow) (*model.CampaignMetrics, error) {
	id := strings.TrimSpace(campaignID)
	if id == "" {
		return nil, fmt.Errorf("get campaign metrics: %w", ErrInvalidCampaignID)
	}
	if !accountIDRe.MatchString(id) {
		// Reuses the same letters/digits/underscores guard the create/toggle paths apply to
		// every id interpolated into a Reddit v3 path. Here it does double duty: the id is
		// also interpolated into the `filter` DSL string below, where a comma would split
		// one filter term into two and silently widen the report's scope.
		return nil, fmt.Errorf("get campaign metrics: %w", ErrInvalidCampaignID)
	}

	// Validated BEFORE the account is resolved, matching the ordering LinkedIn's
	// ValidateMetricsWindow documents: an unsupported window is a permanent 400 no matter
	// what state the account/connection is in, so checking it first stops a misconfigured
	// account from masking it as a retryable failure.
	startsAt, endsAt, err := dateRangeForWindow(window, c.now())
	if err != nil {
		return nil, fmt.Errorf("get campaign metrics: %w", err)
	}

	accountID := strings.TrimSpace(c.account.AccountID)
	if accountID == "" {
		return nil, fmt.Errorf("get campaign metrics: reddit account ID is not configured")
	}
	if !accountIDRe.MatchString(accountID) {
		// The account id is configuration, not upstream content, but it is not echoed here:
		// this error reaches the service's logging branch, and no id belongs in a log line.
		return nil, fmt.Errorf("get campaign metrics: reddit account ID is not a valid identifier")
	}

	// Request body per the spec's GetReportRequestBody schema: {"data": {...}} with
	// starts_at, ends_at, and fields required.
	//
	// CORRECTED against the spec (LFXV2-3282). The previous guessed body was wrong in
	// three ways, each of which would have failed the request or silently changed its
	// meaning:
	//   - `fields` and `breakdowns` are UPPERCASE enum members (IMPRESSIONS, CLICKS,
	//     SPEND, CAMPAIGN_ID), not the lowercase snake_case names guessed here before.
	//   - the campaign is scoped by `filter`, a comma-separated STRING DSL of the form
	//     "{entity}:{field}{op}{value}" — NOT a `campaign_ids` array, which the schema
	//     does not define at all (it sets additionalProperties:false, so the guessed key
	//     would have been rejected outright).
	//   - CAMPAIGN_ID must be requested as a FIELD to appear on each row. Requesting it
	//     only as a breakdown would group by campaign without returning the id, leaving
	//     the provenance check below nothing to verify against.
	//
	// `breakdowns` is deliberately omitted rather than set to CAMPAIGN_ID. The filter
	// already restricts the report to one campaign, so the only effect of a breakdown
	// would be to split the result into more rows; with none, Reddit aggregates over the
	// whole window, which is what CampaignMetrics represents. The accumulation loop below
	// still sums correctly if that assumption is wrong.
	//
	// `time_zone_id` is omitted: the spec documents it as defaulting to UTC, which is the
	// zone dateRangeForWindow renders its bounds in.
	reqBody := map[string]any{
		"data": map[string]any{
			"starts_at": startsAt,
			"ends_at":   endsAt,
			"fields":    []string{"CAMPAIGN_ID", "IMPRESSIONS", "CLICKS", "SPEND"},
			"filter":    "campaign:id==" + id,
		},
	}

	resp, err := c.request(ctx, http.MethodPost, "/ad_accounts/"+accountID+"/reports", reqBody)
	if err != nil {
		return nil, fmt.Errorf("get campaign metrics: %w", err)
	}

	rows, err := decodeReportRows(resp.Data)
	if err != nil {
		return nil, reportDecodeError(err)
	}
	if len(rows) == 0 {
		// An empty metrics array is real zero-activity, not an error — mirrors every
		// sibling platform's "campaign exists but had no activity" handling. Reachable
		// only for an explicit array: decodeReportRows rejects a null or missing metrics
		// field, so "no rows" here always means Reddit said so.
		return &model.CampaignMetrics{CampaignID: id, Window: window}, nil
	}

	var metrics model.CampaignMetrics
	for i, row := range rows {
		if err := row.validate(id); err != nil {
			return nil, reportDecodeError(fmt.Errorf("row %d: %w", i, err))
		}

		// validate() has rejected a nil field and a negative value, so each dereference is
		// safe and each value is individually in range. Only the running SUMS can overflow.
		if *row.Impressions > math.MaxInt64-metrics.Impressions {
			return nil, reportDecodeError(fmt.Errorf("row %d: impressions total would overflow", i))
		}
		metrics.Impressions += *row.Impressions

		if *row.Clicks > math.MaxInt64-metrics.Clicks {
			return nil, reportDecodeError(fmt.Errorf("row %d: clicks total would overflow", i))
		}
		metrics.Clicks += *row.Clicks

		// CORRECTED against the spec (LFXV2-3282): spend is an int64 in MICROCURRENCY
		// ("The amount spent during this report period (microcurrency)"), so it needs no
		// conversion at all. The previous code parsed it as a decimal-currency STRING and
		// multiplied by 1e6 — against the real contract that path fails to decode outright
		// (a JSON number into a Go string), which is the loud failure, but had it been
		// written to coerce instead it would have reported every spend figure one million
		// times too large. This is also the one guess the client's own proven conventions
		// already argued against: every monetary value the create path sends is an integer
		// micro-dollar count (goal_value carries toMicrodollars(BudgetUSD)).
		//
		// NOTE the spec says "microcurrency", not micro-dollars: the unit is the ad
		// account's own billing currency, which this client does not read. CostMicros is
		// therefore micros of an unspecified currency, exactly as it is for X — callers
		// must not sum it across platforms. internal/service records the same caveat.
		if *row.Spend > math.MaxInt64-metrics.CostMicros {
			return nil, reportDecodeError(fmt.Errorf("row %d: cost total would overflow", i))
		}
		metrics.CostMicros += *row.Spend
	}
	// CTR is recomputed from the totals rather than read from the row's own `ctr` field,
	// which the spec also offers. Across multiple rows a per-row rate cannot be summed,
	// and averaging rates weights a quiet day equally with a busy one; deriving it once
	// from the totals is correct for any row count and matches every sibling client.
	if metrics.Impressions > 0 {
		metrics.Ctr = float64(metrics.Clicks) / float64(metrics.Impressions)
	}
	metrics.CampaignID = id
	metrics.Window = window
	return &metrics, nil
}

// reportDecodeError wraps a report-decoding failure as a transportError, the
// classification this client uses for a 2xx whose body could not be believed.
//
// The path is the literal "reports" rather than the real request path: the account id is
// interpolated into that path, and transportError.Err reaches the service's error log.
func reportDecodeError(err error) error {
	return &transportError{
		Method: http.MethodPost,
		Path:   "reports",
		Err:    fmt.Errorf("decode campaign metrics response: %w", err),
	}
}

// reportEnvelope is the response's "data" object, per the spec's Report schema.
//
// CORRECTED against the spec (LFXV2-3282): "data" is an OBJECT carrying a "metrics"
// array, not the bare array the previous code unmarshaled into. Against the real
// contract that decode fails outright rather than misreporting, but it would have made
// every read fail with a JSON type error naming an internal Go type, which says nothing
// about what the endpoint actually returned.
//
// Metrics is a pointer so a missing or null "metrics" field is distinguishable from an
// explicit empty array: the first is a response we cannot interpret, the second is
// genuine zero activity. Reporting the former as zero would publish a failure as a
// measurement.
type reportEnvelope struct {
	Metrics *[]reportRow `json:"metrics"`
}

// decodeReportRows decodes the report envelope's "data" field into metric rows.
//
// A null, missing, or non-object data field, and a null or missing metrics array within
// it, are all errors rather than zero activity. Only an explicit array means the campaign
// genuinely had none.
func decodeReportRows(data json.RawMessage) ([]reportRow, error) {
	if len(data) == 0 {
		return nil, errors.New("response has no \"data\" field")
	}
	var env reportEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		// The decoder's message can quote fragments of the response body, which is
		// unvalidated upstream content that reaches the server log. Report the shape
		// failure and the byte length instead, matching LinkedIn's metrics decoder.
		return nil, fmt.Errorf("\"data\" is not a report object (%d bytes)", len(data))
	}
	if env.Metrics == nil {
		return nil, errors.New("\"data\" has no \"metrics\" array")
	}
	return *env.Metrics, nil
}

// reportRow is one entry of the report response's "metrics" array, per the spec's
// ReportMetric schema. That schema defines over a thousand optional properties; only the
// four this client requests are decoded.
//
// EVERY FIELD IS A POINTER ON PURPOSE, and this is not defensive habit — the spec types
// impressions, clicks, and spend as ["integer","null"], so Reddit may return an explicit
// JSON null for any of them. A value field would decode null to 0, which is
// indistinguishable from a campaign that genuinely served nothing. A pointer makes
// "absent or null" distinguishable from "zero", and validate() turns it into a refusal.
//
// This is stricter than googleads, deliberately and for a documented reason: Google Ads
// is DOCUMENTED to omit zero-valued metrics, so there an empty value is a real zero.
// Reddit's spec documents no such omit-on-zero behaviour, so there is nothing here to
// justify reading an absent metric as a measurement of zero. If a live account later
// shows Reddit does omit zero metrics, relax these guards deliberately, with that
// evidence in hand — not by observing zeros.
type reportRow struct {
	CampaignID  *string `json:"campaign_id"`
	Impressions *int64  `json:"impressions"`
	Clicks      *int64  `json:"clicks"`
	// Spend is int64 microcurrency per the spec — NOT a decimal string. See the unit note
	// in GetCampaignMetrics.
	Spend *int64 `json:"spend"`
}

// validate rejects a row that cannot be believed: a missing or null field, a mismatched
// campaign id, or a negative counter.
//
// No upstream value is interpolated into any message here. These errors reach the
// service's error log, and the response is unvalidated content.
func (r reportRow) validate(wantCampaignID string) error {
	// A missing field means either that Reddit returned an explicit null (which the spec
	// permits for every metric) or that the requested field was not echoed onto the row.
	// Checked first so an unusable row is reported as "the fields are not there" rather
	// than as some downstream symptom of it.
	var missing []string
	// NOTE this particular arm is NOT revert-binding, and that is recorded rather than
	// claimed as coverage. Making CampaignID a value field and dropping this check leaves
	// every test passing: a nil id decodes to "", which the provenance comparison below
	// then rejects anyway, since "" can never equal a valid campaign id. The arm stays
	// because it reports the accurate CAUSE — "the row carried no campaign_id" rather than
	// "the row is for a different campaign" — and that distinction is what a responder
	// acts on. Mutation-verified as surviving; the metric-field arms below are killed.
	if r.CampaignID == nil {
		missing = append(missing, "campaign_id")
	}
	if r.Impressions == nil {
		missing = append(missing, "impressions")
	}
	if r.Clicks == nil {
		missing = append(missing, "clicks")
	}
	if r.Spend == nil {
		missing = append(missing, "spend")
	}
	if len(missing) > 0 {
		// Naming the absent fields is what makes a failure actionable in one read: the
		// responder learns which requested fields the report did not carry, rather than
		// debugging a zero. These names are this package's own literals, matching the
		// `fields` list sent above, so they are safe to echo.
		return fmt.Errorf("row is missing or null field(s) %s — the report did not carry every requested field; expected campaign_id, impressions, clicks, spend", strings.Join(missing, ", "))
	}

	// Do not trust that the `filter` term actually scoped the report to this campaign: a
	// filter that Reddit parsed differently than intended would otherwise fold another
	// campaign's impressions, clicks, and spend into these totals. The ids are NOT echoed
	// — a mismatched id is upstream content.
	if *r.CampaignID != wantCampaignID {
		return errors.New("row campaign id does not match the requested campaign")
	}
	if *r.Impressions < 0 {
		return errors.New("negative impressions")
	}
	if *r.Clicks < 0 {
		return errors.New("negative clicks")
	}
	if *r.Spend < 0 {
		return errors.New("negative spend")
	}
	// Clicks without impressions is not a low number, it is an impossible one: every click
	// is preceded by the impression that carried it. Reaching here means the row does not
	// describe reality, and reporting it would publish Ctr=0 beside a non-zero click
	// count. LinkedIn's metrics client applies the same guard for the same reason.
	if *r.Clicks > 0 && *r.Impressions == 0 {
		return errors.New("row reports clicks with zero impressions")
	}
	return nil
}

// ErrInvalidCampaignID is returned when a caller-supplied campaign ID is empty or
// contains characters outside the safe charset this client requires for every id
// interpolated into a Reddit v3 API path or report filter expression.
var ErrInvalidCampaignID = errors.New("campaign id must be non-empty and contain only letters, digits, and underscores")

// ErrUnsupportedWindow is returned for a model.MetricsWindow this client does not map to a
// Reddit reporting date range.
var ErrUnsupportedWindow = errors.New("unsupported metrics window")

// supportedMetricsWindows is exactly the set dateRangeForWindow maps to a Reddit reporting
// range. Package-level and clock-free so a caller can answer "Reddit cannot serve this
// window" without a Client, credentials, or a network call — the same reason LinkedIn's
// equivalent exists.
var supportedMetricsWindows = map[model.MetricsWindow]struct{}{
	model.MetricsWindowToday:      {},
	model.MetricsWindowLast7Days:  {},
	model.MetricsWindowLast30Days: {},
	model.MetricsWindowThisMonth:  {},
	model.MetricsWindowLastMonth:  {},
}

// ValidateMetricsWindow reports whether this client can map window to a Reddit reporting
// range, returning ErrUnsupportedWindow if it cannot.
func ValidateMetricsWindow(window model.MetricsWindow) error {
	if _, ok := supportedMetricsWindows[window]; !ok {
		return fmt.Errorf("%w: %q", ErrUnsupportedWindow, window)
	}
	return nil
}

// reportTimestampLayout renders a report bound in the format the spec requires for
// starts_at/ends_at: "Must follow the `YYYY-MM-DDTHH:00:00Z` format. Can only be of
// hourly granularity."
//
// CORRECTED against the spec (LFXV2-3282). Both earlier renderings were wrong: a bare
// "YYYY-MM-DD" omits the required time component, and this client's own
// toRedditTimestamp renders a "+00:00" offset where the reporting endpoint documents a
// literal "Z". A rejected format is the loud failure; the quiet one is a format Reddit
// accepts but reads as a different instant, which returns a well-formed report for the
// wrong period — a number that looks entirely healthy. That is why this layout is pinned
// here rather than reusing the write path's helper.
const reportTimestampLayout = "2006-01-02T15:00:00Z"

// dateRangeForWindow maps the shared model.MetricsWindow literal to the starts_at/ends_at
// pair for a report request.
//
// The window is anchored to the UTC calendar date: now is converted with .UTC() before
// year/month/day are extracted, so every caller gets the same range regardless of the
// process clock's zone, and it matches the UTC default the spec documents for
// time_zone_id. Both month boundaries are derived from the first-of-month anchor rather
// than via AddDate(0, -1, 0) on today's day-of-month, since AddDate normalizes an invalid
// day (subtracting a month from the 31st) into the FOLLOWING month, which would silently
// shift this_month and last_month on 29th/30th/31st days.
//
// The end bound is the 23:00 hour of its day, not midnight. The spec permits only hourly
// granularity, so 23:00 is the last addressable hour of an inclusive final day; rendering
// midnight instead would ask for a range that stops as the final day BEGINS and would
// drop almost all of it, reporting a number that is merely low rather than obviously
// wrong. Whether Reddit treats ends_at as inclusive of that hour is not stated by the
// spec and cannot be settled without a live account: if it is exclusive, this range
// under-reports by the final hour. That is recorded rather than assumed away.
//
// now is injected (via c.now()) rather than read from time.Now() directly.
func dateRangeForWindow(window model.MetricsWindow, now time.Time) (startsAt, endsAt string, err error) {
	// Checked first so this function and ValidateMetricsWindow can never disagree about
	// which windows are supported; TestValidateMetricsWindowMatchesDateRangeForWindow pins
	// that they agree for every model.MetricsWindow.
	if err := ValidateMetricsWindow(window); err != nil {
		return "", "", err
	}
	now = now.UTC()
	year, month, day := now.Date()
	today := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	firstOfThisMonth := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)

	var start, end time.Time
	switch window {
	case model.MetricsWindowToday:
		start, end = today, today
	case model.MetricsWindowLast7Days:
		start, end = today.AddDate(0, 0, -6), today // 6 days before today = 7 inclusive
	case model.MetricsWindowLast30Days:
		start, end = today.AddDate(0, 0, -29), today // 29 days before today = 30 inclusive
	case model.MetricsWindowThisMonth:
		start, end = firstOfThisMonth, today
	case model.MetricsWindowLastMonth:
		// One day before the first of this month is always the last day of the previous
		// month, regardless of how many days that month has.
		lastDayOfLastMonth := firstOfThisMonth.AddDate(0, 0, -1)
		start = time.Date(lastDayOfLastMonth.Year(), lastDayOfLastMonth.Month(), 1, 0, 0, 0, 0, time.UTC)
		end = lastDayOfLastMonth
	default:
		// Unreachable: ValidateMetricsWindow above rejects anything not in the switch, and
		// a test pins the two in agreement. Kept as a defensive backstop.
		return "", "", fmt.Errorf("%w: %q", ErrUnsupportedWindow, window)
	}
	return start.Format(reportTimestampLayout), end.Add(23 * time.Hour).Format(reportTimestampLayout), nil
}
