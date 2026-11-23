// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestListAccountCampaignsIn_ReportsOnlyTheScopedCampaigns pins #659: on a shared account the
// read sends a report only for the caller's campaigns. Before, every ACTIVE/PAUSED campaign on
// the account got one — five here, for a caller that keeps two — and on the LF system account
// that spent the reporting budget on other foundations' campaigns.
func TestListAccountCampaignsIn_ReportsOnlyTheScopedCampaigns(t *testing.T) {
	stub := &monitorStub{t: t, listBody: `{"data":` + manyActiveCampaignsBody(5) + `}`, report: healthyReport}
	c := newMonitorClient(t, stub, fixedRedditClock())

	rows, _, err := c.ListAccountCampaignsIn(context.Background(), testAccount.AccountID, 7, false,
		map[string]bool{"camp1": true, "camp3": true, "not-on-the-account": true})
	if err != nil {
		t.Fatalf("ListAccountCampaignsIn: %v", err)
	}
	if len(rows) != 2 || rows[0].CampaignID != "camp1" || rows[1].CampaignID != "camp3" {
		t.Fatalf("rows = %+v, want camp1 and camp3 only", rows)
	}
	reports := stub.reportRequests()
	got := make([]string, 0, len(reports))
	for _, r := range reports {
		got = append(got, campaignIDFromFilter(r.Data.Filter))
	}
	if len(got) != 2 {
		t.Fatalf("reports sent for %v, want exactly camp1 and camp3", got)
	}

	t.Run("empty scope sends no report", func(t *testing.T) {
		stub := &monitorStub{t: t, listBody: `{"data":` + manyActiveCampaignsBody(3) + `}`, report: healthyReport}
		c := newMonitorClient(t, stub, fixedRedditClock())
		rows, _, err := c.ListAccountCampaignsIn(context.Background(), testAccount.AccountID, 7, false, map[string]bool{})
		if err != nil || len(rows) != 0 {
			t.Fatalf("rows = %+v, err = %v; want none", rows, err)
		}
		if n := len(stub.reportRequests()); n != 0 {
			t.Fatalf("%d reports sent, want 0", n)
		}
	})
	t.Run("nil scope reads every campaign", func(t *testing.T) {
		stub := &monitorStub{t: t, listBody: `{"data":` + manyActiveCampaignsBody(3) + `}`, report: healthyReport}
		c := newMonitorClient(t, stub, fixedRedditClock())
		rows, _, err := c.ListAccountCampaignsIn(context.Background(), testAccount.AccountID, 7, false, nil)
		if err != nil || len(rows) != 3 || len(stub.reportRequests()) != 3 {
			t.Fatalf("rows = %d, reports = %d, err = %v; want 3 and 3", len(rows), len(stub.reportRequests()), err)
		}
	})
}

// TestReport429_CarriesOnlyNumericRateLimitHeaders: the 429 that ends the bounded retry keeps
// Reddit's rate-limit headers for the monitor's log line, numeric values only, and never the body.
func TestReport429_CarriesOnlyNumericRateLimitHeaders(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == monitorCampaignsPath {
			_, _ = w.Write([]byte(`{"data":` + manyActiveCampaignsBody(1) + `}`))
			return
		}
		w.Header().Set("X-Ratelimit-Remaining", "0")
		w.Header().Set("X-Ratelimit-Reset", "42")
		w.Header().Set("X-Ratelimit-Policy", "ads-reporting; secret=abc")
		w.Header().Set("X-Other", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"token=SECRET"}`))
	})
	apiSrv := httptest.NewServer(handler)
	t.Cleanup(apiSrv.Close)
	tokenSrv := httptest.NewServer(tokenHandlerReturning("tok"))
	t.Cleanup(tokenSrv.Close)
	c := NewClient(testCreds, testAccount, WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL),
		WithNowFunc(fixedRedditClock()), withRetryBaseDelay(tinyBackoff))

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	rows, err := c.ListAccountCampaigns(context.Background(), testAccount.AccountID, 7)
	if err != nil || len(rows) != 1 || !rows[0].FetchFailed {
		t.Fatalf("rows = %+v, err = %v; want one FetchFailed row", rows, err)
	}
	out := buf.String()
	for _, want := range []string{"campaign reports were throttled", "reddit_429=1", "budget_refused=0", "X-Ratelimit-Remaining:0", "X-Ratelimit-Reset:42"} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q does not contain %q", out, want)
		}
	}
	for _, never := range []string{"SECRET", "secret=abc", "X-Other", "X-Ratelimit-Policy"} {
		if strings.Contains(out, never) {
			t.Errorf("log %q contains %q; only numeric rate-limit headers may be logged", out, never)
		}
	}
}

// TestMonitorThrottleTally_SplitsRedditFromTheLedger: both causes class as throttle on the row,
// but the read's log line counts them apart (#659).
func TestMonitorThrottleTally_SplitsRedditFromTheLedger(t *testing.T) {
	var tally monitorThrottleTally
	tally.add(fmt.Errorf("reddit API POST /x: %w", ErrReportBudgetExhausted))
	tally.add(&apiError{StatusCode: http.StatusTooManyRequests, RateLimit: map[string]string{"Retry-After": "3"}})
	tally.add(&apiError{StatusCode: http.StatusTooManyRequests, RateLimit: map[string]string{"Retry-After": "9"}})
	if tally.reddit429 != 2 || tally.budgetRefused != 1 {
		t.Fatalf("reddit429 = %d, budgetRefused = %d; want 2 and 1", tally.reddit429, tally.budgetRefused)
	}
	if tally.headers["Retry-After"] != "3" {
		t.Errorf("headers = %v, want the first 429's", tally.headers)
	}
	if monitorFailureClass(fmt.Errorf("w: %w", ErrReportBudgetExhausted)) != monitorFailureClass(&apiError{StatusCode: http.StatusTooManyRequests}) {
		t.Fatal("precondition: both causes are expected to share the throttle class")
	}
}
