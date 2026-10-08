// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sync/atomic"
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

// statsJobLeaseLockSQL tries one account's lock. statsJobLeaseUnlockSQL releases it.
const (
	statsJobLeaseLockSQL   = "SELECT pg_try_advisory_lock($1, $2)"
	statsJobLeaseUnlockSQL = "SELECT pg_advisory_unlock($1, $2)"
)

// statsJobLeaseConnectTimeout bounds opening the lease session. The connect runs holding the
// lease's slot, so an unbounded one (a black-holed database host, a DSN with no connect_timeout,
// a caller context with no deadline) would stall every X stats-job admission check until the OS
// TCP timeout.
const statsJobLeaseConnectTimeout = 5 * time.Second

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
// lost. The ping and the lock query run on contexts detached from the request (derived from the
// lease's own context, cancelled only by Close) with their own timeout, so a cancelled or expired
// request cannot make a healthy session look dead; such a request is refused
// (ErrStatsJobLeaseUnavailable) and the leases are kept.
//
// KEYS, NOT ACCOUNTS. Ownership is tracked by advisory-lock KEY (statsJobLeaseKey, a 32-bit FNV
// folded into the two-int lock's second key). Two account ids can hash to the same key; they then
// share ONE lock, so they share one owner on every pod, consistently — Postgres decides by key.
// Tracking by key makes that explicit: an account whose key this session already holds is owned
// with no further lock call, and a lock attempt — and therefore the recovery unlock after a
// failed attempt — only ever happens for a key this session did NOT hold before the attempt. The
// unlock can never release a lock some other account's ownership rests on.
//
// ADMISSION AND SHUTDOWN. One caller at a time uses the session, through a one-slot channel
// semaphore every caller waits on with its OWN context, so neither a request nor Close can be
// held past its deadline by another caller's database work. Close marks the lease closed,
// cancels the lease context — aborting any in-flight connect, ping or lock query — and closes the
// session if it gets the slot before its deadline; if it does not, the in-flight caller sees
// `closed` on its way out and closes the session itself. Whoever holds the slot is the only one
// touching conn, and closing sets it to nil, so the session is closed exactly once.
type StatsJobLease struct {
	pool *Pool

	// sem is the one-slot semaphore guarding conn and heldKeys: a pgx.Conn is not safe for
	// concurrent use. Acquired with the caller's context (acquireSlot).
	sem      chan struct{}
	conn     *pgx.Conn
	heldKeys map[int32]bool

	// closed is set by Close before anything else; leaseCtx is cancelled by Close, and every
	// database call Own makes derives from it.
	closed      atomic.Bool
	leaseCtx    context.Context
	leaseCancel context.CancelFunc

	// connectTimeout bounds opening conn (statsJobLeaseConnectTimeout; shortened by tests).
	connectTimeout time.Duration
	// beforeRelease, nil outside tests, runs between Own's last check and releaseSlot.
	beforeRelease func()
	// lockSQL is the try-lock statement (statsJobLeaseLockSQL). A field only so an in-package
	// test can make it fail AFTER the lock is granted.
	lockSQL string
	// key maps an account to its lock key (statsJobLeaseKey). A field only so an in-package test
	// can force two accounts to collide.
	key func(string) int32
	// ping and closeConn are pgx.Conn.Ping and Close. Fields only so in-package tests can make
	// them slow.
	ping      func(context.Context, *pgx.Conn) error
	closeConn func(context.Context, *pgx.Conn) error
}

// NewStatsJobLease returns a lease that opens its session from pool's connection config. It
// holds nothing, and opens nothing, until Own is called.
func NewStatsJobLease(pool *Pool) *StatsJobLease {
	leaseCtx, leaseCancel := context.WithCancel(context.Background())
	return &StatsJobLease{
		pool: pool, sem: make(chan struct{}, 1), heldKeys: map[int32]bool{},
		leaseCtx: leaseCtx, leaseCancel: leaseCancel,
		connectTimeout: statsJobLeaseConnectTimeout,
		lockSQL:        statsJobLeaseLockSQL,
		key:            statsJobLeaseKey,
		ping:           func(ctx context.Context, c *pgx.Conn) error { return c.Ping(ctx) },
		closeConn:      func(ctx context.Context, c *pgx.Conn) error { return c.Close(ctx) },
	}
}

