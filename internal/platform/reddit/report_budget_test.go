// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// fakeLedgerClock drives a ledger's clock; its sleep advances the clock instead of waiting.
type fakeLedgerClock struct {
	mu     sync.Mutex
	t      time.Time
	sleeps int
}

func (c *fakeLedgerClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeLedgerClock) sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.sleeps++
	c.mu.Unlock()
	return nil
}

func fakeLedger(total, insights int) (*ReportBudgets, *fakeLedgerClock) {
	clk := &fakeLedgerClock{t: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}
	b := newReportBudgets(time.Minute, total, insights, clk.now)
	b.sleep = clk.sleep
	return b, clk
}

func TestReportBudgets_TotalAndInsightLimits(t *testing.T) {
	b, clk := fakeLedger(5, 3)
	// The ledger's clock starts at the real now, so the deadline is 30s ahead on both clocks: the
	// context is live (acquire refuses a done one) and a slot freeing in 60s misses it.
	clk.t = time.Now()
	short, cancelShort := context.WithDeadline(context.Background(), clk.t.Add(30*time.Second))
	defer cancelShort()
	for i := 0; i < 3; i++ {
		if err := b.acquire(short, "t2_a", true); err != nil {
			t.Fatalf("insight attempt %d: %v", i, err)
		}
	}
	// The insight sub-budget is spent; the slot would free only after the deadline.
	if err := b.acquire(short, "t2_a", true); !errors.Is(err, ErrReportBudgetExhausted) {
		t.Fatalf("4th insight attempt = %v, want ErrReportBudgetExhausted", err)
	}
	// Other report reads still have the rest of the total.
	for i := 0; i < 2; i++ {
		if err := b.acquire(short, "t2_a", false); err != nil {
			t.Fatalf("metrics attempt %d: %v", i, err)
		}
	}
	if err := b.acquire(short, "t2_a", false); !errors.Is(err, ErrReportBudgetExhausted) {
		t.Fatalf("6th attempt = %v, want ErrReportBudgetExhausted", err)
	}
	if all, ins := b.used("t2_a"); all != 5 || ins != 3 {
		t.Errorf("used = %d/%d, want 5/3 — a refused attempt must not be counted", all, ins)
	}
	// Another account has its own ledger.
	if err := b.acquire(short, "t2_b", true); err != nil {
		t.Errorf("other account: %v", err)
	}
}

// Without a deadline that forbids it, an attempt waits for the oldest slot to expire.
func TestReportBudgets_WaitsWhenTheSlotFreesInTime(t *testing.T) {
	b, clk := fakeLedger(1, 1)
	if err := b.acquire(context.Background(), "t2_a", true); err != nil {
		t.Fatal(err)
	}
	if err := b.acquire(context.Background(), "t2_a", true); err != nil {
		t.Fatalf("second attempt: %v", err)
	}
	if clk.sleeps != 1 || clk.now().Sub(time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)) != time.Minute {
		t.Errorf("sleeps = %d, clock moved %v; want one wait of the window", clk.sleeps, clk.now())
	}
}

