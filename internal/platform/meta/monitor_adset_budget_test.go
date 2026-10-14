// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Ad-set budget pacing and account-currency scaling for the account monitor (LFXV2-2665).
// CreateCampaign puts the budget on the AD SET (ABO), so the monitor used to read no budget for
// every campaign this service creates and report its pacing unknown; and it divided every budget
// by 100, reading a JPY or KRW budget 100x too small.

const (
	usdAccount = `{"currency":"USD","id":"act_123"}`
	jpyAccount = `{"currency":"JPY","id":"act_123"}`
)

// monitorCampaign is one campaign-list row; budget fields are raw JSON members ("" omits both).
func monitorCampaign(id, status, budgets string) string {
	if budgets != "" {
		budgets = "," + budgets
	}
	return fmt.Sprintf(`{"id":"%s","name":"Campaign %s","status":"%s","start_time":"2026-01-01T00:00:00-0800"%s}`, id, id, status, budgets)
}

func monitorList(rows ...string) string {
	return `{"data":[` + strings.Join(rows, ",") + `],"paging":{}}`
}

// adSetFlight is the flight CreateCampaign writes on every ad set it makes.
const adSetFlight = `"start_time":"2026-10-01T09:00:00-0700","end_time":"2026-10-31T23:59:59+0000"`

// monitorAdSetRow is one account-level /adsets row; budgets is raw JSON members ("" omits both).
// A row whose members name neither start_time nor end_time gets adSetFlight, the flight every ad
// set CreateCampaign makes carries — a budget is paced only against a known flight.
func monitorAdSetRow(id, campaignID, effectiveStatus, budgets string) string {
	if !strings.Contains(budgets, "start_time") && !strings.Contains(budgets, "end_time") {
		if budgets == "" {
			budgets = adSetFlight
		} else {
			budgets += "," + adSetFlight
		}
	}
	budgets = "," + budgets
	return fmt.Sprintf(`{"id":"%s","campaign_id":"%s","effective_status":"%s"%s}`, id, campaignID, effectiveStatus, budgets)
}

func insightsFor(ids ...string) string {
	rows := make([]string, 0, len(ids))
	for _, id := range ids {
		rows = append(rows, fmt.Sprintf(`{"campaign_id":"%s","impressions":"1000","clicks":"10","spend":"5"}`, id))
	}
	return `{"data":[` + strings.Join(rows, ",") + `],"paging":{}}`
}

func listMonitorRows(t *testing.T, f monitorFixture) (map[string]AccountCampaignRow, *recordedURIs) {
	t.Helper()
	srv, rec := monitorServer(t, f)
	rows, err := newMonitorClient(srv).ListAccountCampaigns(context.Background(), "act_123", 30)
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	byID := make(map[string]AccountCampaignRow, len(rows))
	for _, r := range rows {
		byID[r.CampaignID] = r
	}
	return byID, rec
}

func adSetRequests(rec *recordedURIs) []string {
	var out []string
	for _, u := range rec.all() {
		if strings.Contains(u, "/adsets") {
			out = append(out, u)
		}
	}
	return out
}

func assertBudgets(t *testing.T, r AccountCampaignRow, daily, lifetime float64) {
	t.Helper()
	if r.DailyBudget != daily || r.LifetimeBudget != lifetime {
		t.Errorf("campaign %s budgets = daily %v / lifetime %v, want %v / %v", r.CampaignID, r.DailyBudget, r.LifetimeBudget, daily, lifetime)
	}
}

// A CBO campaign carries its own budget: it is read from the campaign, and no ad-set read is
// made when no campaign needs one.
func TestListAccountCampaigns_CBOBudget_NoAdSetRead(t *testing.T) {
	rows, rec := listMonitorRows(t, monitorFixture{
		campaigns: []string{monitorList(monitorCampaign("111", StatusActive, `"daily_budget":"5000"`))},
		insights:  []string{insightsFor("111")},
		currency:  usdAccount,
	})
	assertBudgets(t, rows["111"], 50, 0)
	if got := adSetRequests(rec); len(got) != 0 {
		t.Errorf("no campaign lacked a budget, yet the ad sets were read: %v", got)
	}
}

