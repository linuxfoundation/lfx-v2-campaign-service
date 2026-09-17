// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	// monitorPageSize is the per-page limit for the account-monitor's campaign-list and
	// insights reads. Meta accepts a much larger `limit`, but a smaller page just means
	// more round trips, not a truncated result — see fetchAccountCampaignList and
	// fetchAccountCampaignInsights, both of which now follow `paging.next` to exhaustion.
	monitorPageSize = 500
	// monitorMaxPages bounds the walk at monitorPageSize*monitorMaxPages rows. Exceeding
	// it is an ERROR, never a silently truncated result, matching ListAdAccounts'
	// adAccountMaxPages precedent in accounts.go.
	monitorMaxPages = 50
)

// AccountCampaignRow is one campaign read live from an ad account for the account-monitor
// endpoint, ported from lfx-self-serve's meta-ads.service.ts (getMetaAnalytics +
// buildCampaignMetrics). Numeric fields carry Meta's own raw counters; the
// internal/service/rules layer computes pacing/action items from them.
type AccountCampaignRow struct {
	CampaignID     string
	Name           string
	Status         string
	Impressions    int64
	Clicks         int64
	Ctr            float64
	SpendUSD       float64
	DailyBudget    float64
	LifetimeBudget float64
	StartDate      string
	EndDate        string
	// FetchFailed marks a row whose insights lookup could not be matched/parsed — this
	// dispatcher-level method never fabricates a zero-metrics row for a fetch failure.
	FetchFailed bool
}

