// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package design

import (
	"math"

	"goa.design/goa/v3/expr"

	//nolint:staticcheck // ST1001: the recommended way of using the goa DSL package is with the . import
	. "goa.design/goa/v3/dsl"
)

// Per-operation 200 examples for the six monitor-*-ads-account methods (LFXV2-2665).
//
// AccountMonitor is ONE result type shared by six platforms whose rows differ: Google alone sets
// campaign_url, only Google and Microsoft can be search campaigns, Google and Microsoft report no
// flight dates or lifetime budget, Meta/Reddit/X measure no conversions, each platform has its
// own status literals and pacing formula, and only the report-backed platforms (Microsoft, X)
// set metrics_as_of / metrics_window_* / metrics_pending. No single example is a response all six
// handlers could return, so AccountMonitor and AccountMonitorCampaign carry none (Meta
// "openapi:example" false) and each method declares its own 200 example.
//
// How, given what Goa allows. A Result(AccountMonitor, func() { Example(...) }) duplicates the
// type under the same name, so the schemas collide; and a second Body(AccountMonitor, ...) trips
// Goa's "transport plan mutated prepared design" guard. So each method's 200 body is a type of its
// own that Extends AccountMonitor — same fields, so the generated encoder copies the shared
// result into it and the service is untouched — with an explicit "openapi:typename" on both the
// type and the endpoint body: OpenAPI 3 deduplicates structurally identical schemas by shape, and
// without distinct names all six operations would reference the first one (Google's).
//
// Each example's pacing and action items are exactly what that platform's evaluator
// (internal/service/rules/monitor_*.go) produces for its rows, with days 30 and — for the
// live-read platforms, whose evaluators take a clock — read at accountMonitorExampleAsOf. The
// report-backed examples carry that instant as metrics_as_of and the 30 days ending on it as
// their window. TestPublishedAccountMonitorExamplesMatchTheirOperation recomputes all of it.
const (
	accountMonitorExampleAsOf       = "2026-10-05T14:30:00Z"
	accountMonitorExampleWindowFrom = "2026-09-06"
	accountMonitorExampleWindowTo   = "2026-10-05"
	accountMonitorExampleDays       = 30
)

// accountMonitorExample assembles one operation's example: it fills each row's ctr from its
// clicks and impressions (as every platform does, to 2 decimals), and the totals as the sums of
// the rows — conversions only when some row measured them — so the totals cannot drift from the
// rows they describe.
func accountMonitorExample(accountID string, rows, items []map[string]any, reported bool) map[string]any {
	var spend, conversions float64
	var impressions, clicks int
	measured := false
	campaigns := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		row := map[string]any{"fetch_failed": false, "pacing_unknown": false, "is_search_channel": false,
			"total_budget": 0.0, "start_date": "", "end_date": ""}
		for k, v := range r {
			row[k] = v
		}
		imp, clk := row["impressions"].(int), row["clicks"].(int)
		row["ctr"] = math.Round(float64(clk)/float64(imp)*100*100) / 100
		spend += row["spend"].(float64)
		impressions += imp
		clicks += clk
		if c, ok := row["conversions"].(float64); ok {
			conversions += c
			measured = true
		}
		campaigns = append(campaigns, row)
	}
	totals := map[string]any{"spend": spend, "impressions": impressions, "clicks": clicks,
		"campaign_count": len(rows), "unmeasured_campaigns": 0}
	if measured {
		totals["conversions"] = conversions
	}
	ex := map[string]any{"account_id": accountID, "days": accountMonitorExampleDays,
		"campaigns": campaigns, "action_items": items, "totals": totals}
	if reported {
		ex["metrics_as_of"] = accountMonitorExampleAsOf
		ex["metrics_window_start"] = accountMonitorExampleWindowFrom
		ex["metrics_window_end"] = accountMonitorExampleWindowTo
		ex["metrics_pending"] = false
	}
	return ex
}

// accountMonitorExampleBody is the 200 response DSL for one monitor method: that method's own
// body type, with its own example.
func accountMonitorExampleBody(body expr.UserType, example map[string]any) func() {
	return func() {
		Body(body, func() {
			Meta("openapi:typename", body.Name()+"-response-body")
			Example(example)
		})
	}
}

// accountMonitorBodyType is one platform's 200 body: AccountMonitor's fields under a name of its
// own (see the file comment for why it must be a separate type).
func accountMonitorBodyType(name, platform string) expr.UserType {
	return Type(name, func() {
		Description("The " + platform + " account monitor read. The same fields as every ad-platform account monitor; a schema of its own only so this operation's example can show rows in " + platform + "'s own shape.")
		Extend(AccountMonitor)
		Required("account_id", "days", "campaigns", "action_items", "totals")
		Meta("openapi:example", "false")
		Meta("openapi:typename", name)
	})
}

