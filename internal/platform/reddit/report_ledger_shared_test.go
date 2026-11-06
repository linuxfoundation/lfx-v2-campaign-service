// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/platformtest"
)

// memSharedLedger is an in-memory domain.RedditReportLedger standing in for Postgres: ONE ledger
// several ReportBudgets (several "processes") record their slots in. It never expires a slot, so a
// count over the test's lifetime is a count inside the window. err, when set, fails every call.
type memSharedLedger struct {
	mu       sync.Mutex
	next     int64
	slots    map[int64]bool // id → insights
	limits   []domain.RedditReportLimits
	released []int64
	err      error
}

func newMemSharedLedger() *memSharedLedger { return &memSharedLedger{slots: map[int64]bool{}} }

func (m *memSharedLedger) Take(_ context.Context, _ string, insights bool, lim domain.RedditReportLimits) (domain.RedditReportTake, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return domain.RedditReportTake{}, m.err
	}
	m.limits = append(m.limits, lim)
	all, ins := 0, 0
	for _, i := range m.slots {
		all++
		if i {
			ins++
		}
	}
	if all >= lim.Total || (insights && ins >= lim.Insights) {
		return domain.RedditReportTake{Wait: lim.Window}, nil
	}
	m.next++
	m.slots[m.next] = insights
	return domain.RedditReportTake{SlotID: m.next, Taken: true}, nil
}

func (m *memSharedLedger) Release(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.released = append(m.released, id)
	delete(m.slots, id)
	return nil
}

func (m *memSharedLedger) count() int { m.mu.Lock(); defer m.mu.Unlock(); return len(m.slots) }

func countingReportServer(t *testing.T, sent *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sent.Add(1)
		_, _ = w.Write([]byte(echoReportRow("t3_a")))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A client given no ledger, and a shared budget nothing is bound to, both refuse every report
// BEFORE sending: fail closed, never unbudgeted.
func TestSharedReportBudgets_UnboundFailsClosed(t *testing.T) {
	var sent atomic.Int32
	srv := countingReportServer(t, &sent)
	_, err := budgetClient(t, srv.URL, NewSharedReportBudgets()).GetCampaignMetrics(context.Background(), "t3_a", model.MetricsWindowToday)
	if !errors.Is(err, ErrReportBudgetUnavailable) {
		t.Fatalf("error = %v, want ErrReportBudgetUnavailable", err)
	}
	if errors.Is(err, ErrReportBudgetExhausted) {
		t.Errorf("an unbound ledger is not an exhausted budget: %v", err)
	}
	// defaultReportBudgets is what a client with no WithReportBudgets spends from. This test
	// binary replaces it (report_budget_main_test.go), so check what production builds.
	if d := NewSharedReportBudgets(); !d.shared || d.ledger.Load() != nil {
		t.Errorf("NewSharedReportBudgets: shared=%v bound=%v, want an unbound shared ledger", d.shared, d.ledger.Load() != nil)
	}
	if n := sent.Load(); n != 0 {
		t.Errorf("sent %d requests with no ledger bound", n)
	}
}

// A ledger that cannot be asked (Postgres down) refuses the attempt: nothing is sent.
func TestSharedReportBudgets_LedgerErrorFailsClosed(t *testing.T) {
	var sent atomic.Int32
	srv := countingReportServer(t, &sent)
	l := newMemSharedLedger()
	l.err = errors.New("connection refused")
	b := NewSharedReportBudgets()
	b.BindLedger(l)
	_, err := budgetClient(t, srv.URL, b).GetCampaignMetrics(context.Background(), "t3_a", model.MetricsWindowToday)
	if !errors.Is(err, ErrReportBudgetUnavailable) {
		t.Fatalf("error = %v, want ErrReportBudgetUnavailable", err)
	}
	if n := sent.Load(); n != 0 {
		t.Errorf("sent %d requests while the ledger was failing", n)
	}
}

// The shared budget asks the ledger with the production sizing (45 of 60 per minute, 24 for
// insights) and marks an insight read's attempts.
func TestSharedReportBudgets_ProductionLimits(t *testing.T) {
	l := newMemSharedLedger()
	b := NewSharedReportBudgets()
	b.BindLedger(l)
	if err := b.acquire(context.Background(), "t2_a", true); err != nil {
		t.Fatal(err)
	}
	want := domain.RedditReportLimits{Window: time.Minute, Total: 45, Insights: 24}
	if len(l.limits) != 1 || l.limits[0] != want {
		t.Errorf("limits = %+v, want [%+v]", l.limits, want)
	}
	if !l.slots[1] {
		t.Errorf("the insight attempt was not recorded as one: %+v", l.slots)
	}
}

// TWO PROCESSES, ONE BUDGET. Two shared budgets — two replicas — bound to one ledger, each with
// its own client, run concurrent report reads on one account: together they send at most the
// account's 45, and every read that found no slot before its deadline failed with
// ErrReportBudgetExhausted. With per-process ledgers they would have sent 90.
func TestSharedReportBudgets_TwoProcessesShareOneBudget(t *testing.T) {
	var sent atomic.Int32
	srv := countingReportServer(t, &sent)
	l := newMemSharedLedger()
	var clients []*Client
	for range 2 {
		b := NewSharedReportBudgets()
		b.BindLedger(l)
		clients = append(clients, budgetClient(t, srv.URL, b))
	}
	const perProcess = 60
	var wg sync.WaitGroup
	var ok, exhausted, other atomic.Int32
	for _, c := range clients {
		for range perProcess {
			wg.Go(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_, err := c.GetCampaignMetrics(ctx, "t3_a", model.MetricsWindowToday)
				switch {
				case err == nil:
					ok.Add(1)
				case errors.Is(err, ErrReportBudgetExhausted):
					exhausted.Add(1)
				default:
					other.Add(1)
					t.Errorf("unexpected error: %v", err)
				}
			})
		}
	}
	wg.Wait()
	if n := sent.Load(); n != reportBudgetTotal {
		t.Errorf("two processes sent %d report requests, want exactly the shared %d", n, reportBudgetTotal)
	}
	if ok.Load() != reportBudgetTotal || exhausted.Load() != 2*perProcess-reportBudgetTotal {
		t.Errorf("ok %d, exhausted %d; want %d and %d", ok.Load(), exhausted.Load(), reportBudgetTotal, 2*perProcess-reportBudgetTotal)
	}
}