// The shape CreateCampaign produces: no campaign-level budget, the amount on the ad set. The
// campaign's ad sets are summed, and another campaign's ad sets are not part of its expected
// spend. A CBO campaign in the same account keeps its campaign-level budget even though its ad
// sets appear in the account-level read.
func TestListAccountCampaigns_ABOBudget_SumsActiveAdSets(t *testing.T) {
	rows, rec := listMonitorRows(t, monitorFixture{
		campaigns: []string{monitorList(
			monitorCampaign("111", StatusActive, ""),
			monitorCampaign("222", StatusActive, `"lifetime_budget":"0"`),
			monitorCampaign("333", StatusActive, `"daily_budget":"7000"`),
		)},
		insights: []string{insightsFor("111", "222", "333")},
		currency: usdAccount,
		adSets: []string{`{"data":[` + strings.Join([]string{
			monitorAdSetRow("9001", "111", "ACTIVE", `"daily_budget":"1000"`),
			monitorAdSetRow("9002", "111", "ACTIVE", `"daily_budget":"2500","lifetime_budget":"0"`),
			monitorAdSetRow("9005", "222", "ACTIVE", `"lifetime_budget":"300000","start_time":"2026-10-01T09:00:00-0700","end_time":"2026-10-31T23:59:59+0000"`),
			monitorAdSetRow("9006", "333", "ACTIVE", ""),
		}, ",") + `],"paging":{}}`},
	})
	assertBudgets(t, rows["111"], 35, 0)
	assertBudgets(t, rows["222"], 0, 3000)
	if r := rows["222"]; r.StartDate != "2026-10-01" || r.EndDate != "2026-10-31" {
		t.Errorf("lifetime ABO campaign flight = %q..%q, want the ad set's 2026-10-01..2026-10-31", r.StartDate, r.EndDate)
	}
	assertBudgets(t, rows["333"], 70, 0) // CBO: the campaign's own budget, untouched
	got := adSetRequests(rec)
	if len(got) != 1 {
		t.Fatalf("want exactly ONE account-level ad-set read, got %v", got)
	}
	if !strings.HasPrefix(got[0], "/act_123/adsets?fields=id,campaign_id,daily_budget,lifetime_budget,effective_status,start_time,end_time&limit=500") {
		t.Errorf("ad-set read = %s, want the account-level edge with the budget fields", got[0])
	}
	if strings.Contains(got[0], "tok-secret-abc") {
		t.Errorf("ad-set read leaked the access token: %s", got[0])
	}
}

// A paused ad set's budget is not what the campaign is expected to spend now, but the campaign's
// Insights spend still includes what it spent in the window before it stopped — and when it
// stopped is not on the read. Summing only the active ad sets compared 140 spent against 70
// expected (two ad sets at 10/day for a week, one then paused): a false overspend (PR #316
// review). Any non-ACTIVE ad set leaves the campaign's ad-set budget unknown; a campaign whose
// ad sets are all ACTIVE beside it is unaffected.
func TestListAccountCampaigns_ABOBudget_NonActiveAdSetIsUnknown(t *testing.T) {
	for _, status := range []string{"PAUSED", "CAMPAIGN_PAUSED", "ARCHIVED", "IN_PROCESS", "WITH_ISSUES"} {
		t.Run(status, func(t *testing.T) {
			rows, _ := listMonitorRows(t, monitorFixture{
				campaigns: []string{monitorList(monitorCampaign("111", StatusActive, ""), monitorCampaign("222", StatusActive, ""))},
				insights:  []string{insightsFor("111", "222")},
				currency:  usdAccount,
				adSets: []string{monitorList(
					monitorAdSetRow("9001", "111", "ACTIVE", `"daily_budget":"1000"`),
					monitorAdSetRow("9002", "111", status, `"daily_budget":"1000"`),
					monitorAdSetRow("9003", "222", "ACTIVE", `"daily_budget":"4000"`),
				)},
			})
			assertBudgets(t, rows["111"], 0, 0)
			assertBudgets(t, rows["222"], 40, 0)
		})
	}
}