// ListAccountCampaigns ports getMetaAnalytics: an account-level insights read
// (level=campaign, fully paginated — see fetchAccountCampaignInsights) merged with a
// campaign-list read for status/budget/schedule fields insights does not carry. Unlike the
// BFF, which logs a warning and silently drops rows past its first page, both reads here
// walk every page (LFXV2-2519 Part 3): pagination is a pre-cutover fix, not one of the five
// ported-verbatim bugs.
//
// days selects an explicit `time_range` for the insights read rather than the BFF's hardcoded
// date_preset=last_30d — getMetaAnalytics ignored its own caller-supplied window entirely,
// which is a data-correctness bug (a days=7 request silently answered with 30 days of spend),
// not one of the five threshold/labeling quirks ported verbatim. See fetchAccountCampaignInsights.
//
// accountID must already be in Meta's "act_<digits>" form (the same form AccountConfig.
// AccountID and normalizeMetaAccountID produce). On the account-monitor path this is a
// caller-supplied id (MonitorMetaAdsAccountPayload.AccountID), not the resolved connection's
// own account — validated at the design layer for an HTTP caller (Pattern/MaxLength; see
// design/connection.go and docs/knowledge/architecture/account-monitor-endpoints.md for why no
// MinLength is declared alongside them) before it reaches here, and again below as
// defense-in-depth for a non-HTTP caller.
func (c *Client) ListAccountCampaigns(ctx context.Context, accountID string, days int) ([]AccountCampaignRow, error) {
	id := strings.TrimSpace(accountID)
	if err := ValidateAccountID(id); err != nil {
		return nil, fmt.Errorf("list account campaigns: %w", err)
	}

	campaigns, err := c.fetchAccountCampaignList(ctx, id)
	if err != nil {
		return nil, err
	}
	insightsByID, err := c.fetchAccountCampaignInsights(ctx, id, days)
	if err != nil {
		return nil, err
	}

	rows := make([]AccountCampaignRow, 0, len(campaigns))
	for _, cm := range campaigns {
		row := AccountCampaignRow{
			CampaignID:     cm.ID,
			Name:           cm.Name,
			Status:         cm.Status,
			DailyBudget:    cm.DailyBudget,
			LifetimeBudget: cm.LifetimeBudget,
			StartDate:      cm.StartDate,
			EndDate:        cm.EndDate,
		}
		if ins, ok := insightsByID[cm.ID]; ok {
			row.Impressions = ins.Impressions
			row.Clicks = ins.Clicks
			row.SpendUSD = ins.SpendUSD
			if row.Impressions > 0 {
				row.Ctr = float64(row.Clicks) / float64(row.Impressions) * 100
			}
		}
		// Campaign-list filter ported from getMetaAnalytics: `c.impressions>0 ||
		// c.status==='ACTIVE'` — everything else (a PAUSED campaign that never delivered)
		// is dropped from the monitor view entirely, exactly as upstream.
		if row.Impressions > 0 || row.Status == StatusActive {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

type metaCampaignListEntry struct {
	ID             string
	Name           string
	Status         string
	DailyBudget    float64
	LifetimeBudget float64
	StartDate      string
	EndDate        string
}

// fetchAccountCampaignList reads GET /act_.../campaigns for status/budget/schedule fields
// that Insights does not report. Budgets arrive from Meta as minor-unit strings (cents);
// converted to whole currency units here so the rest of this port works in the same units
// meta-ads.service.ts does (dollars).
//
// It follows `paging.cursors.after` to exhaustion (LFXV2-2519 Part 3, Misha's pre-cutover
// condition) rather than reading one page as the ported BFF logic does: past 100 campaigns
// that single-page read silently dropped rows, which is a data-completeness defect, not a
// threshold quirk safe to carry over verbatim like the five other known bugs. The cursor
// walk mirrors ListAdAccounts in accounts.go: the path is rebuilt from the opaque cursor
// rather than following Meta's absolute `paging.next` URL, since that URL carries the
// access_token as a query parameter.
func (c *Client) fetchAccountCampaignList(ctx context.Context, accountID string) ([]metaCampaignListEntry, error) {
	out := make([]metaCampaignListEntry, 0, monitorPageSize)
	after := ""
	seen := make(map[string]struct{})
	for page := 0; page < monitorMaxPages; page++ {
		path := "/" + accountID + "/campaigns?fields=id,name,status,daily_budget,lifetime_budget,start_time,stop_time&limit=" + strconv.Itoa(monitorPageSize)
		if after != "" {
			path += "&after=" + url.QueryEscape(after)
		}
		var resp struct {
			Data *[]struct {
				ID             string `json:"id"`
				Name           string `json:"name"`
				Status         string `json:"status"`
				DailyBudget    string `json:"daily_budget"`
				LifetimeBudget string `json:"lifetime_budget"`
				StartTime      string `json:"start_time"`
				StopTime       string `json:"stop_time"`
			} `json:"data"`
			Paging struct {
				Cursors struct {
					After string `json:"after"`
				} `json:"cursors"`
				Next string `json:"next"`
			} `json:"paging"`
		}
		if err := c.doRequest(ctx, http.MethodGet, path, nil, &resp); err != nil {
			return nil, fmt.Errorf("list account campaigns: %w", err)
		}
		if resp.Data == nil {
			return nil, &transportError{
				Method: http.MethodGet,
				Path:   path,
				Err:    fmt.Errorf("campaign list returned a 2xx response with no data field"),
			}
		}
		for _, d := range *resp.Data {
			out = append(out, metaCampaignListEntry{
				ID:             d.ID,
				Name:           d.Name,
				Status:         d.Status,
				DailyBudget:    minorUnitsToWhole(d.DailyBudget),
				LifetimeBudget: minorUnitsToWhole(d.LifetimeBudget),
				StartDate:      dateOnly(d.StartTime),
				EndDate:        dateOnly(d.StopTime),
			})
		}
		if resp.Paging.Next == "" {
			return out, nil // fully enumerated
		}
		after = strings.TrimSpace(resp.Paging.Cursors.After)
		if after == "" {
			return nil, fmt.Errorf("list account campaigns has more pages but no cursor; cannot guarantee every campaign was enumerated")
		}
		if _, dup := seen[after]; dup {
			return nil, fmt.Errorf("list account campaigns did not terminate (repeated paging cursor)")
		}
		seen[after] = struct{}{}
	}
	return nil, fmt.Errorf("list account campaigns exceeded %d pages; too many campaigns to enumerate", monitorMaxPages)
}

type metaInsightsRow struct {
	Impressions int64
	Clicks      int64
	SpendUSD    float64
}

// fetchAccountCampaignInsights ports getMetaAnalytics' account-level insights read
// (level=campaign), following `paging.cursors.after` to exhaustion rather than reading one
// page — see fetchAccountCampaignList's doc comment for why this departs from the ported
// BFF behavior.
//
// days is rendered as an explicit `time_range={"since":...,"until":...}` rather than a
// date_preset — Meta's Graph API Insights accepts time_range as a direct alternative,
// letting the caller-supplied window reach the query at all (the BFF's date_preset=last_30d
// ignored it). The dates are rendered from UTC and inclusive of `days` calendar days ending
// today, matching the same days-1 convention Google/Reddit's monitor dispatchers already use;
// Meta itself interprets since/until in the ad account's configured timezone, so a boundary
// day's spend can differ slightly from a strict UTC reading.
func (c *Client) fetchAccountCampaignInsights(ctx context.Context, accountID string, days int) (map[string]metaInsightsRow, error) {
	end := c.timeNow().UTC()
	start := end.AddDate(0, 0, -(days - 1))
	timeRange := `{"since":"` + start.Format("2006-01-02") + `","until":"` + end.Format("2006-01-02") + `"}`

	out := make(map[string]metaInsightsRow, monitorPageSize)
	after := ""
	seen := make(map[string]struct{})
	for page := 0; page < monitorMaxPages; page++ {
		path := "/" + accountID + "/insights?level=campaign&fields=campaign_id,impressions,clicks,spend&time_range=" + url.QueryEscape(timeRange) + "&limit=" + strconv.Itoa(monitorPageSize)
		if after != "" {
			path += "&after=" + url.QueryEscape(after)
		}
		var resp struct {
			Data *[]struct {
				CampaignID  string `json:"campaign_id"`
				Impressions string `json:"impressions"`
				Clicks      string `json:"clicks"`
				Spend       string `json:"spend"`
			} `json:"data"`
			Paging struct {
				Cursors struct {
					After string `json:"after"`
				} `json:"cursors"`
				Next string `json:"next"`
			} `json:"paging"`
		}
		if err := c.doRequest(ctx, http.MethodGet, path, nil, &resp); err != nil {
			return nil, fmt.Errorf("list account campaign insights: %w", err)
		}
		if resp.Data == nil {
			return nil, &transportError{
				Method: http.MethodGet,
				Path:   path,
				Err:    fmt.Errorf("account insights returned a 2xx response with no data field"),
			}
		}
		for _, row := range *resp.Data {
			impressions, errI := parseMetricInt(row.Impressions)
			clicks, errC := parseMetricInt(row.Clicks)
			if errI != nil || errC != nil {
				// Per-row parse failure: skip this row rather than fail the whole read, but
				// never fabricate a zero — the caller sees no insights entry for this
				// campaign id and its row is FetchFailed-marked one layer up.
				continue
			}
			spend, _ := strconv.ParseFloat(row.Spend, 64)
			out[row.CampaignID] = metaInsightsRow{Impressions: impressions, Clicks: clicks, SpendUSD: spend}
		}
		if resp.Paging.Next == "" {
			return out, nil // fully enumerated
		}
		after = strings.TrimSpace(resp.Paging.Cursors.After)
		if after == "" {
			return nil, fmt.Errorf("list account campaign insights has more pages but no cursor; cannot guarantee every campaign was enumerated")
		}
		if _, dup := seen[after]; dup {
			return nil, fmt.Errorf("list account campaign insights did not terminate (repeated paging cursor)")
		}
		seen[after] = struct{}{}
	}
	return nil, fmt.Errorf("list account campaign insights exceeded %d pages; too many rows to enumerate", monitorMaxPages)
}

// minorUnitsToWhole converts a Meta budget string (minor units, e.g. cents) to whole
// currency units. Empty/unparseable input yields 0 — a campaign with no budget field set
// (funded the other way) is 0 in this port's model, matching model.AccountCampaignMetrics'
// BudgetDay/TotalBudget contract.
func minorUnitsToWhole(s string) float64 {
	if s == "" {
		return 0
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return n / 100
}

// dateOnly truncates a Meta ISO-8601 timestamp ("2026-01-15T00:00:00-0800") to its
// YYYY-MM-DD date component, matching model.AccountCampaignMetrics.StartDate/EndDate's
// date-only contract. Empty/malformed input yields "".
func dateOnly(s string) string {
	if len(s) < 10 {
		return ""
	}
	return s[:10]
}