var (
	googleAccountMonitorBody    = accountMonitorBodyType("google-ads-account-monitor", "Google Ads")
	linkedinAccountMonitorBody  = accountMonitorBodyType("linkedin-ads-account-monitor", "LinkedIn Ads")
	metaAccountMonitorBody      = accountMonitorBodyType("meta-ads-account-monitor", "Meta Ads")
	microsoftAccountMonitorBody = accountMonitorBodyType("microsoft-ads-account-monitor", "Microsoft Ads")
	redditAccountMonitorBody    = accountMonitorBodyType("reddit-ads-account-monitor", "Reddit Ads")
	twitterAccountMonitorBody   = accountMonitorBodyType("twitter-ads-account-monitor", "X Ads")
)

// Google Ads: daily budgets paced as budget_day*days, no flight dates, campaign_url set.
// 1890/(150*30) = 42% (underspending); 4200/(200*30) = 70% (normal).
func googleAccountMonitorExample() map[string]any {
	return accountMonitorExample("8666746580", []map[string]any{
		{
			"platform_campaign_id": "24183781329", "name": "KubeCon NA 2026 - Search", "status": "ENABLED",
			"spend": 1890.0, "impressions": 48200, "clicks": 3110, "conversions": 12.5,
			"budget_day": 150.0, "is_search_channel": true, "pacing_pct": 42.0, "pacing_label": "underspending",
			"campaign_url": "https://ads.google.com/aw/campaigns?campaignId=24183781329",
		},
		{
			"platform_campaign_id": "24183781330", "name": "KubeCon NA 2026 - Brand", "status": "ENABLED",
			"spend": 4200.0, "impressions": 136000, "clicks": 8310, "conversions": 200.0,
			"budget_day": 200.0, "is_search_channel": true, "pacing_pct": 70.0, "pacing_label": "normal",
			"campaign_url": "https://ads.google.com/aw/campaigns?campaignId=24183781330",
		},
	}, []map[string]any{{
		"campaign_id": "24183781329", "campaign_name": "KubeCon NA 2026 - Search", "priority": "MED",
		"issue":  "Only spending 42% of $150.00/day budget — $1890.00 spent vs $4500.00 expected",
		"action": "Broaden targeting (locations, audiences), add broad match keywords, or increase bids to win more auctions",
	}}, false)
}

// LinkedIn: pacing is UNROUNDED, and a daily budget is expected over the days from 29 days
// before the read instant to that instant (29), so 1305/(100*29) = 45% (underspending). A
// lifetime budget is prorated over its flight: 12100 over Aug 1–Nov 30 (121 days) is 100/day,
// expected over the same 29 days, so 2320/2900 = 80% (normal).
func linkedinAccountMonitorExample() map[string]any {
	return accountMonitorExample("512345678", []map[string]any{
		{
			"platform_campaign_id": "612345001", "name": "KubeCon NA 2026 - Sponsored Content", "status": "ACTIVE",
			"spend": 1305.0, "impressions": 52000, "clicks": 390, "conversions": 9.0,
			"budget_day": 100.0, "start_date": "2026-08-01", "end_date": "2026-11-30",
			"pacing_pct": 45.0, "pacing_label": "underspending",
		},
		{
			"platform_campaign_id": "612345002", "name": "KubeCon NA 2026 - Lead Gen", "status": "ACTIVE",
			"spend": 2320.0, "impressions": 61000, "clicks": 520, "conversions": 31.0,
			"budget_day": 0.0, "total_budget": 12100.0, "start_date": "2026-08-01", "end_date": "2026-11-30",
			"pacing_pct": 80.0, "pacing_label": "normal",
		},
	}, []map[string]any{{
		"campaign_id": "612345001", "campaign_name": "KubeCon NA 2026 - Sponsored Content", "priority": "HIGH",
		"issue":  "Underspending — pacing below 50%",
		"action": "Check targeting breadth, bid strategy, or budget floor",
	}}, false)
}

// Meta: a daily budget is paced as budget_day*days: 960/(80*30) = 40% (underspending). A
// lifetime budget is prorated over the elapsed flight: 5900 over Sep 1–Oct 30 (59 days) is
// 100/day, 35 days elapsed by Oct 5, so 2800/3500 = 80% (normal). No conversions measured.
func metaAccountMonitorExample() map[string]any {
	return accountMonitorExample("act_8666746580", []map[string]any{
		{
			"platform_campaign_id": "120210000000000001", "name": "KubeCon NA 2026 - Prospecting", "status": "ACTIVE",
			"spend": 960.0, "impressions": 88000, "clicks": 1320,
			"budget_day": 80.0, "start_date": "2026-08-01", "end_date": "2026-11-30",
			"pacing_pct": 40.0, "pacing_label": "underspending",
		},
		{
			"platform_campaign_id": "120210000000000002", "name": "KubeCon NA 2026 - Retargeting", "status": "ACTIVE",
			"spend": 2800.0, "impressions": 150000, "clicks": 2400,
			"budget_day": 0.0, "total_budget": 5900.0, "start_date": "2026-09-01", "end_date": "2026-10-30",
			"pacing_pct": 80.0, "pacing_label": "normal",
		},
	}, []map[string]any{{
		"campaign_id": "120210000000000001", "campaign_name": "KubeCon NA 2026 - Prospecting", "priority": "MED",
		"issue":  "Underspending: 40% of budget used ($960.00 of $2400.00)",
		"action": "Broaden audience targeting or increase bid cap to improve delivery",
	}}, false)
}

