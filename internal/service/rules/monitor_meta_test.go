// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package rules

import (
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// withMetaWindow sets on every row the Insights window the Meta read would send for `days` at
// `now` — `days` UTC calendar days ending on now's date, inclusive — the way
// meta.Client.ListAccountCampaigns stamps MetricsWindowStart/MetricsWindowEnd on each row.
func withMetaWindow(rows []model.AccountCampaignMetrics, days int, now time.Time) []model.AccountCampaignMetrics {
	until := now.UTC()
	since := until.AddDate(0, 0, -(days - 1))
	out := make([]model.AccountCampaignMetrics, len(rows))
	for i, m := range rows {
		m.MetricsWindowStart, m.MetricsWindowEnd = since.Format("2006-01-02"), until.Format("2006-01-02")
		out[i] = m
	}
	return out
}

// metaPct is metaPacingPct for one row read over `days` ending at `now`.
func metaPct(m model.AccountCampaignMetrics, days int, now time.Time) (float64, bool) {
	return metaPacingPct(withMetaWindow([]model.AccountCampaignMetrics{m}, days, now)[0])
}

// evalMeta is EvaluateMetaMonitor over rows read over `days` ending at `now`.
func evalMeta(rows []model.AccountCampaignMetrics, days int, now time.Time) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem) {
	return EvaluateMetaMonitor(withMetaWindow(rows, days, now))
}

// TestMetaPacingPct_ScheduleBranch pins the TotalBudget+start-date branch: the lifetime budget is
// spread evenly over the flight (EndDate inclusive) and the expectation covers only the days of
// the requested window the flight ran on, because m.Spend covers only that window.
func TestMetaPacingPct_ScheduleBranch(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	m := model.AccountCampaignMetrics{
		TotalBudget: 200,
		StartDate:   "2026-06-05",
		EndDate:     "2026-06-24", // 20-day flight, 06-05..06-24 inclusive
		Spend:       100,
	}
	pct, computable := metaPct(m, 10, now)
	// window 06-06..06-15 = 10 flight days; expected = 200/20*10 = 100; spend 100 → 100%.
	if !computable {
		t.Fatalf("computable = false, want true")
	}
	if pct != 100 {
		t.Errorf("pct = %v, want 100", pct)
	}
}

// TestMetaPacingPct_LifetimePacesTheWindowNotTheFlightToDate pins the PR #316 review fix: spend
// covers only the requested window, so the expectation must too. A 9,000 lifetime budget over a
// 90-day flight, read on day 60 with days=7, spending exactly 100/day, is on plan: 700 of 700.
// Expecting the flight-to-date plan (~5,900) instead reported ~12% — a false underspend.
func TestMetaPacingPct_LifetimePacesTheWindowNotTheFlightToDate(t *testing.T) {
	m := model.AccountCampaignMetrics{
		TotalBudget: 9000,
		StartDate:   "2026-06-01",
		EndDate:     "2026-08-29", // 90 days inclusive
		Spend:       700,
	}
	now := time.Date(2026, 7, 30, 15, 0, 0, 0, time.UTC) // flight day 60
	pct, computable := metaPct(m, 7, now)
	if !computable || pct != 100 {
		t.Fatalf("pct = %v (computable %v), want 100: 700 spent against 9000/90*7 = 700 expected", pct, computable)
	}
	rows, items := evalMeta([]model.AccountCampaignMetrics{{PlatformCampaignID: "1", Name: "c", Status: "ACTIVE",
		Impressions: 10000, Clicks: 100, TotalBudget: 9000, StartDate: "2026-06-01", EndDate: "2026-08-29", Spend: 700}}, 7, now)
	if rows[0].PacingLabel == model.MonitorPacingUnderspending {
		t.Errorf("an on-plan lifetime campaign is labelled underspending: %+v", rows[0])
	}
	for _, it := range items {
		if strContains(it.Issue, "Underspending") {
			t.Errorf("unexpected underspending item: %q", it.Issue)
		}
	}

	// Half the plan in the window is 50%, and the text names the window's plan, not the whole
	// lifetime budget.
	m.Spend = 350
	if pct, _ := metaPct(m, 7, now); pct != 50 {
		t.Errorf("pct = %v, want 50", pct)
	}
	_, items = evalMeta([]model.AccountCampaignMetrics{{PlatformCampaignID: "1", Name: "c", Status: "ACTIVE",
		Impressions: 10000, Clicks: 100, TotalBudget: 9000, StartDate: "2026-06-01", EndDate: "2026-08-29", Spend: 300}}, 7, now)
	mustContainIssue(t, items, "(300.00 in account currency of 700.00 in account currency)", model.MonitorPriorityMed)
}

