// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestListAccountCampaigns_UsesInjectedClockNotWallClock pins a real defect found in round-14
// review: ListAccountCampaigns' [start, end] date window used to be computed by its caller
// (internal/dispatch/googleads.go) from the bare wall clock, unlike the equivalent Meta/LinkedIn
// windows, which already read the client's injected clock (see the sibling test of this name in
// internal/platform/meta and internal/platform/linkedin). That made Google's monitor window the
// only one of the four platforms that could not be pinned deterministically in a test. The
// window now comes from c.now, set here to a fixed value nowhere near the actual wall clock, so
// a test that failed to route through c.now would render a date range far outside what this
// test asserts.
func TestListAccountCampaigns_UsesInjectedClockNotWallClock(t *testing.T) {
	var mu sync.Mutex
	var gotBody string
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	t.Cleanup(apiSrv.Close)

	fixedNow := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	c := NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(func() time.Time { return fixedNow }))

	if _, err := c.ListAccountCampaigns(context.Background(), "1234567890", 7); err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}

	mu.Lock()
	body := gotBody
	mu.Unlock()
	// days=7 ending 2026-09-17 (inclusive) starts 2026-09-11 — the same days-1 convention the
	// LinkedIn/Meta/Reddit monitor dispatchers share. A wall-clock-derived window would not
	// contain this literal range regardless of when the test happens to run.
	if !strings.Contains(body, "2026-09-11") || !strings.Contains(body, "2026-09-17") {
		t.Errorf("query body = %s, want it to contain the injected-clock window 2026-09-11..2026-09-17", body)
	}
}

// TestListAccountCampaigns_MalformedMetrics_MarksFetchFailed pins a round-19 review fix: a
// campaign row whose GAQL metrics fields fail to parse used to be silently skipped — no
// FetchFailed marker was ever set anywhere in this package — so the rule engine (which already
// defensively checks FetchFailed) could never actually reach that branch and instead read the
// row's zero-valued accumulator as a genuine "no delivery" measurement.
func TestListAccountCampaigns_MalformedMetrics_MarksFetchFailed(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{
			"campaign":{"id":"111","name":"Campaign One","status":"ENABLED","advertisingChannelType":"SEARCH"},
			"campaignBudget":{"amountMicros":"5000000"},
			"metrics":{"impressions":"not-a-number","clicks":"50","costMicros":"12500000"}
		}]}`)
	}))
	t.Cleanup(apiSrv.Close)

	c := NewClient(testCreds(), testAccount(), WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL))

	rows, err := c.ListAccountCampaigns(context.Background(), "1234567890", 7)
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
	}
	if !rows[0].FetchFailed {
		t.Errorf("row for campaign 111 = %+v, want FetchFailed=true for a campaign whose metrics failed to parse", rows[0])
	}
	if rows[0].Impressions != 0 || rows[0].Clicks != 0 || rows[0].SpendUSD != 0 {
		t.Errorf("row for campaign 111 = %+v, want zero metrics (never fabricated) alongside FetchFailed", rows[0])
	}
}

// TestListAccountCampaigns_MalformedBudget_MarksFetchFailed pins a round-24 review fix: a
// present but unparseable campaign_budget.amount_micros used to silently
// become a real $0 daily budget via microsToUSD, which returned 0 on any parse error with no
// signal to the caller. The rule engine could then read that fabricated $0 as a real budget-less
// campaign instead of unknown upstream data. Contrast with an empty amount_micros (no budget set
// at all) and the -1 sentinel, both legitimate zero budgets — see
// TestMicrosToUSD_EmptyAndSentinelAreNotFailures.
func TestListAccountCampaigns_MalformedBudget_MarksFetchFailed(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{
			"campaign":{"id":"222","name":"Campaign Two","status":"ENABLED","advertisingChannelType":"SEARCH"},
			"campaignBudget":{"amountMicros":"not-a-number"},
			"metrics":{"impressions":"100","clicks":"5","costMicros":"2500000"}
		}]}`)
	}))
	t.Cleanup(apiSrv.Close)

	c := NewClient(testCreds(), testAccount(), WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL))

	rows, err := c.ListAccountCampaigns(context.Background(), "1234567890", 7)
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
	}
	if !rows[0].FetchFailed {
		t.Errorf("row for campaign 222 = %+v, want FetchFailed=true for a campaign whose budget failed to parse", rows[0])
	}
	if rows[0].BudgetDailyUSD != 0 {
		t.Errorf("row for campaign 222 = %+v, want BudgetDailyUSD=0 (never fabricated) alongside FetchFailed", rows[0])
	}
}

// TestMicrosToUSD_EmptyAndSentinelAreNotFailures pins the legitimate-zero-budget cases that
// must NOT set FetchFailed, so a future edit can't collapse them into the malformed-input case
// above.
func TestMicrosToUSD_EmptyAndSentinelAreNotFailures(t *testing.T) {
	for _, s := range []string{"", "-1"} {
		usd, ok := microsToUSD(s)
		if !ok {
			t.Errorf("microsToUSD(%q) ok = false, want true (a legitimate zero budget, not malformed data)", s)
		}
		if usd != 0 {
			t.Errorf("microsToUSD(%q) = %v, want 0", s, usd)
		}
	}
	if usd, ok := microsToUSD("garbage"); ok || usd != 0 {
		t.Errorf("microsToUSD(%q) = (%v, %v), want (0, false)", "garbage", usd, ok)
	}
	// Only Google's exact -1 sentinel is a legitimate zero; any other negative value is
	// malformed data, not a variant of "no budget set" (round-24 review).
	if usd, ok := microsToUSD("-5"); ok || usd != 0 {
		t.Errorf("microsToUSD(%q) = (%v, %v), want (0, false)", "-5", usd, ok)
	}
}