var _ domain.StatsJobLease = (*StatsJobLease)(nil)

// statsJobLeaseKey is the second lock key for accountID: FNV-1a over the id, folded to int32.
// Colliding ids share one lock and so one owner (see KEYS, NOT ACCOUNTS above).
func statsJobLeaseKey(accountID string) int32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(accountID))
	return int32(h.Sum32()) //nolint:gosec // a hash folded to the lock's int4 key, overflow intended
}

// unavailable wraps ErrStatsJobLeaseUnavailable with a cause.
func unavailable(cause error) error {
	return fmt.Errorf("%w: %w", domain.ErrStatsJobLeaseUnavailable, cause)
}

var errLeaseClosed = errors.New("shutting down")

// acquireSlot takes the one-slot semaphore, giving up when ctx ends or the lease is closed.
func (l *StatsJobLease) acquireSlot(ctx context.Context) error {
	select {
	case l.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-l.leaseCtx.Done():
		return errLeaseClosed
	}
}

// releaseSlot hands the slot back. If the lease was closed while the slot was held, the holder
// closes the session on its way out — Close may have given up waiting for the slot. It reports
// whether it did, so Own can refuse ownership on the SAME reading of closed that decided the
// session's fate: a success can never be returned for a session this call itself closed. (If
// Close begins after that reading, Close closes the session once it gets the slot — after Own has
// returned, which is ordinary shutdown of an already-admitted request.)
func (l *StatsJobLease) releaseSlot() (closedSession bool) {
	if l.closed.Load() {
		closedSession = true
		if l.conn != nil {
			closeCtx, cancel := context.WithTimeout(context.Background(), statsJobLeasePingTimeout)
			l.closeSession(closeCtx, closeCtx)
			cancel()
		}
	}
	<-l.sem
	return closedSession
}

// dbCtx is a context for one database step: detached from the request, cancelled by Close, and
// bounded by d.
func (l *StatsJobLease) dbCtx(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(l.leaseCtx, d)
}