// TestMetaPacingPct_LifetimeWindowEdges: a window that only partly overlaps the flight expects
// only the overlapping days; one that does not overlap it at all is not computable.
func TestMetaPacingPct_LifetimeWindowEdges(t *testing.T) {
	m := model.AccountCampaignMetrics{TotalBudget: 1000, StartDate: "2026-07-27", EndDate: "2026-08-05", Spend: 400} // 10 days
	now := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)
	// window 07-24..07-30, flight from 07-27: 4 overlapping days → expected 400.
	if pct, ok := metaPct(m, 7, now); !ok || pct != 100 {
		t.Errorf("partial overlap: pct = %v (ok %v), want 100", pct, ok)
	}
	notStarted := m
	notStarted.StartDate, notStarted.EndDate = "2026-08-10", "2026-08-20"
	if _, ok := metaPct(notStarted, 7, now); ok {
		t.Error("a flight that starts after the window was paced; its zero spend is correct, not underspending")
	}
	ended := m
	ended.StartDate, ended.EndDate = "2026-06-01", "2026-07-10"
	if _, ok := metaPct(ended, 7, now); ok {
		t.Error("a flight that ended before the window was paced")
	}
}

// TestMetaPacingPct_LifetimeEndDateIsInclusive: EndDate is a day the flight still runs on
// (CreateCampaign writes end_time as 23:59:59 of it). A window that reaches only the flight's
// last day expects that day's share; treating EndDate as an exclusive midnight dropped it, so
// the window did not overlap the flight at all (PR #316 review).
func TestMetaPacingPct_LifetimeEndDateIsInclusive(t *testing.T) {
	m := model.AccountCampaignMetrics{TotalBudget: 1000, StartDate: "2026-07-21", EndDate: "2026-07-30", Spend: 100} // 10 days
	now := time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC)
	// window 07-30..08-05: only 07-30, the flight's last day, is in it → expected 100.
	if pct, ok := metaPct(m, 7, now); !ok || pct != 100 {
		t.Errorf("last-day overlap: pct = %v (ok %v), want 100 (1000/10 × 1 day)", pct, ok)
	}
}

// TestMetaPacingPct_MidnightStopIsTheDayBefore pins the PR #316 review example end to end in the
// rules layer: a 1,000 lifetime flight running 2026-10-01 00:00 through 2026-10-11 00:00 is TEN
// days, and the Meta read hands it over as EndDate 2026-10-10 (meta.endDateOnly). Spending 700
// over 10-01..10-07 is exactly on plan: 1000/10*7 = 700, 100%. The old reading, EndDate
// 2026-10-11, counted 11 days and expected ~636 — ~110%, a false overspend.
func TestMetaPacingPct_MidnightStopIsTheDayBefore(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC) // window 10-01..10-07
	m := model.AccountCampaignMetrics{TotalBudget: 1000, StartDate: "2026-10-01", EndDate: "2026-10-10", Spend: 700}
	if pct, ok := metaPct(m, 7, now); !ok || pct != 100 {
		t.Errorf("midnight stop read as 10-10: pct = %v (ok %v), want 100 (1000/10 × 7 = 700)", pct, ok)
	}
	if exp, ok := metaExpectedSpend(withMetaWindow([]model.AccountCampaignMetrics{m}, 7, now)[0]); !ok || exp != 700 {
		t.Errorf("expected = %v (ok %v), want 700 over a 10-day flight", exp, ok)
	}
	// What the midnight's own date would have produced: 11 flight days, a false overspend.
	stale := m
	stale.EndDate = "2026-10-11"
	if pct, _ := metaPct(stale, 7, now); pct != 110 {
		t.Errorf("an 11-day reading = %v, want 110 (the overcount the read now avoids)", pct)
	}
}

