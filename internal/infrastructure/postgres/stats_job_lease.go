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

	"github.com/jackc/pgx/v5"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// statsJobLeaseClass is the first key of the two-int advisory lock the X stats-job lease takes:
// "XSTJ". The two-int form (pg_try_advisory_lock(int, int)) is a separate key space from the
// one-bigint form ClaimCampaignVersion uses, so the two can never collide.
const statsJobLeaseClass int32 = 0x5853544A

// statsJobLeasePingTimeout bounds the liveness check of a held lease's connection.
const statsJobLeasePingTimeout = 2 * time.Second

// StatsJobLease implements domain.StatsJobLease with SESSION advisory locks, one per X ad account,
// all held on ONE dedicated Postgres session for as long as this process owns the accounts.
//
// ONE SESSION OUTSIDE THE POOL. The session is opened with pgx.ConnectConfig from the business
// pool's own connection config (same DSN, credentials and TLS), not checked out of the pool:
// a lease held for the life of the process must not pin business-pool connections, or a small
// pool (pool_max_conns=1 is a real configuration here) and a few X accounts would starve ordinary
// requests and the readiness probe. Session advisory locks are per session and any number of keys
// can share one, so every account this pod owns costs nothing beyond that one connection. The
// service therefore holds ONE Postgres connection beyond its pool while it owns any X account.
//
// Session-level, not transaction-level, because ownership must outlast any one request: the
// lock is taken once and kept, so the same pod keeps owning the account (its in-memory job
// budget and pacer stay authoritative) instead of ownership flapping per request. Another pod's
// pg_try_advisory_lock fails while this session lives, so only one pod runs X stats jobs per
// account.
//
// LEASE LOSS. Every lock lives exactly as long as the session. A failover, a terminated backend
// or a severed connection drops ALL of them server-side, after which another pod can take any of
// them. So Own pings the session on every call: a dead session is closed, EVERY account is marked
// not held, and the session is reopened lazily and each account re-acquired with try-lock on its
// own next check — never assumed. New stats-job submissions therefore stop at the first call
// after a loss. A submission ALREADY admitted when the session dies is not recalled: this is
// admission control, not fencing, and the window is one read's job creation.
//
// The caller's context bounds what a request waits for; it never decides that the session is
// lost. The ping and the lock query run on detached contexts with their own timeout, so a
// cancelled or expired request cannot make a healthy session look dead; such a request is
// refused (ErrStatsJobLeaseUnavailable) and the leases are kept.
type StatsJobLease struct {
	pool *Pool

	// mu serializes every use of conn and held: a pgx.Conn is not safe for concurrent use.
	mu     sync.Mutex
	conn   *pgx.Conn
	held   map[string]bool
	closed bool
}

// NewStatsJobLease returns a lease that opens its session from pool's connection config. It
// holds nothing, and opens nothing, until Own is called.
func NewStatsJobLease(pool *Pool) *StatsJobLease {
	return &StatsJobLease{pool: pool, held: map[string]bool{}}
}

var _ domain.StatsJobLease = (*StatsJobLease)(nil)

// statsJobLeaseKey is the second lock key for accountID: FNV-1a over the id, folded to int32.
// A collision would only make two accounts share one owner, which is harmless.
func statsJobLeaseKey(accountID string) int32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(accountID))
	return int32(h.Sum32()) //nolint:gosec // a hash folded to the lock's int4 key, overflow intended
}

// Own implements domain.StatsJobLease. Only a lock Postgres reports as held elsewhere is
// ErrStatsJobLeaseNotHeld; everything else that prevents establishing ownership is
// ErrStatsJobLeaseUnavailable.
func (l *StatsJobLease) Own(ctx context.Context, accountID string) error {
	if l == nil || l.pool == nil || l.pool.Pool == nil {
		return fmt.Errorf("%w: no database to hold the lease on", domain.ErrStatsJobLeaseUnavailable)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return fmt.Errorf("%w: shutting down", domain.ErrStatsJobLeaseUnavailable)
	}
	if l.conn != nil {
		pingCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), statsJobLeasePingTimeout)
		err := l.conn.Ping(pingCtx)
		cancel()
		switch {
		case err != nil && ctx.Err() != nil:
			// A failed ping on a request that has already ended proves nothing about the
			// session. Keep every lease; the next live check decides.
			return fmt.Errorf("%w: %w", domain.ErrStatsJobLeaseUnavailable, ctx.Err())
		case err != nil:
			slog.WarnContext(ctx, "x stats-job lease session failed its liveness check; every lease on it is treated as lost",
				"accounts", len(l.held), "error", err)
			l.dropSession(ctx)
		case l.held[accountID]:
			if cerr := ctx.Err(); cerr != nil {
				// Still the owner, but this request is over: it must not go on to create jobs.
				return fmt.Errorf("%w: %w", domain.ErrStatsJobLeaseUnavailable, cerr)
			}
			return nil
		}
	}
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("%w: %w", domain.ErrStatsJobLeaseUnavailable, cerr)
	}
	if l.conn == nil {
		conn, err := pgx.ConnectConfig(ctx, l.pool.Config().ConnConfig.Copy())
		if err != nil {
			// pgx's connect error can name the host; it is logged by the caller's classifier
			// through safeErrSummary, never sent to a client.
			return fmt.Errorf("%w: open the lease session: %w", domain.ErrStatsJobLeaseUnavailable, err)
		}
		l.conn = conn
	}
	lockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), statsJobLeasePingTimeout)
	var acquired bool
	err := l.conn.QueryRow(lockCtx, "SELECT pg_try_advisory_lock($1, $2)", statsJobLeaseClass, statsJobLeaseKey(accountID)).Scan(&acquired)
	cancel()
	if err != nil {
		// The session's state is unknown (the lock may even have been granted): drop it, and
		// with it every lease, rather than guess. Each account re-acquires on its next check.
		l.dropSession(ctx)
		return fmt.Errorf("%w: try the lock: %w", domain.ErrStatsJobLeaseUnavailable, err)
	}
	if !acquired {
		// Postgres answered "held elsewhere". The ONLY ErrStatsJobLeaseNotHeld.
		return domain.ErrStatsJobLeaseNotHeld
	}
	l.held[accountID] = true
	return nil
}

// dropSession closes the lease session (Postgres drops every advisory lock on it) and forgets
// every held account. Caller holds mu.
func (l *StatsJobLease) dropSession(ctx context.Context) {
	if l.conn != nil {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lockReleaseTimeout)
		if err := l.conn.Close(closeCtx); err != nil && !errors.Is(err, context.Canceled) {
			slog.WarnContext(ctx, "failed to close x stats-job lease session", "error", err)
		}
		cancel()
		l.conn = nil
	}
	l.held = map[string]bool{}
}

// Close releases every lease by closing the session, and makes later Own calls refuse.
func (l *StatsJobLease) Close(ctx context.Context) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	l.dropSession(ctx)
}

// StatsJobLeaseLockKeys returns the two advisory-lock keys the lease takes for accountID, as they
// appear in pg_locks (classid, objid with objsubid 2) — for operators finding the owning backend,
// and for the live tests.
func StatsJobLeaseLockKeys(accountID string) (class, key int32) {
	return statsJobLeaseClass, statsJobLeaseKey(accountID)
}
