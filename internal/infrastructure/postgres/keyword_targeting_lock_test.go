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
	unlock()
}
