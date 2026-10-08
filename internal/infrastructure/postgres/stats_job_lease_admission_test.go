// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// stubbedLease is a lease with a (lazy, never-dialled) pool, a stand-in session, and counting
// stubs for the session's ping and close.
func stubbedLease(t *testing.T) (*StatsJobLease, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://u@127.0.0.1:1/db?sslmode=disable")
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	t.Cleanup(pool.Close)
	lease := NewStatsJobLease(&Pool{Pool: pool})
	lease.conn = &pgx.Conn{} // never touched: ping and closeConn are stubbed
	var pings, closes atomic.Int32
	lease.ping = func(context.Context, *pgx.Conn) error { pings.Add(1); return nil }
	lease.closeConn = func(context.Context, *pgx.Conn) error { closes.Add(1); return nil }
	return lease, &pings, &closes
}

// An already-ended request does no database work: no ping, every lease kept, Unavailable at once.
func TestStatsJobLease_EndedRequestSkipsThePing(t *testing.T) {
	lease, pings, closes := stubbedLease(t)
	lease.heldKeys[lease.key("acc")] = true
	lease.ping = func(ctx context.Context, _ *pgx.Conn) error {
		pings.Add(1)
		<-ctx.Done() // a slow ping
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Repeated: with the slot free, the slot wait's select picks at random between the free slot
	// and the dead context, so one call alone could pass without the post-acquire check.
	for i := 0; i < 32; i++ {
		start := time.Now()
		err := lease.Own(ctx, "acc")
		if !errors.Is(err, domain.ErrStatsJobLeaseUnavailable) || !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want Unavailable wrapping context.Canceled", err)
		}
		if took := time.Since(start); took > 500*time.Millisecond {
			t.Fatalf("Own took %s on a cancelled request", took)
		}
	}
	if pings.Load() != 0 || closes.Load() != 0 || !lease.heldKeys[lease.key("acc")] {
		t.Errorf("pings %d, closes %d, held %v: a cancelled request must touch nothing", pings.Load(), closes.Load(), lease.heldKeys)
	}
	lease.Close(context.Background())
}

// Close must return within its deadline even while an Own holds the slot inside a database step
// that ignores cancellation; the in-flight Own then closes the session on its way out, once.
func TestStatsJobLease_CloseReturnsWhileAnOwnIsStuck(t *testing.T) {
	lease, _, closes := stubbedLease(t)
	inPing, unblock := make(chan struct{}), make(chan struct{})
	lease.ping = func(context.Context, *pgx.Conn) error {
		close(inPing)
		<-unblock // a step that does not honour its context
		return errors.New("connection reset")
	}
	owned := make(chan error, 1)
	go func() { owned <- lease.Own(context.Background(), "acc") }()
	<-inPing

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	lease.Close(ctx)
	if took := time.Since(start); took > 400*time.Millisecond {
		t.Errorf("Close took %s with a 100ms deadline while an Own was stuck", took)
	}
	close(unblock)
	if err := <-owned; !errors.Is(err, domain.ErrStatsJobLeaseUnavailable) {
		t.Errorf("the in-flight Own: %v, want Unavailable", err)
	}
	if n := closes.Load(); n != 1 {
		t.Errorf("session closed %d time(s), want exactly once (by the Own on its way out)", n)
	}
	if lease.conn != nil {
		t.Error("the session is still set after Close")
	}
	if err := lease.Own(context.Background(), "acc"); !errors.Is(err, domain.ErrStatsJobLeaseUnavailable) {
		t.Errorf("Own after Close: %v, want Unavailable", err)
	}
}

// Close cancels the lease context, so an Own in a step that DOES honour cancellation ends at
// once; Close then gets the slot and closes the session itself, once.
func TestStatsJobLease_CloseCancelsAnInFlightOwn(t *testing.T) {
	lease, _, closes := stubbedLease(t)
	inPing := make(chan struct{})
	lease.ping = func(ctx context.Context, _ *pgx.Conn) error {
		close(inPing)
		<-ctx.Done()
		return ctx.Err()
	}
	owned := make(chan error, 1)
	go func() { owned <- lease.Own(context.Background(), "acc") }()
	<-inPing
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	lease.Close(ctx)
	if took := time.Since(start); took > time.Second {
		t.Errorf("Close took %s: it must cancel the in-flight database step", took)
	}
	if err := <-owned; !errors.Is(err, domain.ErrStatsJobLeaseUnavailable) {
		t.Errorf("the in-flight Own: %v, want Unavailable", err)
	}
	if n := closes.Load(); n != 1 {
		t.Errorf("session closed %d time(s), want exactly once", n)
	}
}

// Two account ids whose keys collide share ONE lock: once one is held, the other is owned without
// any lock call — so a failing lock statement can never reach the recovery unlock and release the
// lock the first account's ownership rests on.
func TestLiveStatsJobLease_CollidingKeysShareOneLock(t *testing.T) {
	ctx := context.Background()
	pool := liveLeasePool(t)
	const fn = "xstats_lease_inject_collide"
	if _, err := pool.Exec(ctx, `CREATE OR REPLACE FUNCTION `+fn+`(granted boolean) RETURNS boolean
		LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected after the lock'; END $$`); err != nil {
		t.Fatalf("create the failure function: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS `+fn+`(boolean)`) })

	suffix := time.Now().Format("150405.000000000")
	a, b := "xcollide-a-"+suffix, "xcollide-b-"+suffix
	lease := NewStatsJobLease(&Pool{Pool: pool})
	t.Cleanup(func() { lease.Close(context.Background()) })
	shared := statsJobLeaseKey(a)
	lease.key = func(string) int32 { return shared } // force a and b onto one key

	if err := lease.Own(ctx, a); err != nil {
		t.Fatalf("Own(a): %v", err)
	}
	pid := lockHolderPID(ctx, t, pool, a)
	if pid == 0 {
		t.Fatal("no backend holds a's key")
	}
	lease.lockSQL = "SELECT " + fn + "(pg_try_advisory_lock($1, $2))" // any lock attempt now fails
	if err := lease.Own(ctx, b); err != nil {
		t.Errorf("Own(b) on a's key: %v; b shares a's lock and is owned with it", err)
	}
	if got := lockHolderPID(ctx, t, pool, a); got != pid {
		t.Errorf("a's key holder = %d after Own(b), want %d: the shared lock was released", got, pid)
	}
	if err := lease.Own(ctx, a); err != nil {
		t.Errorf("Own(a) after Own(b): %v", err)
	}
}

// Close with an already-expired context and nothing in flight must still close the session: the
// slot is free, so there is no other holder to close it on its way out. Looped because a select
// with both a free slot and a done context ready picks between them at random.
func TestStatsJobLease_ExpiredCloseStillClosesAnIdleSession(t *testing.T) {
	for i := 0; i < 32; i++ {
		lease, _, closes := stubbedLease(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		lease.Close(ctx)
		if closes.Load() != 1 || lease.conn != nil {
			t.Fatalf("run %d: closes = %d, conn nil = %v; want the idle session closed once", i, closes.Load(), lease.conn == nil)
		}
	}
}