// TestMetaPacingPct_OpenEndedLifetimeIsUnknown: a lifetime budget with no end date has no flight
// length to spread it over. Reading the flight as "ends today" expected the WHOLE budget at once
// on a flight's first day — a false underspend — so it is pacing unknown (PR #316 review).
func TestMetaPacingPct_OpenEndedLifetimeIsUnknown(t *testing.T) {
	now := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)
	for _, start := range []string{"2026-07-30", "2026-07-01", "2026-01-01"} {
		m := model.AccountCampaignMetrics{TotalBudget: 3000, StartDate: start, Spend: 100}
		if pct, ok := metaPct(m, 7, now); ok {
			t.Errorf("open-ended lifetime from %s paced at %v%%, want pacing unknown", start, pct)
		}
	}
	rows, items := evalMeta([]model.AccountCampaignMetrics{{PlatformCampaignID: "1", Name: "c", Status: "ACTIVE",
		Impressions: 1000, Clicks: 10, TotalBudget: 3000, StartDate: "2026-07-30", Spend: 100}}, 7, now)
	if !rows[0].Metrics.PacingUnknown {
		t.Errorf("row = %+v, want PacingUnknown for an open-ended lifetime flight", rows[0])
	}
	for _, it := range items {
		if strContains(it.Issue, "Underspending") || strContains(it.Issue, "Budget ") {
			t.Errorf("pacing item for an open-ended lifetime flight: %q", it.Issue)
		}
	}
}

// TestMetaPacingPct_DailyCountsOnlyTheFlightsDaysInTheWindow: a daily budget is expected only on
// the days of the window the flight ran on. A campaign started 7 days ago spending its full
// daily budget, read with days=30, used to be expected to spend for all 30 days and read ~23% —
// a false underspend on every new campaign (PR #316 review). A flight that ended inside the
// window is cut at its (inclusive) end the same way, and one outside the window is unknown.
func TestMetaPacingPct_DailyCountsOnlyTheFlightsDaysInTheWindow(t *testing.T) {
	now := time.Date(2026, 7, 30, 15, 0, 0, 0, time.UTC)
	started := model.AccountCampaignMetrics{BudgetDay: 50, StartDate: "2026-07-24", Spend: 350} // 07-24..07-30 = 7 days
	if pct, ok := metaPct(started, 30, now); !ok || pct != 100 {
		t.Errorf("started 7 days ago, days=30: pct = %v (ok %v), want 100 (350 of 50 × 7)", pct, ok)
	}
	rows, items := evalMeta([]model.AccountCampaignMetrics{{PlatformCampaignID: "1", Name: "c", Status: "ACTIVE",
		Impressions: 10000, Clicks: 100, BudgetDay: 50, StartDate: "2026-07-24", Spend: 350}}, 30, now)
	if rows[0].PacingLabel == model.MonitorPacingUnderspending || rows[0].Metrics.PacingUnknown {
		t.Errorf("a new campaign spending its full daily budget = %+v, want paced, not underspending", rows[0])
	}
	for _, it := range items {
		if strContains(it.Issue, "Underspending") {
			t.Errorf("unexpected underspending item: %q", it.Issue)
		}
	}

	ended := model.AccountCampaignMetrics{BudgetDay: 50, StartDate: "2026-01-01", EndDate: "2026-07-20", Spend: 250} // 07-16..07-20 in a 15-day window
	if pct, ok := metaPct(ended, 15, now); !ok || pct != 100 {
		t.Errorf("ended inside the window: pct = %v (ok %v), want 100 (250 of 50 × 5)", pct, ok)
	}
	future := model.AccountCampaignMetrics{BudgetDay: 50, StartDate: "2026-08-10", Spend: 0}
	if _, ok := metaPct(future, 30, now); ok {
		t.Error("a daily flight that starts after the window was paced; its zero spend is correct, not underspending")
	}
}