// pagedThrottledServer answers the reports operation: the GENDER report is throttled once (429) and
// then spans two pages; every other report is one empty page.
func pagedThrottledServer(t *testing.T, sent *atomic.Int32) *httptest.Server {
	t.Helper()
	var genderCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent.Add(1)
		var req reportRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		if len(req.Data.Breakdowns) == 1 && req.Data.Breakdowns[0] == "GENDER" {
			switch genderCalls.Add(1) {
			case 1:
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{}`))
			case 2:
				_, _ = w.Write([]byte(`{"data":{"metrics":[]},"pagination":{"next_url":"/api/v3/ad_accounts/t2_test/reports?page.token=2"}}`))
			default:
				_, _ = w.Write([]byte(`{"data":{"metrics":[]},"pagination":{}}`))
			}
			return
		}
		_, _ = w.Write([]byte(`{"data":{"metrics":[]},"pagination":{}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func budgetClient(t *testing.T, apiURL string, b *ReportBudgets) *Client {
	t.Helper()
	tok := httptest.NewServer(tokenHandlerReturning("tok"))
	t.Cleanup(tok.Close)
	return NewClient(testCreds, testAccount, WithBaseURL(apiURL+"/api/v3"), WithTokenURL(tok.URL),
		WithNowFunc(fixedRedditClock()), WithReportBudgets(b), withRetryBaseDelay(time.Millisecond))
}

// Every attempt is counted: each page and each 429 retry, not only each report's first page.
func TestReportBudgets_CountsEveryPageAndRetry(t *testing.T) {
	var sent atomic.Int32
	srv := pagedThrottledServer(t, &sent)
	b := NewReportBudgets(time.Minute, 100, 100)
	if _, err := budgetClient(t, srv.URL, b).GetAudienceInsights(context.Background(), model.MetricsWindowToday, []string{"t3_a"}); err != nil {
		t.Fatalf("GetAudienceInsights: %v", err)
	}
	// GENDER: 429 + retry page 1 + page 2 = 3; COUNTRY and COMMUNITY: 1 each.
	all, ins := b.used("t2_test")
	if all != 5 || ins != 5 || sent.Load() != 5 {
		t.Errorf("ledger %d/%d, sent %d; want 5/5/5", all, ins, sent.Load())
	}
}

// The campaign metrics read spends from the same ledger — outside the insight sub-budget — so the
// budget accounts for every report caller, not only the audience read.
func TestReportBudgets_MetricsReadSpendsFromTheSameLedger(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(echoReportRow("t3_a")))
	}))
	t.Cleanup(srv.Close)
	b := NewReportBudgets(time.Minute, 100, 100)
	if _, err := budgetClient(t, srv.URL, b).GetCampaignMetrics(context.Background(), "t3_a", model.MetricsWindowToday); err != nil {
		t.Fatalf("GetCampaignMetrics: %v", err)
	}
	if all, ins := b.used("t2_test"); all != 1 || ins != 0 {
		t.Errorf("ledger = %d/%d, want 1/0", all, ins)
	}
}

// Concurrent reads by DIFFERENT clients on one account share the ledger: together they never send
// more than the insight budget inside the window, and the ones that cannot fit before their
// deadline fail with ErrReportBudgetExhausted — nothing partial.
func TestReportBudgets_ConcurrentReadsShareTheBudgetAndFailWhole(t *testing.T) {
	var sent atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sent.Add(1)
		_, _ = w.Write([]byte(`{"data":{"metrics":[]},"pagination":{}}`))
	}))
	t.Cleanup(srv.Close)
	b := NewReportBudgets(time.Minute, 100, 7) // two full reads need 6 each
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			ai, err := budgetClient(t, srv.URL, b).GetAudienceInsights(ctx, model.MetricsWindowToday, []string{"t3_a", "t3_b"})
			if err != nil && ai != nil {
				t.Errorf("a failed read returned %+v", ai)
			}
			errs[i] = err
		}()
	}
	wg.Wait()
	if n := sent.Load(); n > 7 {
		t.Fatalf("sent %d requests, budget is 7", n)
	}
	succeeded, exhausted := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrReportBudgetExhausted):
			exhausted++
		default:
			t.Errorf("unexpected error %v", err)
		}
	}
	if succeeded > 1 || exhausted < 2 {
		t.Errorf("succeeded %d, exhausted %d; at most one 6-request read fits a budget of 7", succeeded, exhausted)
	}
}

// An exhausted budget refuses BEFORE sending: no request goes out without a slot.
func TestReportBudgets_ExhaustedSendsNothing(t *testing.T) {
	var sent atomic.Int32
	srv := pagedThrottledServer(t, &sent)
	b := NewReportBudgets(time.Minute, 100, 0)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := budgetClient(t, srv.URL, b).GetAudienceInsights(ctx, model.MetricsWindowToday, []string{"t3_a"})
	if !errors.Is(err, ErrReportBudgetExhausted) {
		t.Fatalf("error = %v, want ErrReportBudgetExhausted", err)
	}
	if n := sent.Load(); n != 0 {
		t.Errorf("sent %d requests with no budget", n)
	}
}

