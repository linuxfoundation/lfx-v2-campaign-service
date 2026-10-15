// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/identityjson"
)

// This file is the project-scoped BROKEN-DOWN report read shared by the audience read
// (audience.go: GENDER, COUNTRY, COMMUNITY) and the keyword performance read
// (keyword_performance.go: KEYWORD). Both ask the one verified reporting operation,
// POST /ad_accounts/{id}/reports, for one campaign at a time, grouped by one breakdown, and both
// must refuse exactly the same things — so the scope, budget, fan-out, decoding and guards live
// here once, and each read only says which breakdowns it wants and how it folds the totals.

const (
	// MaxReportCampaigns bounds the campaigns one broken-down read covers: one report per campaign
	// per breakdown (see readBreakdownReports for why), so the audience read is at most 8 × 3 = 24
	// reports — exactly the insight sub-budget (reportBudgetInsights) — and the keyword read 8. It
	// does NOT bound the requests sent: a report may span pages and a 429 is retried, and each of
	// those attempts is its own request. What bounds the requests is the ad account's process-wide
	// reporting budget (report_budget.go), from which every attempt — of these reads, the metrics
	// read and the account monitor alike — takes a slot before it is sent.
	MaxReportCampaigns = 8
	// reportBreakdownConcurrency bounds the reports in flight at once, below the monitor's own 5.
	reportBreakdownConcurrency = 3
	// breakdownMaxPages bounds one report's pagination walk. A walk that is still paging after this
	// many pages is not a result a read can trust to be complete — and a partial distribution looks
	// exactly like a whole one.
	breakdownMaxPages = 10
)

// ErrReportScopeTooLarge marks a broken-down read refused locally because the scope exceeds
// MaxReportCampaigns. Permanent while the project owns that many campaigns.
var ErrReportScopeTooLarge = errors.New("reddit-ads: too many campaigns to scope one report read to")

// ErrReportScopeInvalid marks a broken-down read refused locally because a scope entry is not a
// valid Reddit campaign id, or the scope is empty. Refused rather than dropped: a reduced scope
// would be presented as the project's whole picture, and an empty one has no campaign to filter on.
var ErrReportScopeInvalid = errors.New("reddit-ads: report read scope is empty or holds an invalid campaign id")

// errBreakdownPageCap is the walk's refusal once breakdownMaxPages pages still point at another.
var errBreakdownPageCap = fmt.Errorf("report still had more pages after %d; refusing a partial read", breakdownMaxPages)

// reportBreakdown is one report shape: the spec's `breakdowns` enum member, the row key its value
// arrives under, and the check a present (non-null) value must pass to be returned verbatim.
// dimension is the caller's own token for it.
type reportBreakdown struct {
	dimension string
	breakdown string
	rowKey    string
	// valueErr reports why a present value is untrustworthy; the message must not quote it.
	valueErr func(v string) error
}

// breakdownTotals is one breakdown value's counters within one campaign's report.
type breakdownTotals struct {
	Impressions int64
	Clicks      int64
	// CostMicros is SPEND, already microcurrency per the spec, in the ad account's own currency.
	CostMicros int64
}

// breakdownRead is a validated, ready-to-send broken-down read: the bound account, the window's
// report range and the de-duplicated campaign ids.
type breakdownRead struct {
	op               string
	accountID        string
	startsAt, endsAt string
	campaignIDs      []string
}

