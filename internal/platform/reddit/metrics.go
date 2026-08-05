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
	"strconv"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// GetCampaignMetrics reads live campaign performance metrics (impressions, clicks,
// spend, derived CTR) for a single campaign from the Reddit Ads v3 reporting endpoint.
//
// # THE REQUEST/RESPONSE CONTRACT BELOW IS AN UNVERIFIED, BEST-EFFORT GUESS
//
// Unlike this package's create/toggle endpoints (ported from a working upstream
// TypeScript client) and unlike the Meta/LinkedIn/X metrics clients (built against
// each platform's PUBLIC API docs, even where unverified against a live account),
// Reddit's v3 reporting/metrics endpoint has NO public documentation at all: it is
// gated behind Reddit's developer portal and a private Postman collection
// (postman.com/reddit-ads-api). Third-party integrations (Supermetrics, Domo,
// Unified.to, Bright Analytics) prove the capability exists but do not publish the
// request/response shape they use. This was investigated and recorded as BLOCKED on
// LFXV2-2995 before this file was written.
//
// The implementation below is inferred ONLY from this package's own proven,
// already-working v3 conventions (see client.go): the "/ad_accounts/{account_id}/..."
// resource nesting, the OAuth2 bearer auth + request() retry/backoff plumbing, and the
// {"data": ...} JSON:API-flavored envelope every other endpoint in this client uses.
// It has NOT been verified against a live Reddit Ads account or against Reddit's real
// (gated) reporting contract, and may not match it. Do not treat this as a confirmed
// integration — treat every field name, the request shape, and the response shape as a
// placeholder to be corrected once official access (adsapi-partner-support@reddit.com,
// or Postman collection access) confirms the real contract.
func (c *Client) GetCampaignMetrics(ctx context.Context, campaignID string, window model.MetricsWindow) (*model.CampaignMetrics, error) {
	id := strings.TrimSpace(campaignID)
	if id == "" {
		return nil, fmt.Errorf("get campaign metrics: %w", ErrInvalidCampaignID)
	}
	if !accountIDRe.MatchString(id) {
		// Reuses the same letters/digits/underscores guard the create/toggle paths apply to
		// every id interpolated into a Reddit v3 path, so a stored id carrying '/', '?', or
		// '#' can't redirect the report request to a different resource.
		return nil, fmt.Errorf("get campaign metrics: %w", ErrInvalidCampaignID)
	}

	accountID := strings.TrimSpace(c.account.AccountID)
	if accountID == "" {
		return nil, fmt.Errorf("get campaign metrics: reddit account ID is not configured")
	}
	if !accountIDRe.MatchString(accountID) {
		return nil, fmt.Errorf("get campaign metrics: invalid reddit account ID %q", accountID)
	}

	startDate, endDate, err := dateRangeForWindow(window, c.now())
	if err != nil {
		return nil, fmt.Errorf("get campaign metrics: %w", err)
	}

	// GUESSED request shape: a POST carrying the same {"data": {...}} envelope the
	// create/toggle endpoints use, with the campaign scoped via campaign_ids and the
	// window as an ISO 8601 date range. The real contract may instead be a GET with
	// query params, a different date format, or different field names entirely.
	reqBody := map[string]any{
		"data": map[string]any{
			"starts_at":    startDate,
			"ends_at":      endDate,
			"campaign_ids": []string{id},
			"breakdowns":   []string{"campaign_id"},
			"fields":       []string{"impressions", "clicks", "spend"},
		},
	}

	resp, err := c.request(ctx, http.MethodPost, "/ad_accounts/"+accountID+"/reports", reqBody)
	if err != nil {
		return nil, fmt.Errorf("get campaign metrics: %w", err)
	}

	var rows []reportRow
	if err := json.Unmarshal(resp.Data, &rows); err != nil {
		return nil, &transportError{Method: http.MethodPost, Path: "reports", Err: fmt.Errorf("decode campaign metrics response: %w", err)}
	}
	if len(rows) == 0 {
		// No rows for the window is treated as real zero-activity, not an error — mirrors
		// every sibling platform's "campaign exists but had no activity" handling.
		return &model.CampaignMetrics{CampaignID: id, Window: window}, nil
	}

	var metrics model.CampaignMetrics
	for _, row := range rows {
		// The campaign_ids filter and this report contract are both UNVERIFIED — do not
		// trust that Reddit actually scoped the response to the requested campaign. A
		// blank or mismatched row id would otherwise silently fold another campaign's
		// impressions/clicks/spend into this one's totals.
		if row.CampaignID != id {
			return nil, &transportError{Method: http.MethodPost, Path: "reports", Err: fmt.Errorf("decode campaign metrics response: row campaign id %q does not match requested campaign %q", row.CampaignID, id)}
		}

		// Validate counter values before accumulation: reject negative impressions/clicks and
		// detect overflow before it corrupts the totals.
		if row.Impressions < 0 {
			return nil, &transportError{Method: http.MethodPost, Path: "reports", Err: fmt.Errorf("decode campaign metrics response: negative impressions %d", row.Impressions)}
		}
		if row.Clicks < 0 {
			return nil, &transportError{Method: http.MethodPost, Path: "reports", Err: fmt.Errorf("decode campaign metrics response: negative clicks %d", row.Clicks)}
		}
		// Checked addition for impressions: overflow if metrics.Impressions > MaxInt64 - row.Impressions.
		if row.Impressions > 0 && metrics.Impressions > math.MaxInt64-row.Impressions {
			return nil, &transportError{Method: http.MethodPost, Path: "reports", Err: fmt.Errorf("decode campaign metrics response: impressions total would overflow")}
		}
		metrics.Impressions += row.Impressions

		// Checked addition for clicks.
		if row.Clicks > 0 && metrics.Clicks > math.MaxInt64-row.Clicks {
			return nil, &transportError{Method: http.MethodPost, Path: "reports", Err: fmt.Errorf("decode campaign metrics response: clicks total would overflow")}
		}
		metrics.Clicks += row.Clicks

		if row.Spend != "" {
			spend, err := strconv.ParseFloat(row.Spend, 64)
			// ParseFloat accepts "NaN"/"Inf" as valid floats, and a finite-but-huge value
			// overflows the micros conversion below — reject both as the same malformed-
			// response error used elsewhere, rather than letting either corrupt CostMicros.
			if err != nil || math.IsNaN(spend) || math.IsInf(spend, 0) || spend < 0 || spend > math.MaxInt64/1_000_000 {
				if err == nil {
					err = fmt.Errorf("out of range")
				}
				return nil, &transportError{Method: http.MethodPost, Path: "reports", Err: fmt.Errorf("decode campaign metrics response: invalid spend %q: %w", row.Spend, err)}
			}
			// GUESSED: spend is assumed to be a decimal-currency string (mirroring Meta/
			// LinkedIn's reporting convention) rather than already-micro-scaled (like X's
			// billed_charge_local_micro) — this is exactly the kind of detail that cannot be
			// confirmed without the real contract, and rounds (not truncates) to avoid losing
			// a fractional micro-unit.
			costMicros := int64(math.Round(spend * 1_000_000))
			// Checked addition for cost: overflow if metrics.CostMicros > MaxInt64 - costMicros.
			if costMicros > 0 && metrics.CostMicros > math.MaxInt64-costMicros {
				return nil, &transportError{Method: http.MethodPost, Path: "reports", Err: fmt.Errorf("decode campaign metrics response: cost total would overflow")}
			}
			metrics.CostMicros += costMicros
		}
	}
	if metrics.Impressions > 0 {
		metrics.Ctr = float64(metrics.Clicks) / float64(metrics.Impressions)
	}
	metrics.CampaignID = id
	metrics.Window = window
	return &metrics, nil
}