// TestEvaluateMetaMonitor_PacesTheReadsOwnWindow: the evaluator takes no clock; it paces against
// the window stamped on the row — the since/until the Insights read actually sent. The service
// used to capture `now` before the read while the read took its own clock reading, so a request
// crossing UTC midnight paced one day's plan against the next day's spend (PR #316 review).
// Here the read ran just after midnight on 07-31: the window is 07-25..07-31 and the flight
// started on 07-31, so exactly one flight day is in the window. A pre-midnight `now` (07-30)
// would have put the flight wholly outside the window.
func TestEvaluateMetaMonitor_PacesTheReadsOwnWindow(t *testing.T) {
	row := model.AccountCampaignMetrics{PlatformCampaignID: "1", Name: "c", Status: "ACTIVE", Impressions: 1000, Clicks: 10,
		BudgetDay: 40, StartDate: "2026-07-31", Spend: 40, MetricsWindowStart: "2026-07-25", MetricsWindowEnd: "2026-07-31"}
	rows, _ := EvaluateMetaMonitor([]model.AccountCampaignMetrics{row})
	if rows[0].Metrics.PacingUnknown || rows[0].PacingPct != 100 {
		t.Errorf("row = %+v, want 100%%: 40 spent on the window's one flight day at 40/day", rows[0])
	}

	// A row with no window states no days its spend covers: pacing unknown, never a guess.
	for _, w := range [][2]string{{"", ""}, {"2026-07-25", ""}, {"", "2026-07-31"}, {"2026-07-31", "2026-07-25"}, {"bad", "2026-07-31"}} {
		r := row
		r.MetricsWindowStart, r.MetricsWindowEnd = w[0], w[1]
		out, _ := EvaluateMetaMonitor([]model.AccountCampaignMetrics{r})
		if !out[0].Metrics.PacingUnknown {
			t.Errorf("window %q..%q: row = %+v, want PacingUnknown", w[0], w[1], out[0])
		}
	}
}

// TestMetaPacingPct_FlatDailyBudgetBranch pins the BudgetDay*days fallback used when no
// TotalBudget/start-date schedule is present.
func TestMetaPacingPct_FlatDailyBudgetBranch(t *testing.T) {
	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	m := model.AccountCampaignMetrics{BudgetDay: 10, Spend: 45}
	pct, computable := metaPct(m, 5, now)
	// expected = 10*5 = 50, spend/expected*100 = 90.
	if !computable {
		t.Fatalf("computable = false, want true")
	}
	if pct != 90 {
		t.Errorf("pct = %v, want 90", pct)
	}
}

// TestMetaPacingPct_NoBudgetIsNotComputable pins the fix for the defect an earlier revision of
// this test merely observed: metaPacingPct's second return was hardcoded false on every path,
// including the no-budget fallback, so the caller's guard on it was dead and a genuinely
// unmeasurable row was indistinguishable from a real 0% reading.
//
// The flag now means "computable", and a campaign with no budget information says false.
func TestMetaPacingPct_NoBudgetIsNotComputable(t *testing.T) {
	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	m := model.AccountCampaignMetrics{} // no budget info at all
	pct, computable := metaPct(m, 10, now)
	if pct != 0 {
		t.Errorf("pct = %v, want 0 for a campaign with no budget information", pct)
	}
	if computable {
		t.Errorf("computable = true for a campaign with no budget; there is nothing to pace against")
	}
}

// TestMetaLabel_OverspendingBandNeverUsesThe130Member pins the same BFF-side oddity
// LinkedIn's port reproduces: Meta's label switch only ever produces "overspending" for
// pacingPct > constrained (100) — the shared CAMPAIGN_PACING_THRESHOLDS' 130 "overspending"
// member is never actually compared against. A pacingPct of 105 must already read as
// overspending, not merely constrained.
func TestMetaLabel_OverspendingBandNeverUsesThe130Member(t *testing.T) {
	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	rows := []model.AccountCampaignMetrics{
		{PlatformCampaignID: "1", Name: "c", BudgetDay: 100, Spend: 105}, // pct = 105
	}
	out, _ := evalMeta(rows, 1, now)
	if len(out) != 1 {
		t.Fatalf("got %d rows, want 1", len(out))
	}
	if out[0].PacingLabel != model.MonitorPacingOverspending {
		t.Errorf("label = %q, want overspending at 105%% pacing (never compares against the 130 threshold)", out[0].PacingLabel)
	}
}