// Reddit: a daily budget is expected over the 30 report days its flight covers: 600/(50*30) =
// 40% (underspending). A lifetime budget is prorated over the elapsed flight: 5900 over
// Sep 1–Oct 30 is 100/day, 35 days elapsed, so 2800/3500 = 80% (normal). No conversions measured.
func redditAccountMonitorExample() map[string]any {
	return accountMonitorExample("t2_gv9wtbfa", []map[string]any{
		{
			"platform_campaign_id": "2045000000000000001", "name": "KubeCon NA 2026 - r/kubernetes", "status": "ACTIVE",
			"spend": 600.0, "impressions": 64000, "clicks": 448,
			"budget_day": 50.0, "start_date": "2026-08-01", "end_date": "2026-11-30",
			"pacing_pct": 40.0, "pacing_label": "underspending",
		},
		{
			"platform_campaign_id": "2045000000000000002", "name": "KubeCon NA 2026 - r/devops", "status": "ACTIVE",
			"spend": 2800.0, "impressions": 210000, "clicks": 1680,
			"budget_day": 0.0, "total_budget": 5900.0, "start_date": "2026-09-01", "end_date": "2026-10-30",
			"pacing_pct": 80.0, "pacing_label": "normal",
		},
	}, []map[string]any{{
		"campaign_id": "2045000000000000001", "campaign_name": "KubeCon NA 2026 - r/kubernetes", "priority": "HIGH",
		"issue":  "Underspending at 40% of budget — $600.00 spent",
		"action": "Broaden targeting (add subreddits/interests), increase bid, or expand geographic targeting",
	}}, false)
}

// Microsoft Ads: report-backed; daily budgets only, no flight dates, paced as budget_day*days:
// 720/(60*30) = 40% (underspending), 1000/(40*30) = 83% (normal).
func microsoftAccountMonitorExample() map[string]any {
	return accountMonitorExample("187654321", []map[string]any{
		{
			"platform_campaign_id": "516000001", "name": "KubeCon NA 2026 - Search", "status": "Active",
			"spend": 720.0, "impressions": 21000, "clicks": 840, "conversions": 6.0,
			"budget_day": 60.0, "is_search_channel": true, "pacing_pct": 40.0, "pacing_label": "underspending",
		},
		{
			"platform_campaign_id": "516000002", "name": "KubeCon NA 2026 - Brand", "status": "Active",
			"spend": 1000.0, "impressions": 26000, "clicks": 1300, "conversions": 22.0,
			"budget_day": 40.0, "is_search_channel": true, "pacing_pct": 83.0, "pacing_label": "normal",
		},
	}, []map[string]any{{
		"campaign_id": "516000001", "campaign_name": "KubeCon NA 2026 - Search", "priority": "MED",
		"issue":  "Only spending 40% of $60.00/day budget — $720.00 spent vs $1800.00 expected",
		"action": "Broaden targeting (locations, audiences), add broad match keywords, or increase bids to win more auctions",
	}}, true)
}

// X: report-backed; evaluated on the report's own window (Sep 6–Oct 5, 30 days). A daily budget
// is expected over the window days its flight covers: 600/(50*30) = 40% (underspending). A total
// budget is prorated over its flight: 5900 over Sep 1–Oct 30 (60 days), 30 of them in the
// window, so 2360/2950 = 80% (normal). No conversions measured.
func twitterAccountMonitorExample() map[string]any {
	return accountMonitorExample("18ce54d4x5t", []map[string]any{
		{
			"platform_campaign_id": "hd8ns", "name": "KubeCon NA 2026 - Promoted Posts", "status": "ACTIVE",
			"spend": 600.0, "impressions": 70000, "clicks": 490,
			"budget_day": 50.0, "start_date": "2026-08-01", "end_date": "2026-11-30",
			"pacing_pct": 40.0, "pacing_label": "underspending",
		},
		{
			"platform_campaign_id": "hd8nt", "name": "KubeCon NA 2026 - Followers", "status": "ACTIVE",
			"spend": 2360.0, "impressions": 180000, "clicks": 1260,
			"budget_day": 0.0, "total_budget": 5900.0, "start_date": "2026-09-01", "end_date": "2026-10-30",
			"pacing_pct": 80.0, "pacing_label": "normal",
		},
	}, []map[string]any{{
		"campaign_id": "hd8ns", "campaign_name": "KubeCon NA 2026 - Promoted Posts", "priority": "MED",
		"issue":  "Underspending at 40% of budget — 600.00 in account currency spent in the window",
		"action": "Broaden targeting (keywords, follower look-alikes, locations) or raise the bid so the line items win more auctions",
	}}, true)
}