// reportRow is the GUESSED shape of one row in the reporting response's "data" array —
// see the UNVERIFIED-CONTRACT warning on GetCampaignMetrics.
type reportRow struct {
	CampaignID  string `json:"campaign_id"`
	Impressions int64  `json:"impressions"`
	Clicks      int64  `json:"clicks"`
	// Spend is assumed to be a decimal-currency string (e.g. "12.34"), mirroring how this
	// client already parses Reddit's other decimal-string numeric fields.
	Spend string `json:"spend"`
}

// ErrInvalidCampaignID is returned when a caller-supplied campaign ID is empty or
// contains characters outside the safe charset this client requires for every id
// interpolated into a Reddit v3 API path.
var ErrInvalidCampaignID = errors.New("campaign id must be non-empty and contain only letters, digits, and underscores")

// ErrUnsupportedWindow is returned for a model.MetricsWindow this client does not map to a
// Reddit reporting date range.
var ErrUnsupportedWindow = errors.New("unsupported metrics window")

// dateRangeForWindow maps the shared model.MetricsWindow literal to a start/end date pair
// (YYYY-MM-DD, GUESSED format — see the UNVERIFIED-CONTRACT warning on GetCampaignMetrics).
// now is injected (via c.now(), already used elsewhere in this client for testability) rather
// than calling time.Now() directly.
func dateRangeForWindow(window model.MetricsWindow, now time.Time) (startDate, endDate string, err error) {
	now = now.UTC()
	end := now
	var start time.Time
	switch window {
	case model.MetricsWindowToday:
		start = now
	case model.MetricsWindowLast7Days:
		start = now.AddDate(0, 0, -6)
	case model.MetricsWindowLast30Days:
		start = now.AddDate(0, 0, -29)
	case model.MetricsWindowThisMonth:
		start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	case model.MetricsWindowLastMonth:
		firstOfThisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		end = firstOfThisMonth.AddDate(0, 0, -1)
		start = time.Date(end.Year(), end.Month(), 1, 0, 0, 0, 0, time.UTC)
	default:
		return "", "", fmt.Errorf("%w: %q", ErrUnsupportedWindow, window)
	}
	return start.Format("2006-01-02"), end.Format("2006-01-02"), nil
}
