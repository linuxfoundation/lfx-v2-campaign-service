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

// leaseHolderPID is the backend holding accountID's stats-job lease, or 0.
func leaseHolderPID(ctx context.Context, t *testing.T, pool *pgxpool.Pool, accountID string) int32 {
	t.Helper()
	class, key := postgres.StatsJobLeaseLockKeys(accountID)
	var pid int32
	err := pool.QueryRow(ctx, `SELECT COALESCE(MAX(pid), 0) FROM pg_locks
		WHERE locktype = 'advisory' AND objsubid = 2 AND granted
		  AND classid::bigint = ($1::int4::bigint & 4294967295)
		  AND objid::bigint = ($2::int4::bigint & 4294967295)`, class, key).Scan(&pid)
	if err != nil {
		t.Fatalf("read pg_locks: %v", err)
	}
	return pid
}

// Two instances (two leases, as two pods would hold) cannot both own one account; the owner keeps
// it across calls; other accounts are independent; Close hands it over.
func TestLiveStatsJobLease_OneOwnerPerAccountAndReleaseOnClose(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	account, other := dbtest.UniqueID(t, "xacct"), dbtest.UniqueID(t, "xother")
	a := postgres.NewStatsJobLease(&postgres.Pool{Pool: pool})
	b := postgres.NewStatsJobLease(&postgres.Pool{Pool: pool})
	t.Cleanup(func() { a.Close(ctx); b.Close(ctx) })

	if err := a.Own(ctx, account); err != nil {
		t.Fatalf("a.Own: %v", err)
	}
	if err := b.Own(ctx, account); !errors.Is(err, domain.ErrStatsJobLeaseNotHeld) {
		t.Fatalf("b.Own while a holds it: err = %v, want ErrStatsJobLeaseNotHeld", err)
	}
	if err := a.Own(ctx, account); err != nil {
		t.Errorf("a.Own again: %v; the owner must keep the lease", err)
	}
	if err := b.Own(ctx, other); err != nil {
		t.Errorf("b.Own of another account: %v", err)
	}
	if leaseHolderPID(ctx, t, pool, account) == 0 {
		t.Fatal("no backend holds the lease a owns")
	}

	a.Close(ctx)
	if pid := leaseHolderPID(ctx, t, pool, account); pid != 0 {
		t.Errorf("backend %d still holds the lease after Close", pid)
	}
	if err := b.Own(ctx, account); err != nil {
		t.Errorf("b.Own after a closed: %v", err)
	}
	// A closed (shutting-down) lease refuses as Unavailable: nothing was asked of Postgres.
	if err := a.Own(ctx, account); !errors.Is(err, domain.ErrStatsJobLeaseUnavailable) {
		t.Errorf("a closed lease must refuse: %v", err)
	}
}

// Losing the session (a terminated backend, a failover) loses the lease: the former owner's next
// call refuses rather than carrying on, and another instance can take it.
func TestLiveStatsJobLease_SessionLossStopsTheFormerOwner(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	account := dbtest.UniqueID(t, "xlost")
	a := postgres.NewStatsJobLease(&postgres.Pool{Pool: pool})
	b := postgres.NewStatsJobLease(&postgres.Pool{Pool: pool})
	t.Cleanup(func() { a.Close(ctx); b.Close(ctx) })

	second := dbtest.UniqueID(t, "xlost2")
	for _, acc := range []string{account, second} {
		if err := a.Own(ctx, acc); err != nil {
			t.Fatalf("a.Own(%s): %v", acc, err)
		}
	}
	pid := leaseHolderPID(ctx, t, pool, account)
	if pid == 0 || leaseHolderPID(ctx, t, pool, second) != pid {
		t.Fatal("a's two leases are not on one backend")
	}
	// The timeout form waits until the backend has actually exited (and so dropped its locks).
	var ok bool
	if err := pool.QueryRow(ctx, "SELECT pg_terminate_backend($1, 5000)", pid).Scan(&ok); err != nil || !ok {
		t.Fatalf("terminate the owner's backend: %v (%v)", err, ok)
	}
	if err := b.Own(ctx, account); err != nil {
		t.Fatalf("b.Own after the owner's session died: %v", err)
	}
	if err := a.Own(ctx, account); !errors.Is(err, domain.ErrStatsJobLeaseNotHeld) {
		t.Errorf("a.Own after losing its session: err = %v, want ErrStatsJobLeaseNotHeld", err)
	}
	// Every lease on the lost session was lost, and each is RE-ACQUIRED on its own check: the
	// second account (taken by nobody meanwhile) comes back on a's new session.
	if err := a.Own(ctx, second); err != nil {
		t.Errorf("a.Own(second) after the loss: %v; it must re-acquire, not refuse", err)
	}
	if got := leaseHolderPID(ctx, t, pool, second); got == 0 || got == pid {
		t.Errorf("second's holder %d: want a's NEW session, not the terminated %d", got, pid)
	}
}

