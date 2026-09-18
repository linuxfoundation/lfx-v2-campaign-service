// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// AccountCampaignRow is one campaign read live from an ad account for the account-monitor
// endpoint, ported from lfx-self-serve's campaign-metrics.service.ts (getMonitorData /
// parseCampaignMetrics). Numeric fields carry Google Ads' own raw counters; the
// internal/service/rules layer computes pacing/action items from them.
type AccountCampaignRow struct {
	CampaignID      string
	Name            string
	Status          string
	IsSearchChannel bool
	Impressions     int64
	Clicks          int64
	Ctr             float64
	SpendUSD        float64
	Conversions     *float64
	BudgetDailyUSD  float64
	// FetchFailed marks a row some part of whose upstream data could not be trusted: either its
	// GAQL metrics fields could not be parsed (round-19 review), or its campaign_budget.
	// amount_micros was present but unparseable alongside otherwise-good metrics (round-24/25
	// review) — see microsToUSD and the parse sites in ListAccountCampaigns below. In the budget
	// case, Impressions/Clicks/SpendUSD may be genuinely non-zero while BudgetDailyUSD is the
	// untrusted field.
	FetchFailed bool
}

// monitorGaqlRow mirrors gaqlMetricsRow's string-encoded-integer convention (see that
// type's doc comment) plus the campaign/budget fields the account-monitor query adds.
type monitorGaqlRow struct {
	Campaign struct {
		ID                     string `json:"id"`
		Name                   string `json:"name"`
		Status                 string `json:"status"`
		AdvertisingChannelType string `json:"advertisingChannelType"`
	} `json:"campaign"`
	CampaignBudget struct {
		AmountMicros string `json:"amountMicros"`
	} `json:"campaignBudget"`
	Metrics struct {
		Impressions string   `json:"impressions"`
		Clicks      string   `json:"clicks"`
		CostMicros  string   `json:"costMicros"`
		Conversions *float64 `json:"conversions"`
	} `json:"metrics"`
}

