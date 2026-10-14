// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service/rules"
)

// accountMonitorExampleNow is the instant the live-read platforms' published examples are read
// at (design/connection.go, accountMonitorExampleAsOf). Their evaluators take a "now"; the
// report-backed platforms' examples carry their own window instead.
var accountMonitorExampleNow = time.Date(2026, 10, 5, 14, 30, 0, 0, time.UTC)

// accountMonitorExamplePlatform is what one monitor operation's example must look like: the
// evaluator its handler runs, and the per-platform facts of its rows.
type accountMonitorExamplePlatform struct {
	evaluate func(rows []model.AccountCampaignMetrics, days int, ex map[string]any) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem, error)
	// activeStatus is the platform's own literal for a serving campaign; every example row uses it.
	activeStatus string
	// campaignURL: only Google Ads rows carry campaign_url.
	campaignURL bool
	// searchChannel: whether is_search_channel can be true (Google and Microsoft only).
	searchChannel bool
	// flightDates / totalBudget: whether the platform reports flight dates and a lifetime budget.
	flightDates, totalBudget bool
	// conversions: whether rows carry a conversions measurement (absent on Meta, Reddit and X).
	conversions bool
	// reported: Microsoft and X are report-backed, so their responses carry metrics_as_of,
	// metrics_window_start/end and metrics_pending; every other platform omits all four.
	reported bool
}

// accountMonitorExamplePlatforms maps every ad-monitor operation to its handler's evaluator — the
// same call the ConnectionService method makes (connection_monitor.go). An operation missing
// here fails the test rather than going unchecked.
var accountMonitorExamplePlatforms = map[string]accountMonitorExamplePlatform{
	"monitor-google-ads-account": {
		evaluate: func(rows []model.AccountCampaignMetrics, days int, _ map[string]any) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem, error) {
			r, i := rules.EvaluateGoogleMonitor(rows, days)
			return r, i, nil
		},
		activeStatus: "ENABLED", campaignURL: true, searchChannel: true, conversions: true,
	},
	"monitor-linkedin-ads-account": {
		evaluate: func(rows []model.AccountCampaignMetrics, days int, _ map[string]any) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem, error) {
			r, i := rules.EvaluateLinkedInMonitor(rows, days, accountMonitorExampleNow)
			return r, i, nil
		},
		activeStatus: "ACTIVE", flightDates: true, totalBudget: true, conversions: true,
	},
	"monitor-meta-ads-account": {
		evaluate: func(rows []model.AccountCampaignMetrics, days int, _ map[string]any) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem, error) {
			r, i := rules.EvaluateMetaMonitor(rows, days, accountMonitorExampleNow)
			return r, i, nil
		},
		activeStatus: "ACTIVE", flightDates: true, totalBudget: true,
	},
	"monitor-reddit-ads-account": {
		evaluate: func(rows []model.AccountCampaignMetrics, days int, _ map[string]any) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem, error) {
			r, i := rules.EvaluateRedditMonitor(rows, days, accountMonitorExampleNow)
			return r, i, nil
		},
		activeStatus: "ACTIVE", flightDates: true, totalBudget: true,
	},
	"monitor-microsoft-ads-account": {
		evaluate: func(rows []model.AccountCampaignMetrics, days int, _ map[string]any) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem, error) {
			r, i := rules.EvaluateMicrosoftMonitor(rows, days)
			return r, i, nil
		},
		activeStatus: "Active", searchChannel: true, conversions: true, reported: true,
	},
	"monitor-twitter-ads-account": {
		// MonitorTwitterAdsAccount evaluates on the saved report's own window, not on days.
		evaluate: func(rows []model.AccountCampaignMetrics, _ int, ex map[string]any) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem, error) {
			ws, err1 := time.Parse(time.DateOnly, fmt.Sprint(ex["metrics_window_start"]))
			we, err2 := time.Parse(time.DateOnly, fmt.Sprint(ex["metrics_window_end"]))
			if err1 != nil || err2 != nil {
				return nil, nil, fmt.Errorf("unparseable metrics window %v..%v", ex["metrics_window_start"], ex["metrics_window_end"])
			}
			r, i := rules.EvaluateTwitterMonitor(rows, &ws, &we)
			return r, i, nil
		},
		activeStatus: "ACTIVE", flightDates: true, totalBudget: true, reported: true,
	},
}

// accountMonitorOpenAPI3Specs are the OpenAPI 3 documents, which carry a 200 example per
// operation. OpenAPI 2 (Swagger) has no per-operation response example, so there the shared
// AccountMonitor definition must carry NO account-level example at all — any one would describe
// five of the six platforms wrongly.
var accountMonitorOpenAPI3Specs = []string{
	filepath.Join("..", "..", "gen", "http", "openapi3.json"),
	filepath.Join("..", "..", "cmd", "campaign-service", "kodata", "gen", "http", "openapi3.json"),
}