// A campaign whose ad sets mix daily and lifetime budgets has no single pacing expectation, and
// one with no active ad set has nothing to pace against: both stay at unknown (0/0), never a
// partial or mixed figure.
func TestListAccountCampaigns_ABOBudget_MixedOrNoneIsUnknown(t *testing.T) {
	rows, _ := listMonitorRows(t, monitorFixture{
		campaigns: []string{monitorList(
			monitorCampaign("111", StatusActive, ""),
			monitorCampaign("222", StatusActive, ""),
			monitorCampaign("333", StatusActive, ""),
		)},
		insights: []string{insightsFor("111", "222", "333")},
		currency: usdAccount,
		adSets: []string{monitorList(
			monitorAdSetRow("9001", "111", "ACTIVE", `"daily_budget":"1000"`),
			monitorAdSetRow("9002", "111", "ACTIVE", `"lifetime_budget":"100000"`),
			monitorAdSetRow("9003", "222", "PAUSED", `"daily_budget":"1000"`),
			monitorAdSetRow("9004", "333", "ACTIVE", `"daily_budget":"4000"`),
		)},
	})
	assertBudgets(t, rows["111"], 0, 0)
	assertBudgets(t, rows["222"], 0, 0)
	assertBudgets(t, rows["333"], 40, 0)
}