// ListAccountCampaigns ports getMonitorData: every campaign visible on customerID over an
// inclusive `days`-day window ending today (aggregated per campaign; segments.date is filtered
// by BETWEEN but never selected, so — same UNVERIFIED ASSUMPTION GetCampaignMetrics documents —
// Google Ads returns one row per campaign rather than one per day; enforced below exactly as
// GetCampaignMetrics enforces it for the single-campaign path, but per campaign.id rather than
// for the whole result set, since this query legitimately returns many campaigns).
//
// The window is rendered from the client's injected clock (c.now, not the wall clock) so it
// stays deterministic and testable — mirroring the equivalent Meta/LinkedIn fix (see
// TestListAccountCampaigns_UsesInjectedClockNotWallClock in this package and its siblings in
// internal/platform/meta and internal/platform/linkedin).
//
// customerID must already be digits-only (validated by gaqlSearchForCustomer); this method
// adds no additional validation of its own.
//
// days carries no such check here either: this client trusts days is already within the
// service layer's 7..90 bound (internal/service/connection_monitor.go's validateMonitorDays,
// itself mirroring the design layer's Minimum/Maximum) before it reaches the BETWEEN window
// below. That is a deliberate asymmetry, not an oversight, matching the Meta/LinkedIn
// siblings' equivalent comment — today's only caller is that service layer; a future
// non-HTTP caller of this package directly would need to add its own bound rather than rely
// on one here. An unchecked days <= 0 would silently invert the BETWEEN window rather than
// error.
func (c *Client) ListAccountCampaigns(ctx context.Context, customerID string, days int) ([]AccountCampaignRow, error) {
	end := c.now().UTC()
	start := end.AddDate(0, 0, -(days - 1))
	startDate := start.Format("2006-01-02")
	endDate := end.Format("2006-01-02")
	// The WHERE clause mirrors getMonitorData's GAQL query verbatim (campaign-metrics.service.ts)
	// beyond the date window: channel type, status, and impressions>0 are load-bearing filters
	// on the OLD path, not incidental — dropping any of them widens this read to every campaign
	// the account has ever run (including years of REMOVED history), which both produces a
	// non-empty differential diff against the BFF and defeats CampaignCount/campaigns-array
	// agreement downstream.
	query := fmt.Sprintf(
		"SELECT campaign.id, campaign.name, campaign.status, campaign.advertising_channel_type, "+
			"campaign_budget.amount_micros, metrics.impressions, metrics.clicks, metrics.cost_micros, "+
			"metrics.conversions FROM campaign WHERE segments.date BETWEEN '%s' AND '%s' "+
			"AND campaign.advertising_channel_type IN ('SEARCH', 'DEMAND_GEN') "+
			"AND campaign.status IN ('ENABLED', 'PAUSED') "+
			"AND metrics.impressions > 0",
		startDate, endDate,
	)
	raw, err := c.gaqlSearchForCustomer(ctx, customerID, query)
	if err != nil {
		return nil, fmt.Errorf("list account campaigns: %w", err)
	}

	byID := make(map[string]*AccountCampaignRow)
	order := make([]string, 0, len(raw))
	for _, r := range raw {
		var row monitorGaqlRow
		if err := json.Unmarshal(r, &row); err != nil {
			return nil, &transportError{
				Method: http.MethodPost,
				Path:   c.customerPath("googleAds:search"),
				Err:    fmt.Errorf("decode account monitor row: %w", err),
			}
		}
		id := row.Campaign.ID
		if id == "" {
			continue
		}
		existing, ok := byID[id]
		budgetUSD, budgetOK := microsToUSD(row.CampaignBudget.AmountMicros)
		if !ok {
			existing = &AccountCampaignRow{
				CampaignID:      id,
				Name:            row.Campaign.Name,
				Status:          row.Campaign.Status,
				IsSearchChannel: row.Campaign.AdvertisingChannelType == "SEARCH",
				BudgetDailyUSD:  budgetUSD,
			}
			byID[id] = existing
			order = append(order, id)
		}
		if !budgetOK {
			// A present but unparseable amount_micros is malformed upstream data, not a
			// legitimate "no budget set" (that's the empty-string / -1-sentinel case, which
			// budgetOK still reports true for). Mark FetchFailed so the rule engine treats
			// this row's budget as unknown rather than a real $0 (round-24 review). Checked on
			// EVERY row, not only the first sighting: segments.date makes this query multi-row
			// per campaign (see the doc comment above), so a malformed amount_micros on an
			// intermediate row must still mark the campaign FetchFailed even though the first
			// row's budget already parsed and was assigned to BudgetDailyUSD.
			existing.FetchFailed = true
		}
		impressions, errI := parseMetricInt(row.Metrics.Impressions)
		clicks, errC := parseMetricInt(row.Metrics.Clicks)
		cost, errCost := parseMetricInt(row.Metrics.CostMicros)
		if errI != nil || errC != nil || errCost != nil {
			// Mark the row FetchFailed rather than fail the whole account read; the campaign
			// still surfaces with its identity fields. Never fabricate a zero in their place
			// — leave the accumulator's numeric fields untouched for this row (round-19
			// review: this used to leave FetchFailed unset too, so the rule engine read the
			// zero-valued accumulator as a real "no delivery" measurement instead of unknown).
			existing.FetchFailed = true
			continue
		}
		existing.Impressions += impressions
		existing.Clicks += clicks
		existing.SpendUSD += float64(cost) / 1_000_000
		if row.Metrics.Conversions != nil {
			c := *row.Metrics.Conversions
			if existing.Conversions == nil {
				existing.Conversions = &c
			} else {
				*existing.Conversions += c
			}
		}
	}

	out := make([]AccountCampaignRow, 0, len(order))
	for _, id := range order {
		r := byID[id]
		if r.Impressions > 0 {
			r.Ctr = float64(r.Clicks) / float64(r.Impressions) * 100
		}
		out = append(out, *r)
	}
	return out, nil
}

// microsToUSD converts a Google Ads amount_micros string to whole currency units. An empty
// string or Google's exact sentinel -1 (both meaning "no budget amount set") yield (0, true) —
// a legitimate zero budget, not a failure. A present but unparseable value, OR any other
// negative value (Google defines only -1, not "negative" in general, as the sentinel), yields
// (0, false): the caller must treat that as unknown, never as a real $0 (round-24 review — see
// the FetchFailed comment at the call site in ListAccountCampaigns).
func microsToUSD(s string) (usd float64, ok bool) {
	if s == "" {
		return 0, true
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, false
	}
	if n == -1 {
		return 0, true
	}
	if n < 0 {
		return 0, false
	}
	return float64(n) / 1_000_000, true
}
