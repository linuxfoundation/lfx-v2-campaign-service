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

// adSetRow is one account-level /adsets row; budgets is raw JSON members ("" omits both).
func monitorAdSetRow(id, campaignID, effectiveStatus, budgets string) string {
	if budgets != "" {
		budgets = "," + budgets
	}
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

// The shape CreateCampaign produces: no campaign-level budget, the amount on the ad set. Only
// ACTIVE ad sets count; a paused ad set, one under a paused campaign, and another campaign's ad
// sets are not part of this campaign's expected spend. A CBO campaign in the same account keeps
// its campaign-level budget even though its ad sets appear in the account-level read.
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
			monitorAdSetRow("9003", "111", "PAUSED", `"daily_budget":"99900"`),
			monitorAdSetRow("9004", "111", "CAMPAIGN_PAUSED", `"daily_budget":"99900"`),
			monitorAdSetRow("9005", "222", "ACTIVE", `"lifetime_budget":"300000"`),
			monitorAdSetRow("9006", "333", "ACTIVE", ""),
		}, ",") + `],"paging":{}}`},
	})
	assertBudgets(t, rows["111"], 35, 0)
	assertBudgets(t, rows["222"], 0, 3000)
	assertBudgets(t, rows["333"], 70, 0) // CBO: the campaign's own budget, untouched
	got := adSetRequests(rec)
	if len(got) != 1 {
		t.Fatalf("want exactly ONE account-level ad-set read, got %v", got)
	}
	if !strings.HasPrefix(got[0], "/act_123/adsets?fields=id,campaign_id,daily_budget,lifetime_budget,effective_status&limit=500") {
		t.Errorf("ad-set read = %s, want the account-level edge with the budget fields", got[0])
	}
	if strings.Contains(got[0], "tok-secret-abc") {
		t.Errorf("ad-set read leaked the access token: %s", got[0])
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