// Per-ad-set defects make only that ad set's campaign unknown: an explicit null is not zero and
// not absent, an active ad set with no budget leaves nothing honest to sum, and an ad set
// reporting both budgets contradicts Meta's documented exclusivity.
func TestListAccountCampaigns_ABOBudget_RowDefectsFailThatCampaignClosed(t *testing.T) {
	for _, tc := range []struct{ name, budgets string }{
		{"explicit null daily budget", `"daily_budget":null`},
		{"explicit null lifetime budget beside a daily one", `"daily_budget":"1000","lifetime_budget":null`},
		{"numeric rather than string budget", `"daily_budget":1000`},
		{"fractional minor units", `"daily_budget":"1000.5"`},
		{"negative budget", `"daily_budget":"-1000"`},
		{"no budget on an active ad set", ``},
		{"both budgets", `"daily_budget":"1000","lifetime_budget":"100000"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, _ := listMonitorRows(t, monitorFixture{
				campaigns: []string{monitorList(monitorCampaign("111", StatusActive, ""), monitorCampaign("222", StatusActive, ""))},
				insights:  []string{insightsFor("111", "222")},
				currency:  usdAccount,
				adSets: []string{monitorList(
					monitorAdSetRow("9001", "111", "ACTIVE", `"daily_budget":"2000"`),
					monitorAdSetRow("9002", "111", "ACTIVE", tc.budgets),
					monitorAdSetRow("9003", "222", "ACTIVE", `"daily_budget":"4000"`),
				)},
			})
			assertBudgets(t, rows["111"], 0, 0)
			assertBudgets(t, rows["222"], 40, 0)
		})
	}
}

// Whole-read defects: the answer cannot be trusted at all, so every campaign that needed it
// stays unknown — but the monitor read itself still succeeds, with a CBO campaign's budget and
// every campaign's delivery intact.
func TestListAccountCampaigns_ABOBudget_UntrustworthyReadLeavesUnknown(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"duplicate key in a row", `{"data":[{"id":"9001","campaign_id":"111","effective_status":"ACTIVE","daily_budget":"1000","daily_budget":"900000"}],"paging":{}}`},
		{"duplicate data key", `{"data":[],"data":[` + monitorAdSetRow("9001", "111", "ACTIVE", `"daily_budget":"1000"`) + `],"paging":{}}`},
		{"no data field", `{"paging":{}}`},
		{"repeated ad set row", monitorList(monitorAdSetRow("9001", "111", "ACTIVE", `"daily_budget":"1000"`), monitorAdSetRow("9001", "111", "ACTIVE", `"daily_budget":"1000"`))},
		{"row with no campaign_id", monitorList(`{"id":"9001","effective_status":"ACTIVE","daily_budget":"1000"}`)},
		{"row with no id", monitorList(`{"campaign_id":"111","effective_status":"ACTIVE","daily_budget":"1000"}`)},
		{"next page with no cursor", `{"data":[],"paging":{"next":"https://graph.facebook.com/next"}}`},
		// Syntax-invalid JSON in a 200 used to fail inside doRequest's decode as a transportError,
		// which bypassed this fallback and failed the whole monitor, metrics included (PR #316
		// review).
		{"syntax-invalid JSON", `{"data":[` + monitorAdSetRow("9001", "111", "ACTIVE", `"daily_budget":"1000"`)},
		{"not JSON at all", `<html>upstream proxy error</html>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, _ := listMonitorRows(t, monitorFixture{
				campaigns: []string{monitorList(monitorCampaign("111", StatusActive, ""), monitorCampaign("333", StatusActive, `"daily_budget":"7000"`))},
				insights:  []string{insightsFor("111", "333")},
				currency:  usdAccount,
				adSets:    []string{tc.body},
			})
			assertBudgets(t, rows["111"], 0, 0)
			assertBudgets(t, rows["333"], 70, 0)
			if rows["111"].Impressions != 1000 || rows["111"].FetchFailed {
				t.Errorf("an unreadable ad-set read must not touch delivery: %+v", rows["111"])
			}
		})
	}
}

// The walk is bounded. A `paging.next` on the last allowed page fails the read closed: the
// campaigns that needed it are unknown, never paced against a partial sum.
func TestListAccountCampaigns_ABOBudget_PageCapIsUnknownNotPartial(t *testing.T) {
	rows, rec := listMonitorRows(t, monitorFixture{
		campaigns: []string{monitorList(monitorCampaign("111", StatusActive, ""))},
		insights:  []string{insightsFor("111")},
		currency:  usdAccount,
		adSetsFn: func(n int) string {
			return fmt.Sprintf(`{"data":[%s],"paging":{"cursors":{"after":"c%d"},"next":"https://graph.facebook.com/next?after=c%d"}}`,
				monitorAdSetRow(fmt.Sprintf("%d", 9000+n), "111", "ACTIVE", `"daily_budget":"1000"`), n, n)
		},
	})
	assertBudgets(t, rows["111"], 0, 0)
	if got := len(adSetRequests(rec)); got != monitorAdSetMaxPages {
		t.Errorf("ad-set pages read = %d, want the cap %d", got, monitorAdSetMaxPages)
	}
}

// A failed REQUEST is not an unreadable answer: it fails the monitor read like the campaign-list
// and insights reads do, rather than presenting every campaign as unknown with no error.
func TestListAccountCampaigns_ABOBudget_RequestFailureIsAnError(t *testing.T) {
	srv, _ := monitorServer(t, monitorFixture{
		campaigns:    []string{monitorList(monitorCampaign("111", StatusActive, ""))},
		insights:     []string{insightsFor("111")},
		currency:     usdAccount,
		adSetsStatus: http.StatusBadRequest,
	})
	_, err := newMonitorClient(srv).ListAccountCampaigns(context.Background(), "act_123", 30)
	if err == nil || !strings.Contains(err.Error(), "ad-set budgets") {
		t.Fatalf("err = %v, want the ad-set read's request failure", err)
	}
}

// Budgets are minor units of the ACCOUNT's currency: 100 per unit for USD, 1 for JPY. Dividing
// by 100 unconditionally read a ¥3,000 budget as ¥30.
func TestListAccountCampaigns_BudgetsUseAccountCurrencyOffset(t *testing.T) {
	for _, tc := range []struct {
		name, currency            string
		wantCampaign, wantAdSetDB float64
	}{
		{"USD divides by 100", usdAccount, 30, 15},
		{"JPY is already whole units", jpyAccount, 3000, 1500},
		{"KRW is already whole units", `{"currency":"KRW"}`, 3000, 1500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, _ := listMonitorRows(t, monitorFixture{
				campaigns: []string{monitorList(monitorCampaign("111", StatusActive, `"daily_budget":"3000"`), monitorCampaign("222", StatusActive, ""))},
				insights:  []string{insightsFor("111", "222")},
				currency:  tc.currency,
				adSets:    []string{monitorList(monitorAdSetRow("9001", "222", "ACTIVE", `"daily_budget":"1500"`))},
			})
			assertBudgets(t, rows["111"], tc.wantCampaign, 0)
			assertBudgets(t, rows["222"], tc.wantAdSetDB, 0)
		})
	}
}

