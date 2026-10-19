// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

func keywordTargetingLockHolders(ctx context.Context, t *testing.T, pool *pgxpool.Pool, scope string) int {
	t.Helper()
	var n int
	err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks
		WHERE locktype = 'advisory' AND objsubid = 2 AND granted
		  AND classid::bigint = ($1::int4::bigint & 4294967295)
		  AND objid::bigint = ($2::int4::bigint & 4294967295)`, keywordTargetingLockClass, keywordTargetingLockKey(scope)).Scan(&n)
	if err != nil {
		t.Fatalf("read pg_locks: %v", err)
	}
	return n
}

// A try-lock that FAILS AFTER Postgres granted the lock (the error is raised once
// pg_try_advisory_lock has run, and a session lock survives the statement's abort) must not hand
// that session back to the pool: the connection is destroyed, so Postgres drops the lock with it,
// and the refusal is a lock failure — not "in progress", not a held lock.
func TestLiveKeywordTargetingWriteLock_FailedTryLockDestroysTheConnection(t *testing.T) {
	ctx := context.Background()
	pool := liveLeasePool(t)
	const fn = "kwt_lock_inject_failure"
	if _, err := pool.Exec(ctx, `CREATE OR REPLACE FUNCTION `+fn+`(granted boolean) RETURNS boolean
		LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected after the lock'; END $$`); err != nil {
		t.Fatalf("create the failure function: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS `+fn+`(boolean)`) })

	scope := "reddit/kwt-destroy-" + time.Now().Format("150405.000000000") + "/ad_groups/t5_ag"
	l := NewKeywordTargetingWriteLockWithWait(&Pool{Pool: pool}, time.Second, 20*time.Millisecond)
	l.lockSQL = "SELECT " + fn + "(pg_try_advisory_lock($1, $2))"
	_, err := l.Lock(ctx, scope)
	if err == nil || errors.Is(err, domain.ErrKeywordTargetingWriteInProgress) {
		t.Fatalf("Lock with the injected failure: err = %v, want the try-lock's failure", err)
	}

	// Had the session gone back to the pool it would still be alive, holding the lock granted inside
	// the failed statement. Destroyed, Postgres drops that lock as its backend exits, and pgxpool
	// finishes destroying the released resource, both a moment after the close.
	deadline := time.Now().Add(5 * time.Second)
	for keywordTargetingLockHolders(ctx, t, pool, scope) != 0 || pool.Stat().AcquiredConns() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("after the failed try: %d holders of the lock granted inside the failed statement, %d connections checked out — its session was not destroyed",
				keywordTargetingLockHolders(ctx, t, pool, scope), pool.Stat().AcquiredConns())
		}
		time.Sleep(10 * time.Millisecond)
	}
	l.lockSQL = keywordTargetingLockSQL
	unlock, err := l.Lock(ctx, scope)
	if err != nil {
		t.Fatalf("Lock after the failure: %v", err)
	}
	unlock.Release()
}

// waitForKeywordTargetingLockState polls until scope's lock has want holders and no connection is
// checked out of pool (when free), failing after 5s. Release is asynchronous on the cooldown
// paths, and Postgres drops a destroyed session's lock only as its backend exits.
func waitForKeywordTargetingLockState(ctx context.Context, t *testing.T, pool *pgxpool.Pool, scope string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		n := keywordTargetingLockHolders(ctx, t, pool, scope)
		acquired := pool.Stat().AcquiredConns()
		if n == want && (want != 0 || acquired == 0) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("scope %s: %d holders (want %d), %d connections checked out", scope, n, want, acquired)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A hold kept after an UNCONFIRMED write stays held, detached from the request, until its cooldown
// ends; a later Release ends it early; and process shutdown (StopCooldownsForShutdown — what
// Container.Close calls before closing the pool) releases a cooling hold at once and returns its
// connection, so it can neither outlive the process's orderly close nor block pgxpool.Close.
func TestLiveKeywordTargetingWriteLock_CooldownHoldAndShutdown(t *testing.T) {
	resetCooldownState(t)
	t.Cleanup(func() { resetCooldownState(t) })
	ctx := context.Background()
	pool := liveLeasePool(t)
	l := NewKeywordTargetingWriteLockWithWait(&Pool{Pool: pool}, 50*time.Millisecond, 10*time.Millisecond)
	suffix := time.Now().Format("150405.000000000")

	t.Run("held through the cooldown, then released", func(t *testing.T) {
		scope := "reddit/kwt-cool-" + suffix + "/ad_groups/t5_a"
		reqCtx, cancel := context.WithCancel(ctx)
		hold, err := l.Lock(reqCtx, scope)
		if err != nil {
			t.Fatalf("Lock: %v", err)
		}
		hold.ReleaseAfterCooldown(300 * time.Millisecond)
		cancel() // the request ends; the hold must not
		if _, err := l.Lock(ctx, scope); !errors.Is(err, domain.ErrKeywordTargetingWriteInProgress) {
			t.Fatalf("Lock during the cooldown: %v, want ErrKeywordTargetingWriteInProgress", err)
		}
		waitForKeywordTargetingLockState(ctx, t, pool, scope, 0)
		next, err := l.Lock(ctx, scope)
		if err != nil {
			t.Fatalf("Lock after the cooldown: %v", err)
		}
		next.Release()
	})

	t.Run("an explicit Release ends the cooldown early", func(t *testing.T) {
		scope := "reddit/kwt-early-" + suffix + "/ad_groups/t5_b"
		hold, err := l.Lock(ctx, scope)
		if err != nil {
			t.Fatalf("Lock: %v", err)
		}
		hold.ReleaseAfterCooldown(time.Hour)
		hold.Release()
		waitForKeywordTargetingLockState(ctx, t, pool, scope, 0)
	})

	t.Run("shutdown releases a cooling hold", func(t *testing.T) {
		scope := "reddit/kwt-shutdown-" + suffix + "/ad_groups/t5_c"
		hold, err := l.Lock(ctx, scope)
		if err != nil {
			t.Fatalf("Lock: %v", err)
		}
		hold.ReleaseAfterCooldown(time.Hour)
		if n := keywordTargetingLockHolders(ctx, t, pool, scope); n != 1 {
			t.Fatalf("%d holders while cooling, want 1", n)
		}
		start := time.Now()
		StopCooldownsForShutdown(2 * time.Second)
		if took := time.Since(start); took > 2*time.Second {
			t.Errorf("shutdown waited %s for the cooling hold", took)
		}
		waitForKeywordTargetingLockState(ctx, t, pool, scope, 0)

		// After shutdown began, a new cooling hold releases at once rather than parking.
		late := "reddit/kwt-late-" + suffix + "/ad_groups/t5_d"
		h2, err := l.Lock(ctx, late)
		if err != nil {
			t.Fatalf("Lock after shutdown: %v", err)
		}
		h2.ReleaseAfterCooldown(time.Hour)
		waitForKeywordTargetingLockState(ctx, t, pool, late, 0)
	})
}