// Every published ad-monitor 200 example is a response ITS OWN operation's handler could return:
// rows in that platform's shape (status literal, campaign_url, is_search_channel, flight dates,
// lifetime budget, conversions), pacing and action items exactly what that platform's evaluator
// produces for those rows, totals that sum them, and the report-window fields present exactly on
// the report-backed platforms. Every account-monitor example anywhere in a spec must be one of
// these per-operation examples: a shared one cannot be right for six platforms.
func TestPublishedAccountMonitorExamplesMatchTheirOperation(t *testing.T) {
	for _, spec := range accountMonitorOpenAPI3Specs {
		t.Run(spec, func(t *testing.T) {
			doc := readAccountMonitorSpec(t, spec)
			paths, _ := doc["paths"].(map[string]any)
			checked := map[string]bool{}
			attributed := map[string]bool{}
			for p, item := range paths {
				ops, _ := item.(map[string]any)
				for method, o := range ops {
					op, _ := o.(map[string]any)
					id, _ := op["operationId"].(string)
					name := id[strings.LastIndex(id, "#")+1:]
					if !strings.HasPrefix(name, "monitor-") || !strings.HasSuffix(name, "-ads-account") {
						continue
					}
					where := fmt.Sprintf("%s %s (%s)", strings.ToUpper(method), p, name)
					platform, ok := accountMonitorExamplePlatforms[name]
					if !ok {
						t.Errorf("%s: no evaluator mapped for this monitor operation; add it to accountMonitorExamplePlatforms", where)
						continue
					}
					ex := accountMonitor200Example(op)
					if ex == nil {
						t.Errorf("%s: no 200 example", where)
						continue
					}
					attributed[fmt.Sprintf("/paths/%s/%s/responses/200/content/application/json/example", p, method)] = true
					checked[name] = true
					checkAccountMonitorExample(t, where, platform, ex)
				}
			}
			for name := range accountMonitorExamplePlatforms {
				if !checked[name] {
					t.Errorf("operation %s not found in %s; the scan is vacuous for it", name, spec)
				}
			}
			for path, ex := range hubspotMonitorSpecExamples(t, spec) {
				if attributed[path] {
					continue
				}
				hubspotMonitorVisit(ex, func(v any) {
					if n, ok := v.(map[string]any); ok && isAccountMonitorEnvelope(n) {
						t.Errorf("%s: account-monitor example not attached to a monitor operation; it cannot be right for every platform", path)
					}
				})
			}
		})
	}
	for _, spec := range hubspotMonitorSpecs {
		if strings.HasSuffix(spec, "openapi3.json") {
			continue
		}
		for path, ex := range hubspotMonitorSpecExamples(t, spec) {
			hubspotMonitorVisit(ex, func(v any) {
				if n, ok := v.(map[string]any); ok && isAccountMonitorEnvelope(n) {
					t.Errorf("%s %s: OpenAPI 2 carries no per-operation example, so no account-monitor example may appear here", spec, path)
				}
			})
		}
	}
}

func isAccountMonitorEnvelope(n map[string]any) bool {
	t, ok := n["totals"].(map[string]any)
	if !ok {
		return false
	}
	_, hasCount := t["campaign_count"]
	_, hasCampaigns := n["campaigns"]
	return hasCount && hasCampaigns
}

func readAccountMonitorSpec(t *testing.T, rel string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(rel) //nolint:gosec // fixed repo-relative path in a test
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	return doc
}

func accountMonitor200Example(op map[string]any) map[string]any {
	r, _ := op["responses"].(map[string]any)
	ok, _ := r["200"].(map[string]any)
	c, _ := ok["content"].(map[string]any)
	j, _ := c["application/json"].(map[string]any)
	ex, _ := j["example"].(map[string]any)
	return ex
}

