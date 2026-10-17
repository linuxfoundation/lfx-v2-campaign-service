// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
)

// targetingLockHolders counts the backends holding scope's keyword-targeting lock.
func targetingLockHolders(ctx context.Context, t *testing.T, pool *pgxpool.Pool, scope string) int {
	t.Helper()
	class, key := postgres.KeywordTargetingLockKeys(scope)
	var n int
	err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks
		WHERE locktype = 'advisory' AND objsubid = 2 AND granted
		  AND classid::bigint = ($1::int4::bigint & 4294967295)
		  AND objid::bigint = ($2::int4::bigint & 4294967295)`, class, key).Scan(&n)
	if err != nil {
		t.Fatalf("read pg_locks: %v", err)
	}
	return n
}

// Two lock instances (as two replicas hold them) serialize one scope: the second is refused after
// its bounded wait while the first holds it, gets it once the first releases, and another scope
// never waits. Release leaves nothing held and is idempotent.
func TestLiveKeywordTargetingWriteLock_SerializesOneScopeAcrossInstances(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	scope := "reddit/" + dbtest.UniqueID(t, "acct") + "/ad_groups/t5_ag"
	other := "reddit/" + dbtest.UniqueID(t, "acct") + "/ad_groups/t5_other"
	const wait = 300 * time.Millisecond
	a := postgres.NewKeywordTargetingWriteLockWithWait(&postgres.Pool{Pool: pool}, wait, 20*time.Millisecond)
	b := postgres.NewKeywordTargetingWriteLockWithWait(&postgres.Pool{Pool: pool}, wait, 20*time.Millisecond)

	unlockA, err := a.Lock(ctx, scope)
	if err != nil {
		t.Fatalf("a.Lock: %v", err)
	}
	if n := targetingLockHolders(ctx, t, pool, scope); n != 1 {
		t.Fatalf("%d backends hold the lock a took, want 1", n)
	}

	start := time.Now()
	if _, err := b.Lock(ctx, scope); !errors.Is(err, domain.ErrKeywordTargetingWriteInProgress) {
		t.Fatalf("b.Lock while a holds it: err = %v, want ErrKeywordTargetingWriteInProgress", err)
	}
	if waited := time.Since(start); waited < wait {
		t.Errorf("b was refused after %s, before its %s wait", waited, wait)
	}

	unlockOther, err := b.Lock(ctx, other)
	if err != nil {
		t.Fatalf("b.Lock of another scope while a holds %s: %v", scope, err)
	}
	unlockOther()

	// A waiter gets the scope once the holder releases it, inside its wait.
	patient := postgres.NewKeywordTargetingWriteLockWithWait(&postgres.Pool{Pool: pool}, 10*time.Second, 20*time.Millisecond)
	got := make(chan error, 1)
	go func() {
		unlock, err := patient.Lock(ctx, scope)
		if err == nil {
			unlock()
		}
		got <- err
	}()
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-got:
		t.Fatalf("the waiter finished while a held the lock: %v", err)
	default:
	}
	unlockA()
	unlockA() // idempotent: must not unlock or release anything twice
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("waiter after release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter never got the released lock")
	}
	if n := targetingLockHolders(ctx, t, pool, scope); n != 0 {
		t.Errorf("%d backends still hold the lock after every release", n)
	}
}

// A request that ends while waiting is refused with its context's error — not as "in progress" —
// and leaves nothing held.
func TestLiveKeywordTargetingWriteLock_CancelledWait(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	scope := "reddit/" + dbtest.UniqueID(t, "acct") + "/ad_groups/t5_ag"
	l := postgres.NewKeywordTargetingWriteLockWithWait(&postgres.Pool{Pool: pool}, 10*time.Second, 20*time.Millisecond)
	unlock, err := l.Lock(ctx, scope)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	defer unlock()
	cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	_, err = l.Lock(cctx, scope)
	if err == nil || errors.Is(err, domain.ErrKeywordTargetingWriteInProgress) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled wait: err = %v, want the context's deadline", err)
	}
	if n := targetingLockHolders(ctx, t, pool, scope); n != 1 {
		t.Errorf("%d holders, want only the first", n)
	}
}

// A SATURATED POOL is bounded by the lock's wait, not by the caller's longer deadline. With
// pool_max_conns=1 — a real configuration — the holder's session is the pool's only connection, so
// a second write cannot even check one out to ask; it must still be refused as in progress (409)
// at about its wait, not block until its request deadline and end as a 503.
func TestLiveKeywordTargetingWriteLock_SaturatedPoolRefusesAtTheWait(t *testing.T) {
	ctx := context.Background()
	_ = dbtest.Pool(t) // skip/fail semantics and the migrated schema
	cfg, err := pgxpool.ParseConfig(dbtest.DSN())
	if err != nil {
		t.Fatalf("parse DSN: %s", dbtest.SafeDSNErr(err))
	}
	cfg.MaxConns = 1
	one, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open one-connection pool: %s", dbtest.SafeDSNErr(err))
	}
	t.Cleanup(one.Close)

	scope := "reddit/" + dbtest.UniqueID(t, "acct") + "/ad_groups/t5_ag"
	const wait = 300 * time.Millisecond
	a := postgres.NewKeywordTargetingWriteLockWithWait(&postgres.Pool{Pool: one}, wait, 20*time.Millisecond)
	b := postgres.NewKeywordTargetingWriteLockWithWait(&postgres.Pool{Pool: one}, wait, 20*time.Millisecond)
	unlockA, err := a.Lock(ctx, scope)
	if err != nil {
		t.Fatalf("a.Lock: %v", err)
	}

	// The caller's own deadline is far longer than the wait, as the 45s mutation timeout is.
	callerCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	start := time.Now()
	_, err = b.Lock(callerCtx, scope)
	took := time.Since(start)
	if !errors.Is(err, domain.ErrKeywordTargetingWriteInProgress) {
		t.Fatalf("b.Lock on a saturated pool: err = %v, want ErrKeywordTargetingWriteInProgress", err)
	}
	if took < wait || took > wait+2*time.Second {
		t.Errorf("b was refused after %s; want about its %s wait", took, wait)
	}

	unlockA()
	if s := one.Stat(); s.AcquiredConns() != 0 {
		t.Errorf("%d connections still checked out after release", s.AcquiredConns())
	}
	unlockB, err := b.Lock(ctx, scope)
	if err != nil {
		t.Fatalf("b.Lock after a released: %v", err)
	}
	unlockB()
}