// Own implements domain.StatsJobLease. Only a lock Postgres reports as held elsewhere is
// ErrStatsJobLeaseNotHeld; everything else that prevents establishing ownership is
// ErrStatsJobLeaseUnavailable.
func (l *StatsJobLease) Own(ctx context.Context, accountID string) (err error) {
	if l == nil || l.pool == nil || l.pool.Pool == nil {
		return fmt.Errorf("%w: no database to hold the lease on", domain.ErrStatsJobLeaseUnavailable)
	}
	if l.closed.Load() {
		return unavailable(errLeaseClosed)
	}
	if err := l.acquireSlot(ctx); err != nil {
		return unavailable(err)
	}
	defer func() {
		if l.beforeRelease != nil {
			l.beforeRelease() // test hook: the gap between admit and releaseSlot
		}
		if l.releaseSlot() && err == nil {
			err = unavailable(errLeaseClosed)
		}
	}()
	// A request that has already ended does no database work at all: no ping, no lock. Every
	// lease is kept.
	if cerr := ctx.Err(); cerr != nil {
		return unavailable(cerr)
	}
	if l.closed.Load() {
		return unavailable(errLeaseClosed)
	}
	key := l.key(accountID)
	if l.conn != nil {
		pingCtx, cancel := l.dbCtx(statsJobLeasePingTimeout)
		err := l.ping(pingCtx, l.conn)
		cancel()
		switch {
		case err != nil && l.closed.Load():
			return unavailable(errLeaseClosed) // releaseSlot closes the session
		case err != nil && ctx.Err() != nil:
			// A failed ping on a request that has ended meanwhile proves nothing about the
			// session. Keep every lease; the next live check decides.
			return unavailable(ctx.Err())
		case err != nil:
			slog.WarnContext(ctx, "x stats-job lease session failed its liveness check; every lease on it is treated as lost",
				"locks", len(l.heldKeys), "error", err)
			l.dropSession(ctx)
		case l.heldKeys[key]:
			return l.admit(ctx)
		}
	}
	if cerr := ctx.Err(); cerr != nil {
		return unavailable(cerr)
	}
	if l.conn == nil {
		connCtx, cancel := l.dbCtx(l.connectTimeout)
		stop := context.AfterFunc(ctx, cancel) // a request that ends stops waiting for the connect
		conn, err := pgx.ConnectConfig(connCtx, l.pool.Config().ConnConfig.Copy())
		stop()
		cancel()
		if err != nil {
			// pgx's connect error can name the host; it is logged by the caller's classifier
			// through safeErrSummary, never sent to a client.
			return fmt.Errorf("%w: open the lease session: %w", domain.ErrStatsJobLeaseUnavailable, err)
		}
		l.conn = conn
		if l.closed.Load() {
			return unavailable(errLeaseClosed) // releaseSlot closes the new session
		}
	}
	// Reached only for a key this session does not hold, so the recovery unlock below can only
	// ever release a lock this attempt itself may have taken.
	lockCtx, cancel := l.dbCtx(statsJobLeasePingTimeout)
	var acquired bool
	lockErr := l.conn.QueryRow(lockCtx, l.lockSQL, statsJobLeaseClass, key).Scan(&acquired)
	cancel()
	if lockErr != nil {
		if l.closed.Load() {
			return unavailable(errLeaseClosed) // closing the session drops whatever was granted
		}
		return l.recoverFailedLock(ctx, key, lockErr)
	}
	if !acquired {
		// Postgres answered "held elsewhere". The ONLY ErrStatsJobLeaseNotHeld.
		return domain.ErrStatsJobLeaseNotHeld
	}
	// Tracked even if this request is refused below: the lock IS held by this session, and an
	// untracked one would be orphaned.
	l.heldKeys[key] = true
	return l.admit(ctx)
}

// admit is the last step of every successful Own: ownership is reported only if the request is
// still live and Close has not begun. The database steps are detached from the request, so it
// can end during a successful connect, ping or lock query; and Close may mark the lease closed
// while Own holds the slot, after which releaseSlot closes the session and drops every lock —
// reporting ownership then would let the caller create jobs after another pod takes the account.
// Either way the lease state is kept as it is; only this request is refused.
func (l *StatsJobLease) admit(ctx context.Context) error {
	if l.closed.Load() {
		return unavailable(errLeaseClosed)
	}
	if cerr := ctx.Err(); cerr != nil {
		return unavailable(cerr)
	}
	return nil
}