func checkAccountMonitorExample(t *testing.T, where string, p accountMonitorExamplePlatform, ex map[string]any) {
	t.Helper()
	num := func(m map[string]any, k string) float64 { f, _ := m[k].(float64); return f }
	days := int(num(ex, "days"))
	if days <= 0 {
		t.Errorf("%s: example has no days", where)
		return
	}
	for _, k := range []string{"metrics_as_of", "metrics_window_start", "metrics_window_end", "metrics_pending"} {
		if _, has := ex[k]; has != p.reported {
			t.Errorf("%s: %s present=%v, but this platform sets it=%v", where, k, has, p.reported)
		}
	}
	rawRows, _ := ex["campaigns"].([]any)
	rows := make([]model.AccountCampaignMetrics, 0, len(rawRows))
	var spend, conv float64
	var impressions, clicks int64
	anyConv := false
	for i, r := range rawRows {
		row, _ := r.(map[string]any)
		at := fmt.Sprintf("%s campaigns[%d] (%v)", where, i, row["name"])
		if row["status"] != p.activeStatus {
			t.Errorf("%s: status %v, want this platform's serving literal %q", at, row["status"], p.activeStatus)
		}
		if _, has := row["campaign_url"]; has != p.campaignURL {
			t.Errorf("%s: campaign_url present=%v, but only Google Ads rows carry it", at, has)
		}
		if row["is_search_channel"] == true && !p.searchChannel {
			t.Errorf("%s: is_search_channel=true on a platform that never sets it", at)
		}
		if !p.flightDates && (row["start_date"] != "" || row["end_date"] != "") {
			t.Errorf("%s: flight dates %v..%v on a platform that reports none", at, row["start_date"], row["end_date"])
		}
		if !p.totalBudget && num(row, "total_budget") != 0 {
			t.Errorf("%s: total_budget %v on a platform with daily budgets only", at, row["total_budget"])
		}
		c, hasConv := row["conversions"].(float64)
		if hasConv != p.conversions {
			t.Errorf("%s: conversions present=%v, but this platform measures them=%v", at, hasConv, p.conversions)
		}
		if imp := num(row, "impressions"); imp > 0 && math.Abs(num(row, "ctr")-num(row, "clicks")/imp*100) > 0.005 {
			t.Errorf("%s: ctr %v is not clicks/impressions*100", at, row["ctr"])
		}
		m := model.AccountCampaignMetrics{
			PlatformCampaignID: fmt.Sprint(row["platform_campaign_id"]),
			Name:               fmt.Sprint(row["name"]),
			Status:             fmt.Sprint(row["status"]),
			Spend:              num(row, "spend"),
			Impressions:        int64(num(row, "impressions")),
			Clicks:             int64(num(row, "clicks")),
			Ctr:                num(row, "ctr"),
			BudgetDay:          num(row, "budget_day"),
			TotalBudget:        num(row, "total_budget"),
			StartDate:          fmt.Sprint(row["start_date"]),
			EndDate:            fmt.Sprint(row["end_date"]),
			IsSearchChannel:    row["is_search_channel"] == true,
			FetchFailed:        row["fetch_failed"] == true,
		}
		if hasConv {
			m.Conversions = &c
			conv += c
			anyConv = true
		}
		spend += m.Spend
		impressions += m.Impressions
		clicks += m.Clicks
		rows = append(rows, m)
	}

	got, wantItems, err := p.evaluate(rows, days, ex)
	if err != nil {
		t.Errorf("%s: %v", where, err)
		return
	}
	if len(got) != len(rows) {
		t.Errorf("%s: the evaluator returns %d of the %d example rows", where, len(got), len(rows))
		return
	}
	for i, w := range got {
		row, _ := rawRows[i].(map[string]any)
		if num(row, "pacing_pct") != w.PacingPct || row["pacing_label"] != string(w.PacingLabel) ||
			row["pacing_unknown"] != w.Metrics.PacingUnknown {
			t.Errorf("%s campaigns[%d] (%v): publishes pacing_pct=%v label=%v pacing_unknown=%v, but the platform's evaluator gives %v/%s/%v",
				where, i, row["name"], row["pacing_pct"], row["pacing_label"], row["pacing_unknown"],
				w.PacingPct, w.PacingLabel, w.Metrics.PacingUnknown)
		}
	}
	items, _ := ex["action_items"].([]any)
	gotItems := make([]model.AccountMonitorActionItem, 0, len(items))
	for _, it := range items {
		m, _ := it.(map[string]any)
		gotItems = append(gotItems, model.AccountMonitorActionItem{
			CampaignID: fmt.Sprint(m["campaign_id"]), CampaignName: fmt.Sprint(m["campaign_name"]),
			Priority: model.MonitorPriority(fmt.Sprint(m["priority"])),
			Issue:    fmt.Sprint(m["issue"]), Action: fmt.Sprint(m["action"]),
		})
	}
	if !reflect.DeepEqual(gotItems, wantItems) {
		t.Errorf("%s: action_items = %+v, but the platform's evaluator gives %+v", where, gotItems, wantItems)
	}

	totals, _ := ex["totals"].(map[string]any)
	if math.Abs(num(totals, "spend")-spend) > 1e-6 || int64(num(totals, "impressions")) != impressions || int64(num(totals, "clicks")) != clicks {
		t.Errorf("%s: totals %v are not the sum of the rows (spend %v, impressions %d, clicks %d)", where, totals, spend, impressions, clicks)
	}
	tc, hasTC := totals["conversions"].(float64)
	if hasTC != anyConv || (hasTC && math.Abs(tc-conv) > 1e-6) {
		t.Errorf("%s: totals.conversions = %v (present=%v), want the rows' sum %v (present=%v)", where, totals["conversions"], hasTC, conv, anyConv)
	}
}