// A currency outside the supported map has no known scale: every budget is left unknown rather
// than guessed at 100, and no ad-set read is spent on budgets that could not be converted.
func TestListAccountCampaigns_UnknownCurrencyLeavesBudgetsUnknown(t *testing.T) {
	rows, rec := listMonitorRows(t, monitorFixture{
		campaigns: []string{monitorList(monitorCampaign("111", StatusActive, `"daily_budget":"3000"`), monitorCampaign("222", StatusActive, ""))},
		insights:  []string{insightsFor("111", "222")},
		currency:  `{"currency":"ZZZ"}`,
	})
	assertBudgets(t, rows["111"], 0, 0)
	assertBudgets(t, rows["222"], 0, 0)
	if got := adSetRequests(rec); len(got) != 0 {
		t.Errorf("ad sets read for an account whose budgets cannot be scaled: %v", got)
	}
	if rows["111"].Impressions != 1000 {
		t.Errorf("delivery must survive an unknown currency: %+v", rows["111"])
	}
}

// The currency read is held to the same identity check as every other attributing read: a
// duplicated currency key is refused, not resolved in favour of the last value.
func TestListAccountCampaigns_DuplicateCurrencyKeyIsAnError(t *testing.T) {
	srv, _ := monitorServer(t, monitorFixture{
		campaigns: []string{monitorList(monitorCampaign("111", StatusActive, `"daily_budget":"3000"`))},
		insights:  []string{insightsFor("111")},
		currency:  `{"currency":"USD","currency":"JPY"}`,
	})
	if _, err := newMonitorClient(srv).ListAccountCampaigns(context.Background(), "act_123", 30); err == nil {
		t.Fatal("a currency body with a duplicated key was accepted")
	}
}

// A lifetime budget is paced against its flight, and CreateCampaign writes the flight on the AD
// SET — the campaign-list row has no dates for it (PR #316 review). The active ad sets' shared
// window becomes the row's flight; disagreeing or unreadable windows leave the lifetime budget
// unknown rather than paced against the wrong schedule. Daily pacing is limited to the days the
// flight ran in the window too (PR #316 review), so a daily budget is likewise used only with one
// shared window.
func TestListAccountCampaigns_ABOBudget_FlightFromAdSets(t *testing.T) {
	const (
		oct     = `"start_time":"2026-10-01T09:00:00-0700","end_time":"2026-10-31T23:59:59+0000"`
		nov     = `"start_time":"2026-11-01T09:00:00-0700","end_time":"2026-11-30T23:59:59+0000"`
		openEnd = `"start_time":"2026-10-01T09:00:00-0700"`
	)
	life := func(window string) string { return `"lifetime_budget":"100000",` + window }
	daily := func(window string) string { return `"daily_budget":"1000",` + window }
	for _, tc := range []struct {
		name                string
		adSets              []string
		wantDaily, wantLife float64
		wantStart, wantEnd  string
	}{
		{"one lifetime ad set gives its flight", []string{life(oct)}, 0, 1000, "2026-10-01", "2026-10-31"},
		{"lifetime ad sets sharing one window are summed on it", []string{life(oct), life(oct)}, 0, 2000, "2026-10-01", "2026-10-31"},
		{"an open-ended flight keeps an empty end", []string{life(openEnd)}, 0, 1000, "2026-10-01", ""},
		{"lifetime ad sets with differing windows are unknown", []string{life(oct), life(nov)}, 0, 0, "", ""},
		{"an open-ended lifetime ad set beside a bounded one is unknown", []string{life(oct), life(openEnd)}, 0, 0, "", ""},
		{"lifetime ad sets sharing a start but not an end are unknown", []string{life(oct), life(`"start_time":"2026-10-01T09:00:00-0700","end_time":"2026-11-30T23:59:59+0000"`)}, 0, 0, "", ""},
		{"a daily and a lifetime ad set sharing a window are unknown", []string{daily(oct), life(oct)}, 0, 0, "", ""},
		{"a lifetime ad set with no start_time is unknown", []string{`"lifetime_budget":"100000","end_time":"2026-10-31T23:59:59+0000"`}, 0, 0, "", ""},
		{"a null start_time is unknown", []string{life(`"start_time":null`)}, 0, 0, "", ""},
		{"a null end_time is unknown", []string{life(`"start_time":"2026-10-01T09:00:00-0700","end_time":null`)}, 0, 0, "", ""},
		{"a malformed start_time is unknown", []string{life(`"start_time":"0000-00-00T00:00:00-0700"`)}, 0, 0, "", ""},
		{"daily ad sets sharing a window show it", []string{daily(oct), daily(oct)}, 20, 0, "2026-10-01", "2026-10-31"},
		{"daily ad sets with differing windows are unknown", []string{daily(oct), daily(nov)}, 0, 0, "", ""},
		{"a daily ad set with no start_time is unknown", []string{`"daily_budget":"1000","end_time":"2026-10-31T23:59:59+0000"`}, 0, 0, "", ""},
		{"an open-ended daily ad set keeps its budget", []string{daily(openEnd)}, 10, 0, "2026-10-01", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rowsJSON := make([]string, 0, len(tc.adSets))
			for i, b := range tc.adSets {
				rowsJSON = append(rowsJSON, monitorAdSetRow(fmt.Sprintf("%d", 9001+i), "111", "ACTIVE", b))
			}
			// The campaign-list row carries no dates, as for a campaign CreateCampaign made.
			campaign := `{"id":"111","name":"Campaign 111","status":"ACTIVE"}`
			rows, _ := listMonitorRows(t, monitorFixture{
				campaigns: []string{monitorList(campaign)},
				insights:  []string{insightsFor("111")},
				currency:  usdAccount,
				adSets:    []string{monitorList(rowsJSON...)},
			})
			r := rows["111"]
			assertBudgets(t, r, tc.wantDaily, tc.wantLife)
			if r.StartDate != tc.wantStart || r.EndDate != tc.wantEnd {
				t.Errorf("flight = %q..%q, want %q..%q", r.StartDate, r.EndDate, tc.wantStart, tc.wantEnd)
			}
		})
	}
}

