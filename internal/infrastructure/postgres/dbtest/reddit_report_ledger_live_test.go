// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
)

var productionReportLimits = domain.RedditReportLimits{Window: time.Minute, Total: 45, Insights: 24}

// processPool opens a SEPARATE pool on the test database — another replica's connections, not
// the shared harness pool's — closed at cleanup.
func processPool(t *testing.T) *postgres.Pool {
	t.Helper()
	dbtest.Pool(t) // skips without a database; migrates once
	p, err := pgxpool.New(context.Background(), dbtest.DSN())
	if err != nil {
		t.Fatalf("open a second pool: %s", dbtest.SafeDSNErr(err))
	}
	t.Cleanup(p.Close)
	return &postgres.Pool{Pool: p}
}

func heldSlots(t *testing.T, pool *pgxpool.Pool, account string) (all, insights int) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`SELECT count(*), count(*) FILTER (WHERE insights) FROM reddit_report_slots WHERE ad_account_id = $1`, account).Scan(&all, &insights)
	if err != nil {
		t.Fatalf("count slots: %v", err)
	}
	return all, insights
}

// takeConcurrently runs n Takes per ledger at once, released together, and counts the grants.
func takeConcurrently(t *testing.T, ledgers []*postgres.RedditReportLedger, n int, account string, insights bool) (taken int32, waits []time.Duration) {
	t.Helper()
	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
		got   atomic.Int32
		mu    sync.Mutex
	)
	for _, l := range ledgers {
		for range n {
			wg.Go(func() {
				<-start
				res, err := l.Take(context.Background(), account, insights, productionReportLimits)
				if err != nil {
					t.Errorf("Take: %v", err)
					return
				}
				if res.Taken {
					got.Add(1)
					return
				}
				mu.Lock()
				waits = append(waits, res.Wait)
				mu.Unlock()
			})
		}
	}
	close(start)
	wg.Wait()
	return got.Load(), waits
}

// TWO PROCESSES, ONE BUDGET (#380). Two ledgers on two separate pools — two replicas — race 60
// Takes each on one ad account: exactly 45 are granted between them, not 45 each, and every
// refusal carries a wait inside the window. Insight Takes on a second account are held to 24 the
// same way.
func TestLiveRedditReportLedger_TwoProcessesCannotExceedTheBudget(t *testing.T) {
	ledgers := []*postgres.RedditReportLedger{
		postgres.NewRedditReportLedger(processPool(t)),
		postgres.NewRedditReportLedger(processPool(t)),
	}
	pool := dbtest.Pool(t)

	account := dbtest.UniqueID(t, "acct")
	taken, waits := takeConcurrently(t, ledgers, 60, account, false)
	if taken != 45 {
		t.Errorf("two processes were granted %d slots, want exactly 45", taken)
	}
	if all, _ := heldSlots(t, pool, account); all != 45 {
		t.Errorf("%d slots recorded, want 45", all)
	}
	if len(waits) != 120-45 {
		t.Errorf("%d refusals, want %d", len(waits), 120-45)
	}
	for _, w := range waits {
		if w <= 0 || w > time.Minute {
			t.Errorf("refusal wait %s, want inside (0, 1m]", w)
			break
		}
	}

	insightAccount := dbtest.UniqueID(t, "insights")
	taken, _ = takeConcurrently(t, ledgers, 30, insightAccount, true)
	if taken != 24 {
		t.Errorf("two processes were granted %d insight slots, want exactly 24", taken)
	}
	// The 21 the insight reads cannot touch are still there for the metrics read and monitor.
	taken, _ = takeConcurrently(t, ledgers, 15, insightAccount, false)
	if taken != 21 {
		t.Errorf("after 24 insight slots, %d other slots were granted, want 21", taken)
	}
	if all, ins := heldSlots(t, pool, insightAccount); all != 45 || ins != 24 {
		t.Errorf("recorded %d/%d, want 45/24", all, ins)
	}

	// Per account: another account's budget is untouched.
	if res, err := ledgers[0].Take(context.Background(), dbtest.UniqueID(t, "other"), true, productionReportLimits); err != nil || !res.Taken {
		t.Errorf("a fresh account's Take = (%+v, %v), want a slot", res, err)
	}
}

// Release hands a slot back (idempotently), so the next Take on a full budget is granted; and a
// slot leaves the window on the database's clock, after which the account has room again.
func TestLiveRedditReportLedger_ReleaseAndWindow(t *testing.T) {
	pool := dbtest.Pool(t)
	l := postgres.NewRedditReportLedger(&postgres.Pool{Pool: pool})
	ctx := context.Background()
	account := dbtest.UniqueID(t, "acct")
	short := domain.RedditReportLimits{Window: 2 * time.Second, Total: 2, Insights: 2}

	first, err := l.Take(ctx, account, false, short)
	if err != nil || !first.Taken {
		t.Fatalf("Take 1 = (%+v, %v)", first, err)
	}
	if res, err := l.Take(ctx, account, false, short); err != nil || !res.Taken {
		t.Fatalf("Take 2 = (%+v, %v)", res, err)
	}
	full, err := l.Take(ctx, account, false, short)
	if err != nil || full.Taken || full.Wait <= 0 || full.Wait > short.Window {
		t.Fatalf("Take on a full budget = (%+v, %v), want refused with a wait inside the window", full, err)
	}

	for range 2 {
		if err := l.Release(ctx, first.SlotID); err != nil {
			t.Fatalf("Release: %v", err)
		}
	}
	if all, _ := heldSlots(t, pool, account); all != 1 {
		t.Fatalf("%d slots after releasing one of two (twice), want 1", all)
	}
	if res, err := l.Take(ctx, account, false, short); err != nil || !res.Taken {
		t.Fatalf("Take after a release = (%+v, %v), want a slot", res, err)
	}

	refused, err := l.Take(ctx, account, false, short)
	if err != nil || refused.Taken {
		t.Fatalf("Take on a full budget = (%+v, %v), want refused", refused, err)
	}
	time.Sleep(refused.Wait + 100*time.Millisecond)
	if res, err := l.Take(ctx, account, false, short); err != nil || !res.Taken {
		t.Fatalf("Take after the reported wait = (%+v, %v), want a slot", res, err)
	}
	// The Take that found room also pruned the expired slot.
	if all, _ := heldSlots(t, pool, account); all > 2 {
		t.Errorf("%d slots held, want at most the window's 2", all)
	}
}

// Postgres unavailable: the ledger returns an error, never a grant — reddit.ReportBudgets then
// refuses the attempt (fail closed).
func TestLiveRedditReportLedger_UnavailableDatabaseFailsClosed(t *testing.T) {
	p := processPool(t)
	p.Close()
	res, err := postgres.NewRedditReportLedger(p).Take(context.Background(), dbtest.UniqueID(t, "acct"), false, productionReportLimits)
	if err == nil || res.Taken {
		t.Fatalf("Take on a closed pool = (%+v, %v), want an error and no slot", res, err)
	}
	if res, err := postgres.NewRedditReportLedger(nil).Take(context.Background(), "t2_x", false, productionReportLimits); err == nil || res.Taken {
		t.Fatalf("Take with no pool = (%+v, %v), want an error and no slot", res, err)
	}
}