// A pre-send dial failure hands its SHARED slot back to the ledger — the contract the move to
// Postgres must keep — and an attempt that was sent does not.
func TestSharedReportBudgets_PreSendDialFailureReleasesTheSharedSlot(t *testing.T) {
	refused := WithHTTPClient(&http.Client{Transport: platformtest.RefusedDialTransport(t)})
	l := newMemSharedLedger()
	b := NewSharedReportBudgets()
	b.BindLedger(l)
	var reserved atomic.Int32
	b.onReserve = func(string, bool) { reserved.Add(1) }
	_, err := budgetClient(t, platformtest.RefusedURL, b, refused).GetCampaignMetrics(context.Background(), "t3_a", model.MetricsWindowToday)
	if err == nil || !isPreSendDialError(err) {
		t.Fatalf("error = %v, want a pre-send dial failure", err)
	}
	if reserved.Load() != 1 || len(l.released) != 1 || l.count() != 0 {
		t.Errorf("reserved %d, released %v, held %d; want 1 reserved, slot 1 released, 0 held", reserved.Load(), l.released, l.count())
	}

	var sent atomic.Int32
	srv := countingReportServer(t, &sent)
	if _, err := budgetClient(t, srv.URL, b).GetCampaignMetrics(context.Background(), "t3_a", model.MetricsWindowToday); err != nil {
		t.Fatal(err)
	}
	if l.count() != 1 {
		t.Errorf("a sent attempt's slot was handed back: held %d, want 1", l.count())
	}
}

// A full shared budget whose earliest slot frees after the deadline fails at once with
// ErrReportBudgetExhausted — the 503 — and sends nothing.
func TestSharedReportBudgets_FullBudgetFailsWholeBeforeTheDeadline(t *testing.T) {
	var sent atomic.Int32
	srv := countingReportServer(t, &sent)
	l := newMemSharedLedger()
	b := NewSharedReportBudgets()
	b.BindLedger(l)
	for range reportBudgetTotal {
		if err := b.acquire(context.Background(), "t2_test", false); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	_, err := budgetClient(t, srv.URL, b).GetCampaignMetrics(ctx, "t3_a", model.MetricsWindowToday)
	if !errors.Is(err, ErrReportBudgetExhausted) {
		t.Fatalf("error = %v, want ErrReportBudgetExhausted", err)
	}
	if waited := time.Since(start); waited > 500*time.Millisecond {
		t.Errorf("waited %s for a slot that frees after the deadline", waited)
	}
	if n := sent.Load(); n != 0 {
		t.Errorf("sent %d requests on a full budget", n)
	}
}

// blockingSharedLedger's Take waits for its context to end, as a Take stuck behind a slow
// database does, then fails with the context's error.
type blockingSharedLedger struct{ memSharedLedger }

func (*blockingSharedLedger) Take(ctx context.Context, _ string, _ bool, _ domain.RedditReportLimits) (domain.RedditReportTake, error) {
	<-ctx.Done()
	return domain.RedditReportTake{}, ctx.Err()
}

// A Take the caller's own deadline cut short is the budget not answering in time —
// ErrReportBudgetExhausted, as for a wait past the deadline — not the ledger being down.
func TestSharedReportBudgets_DeadlineDuringTakeIsExhausted(t *testing.T) {
	b := NewSharedReportBudgets()
	b.BindLedger(&blockingSharedLedger{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := b.acquire(ctx, "t2_a", false)
	if !errors.Is(err, ErrReportBudgetExhausted) || errors.Is(err, ErrReportBudgetUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquire = %v, want ErrReportBudgetExhausted wrapping the deadline, not Unavailable", err)
	}
}

// A shared-ledger wait is never shortened, is floored, and is spread by at most a tenth (capped).
func TestJitterReportWait(t *testing.T) {
	spread := false
	for range 200 {
		for _, d := range []time.Duration{reportLedgerMinWait, time.Second, time.Minute} {
			got := jitterReportWait(d)
			if got < d || got > d+min(d/10, reportLedgerMaxJitter) {
				t.Fatalf("jitterReportWait(%s) = %s, want within [d, d+min(d/10, %s)]", d, got, reportLedgerMaxJitter)
			}
			if got != d {
				spread = true
			}
		}
	}
	if !spread {
		t.Error("jitterReportWait never added any jitter")
	}
}