// TestMetaActionItems exercises metaActionItems' independent rules.
func TestMetaActionItems(t *testing.T) {
	base := model.AccountCampaignMetrics{PlatformCampaignID: "c1", Name: "c"}

	t.Run("active with zero impressions and zero spend is HIGH", func(t *testing.T) {
		row := base
		row.Status = "ACTIVE"
		items := metaActionItems(row, 0, model.MonitorPacingNormal, 30)
		mustContainIssue(t, items, "no delivery", model.MonitorPriorityHigh)
	})
	t.Run("low CTR above min impressions is MED", func(t *testing.T) {
		row := base
		row.Ctr = 0.1
		row.Impressions = 501
		items := metaActionItems(row, 0, model.MonitorPacingNormal, 30)
		mustContainIssue(t, items, "Low CTR", model.MonitorPriorityMed)
	})
	t.Run("clicks above floor with 0 conversions is MED", func(t *testing.T) {
		row := base
		row.Clicks = 21
		row.Conversions = floatPtr(0)
		items := metaActionItems(row, 0, model.MonitorPacingNormal, 30)
		mustContainIssue(t, items, "0 conversions", model.MonitorPriorityMed)
	})
	t.Run("active underspending is MED", func(t *testing.T) {
		row := base
		row.Status = "ACTIVE"
		items := metaActionItems(row, 30, model.MonitorPacingUnderspending, 30)
		mustContainIssue(t, items, "Underspending", model.MonitorPriorityMed)
	})
	t.Run("active underspending funded by BudgetDay uses budgetDay*days as the denominator", func(t *testing.T) {
		// Pins a round-22 review fix: the message used to hardcode m.TotalBudget as the
		// denominator, which is 0 for a campaign funded by BudgetDay instead — producing a
		// self-contradicting "X.XX of 0.00" message.
		row := base
		row.Status = "ACTIVE"
		row.BudgetDay = 10
		row.Spend = 60
		// Through the evaluator, so the denominator is the one pacing itself used.
		_, items := evalMeta([]model.AccountCampaignMetrics{row}, 30, time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC))
		mustContainIssue(t, items, "Underspending", model.MonitorPriorityMed)
		found := false
		for _, it := range items {
			if strContains(it.Issue, "Underspending") {
				found = true
				if !strContains(it.Issue, "of 300.00 in account currency") {
					t.Errorf("issue = %q, want it to report 300.00 in account currency (budgetDay=10 * days=30), not 0.00", it.Issue)
				}
			}
		}
		if !found {
			t.Fatalf("no Underspending issue found in %+v", items)
		}
	})
	t.Run("money in action-item text is in account currency, never dollars", func(t *testing.T) {
		// Meta reports spend and budgets in the AD ACCOUNT's currency; a JPY or KRW account's
		// amounts used to be printed with "$" (PR #316 review).
		row := base
		row.Status = "ACTIVE"
		row.BudgetDay = 3000
		row.Spend = 9000
		row.Clicks = 100
		row.Conversions = floatPtr(0)
		items := metaActionItems(row, 10, model.MonitorPacingUnderspending, 90000)
		zero := base
		zero.Status = "ACTIVE"
		zero.Impressions, zero.Spend = 0, 0
		items = append(items, metaActionItems(zero, 0, model.MonitorPacingNormal, 30)...)
		if len(items) < 3 {
			t.Fatalf("want the conversions, underspending and no-delivery items, got %+v", items)
		}
		for _, it := range items {
			if strContains(it.Issue, "$") {
				t.Errorf("issue %q prints a dollar sign for an account-currency amount", it.Issue)
			}
		}
		mustContainIssue(t, items, "9000.00 in account currency spent", model.MonitorPriorityMed)
		mustContainIssue(t, items, "(9000.00 in account currency of 90000.00 in account currency)", model.MonitorPriorityMed)
	})
	t.Run("active constrained is MED", func(t *testing.T) {
		row := base
		row.Status = "ACTIVE"
		items := metaActionItems(row, 95, model.MonitorPacingConstrained, 30)
		mustContainIssue(t, items, "Budget constrained", model.MonitorPriorityMed)
	})
	t.Run("paused underspending does not fire (status-gated)", func(t *testing.T) {
		row := base
		row.Status = "PAUSED"
		items := metaActionItems(row, 30, model.MonitorPacingUnderspending, 30)
		for _, it := range items {
			if it.Issue == "" {
				continue
			}
			if it.Priority == model.MonitorPriorityMed && strContains(it.Issue, "Underspending") {
				t.Fatalf("underspending item fired for a PAUSED campaign, want status==ACTIVE gate: %+v", it)
			}
		}
	})
}

