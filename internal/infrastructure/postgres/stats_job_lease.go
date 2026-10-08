// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// statsJobLeaseClass is the first key of the two-int advisory lock the X stats-job lease takes:
// "XSTJ". The two-int form (pg_try_advisory_lock(int, int)) is a separate key space from the
// one-bigint form ClaimCampaignVersion uses, so the two can never collide.
const statsJobLeaseClass int32 = 0x5853544A

// statsJobLeasePingTimeout bounds the liveness check of a held lease's connection.
const statsJobLeasePingTimeout = 2 * time.Second

// StatsJobLease implements domain.StatsJobLease with one SESSION advisory lock per X ad account,
// each held on a DEDICATED pooled connection for as long as this process owns the account.
//
// Session-level, not transaction-level, because ownership must outlast any one request: the
// lock is taken once and kept, so the same pod keeps owning the account (its in-memory job
// budget and pacer stay authoritative) instead of ownership flapping per request. Another pod's
// pg_try_advisory_lock fails while this session lives, so only one pod runs X stats jobs per
// account.
//
// LEASE LOSS. A session lock lives exactly as long as its Postgres session. A failover, a
// terminated backend or a severed connection drops it server-side, after which ANOTHER pod can
// take it. So Own re-verifies a held lease on every call by pinging its connection: a dead
// session means the lease is gone, the connection is destroyed, and the lock is tried afresh —
// typically lost to the pod that took it. New stats-job submissions therefore stop at the first
// call after the loss. A submission ALREADY admitted when the session dies is not recalled: this
// is admission control, not fencing, and the window is one read's job creation.
//
// COST. One pooled connection per owned account, held for the life of the process (a small fixed
// set: the accounts this pod has run stats jobs on). Close returns them before the pool closes,
// since pgxpool.Close blocks on checked-out connections.
type StatsJobLease struct {
	pool *Pool

	// mu serializes Own and Close, so one account is never acquired twice by this process and a
	// connection is never used by two callers at once.
	mu     sync.Mutex
	held   map[string]*pgxpool.Conn
	closed bool
}

// NewStatsJobLease returns a lease bound to pool. It holds nothing until Own is called.
func NewStatsJobLease(pool *Pool) *StatsJobLease {
	return &StatsJobLease{pool: pool, held: map[string]*pgxpool.Conn{}}
}

var _ domain.StatsJobLease = (*StatsJobLease)(nil)

// statsJobLeaseKey is the second lock key for accountID: FNV-1a over the id, folded to int32.
// A collision would only make two accounts share one owner, which is harmless.
func statsJobLeaseKey(accountID string) int32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(accountID))
	return int32(h.Sum32()) //nolint:gosec // a hash folded to the lock's int4 key, overflow intended
}

// Own implements domain.StatsJobLease.
func (l *StatsJobLease) Own(ctx context.Context, accountID string) error {
	if l == nil || l.pool == nil || l.pool.Pool == nil {
		return fmt.Errorf("%w: no database to hold the lease on", domain.ErrStatsJobLeaseNotHeld)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return fmt.Errorf("%w: shutting down", domain.ErrStatsJobLeaseNotHeld)
	}
	if conn, ok := l.held[accountID]; ok {
		pingCtx, cancel := context.WithTimeout(ctx, statsJobLeasePingTimeout)
		err := conn.Ping(pingCtx)
		cancel()
		if err == nil {
			return nil
		}
		// The session — and with it the lock — may be gone. Destroy the connection (never return
		// a possibly lock-bearing session to the pool) and try afresh below.
		slog.WarnContext(ctx, "x stats-job lease connection failed its liveness check; the lease is treated as lost",
			"error", err)
		delete(l.held, accountID)
		destroyLeaseConn(ctx, conn)
	}

	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("%w: acquire a connection: %w", domain.ErrStatsJobLeaseNotHeld, err)
	}
	var acquired bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1, $2)", statsJobLeaseClass, statsJobLeaseKey(accountID)).Scan(&acquired); err != nil {
		// The lock may have been granted server-side even though the client saw an error (a
		// cancelled context mid-call), so the session must not go back to the pool.
		destroyLeaseConn(ctx, conn)
		return fmt.Errorf("%w: try the lock: %w", domain.ErrStatsJobLeaseNotHeld, err)
	}
	if !acquired {
		// Postgres answered "held elsewhere": nothing is held on this session.
		conn.Release()
		return domain.ErrStatsJobLeaseNotHeld
	}
	l.held[accountID] = conn
	return nil
}

// Close releases every held lease and returns its connection to the pool, and makes later Own
// calls refuse. Must run before the pool is closed.
func (l *StatsJobLease) Close(ctx context.Context) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	for accountID, conn := range l.held {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lockReleaseTimeout)
		_, err := conn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1, $2)", statsJobLeaseClass, statsJobLeaseKey(accountID))
		cancel()
		if err != nil {
			destroyLeaseConn(ctx, conn)
		} else {
			conn.Release()
		}
		delete(l.held, accountID)
	}
}

// destroyLeaseConn closes the underlying session (dropping any advisory lock on it) and returns
// the now-dead connection's slot to the pool.
func destroyLeaseConn(ctx context.Context, conn *pgxpool.Conn) {
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lockReleaseTimeout)
	defer cancel()
	if err := conn.Conn().Close(closeCtx); err != nil && !errors.Is(err, context.Canceled) {
		slog.WarnContext(ctx, "failed to close x stats-job lease connection", "error", err)
	}
	conn.Release()
}

// StatsJobLeaseLockKeys returns the two advisory-lock keys the lease takes for accountID, as they
// appear in pg_locks (classid, objid with objsubid 2) — for operators finding the owning backend,
// and for the live tests.
func StatsJobLeaseLockKeys(accountID string) (class, key int32) {
	return statsJobLeaseClass, statsJobLeaseKey(accountID)
}