// recoverFailedLock handles a failed try-lock for a key this session did NOT hold before the
// attempt. The session is shared by every other key this pod holds, so it is dropped only if it
// is actually dead: dropping a live one would release unrelated leases while their admitted jobs
// are in flight, and let another pod take those accounts.
//
//   - Ping (detached, bounded). A failed ping is a genuine loss: drop the session, every lease
//     with it.
//   - Otherwise the session is alive but the statement failed after an unknown point — the lock
//     may have been GRANTED before the error (a timeout mid-statement, an error raised after the
//     lock function ran; a session lock survives the statement's abort). Release it with
//     pg_advisory_unlock for this key, which is a harmless false when it was not granted, so no
//     untracked lock remains. Safe because the key was not held before the attempt (Own checks
//     heldKeys first), so the lock it releases, if any, is the one this attempt took. If even
//     that fails, the session's state is unknown and it is dropped after all.
//
// Either way this account answers Unavailable. Caller holds the slot.
func (l *StatsJobLease) recoverFailedLock(ctx context.Context, key int32, lockErr error) error {
	pingCtx, cancel := l.dbCtx(statsJobLeasePingTimeout)
	pingErr := l.ping(pingCtx, l.conn)
	cancel()
	if pingErr != nil {
		slog.WarnContext(ctx, "x stats-job lease session died during a lock attempt; every lease on it is treated as lost",
			"locks", len(l.heldKeys), "error", pingErr)
		l.dropSession(ctx)
		return fmt.Errorf("%w: try the lock: %w", domain.ErrStatsJobLeaseUnavailable, lockErr)
	}
	unlockCtx, cancel := l.dbCtx(statsJobLeasePingTimeout)
	_, unlockErr := l.conn.Exec(unlockCtx, statsJobLeaseUnlockSQL, statsJobLeaseClass, key)
	cancel()
	if unlockErr != nil {
		slog.WarnContext(ctx, "x stats-job lease could not clear a possibly-granted lock after a failed attempt; dropping the session",
			"locks", len(l.heldKeys), "error", unlockErr)
		l.dropSession(ctx)
	}
	return fmt.Errorf("%w: try the lock: %w", domain.ErrStatsJobLeaseUnavailable, lockErr)
}

// dropSession closes the lease session (Postgres drops every advisory lock on it) and forgets
// every held key. Caller holds the slot.
//
// On the request path the close runs detached, bounded by lockReleaseTimeout: a request ending
// must not leave a dead session half-closed.
func (l *StatsJobLease) dropSession(ctx context.Context) {
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lockReleaseTimeout)
	defer cancel()
	l.closeSession(ctx, closeCtx)
}

// closeSession closes conn under closeCtx and forgets every held key. Caller holds the slot.
func (l *StatsJobLease) closeSession(logCtx, closeCtx context.Context) {
	if l.conn != nil {
		if err := l.closeConn(closeCtx, l.conn); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			slog.WarnContext(logCtx, "failed to close x stats-job lease session", "error", err)
		}
		l.conn = nil
	}
	l.heldKeys = map[int32]bool{}
}

// Close releases every lease by closing the session, and makes later Own calls refuse.
//
// It returns within ctx's deadline: Container.Close calls it inside the shared shutdown budget
// (statsLeaseCloseTimeout in internal/container), and a close that ran past it would push the
// process past DefaultShutdownTimeout and into a SIGKILL mid-drain. It marks the lease closed and
// cancels the lease context first, which aborts an in-flight Own's database step; then it waits
// for the slot with ctx. Given the slot, it closes the session under ctx (bounded by
// lockReleaseTimeout when ctx has no deadline). Not given it in time, it returns, and the caller
// still holding the slot closes the session on its way out. If a close is cut short, the socket
// is still closed, and Postgres drops the session's advisory locks when it notices.
func (l *StatsJobLease) Close(ctx context.Context) {
	if l == nil {
		return
	}
	l.closed.Store(true)
	l.leaseCancel()
	closeCtx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		closeCtx, cancel = context.WithTimeout(ctx, lockReleaseTimeout)
		defer cancel()
	}
	// Take a FREE slot first, without consulting ctx: a select with both cases ready picks at
	// random, so an already-expired ctx could otherwise return with the slot free and the
	// session left open with no holder to close it. Only a slot held by an in-flight Own is
	// waited for, and only until ctx is done.
	select {
	case l.sem <- struct{}{}:
	default:
		select {
		case l.sem <- struct{}{}:
		case <-closeCtx.Done():
			return
		}
	}
	l.closeSession(ctx, closeCtx)
	<-l.sem
}

// StatsJobLeaseLockKeys returns the two advisory-lock keys the lease takes for accountID, as they
// appear in pg_locks (classid, objid with objsubid 2) — for operators finding the owning backend,
// and for the live tests.
func StatsJobLeaseLockKeys(accountID string) (class, key int32) {
	return statsJobLeaseClass, statsJobLeaseKey(accountID)
}