// A check made on a request that has already ended (cancelled or past its deadline) must not
// read as a lost session: the lease stays held in pg_locks, another instance still cannot take
// it, and the next live check still owns it. Only that request is refused, as Unavailable.
func TestLiveStatsJobLease_EndedRequestKeepsTheLease(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	account := dbtest.UniqueID(t, "xkeep")
	a := postgres.NewStatsJobLease(&postgres.Pool{Pool: pool})
	b := postgres.NewStatsJobLease(&postgres.Pool{Pool: pool})
	t.Cleanup(func() { a.Close(ctx); b.Close(ctx) })

	if err := a.Own(ctx, account); err != nil {
		t.Fatalf("a.Own: %v", err)
	}
	pid := leaseHolderPID(ctx, t, pool, account)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	expired, cancel2 := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer cancel2()
	for name, dead := range map[string]context.Context{"cancelled": cancelled, "expired": expired} {
		if err := a.Own(dead, account); !errors.Is(err, domain.ErrStatsJobLeaseUnavailable) {
			t.Errorf("%s: a.Own = %v, want ErrStatsJobLeaseUnavailable", name, err)
		}
		if got := leaseHolderPID(ctx, t, pool, account); got != pid {
			t.Fatalf("%s: lease holder %d after the check, want %d (the lock was dropped)", name, got, pid)
		}
	}
	if err := b.Own(ctx, account); !errors.Is(err, domain.ErrStatsJobLeaseNotHeld) {
		t.Errorf("b.Own: %v, want ErrStatsJobLeaseNotHeld", err)
	}
	if err := a.Own(ctx, account); err != nil {
		t.Errorf("a.Own with a live context: %v; the owner must still own it", err)
	}
}

// A lease that cannot reach a database answers Unavailable, never "another instance owns it".
func TestLiveStatsJobLease_NoDatabaseIsUnavailable(t *testing.T) {
	l := postgres.NewStatsJobLease(&postgres.Pool{})
	err := l.Own(context.Background(), "acc")
	if !errors.Is(err, domain.ErrStatsJobLeaseUnavailable) || errors.Is(err, domain.ErrStatsJobLeaseNotHeld) {
		t.Errorf("err = %v, want only ErrStatsJobLeaseUnavailable", err)
	}
}

// The lease must not pin business-pool connections: with a pool of ONE connection, leases for
// several accounts are held and an ordinary Acquire still succeeds at once. All the locks sit on
// one session outside the pool.
func TestLiveStatsJobLease_DoesNotPinBusinessPoolConnections(t *testing.T) {
	ctx := context.Background()
	_ = dbtest.Pool(t) // migrates, or skips when no database is configured
	cfg, err := pgxpool.ParseConfig(dbtest.DSN())
	if err != nil {
		t.Fatalf("parse DSN: %s", dbtest.SafeDSNErr(err))
	}
	cfg.MaxConns = 1
	small, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open a one-connection pool: %s", dbtest.SafeDSNErr(err))
	}
	t.Cleanup(small.Close)
	lease := postgres.NewStatsJobLease(&postgres.Pool{Pool: small})
	t.Cleanup(func() { lease.Close(ctx) })

	accounts := []string{dbtest.UniqueID(t, "xp1"), dbtest.UniqueID(t, "xp2"), dbtest.UniqueID(t, "xp3")}
	for _, acc := range accounts {
		ownCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := lease.Own(ownCtx, acc)
		cancel()
		if err != nil {
			t.Fatalf("Own(%s) on a one-connection pool: %v", acc, err)
		}
	}
	acqCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	conn, err := small.Acquire(acqCtx)
	if err != nil {
		t.Fatalf("an ordinary Acquire on the business pool failed while leases are held: %v", err)
	}
	conn.Release()
	var pids []int32
	for _, acc := range accounts {
		pids = append(pids, leaseHolderPID(ctx, t, small, acc))
	}
	if pids[0] == 0 || pids[0] != pids[1] || pids[1] != pids[2] {
		t.Errorf("lease holders %v: want every account's lock on ONE session", pids)
	}
}
