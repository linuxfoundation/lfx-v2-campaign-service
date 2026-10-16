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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/platformtest"
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

func budgetClient(t *testing.T, apiURL string, b *ReportBudgets, opts ...Option) *Client {
	t.Helper()
	tok := httptest.NewServer(tokenHandlerReturning("tok"))
	t.Cleanup(tok.Close)
	return NewClient(testCreds, testAccount, append([]Option{WithBaseURL(apiURL + "/api/v3"), WithTokenURL(tok.URL),
		WithNowFunc(fixedRedditClock()), WithReportBudgets(b), withRetryBaseDelay(time.Millisecond)}, opts...)...)
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

// A pre-send dial failure (here: connection refused) is proven unsent, so its slot is handed back;
// an outage must not burn the account's reporting budget.
func TestReportBudgets_PreSendDialFailureReleasesTheSlot(t *testing.T) {
	refused := WithHTTPClient(&http.Client{Transport: platformtest.RefusedDialTransport(t)}) // every API dial is refused
	b := NewReportBudgets(time.Minute, 100, 100)
	// One report request (the metrics read), so no sibling attempt can be cancelled mid-send.
	_, err := budgetClient(t, platformtest.RefusedURL, b, refused).GetCampaignMetrics(context.Background(), "t3_a", model.MetricsWindowToday)
	if err == nil || !isPreSendDialError(err) {
		t.Fatalf("error = %v, want a pre-send dial failure", err)
	}
	if all, _ := b.used("t2_test"); all != 0 {
		t.Errorf("ledger = %d after a refused connection, want 0", all)
	}
}

// The keyword performance read sends through the same slot discipline as every report read: a
// cancelled context or a failed token refresh takes no slot and sends nothing, and a pre-send dial
// failure hands its slot back. One campaign is one KEYWORD report, so no sibling attempt is in
// flight to muddy the count.
func TestReportBudgets_KeywordReadSpendsOnlyOnSentAttempts(t *testing.T) {
	t.Run("cancelled context", func(t *testing.T) {
		var sent atomic.Int32
		srv := pagedThrottledServer(t, &sent)
		b := NewReportBudgets(time.Minute, 100, 100)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := budgetClient(t, srv.URL, b).GetKeywordPerformance(ctx, model.MetricsWindowToday, []string{"t3_a"}); err == nil {
			t.Fatal("GetKeywordPerformance on a cancelled context succeeded")
		}
		if all, ins := b.used("t2_test"); all != 0 || ins != 0 || sent.Load() != 0 {
			t.Errorf("ledger %d/%d, sent %d; want 0/0/0", all, ins, sent.Load())
		}
	})
	t.Run("token refresh fails", func(t *testing.T) {
		var sent atomic.Int32
		srv := pagedThrottledServer(t, &sent)
		tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
		}))
		t.Cleanup(tok.Close)
		b := NewReportBudgets(time.Minute, 100, 100)
		c := NewClient(testCreds, testAccount, WithBaseURL(srv.URL+"/api/v3"), WithTokenURL(tok.URL),
			WithNowFunc(fixedRedditClock()), WithReportBudgets(b), withRetryBaseDelay(time.Millisecond))
		if _, err := c.GetKeywordPerformance(context.Background(), model.MetricsWindowToday, []string{"t3_a"}); err == nil {
			t.Fatal("GetKeywordPerformance with a failing token refresh succeeded")
		}
		if all, ins := b.used("t2_test"); all != 0 || ins != 0 || sent.Load() != 0 {
			t.Errorf("ledger %d/%d, sent %d; want 0/0/0", all, ins, sent.Load())
		}
	})
	t.Run("pre-send dial failure", func(t *testing.T) {
		refused := WithHTTPClient(&http.Client{Transport: platformtest.RefusedDialTransport(t)}) // every API dial is refused
		b := NewReportBudgets(time.Minute, 100, 100)
		var reserved, reservedInsights atomic.Int32
		b.onReserve = func(_ string, insights bool) {
			reserved.Add(1)
			if insights {
				reservedInsights.Add(1)
			}
		}
		_, err := budgetClient(t, platformtest.RefusedURL, b, refused).GetKeywordPerformance(context.Background(), model.MetricsWindowToday, []string{"t3_a"})
		if err == nil || !isPreSendDialError(err) {
			t.Fatalf("error = %v, want a pre-send dial failure", err)
		}
		// A slot WAS reserved — on the insight sub-budget, right before the send — and handed
		// back: the ledger is empty only because of the release, not because none was taken.
		if reserved.Load() != 1 || reservedInsights.Load() != 1 {
			t.Fatalf("reserved %d (insights %d), want exactly 1 insight slot taken before the dial", reserved.Load(), reservedInsights.Load())
		}
		if all, ins := b.used("t2_test"); all != 0 || ins != 0 {
			t.Errorf("ledger = %d/%d after a refused connection, want 0/0 (the reserved slot released)", all, ins)
		}
	})
}

// release hands back exactly the slot it was issued for, once.
func TestReportBudgets_ReleaseIsExactAndIdempotent(t *testing.T) {
	b, _ := fakeLedger(5, 5)
	if err := b.acquire(context.Background(), "t2_a", true); err != nil {
		t.Fatal(err)
	}
	release, err := b.reserve(context.Background(), "t2_a", true)
	if err != nil {
		t.Fatal(err)
	}
	release()
	release()
	if all, ins := b.used("t2_a"); all != 1 || ins != 1 {
		t.Errorf("used = %d/%d, want 1/1 — only the released slot is returned, and only once", all, ins)
	}
}

// Inactive accounts' ledgers do not accumulate: once the window has passed, the next acquire's
// sweep drops every expired ledger, and releasing an account's only slot drops its ledger too, so
// the map returns to empty instead of growing with every ad account ever seen.
func TestReportBudgets_InactiveLedgersAreDropped(t *testing.T) {
	b, clk := fakeLedger(5, 5)
	for _, id := range []string{"t2_a", "t2_b", "t2_c"} {
		if err := b.acquire(context.Background(), id, true); err != nil {
			t.Fatal(err)
		}
	}
	if n := b.accounts(); n != 3 {
		t.Fatalf("accounts = %d, want 3", n)
	}
	clk.mu.Lock()
	clk.t = clk.t.Add(time.Minute + time.Second) // past the window: every slot has expired
	clk.mu.Unlock()
	release, err := b.reserve(context.Background(), "t2_new", false)
	if err != nil {
		t.Fatal(err)
	}
	if n := b.accounts(); n != 1 {
		t.Errorf("accounts after the window = %d, want 1 (only t2_new): inactive ledgers must be dropped", n)
	}
	release()
	if n := b.accounts(); n != 0 {
		t.Errorf("accounts after releasing the only slot = %d, want 0", n)
	}
}