// Only the reporting operation is budgeted.
func TestReportBudgets_OnlyTheReportsPath(t *testing.T) {
	for path, want := range map[string]bool{
		"/ad_accounts/t2_x/reports":   true,
		"/ad_accounts/t2_x/campaigns": false,
		"/ad_accounts/t2_x/reports/1": false,
		"/me":                         false,
	} {
		if got := reportPathRE.MatchString(path); got != want {
			t.Errorf("%s: budgeted = %v, want %v", path, got, want)
		}
	}
}

// An attempt that is never sent spends no slot: a done context — before the first take, or
// after a wait — and a failed token refresh both leave the ledger unchanged, so the quota stays
// with the account's other readers instead of being burnt by attempts that never left.
func TestReportBudgets_UnsentAttemptsSpendNothing(t *testing.T) {
	t.Run("ledger: cancelled before the take", func(t *testing.T) {
		b, _ := fakeLedger(5, 5)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := b.acquire(ctx, "t2_a", true); !errors.Is(err, context.Canceled) || !errors.Is(err, ErrReportBudgetExhausted) {
			t.Fatalf("acquire = %v, want ErrReportBudgetExhausted wrapping context.Canceled", err)
		}
		if all, ins := b.used("t2_a"); all != 0 || ins != 0 {
			t.Errorf("used = %d/%d, want 0/0", all, ins)
		}
	})
	t.Run("ledger: cancelled during the wait", func(t *testing.T) {
		b, clk := fakeLedger(1, 1)
		if err := b.acquire(context.Background(), "t2_a", true); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		// The wait returns WITHOUT an error (as a real timer firing in the same instant as a
		// cancel can), so only the re-check after it stops the take.
		b.sleep = func(c context.Context, d time.Duration) error {
			cancel()
			return clk.sleep(c, d)
		}
		if err := b.acquire(ctx, "t2_a", true); !errors.Is(err, context.Canceled) {
			t.Fatalf("acquire = %v, want context.Canceled", err)
		}
		// The first slot has expired; the cancelled attempt took no new one.
		if all, ins := b.used("t2_a"); all != 0 || ins != 0 {
			t.Errorf("used = %d/%d, want 0/0", all, ins)
		}
	})
	t.Run("client: cancelled context", func(t *testing.T) {
		var sent atomic.Int32
		srv := pagedThrottledServer(t, &sent)
		b := NewReportBudgets(time.Minute, 100, 100)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := budgetClient(t, srv.URL, b).GetAudienceInsights(ctx, model.MetricsWindowToday, []string{"t3_a"}); err == nil {
			t.Fatal("GetAudienceInsights on a cancelled context succeeded")
		}
		if all, ins := b.used("t2_test"); all != 0 || ins != 0 || sent.Load() != 0 {
			t.Errorf("ledger %d/%d, sent %d; want 0/0/0", all, ins, sent.Load())
		}
	})
	t.Run("client: token refresh fails", func(t *testing.T) {
		var sent atomic.Int32
		srv := pagedThrottledServer(t, &sent)
		tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
		}))
		t.Cleanup(tok.Close)
		b := NewReportBudgets(time.Minute, 100, 100)
		c := NewClient(testCreds, testAccount, WithBaseURL(srv.URL+"/api/v3"), WithTokenURL(tok.URL),
			WithNowFunc(fixedRedditClock()), WithReportBudgets(b), withRetryBaseDelay(time.Millisecond))
		if _, err := c.GetAudienceInsights(context.Background(), model.MetricsWindowToday, []string{"t3_a"}); err == nil {
			t.Fatal("GetAudienceInsights with a failing token refresh succeeded")
		}
		if all, ins := b.used("t2_test"); all != 0 || ins != 0 || sent.Load() != 0 {
			t.Errorf("ledger %d/%d, sent %d; want 0/0/0", all, ins, sent.Load())
		}
	})
}