func strContains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// TestEvaluateMetaMonitor_SkipsFetchFailedRows mirrors
// TestEvaluateGoogleMonitor_SkipsFetchFailedRows: a FetchFailed row's zero-value metrics must
// not be run through pacing/action-item evaluation, but the row itself must still be returned.
func TestEvaluateMetaMonitor_SkipsFetchFailedRows(t *testing.T) {
	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	rows := []model.AccountCampaignMetrics{
		{PlatformCampaignID: "1", Name: "Failed Fetch", Status: "ACTIVE", TotalBudget: 500, FetchFailed: true},
	}
	out, items := evalMeta(rows, 30, now)
	if len(out) != 1 {
		t.Fatalf("got %d rows, want 1 — a FetchFailed row must still be returned: %+v", len(out), out)
	}
	if !out[0].Metrics.FetchFailed || !out[0].Metrics.PacingUnknown {
		t.Errorf("FetchFailed row = %+v, want FetchFailed=true and PacingUnknown=true — a failed fetch must not report a computed pacing verdict", out[0])
	}
	// PacingLabel keeps its zero-value "normal" placeholder rather than going unset — pacing_label
	// is a required enum with no "unknown" member; PacingUnknown=true is the signal not to trust it.
	if out[0].PacingLabel != model.MonitorPacingNormal {
		t.Errorf("FetchFailed row PacingLabel = %q, want the documented placeholder %q", out[0].PacingLabel, model.MonitorPacingNormal)
	}
	if len(items) != 0 {
		t.Errorf("FetchFailed row produced action items, want none: %+v", items)
	}
}

// TestMetaPacingPct_BothBudgetsIsUnknown: a row with a daily AND a lifetime budget has no single
// plan (Meta documents them as mutually exclusive). It used to be paced on the lifetime figure
// alone, or dropped to unknown only when that flight was unpaceable; it is now unknown either way,
// matching the ad-set path (PR #316 review).
func TestMetaPacingPct_BothBudgetsIsUnknown(t *testing.T) {
	now := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)
	paceable := model.AccountCampaignMetrics{BudgetDay: 100, TotalBudget: 1000, StartDate: "2026-07-21", EndDate: "2026-07-30", Spend: 700}
	if pct, ok := metaPct(paceable, 7, now); ok {
		t.Errorf("both budgets with a paceable flight paced at %v%%, want unknown", pct)
	}
	openEnded := model.AccountCampaignMetrics{BudgetDay: 100, TotalBudget: 1000, StartDate: "2026-07-21", Spend: 700}
	if pct, ok := metaPct(openEnded, 7, now); ok {
		t.Errorf("both budgets on an open-ended flight paced at %v%%, want unknown", pct)
	}
	rows, _ := evalMeta([]model.AccountCampaignMetrics{{PlatformCampaignID: "1", Name: "c", Status: "ACTIVE",
		Impressions: 1000, Clicks: 10, BudgetDay: 100, TotalBudget: 1000, StartDate: "2026-07-21", EndDate: "2026-07-30", Spend: 700}}, 7, now)
	if !rows[0].Metrics.PacingUnknown {
		t.Errorf("row = %+v, want PacingUnknown for a row with both budgets", rows[0])
	}
}
