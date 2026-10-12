// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// Close is called inside the container's shared shutdown budget, so it must honour its caller's
// deadline rather than replace it with a fresh lockReleaseTimeout: a session close that stalls is
// cut off when the caller's slice of the budget ends.
func TestStatsJobLease_CloseHonoursTheCallerDeadline(t *testing.T) {
	lease := NewStatsJobLease(&Pool{})
	lease.conn = &pgx.Conn{} // never touched: closeConn is replaced below
	var gotDeadline time.Time
	lease.closeConn = func(ctx context.Context, _ *pgx.Conn) error {
		gotDeadline, _ = ctx.Deadline()
		<-ctx.Done() // a session close that never completes on its own
		return ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	want, _ := ctx.Deadline()
	start := time.Now()
	lease.Close(ctx)
	if took := time.Since(start); took > time.Second {
		t.Errorf("Close took %s with a 100ms deadline; it must not strip the caller's deadline", took)
	}
	if !gotDeadline.Equal(want) {
		t.Errorf("session close ran with deadline %v, want the caller's %v", gotDeadline, want)
	}
	if lease.conn != nil || !lease.closed.Load() {
		t.Error("Close must forget the session and refuse later calls")
	}
}

// liveLeasePool opens a pool on TEST_DATABASE_URL, or skips. In-package (not dbtest) so the test
// can reach the lease's lock statement; it needs no migration.
func liveLeasePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping live lease test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func lockHolderPID(ctx context.Context, t *testing.T, pool *pgxpool.Pool, accountID string) int32 {
	t.Helper()
	var pid int32
	err := pool.QueryRow(ctx, `SELECT COALESCE(MAX(pid), 0) FROM pg_locks
		WHERE locktype = 'advisory' AND objsubid = 2 AND granted
		  AND classid::bigint = ($1::int4::bigint & 4294967295)
		  AND objid::bigint = ($2::int4::bigint & 4294967295)`, statsJobLeaseClass, statsJobLeaseKey(accountID)).Scan(&pid)
	if err != nil {
		t.Fatalf("read pg_locks: %v", err)
	}
	return pid
}

// A failed lock attempt for an account this pod does NOT hold must not drop the shared session:
// account A, already owned, stays held by the SAME backend and Own(A) still succeeds. The failure
// is injected AFTER the lock was granted (an error raised once pg_try_advisory_lock has run, and a
// session lock survives the statement's abort), so the attempt must also leave no untracked lock
// for B behind.
func TestLiveStatsJobLease_FailedLockForOneAccountKeepsTheOthers(t *testing.T) {
	ctx := context.Background()
	pool := liveLeasePool(t)
	const fn = "xstats_lease_inject_failure"
	if _, err := pool.Exec(ctx, `CREATE OR REPLACE FUNCTION `+fn+`(granted boolean) RETURNS boolean
		LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected after the lock'; END $$`); err != nil {
		t.Fatalf("create the failure function: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS `+fn+`(boolean)`) })

	suffix := time.Now().Format("150405.000000000")
	a, b := "xrecover-a-"+suffix, "xrecover-b-"+suffix
	lease := NewStatsJobLease(&Pool{Pool: pool})
	t.Cleanup(func() { lease.Close(context.Background()) })

	if err := lease.Own(ctx, a); err != nil {
		t.Fatalf("Own(a): %v", err)
	}
	pid := lockHolderPID(ctx, t, pool, a)
	if pid == 0 {
		t.Fatal("no backend holds a")
	}

	lease.lockSQL = "SELECT " + fn + "(pg_try_advisory_lock($1, $2))"
	err := lease.Own(ctx, b)
	lease.lockSQL = statsJobLeaseLockSQL
	if !errors.Is(err, domain.ErrStatsJobLeaseUnavailable) {
		t.Fatalf("Own(b) with the injected failure: err = %v, want ErrStatsJobLeaseUnavailable", err)
	}
	if got := lockHolderPID(ctx, t, pool, a); got != pid {
		t.Errorf("a's holder after b's failure = %d, want the same backend %d (the session was dropped)", got, pid)
	}
	if got := lockHolderPID(ctx, t, pool, b); got != 0 {
		t.Errorf("backend %d still holds an untracked lock for b", got)
	}
	if err := lease.Own(ctx, a); err != nil {
		t.Errorf("Own(a) after b's failure: %v", err)
	}
	if err := lease.Own(ctx, b); err != nil {
		t.Errorf("Own(b) once the statement works again: %v", err)
	}
}