// A malformed effective_status costs only its own campaign its ad-set budget (PR #316 review): it
// used to fail the row's whole decode, which failed the whole read and with it every campaign
// that needed an ad-set budget.
func TestListAccountCampaigns_ABOBudget_BadEffectiveStatusIsLocal(t *testing.T) {
	for _, tc := range []struct{ name, status string }{
		{"numeric", `123`},
		{"null", `null`},
		{"object", `{"v":"ACTIVE"}`},
		{"lowercase token", `"active"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := `{"id":"9001","campaign_id":"111","effective_status":` + tc.status + `,"daily_budget":"1000"}`
			rows, _ := listMonitorRows(t, monitorFixture{
				campaigns: []string{monitorList(monitorCampaign("111", StatusActive, ""), monitorCampaign("222", StatusActive, ""))},
				insights:  []string{insightsFor("111", "222")},
				currency:  usdAccount,
				adSets:    []string{monitorList(bad, monitorAdSetRow("9002", "222", "ACTIVE", `"daily_budget":"4000"`))},
			})
			assertBudgets(t, rows["111"], 0, 0)
			assertBudgets(t, rows["222"], 40, 0)
		})
	}
}

// A negative campaign-level budget is unreadable, not "no budget": it marks the row FetchFailed
// and never sends the campaign to the ad-set fallback (PR #316 review).
func TestListAccountCampaigns_NegativeCampaignBudgetIsUnreadable(t *testing.T) {
	for _, field := range []string{`"daily_budget":"-500"`, `"lifetime_budget":"-90000"`} {
		t.Run(field, func(t *testing.T) {
			rows, rec := listMonitorRows(t, monitorFixture{
				campaigns: []string{monitorList(monitorCampaign("111", StatusActive, field))},
				insights:  []string{insightsFor("111")},
				currency:  usdAccount,
				adSets:    []string{monitorList(monitorAdSetRow("9001", "111", "ACTIVE", `"daily_budget":"4000"`))},
			})
			r := rows["111"]
			if !r.FetchFailed {
				t.Errorf("row = %+v, want FetchFailed for a negative campaign budget", r)
			}
			assertBudgets(t, r, 0, 0)
			if got := adSetRequests(rec); len(got) != 0 {
				t.Errorf("a negative campaign budget sent the campaign to the ad-set fallback: %v", got)
			}
		})
	}
}