// prepareBreakdownRead runs every refusal a broken-down read makes BEFORE contacting Reddit: an
// unmapped window (dateRangeForWindow — the campaign metrics read's mapping, so both cover the
// same UTC days), an unaddressable bound account, and an empty, invalid or oversized scope.
func (c *Client) prepareBreakdownRead(op string, window model.MetricsWindow, campaignIDs []string) (*breakdownRead, error) {
	startsAt, endsAt, err := dateRangeForWindow(window, c.now())
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	accountID := strings.TrimSpace(c.account.AccountID)
	if err := ValidateAccountID(accountID); err != nil {
		// Configuration, not upstream content — but not echoed: this error reaches a log line.
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	ids, err := breakdownScope(op, campaignIDs)
	if err != nil {
		return nil, err
	}
	return &breakdownRead{op: op, accountID: accountID, startsAt: startsAt, endsAt: endsAt, campaignIDs: ids}, nil
}

// breakdownScope validates the campaign ids, collapsing duplicates. An empty, invalid or oversized
// scope is refused before any request.
func breakdownScope(op string, campaignIDs []string) ([]string, error) {
	if len(campaignIDs) == 0 {
		return nil, fmt.Errorf("%s: %w", op, ErrReportScopeInvalid)
	}
	seen := make(map[string]struct{}, len(campaignIDs))
	ids := make([]string, 0, len(campaignIDs))
	for _, id := range campaignIDs {
		// ValidateCampaignID also keeps the id out of the `filter` DSL's syntax: a comma would
		// split one term into two and widen the report.
		if err := ValidateCampaignID(id); err != nil {
			return nil, fmt.Errorf("%s: %w (%d bytes)", op, ErrReportScopeInvalid, len(id))
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) > MaxReportCampaigns {
		return nil, fmt.Errorf("%s: %d campaigns (at most %d): %w", op, len(ids), MaxReportCampaigns, ErrReportScopeTooLarge)
	}
	return ids, nil
}

// readBreakdownReports issues one report per (breakdown, campaign) and returns
// results[b][i] — breakdown b's per-value totals for campaign r.campaignIDs[i]. The unattributed
// value (Reddit's explicit null) is keyed "".
//
// One campaign per request, rather than one request listing every campaign, because the
// single-campaign filter term is the only filter form this repository has verified (the campaign
// metrics read and the account monitor use it): the spec says only that `filter` is "a
// comma-separated list", without saying whether two terms on the same field are OR-ed or AND-ed,
// and an AND would silently answer an empty report — no delivery — for a project that had plenty.
// The filter is ALSO checked on the way back: every row must name the requested campaign.
//
// All or nothing, failing fast: at most reportBreakdownConcurrency reports in flight, and the first
// failure cancels the rest. Request errors have the account id redacted from their path.
func (c *Client) readBreakdownReports(ctx context.Context, r *breakdownRead, breakdowns []reportBreakdown) ([][]map[string]breakdownTotals, error) {
	results := make([][]map[string]breakdownTotals, len(breakdowns))
	// Every attempt of a broken-down read (audience or keyword) draws on the account's insight
	// sub-budget as well as its total (report_budget.go).
	g, gctx := errgroup.WithContext(withInsightsReports(ctx))
	g.SetLimit(reportBreakdownConcurrency)
	for bi, b := range breakdowns {
		results[bi] = make([]map[string]breakdownTotals, len(r.campaignIDs))
		for i, id := range r.campaignIDs {
			g.Go(func() error {
				totals, rerr := c.readBreakdownReport(gctx, r, id, b)
				if rerr != nil {
					return fmt.Errorf("%s (%s): %w", r.op, b.dimension, redactReportPath(rerr, r.accountID))
				}
				results[bi][i] = totals
				return nil
			})
		}
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return results, nil
}

// readBreakdownReport walks one (campaign, breakdown) report's pages and totals its rows by
// breakdown value. The request asks for `breakdowns: [<DIM>]` AND `fields: [CAMPAIGN_ID,
// IMPRESSIONS, CLICKS, SPEND, <DIM>]` — grouped by the dimension and echoing it, as CAMPAIGN_ID is
// requested as a field for provenance. An explicit empty `metrics` array is real zero delivery (no
// values); a missing or null one is an error.
func (c *Client) readBreakdownReport(ctx context.Context, r *breakdownRead, campaignID string, b reportBreakdown) (map[string]breakdownTotals, error) {
	reqBody := map[string]any{
		"data": map[string]any{
			"starts_at":  r.startsAt,
			"ends_at":    r.endsAt,
			"fields":     []string{"CAMPAIGN_ID", "IMPRESSIONS", "CLICKS", "SPEND", b.breakdown},
			"breakdowns": []string{b.breakdown},
			"filter":     "campaign:id==" + campaignID,
		},
	}
	totals := map[string]breakdownTotals{}
	werr := c.walkPagesCapped(ctx, http.MethodPost, "/ad_accounts/"+r.accountID+"/reports", reqBody, breakdownMaxPages, errBreakdownPageCap, func(page int, resp *apiResponse) error {
		// The page is checked RAW, as a whole, before anything is decoded. encoding/json resolves a
		// repeated key — exact or case-folded, at ANY level — in favour of the last value with no
		// error, so `{"data":{"metrics":[…rows…]},"data":{"metrics":[]}}` would decode as Reddit's
		// authoritative empty report and real delivery would read as none; a repeated
		// `impressions` inside a row would replace one counter with another. resp.raw is the body
		// the envelope (Data, Pagination) was decoded from.
		if err := identityjson.Check(resp.raw); err != nil {
			return breakdownDecodeError(fmt.Errorf("page %d: %w", page, err))
		}
		raws, derr := decodeBreakdownRows(resp.Data)
		if derr != nil {
			return breakdownDecodeError(fmt.Errorf("page %d: %w", page, derr))
		}
		for i, raw := range raws {
			// reportRow carries the four fields every row here has, validated by the same
			// reportRow.validate the campaign metrics read uses: a missing OR explicit-null
			// counter is refused (never read as 0), as are a foreign or missing campaign id, a
			// negative counter and clicks without impressions.
			var row reportRow
			var keys map[string]json.RawMessage
			if err := json.Unmarshal(raw, &row); err != nil {
				// The decoder's message can quote a value; report the shape only.
				return breakdownDecodeError(fmt.Errorf("page %d row %d: row is not a report metric object of the expected shape (%d bytes)", page, i, len(raw)))
			}
			if err := json.Unmarshal(raw, &keys); err != nil {
				return breakdownDecodeError(fmt.Errorf("page %d row %d: row is not a JSON object (%d bytes)", page, i, len(raw)))
			}
			if err := row.validate(campaignID); err != nil {
				return breakdownDecodeError(fmt.Errorf("page %d row %d: %w", page, i, err))
			}
			value, verr := breakdownValue(rowKeyFold(keys, b.rowKey), b)
			if verr != nil {
				return breakdownDecodeError(fmt.Errorf("page %d row %d: %w", page, i, verr))
			}
			if _, dup := totals[value]; dup {
				// One campaign broken down by one dimension yields one row per value. A repeat
				// means overlapping pages or a double-reported row; summing it would double-count.
				return breakdownDecodeError(fmt.Errorf("page %d row %d: duplicate row for one breakdown value", page, i))
			}
			totals[value] = breakdownTotals{Impressions: *row.Impressions, Clicks: *row.Clicks, CostMicros: *row.Spend}
		}
		return nil
	})
	if werr != nil {
		return nil, werr
	}
	return totals, nil
}

// decodeBreakdownRows is decodeReportRows for raw rows: a missing, null or non-object `data`, or a
// missing or null `metrics` array, is an error; only an explicit array (possibly empty) is a report.
func decodeBreakdownRows(data json.RawMessage) ([]json.RawMessage, error) {
	if len(data) == 0 || string(data) == "null" {
		return nil, errors.New("response has no \"data\" object")
	}
	var env struct {
		Metrics *[]json.RawMessage `json:"metrics"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("\"data\" is not a report object (%d bytes)", len(data))
	}
	if env.Metrics == nil {
		return nil, errors.New("\"data\" has no \"metrics\" array")
	}
	return *env.Metrics, nil
}

// rowKeyFold returns the row's value under key, matched case-INSENSITIVELY — as encoding/json
// matches the counters decoded into reportRow from the same row — so a `"Gender"` echo is read
// like `"gender"` rather than refused as missing. At most one key can match: identityjson.Check has
// already refused the page if two keys of a row differ only in case.
func rowKeyFold(keys map[string]json.RawMessage, key string) json.RawMessage {
	if v, ok := keys[key]; ok {
		return v
	}
	for k, v := range keys {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return nil
}

// breakdownValue reads one breakdown value: an absent key is an error (the report was not grouped
// by the requested breakdown, so its rows cannot be attributed to a value), an explicit null is the
// unattributed value "" (the spec types every breakdown value as ["string","null"]), and a string
// must pass b.valueErr. No value is echoed in an error.
func breakdownValue(raw json.RawMessage, b reportBreakdown) (string, error) {
	if len(raw) == 0 {
		return "", fmt.Errorf("breakdown %q is missing from the row", b.rowKey)
	}
	if string(raw) == "null" {
		return "", nil
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", fmt.Errorf("breakdown %q is not a string (%d bytes)", b.rowKey, len(raw))
	}
	if err := b.valueErr(v); err != nil {
		return "", fmt.Errorf("breakdown %q: %w", b.rowKey, err)
	}
	return v, nil
}

// breakdownDecodeError is reportDecodeError for the broken-down reads: a 2xx whose body could not
// be believed, classified as a transport failure (→ 503), with the literal path "reports" so no
// account id reaches a log line.
func breakdownDecodeError(err error) error {
	return &transportError{
		Method: http.MethodPost,
		Path:   "reports",
		Err:    fmt.Errorf("decode broken-down report response: %w", err),
	}
}
