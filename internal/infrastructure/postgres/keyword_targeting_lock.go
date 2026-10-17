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

// keywordTargetingLockClass is the first key of the two-int advisory lock a keyword-targeting
// write takes: "RKTW". The two-int form is a separate key space from the one-bigint form
// ClaimCampaignVersion and the binding lock use, and the class keeps it apart from the X
// stats-job lease ("XSTJ"), so none of them can collide.
const keywordTargetingLockClass int32 = 0x524B5457

const (
	// keywordTargetingLockPoll is the gap between try-lock attempts while waiting.
	keywordTargetingLockPoll = 100 * time.Millisecond
	// keywordTargetingUnlockTimeout bounds the unlock round-trip, on a context detached from the
	// request so a cancelled request still unlocks.
	keywordTargetingUnlockTimeout = 5 * time.Second
)

const (
	keywordTargetingLockSQL   = "SELECT pg_try_advisory_lock($1, $2)"
	keywordTargetingUnlockSQL = "SELECT pg_advisory_unlock($1, $2)"
)

// KeywordTargetingWriteLock implements domain.KeywordTargetingWriteLock with SESSION advisory
// locks on pooled connections — the mechanism ClaimCampaignVersion uses — so a Reddit
// whole-targeting write (fresh read, revision compare, PATCH, re-read) runs one at a time per ad
// group across every replica.
//
// TRY AND POLL, never a blocking pg_advisory_lock. A blocked waiter would pin a pooled connection
// for as long as the holder's platform calls run; polling with pg_try_advisory_lock returns the
// connection between attempts, so contention costs ONE held connection per ad group being written
// (the holder's), not one per waiting request. The wait is bounded (domain.KeywordTargetingLockWait)
// and EVERY attempt — the pool checkout included — is cut off at that deadline, so a saturated pool
// cannot stretch the wait to the caller's own (much longer) mutation timeout. When the wait runs
// out — the lock held elsewhere, or no pooled connection to ask with — the write is refused with
// ErrKeywordTargetingWriteInProgress (a retryable 409) and nothing is sent.
//
// POOL COST — the holder occupies one pooled connection for the length of its Reddit calls. The
// lock is a session lock, so the connection that took it must stay checked out from the fresh read
// through the PATCH and the re-read: up to three Reddit requests, bounded by the orchestrator's 45s
// mutation timeout. That is exactly what a toggle's campaign claim (ClaimCampaignVersion) costs.
// Concurrent writes to DIFFERENT ad groups take one pool slot each, and enough of them starve
// ordinary reads and the readiness probe — with pool_max_conns=1 a single write holds the pool's
// only connection, and every other request (a second write included, which is refused at its
// deadline) waits for it. What bounds it is that each holder is one request writing one ad group,
// behind a default-off flag (REDDIT_KEYWORD_TARGETING_WRITES_ENABLED); it is not bounded
// structurally.
//
// WHY NOT A DEDICATED SESSION, as StatsJobLease uses. That lease holds a few long-lived locks on
// ONE session outside the pool; here locks are short and per request. Sharing one session means
// every lock, poll and unlock across all in-flight writes serializes through it (a pgx.Conn is not
// safe for concurrent use), and losing it drops every holder's lock at once — the lease's whole
// recovery machinery for a lock that lives for seconds. A session opened per write instead adds a
// connect (and TLS handshake) to every write and an UNBOUNDED number of connections beyond the
// pool, outside pool_max_conns, against the server's connection limit. Both trade a bounded,
// visible pool cost for an unbounded or shared one, so the claim's pooled-connection design is
// kept; a reserved sub-pool or a lease that holds no connection across platform I/O is the same
// tracked design change ClaimCampaignVersion's comment names.
//
// DURABILITY BOUNDARY, as for ClaimCampaignVersion: the lock lives as long as its session, so a
// severed connection releases it server-side while the holder may still be mid-write. It is a
// contention guard that makes the ordinary case one LFX writer at a time; the post-write re-read
// is what still reports a write that did not land as intended as UNCONFIRMED.
//
// KEYS, NOT SCOPES. A scope is folded to a 32-bit FNV key; two scopes that collide share one
// lock, which only ever over-serializes — it never lets two writers on one scope through.
type KeywordTargetingWriteLock struct {
	pool *Pool
	wait time.Duration
	poll time.Duration
	// lockSQL is the try-lock statement (keywordTargetingLockSQL). A field only so an in-package
	// test can make it fail AFTER the lock is granted.
	lockSQL string
}

// NewKeywordTargetingWriteLock returns the lock on pool with the production wait
// (domain.KeywordTargetingLockWait) and poll.
func NewKeywordTargetingWriteLock(pool *Pool) *KeywordTargetingWriteLock {
	return NewKeywordTargetingWriteLockWithWait(pool, domain.KeywordTargetingLockWait, keywordTargetingLockPoll)
}

// NewKeywordTargetingWriteLockWithWait is NewKeywordTargetingWriteLock with the acquisition wait
// and poll interval supplied, for the live tests.
func NewKeywordTargetingWriteLockWithWait(pool *Pool, wait, poll time.Duration) *KeywordTargetingWriteLock {
	return &KeywordTargetingWriteLock{pool: pool, wait: wait, poll: poll, lockSQL: keywordTargetingLockSQL}
}

var _ domain.KeywordTargetingWriteLock = (*KeywordTargetingWriteLock)(nil)

// keywordTargetingLockKey is the second lock key for scope: FNV-1a over it, folded to int32.
func keywordTargetingLockKey(scope string) int32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(scope))
	return int32(h.Sum32()) //nolint:gosec // a hash folded to the lock's int4 key, overflow intended
}

// KeywordTargetingLockKeys returns the two advisory-lock keys Lock takes for scope, as they
// appear in pg_locks (classid, objid), for the live tests.
func KeywordTargetingLockKeys(scope string) (int32, int32) {
	return keywordTargetingLockClass, keywordTargetingLockKey(scope)
}

// Lock implements domain.KeywordTargetingWriteLock. See the type comment.
func (l *KeywordTargetingWriteLock) Lock(ctx context.Context, scope string) (func(), error) {
	if l == nil || l.pool == nil || l.pool.Pool == nil {
		return nil, errors.New("keyword targeting lock: no database pool")
	}
	key := keywordTargetingLockKey(scope)
	deadline := time.Now().Add(l.wait)
	for {
		conn, got, err := l.try(ctx, deadline, scope, key)
		if err != nil {
			return nil, err
		}
		if got {
			return l.releaser(ctx, conn, scope, key), nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, domain.ErrKeywordTargetingWriteInProgress
		}
		// Sleep at most up to the deadline, so the last attempt is made AT it.
		t := time.NewTimer(min(l.poll, remaining))
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, fmt.Errorf("wait for keyword targeting lock: %w", ctx.Err())
		case <-t.C:
		}
	}
}

// try makes one pg_try_advisory_lock attempt on a freshly checked-out connection, the checkout
// and the query both cut off at deadline. It returns the connection only when the lock was
// granted; otherwise the connection is back in the pool (or destroyed, below).
//
// An attempt cut off by the WAIT's deadline — not by the caller's own context — is the wait
// running out: ErrKeywordTargetingWriteInProgress, as when the lock is seen held at the deadline.
// That is what keeps a saturated pool's refusal a bounded, retryable 409 instead of a 503 at the
// caller's mutation timeout.
func (l *KeywordTargetingWriteLock) try(ctx context.Context, deadline time.Time, scope string, key int32) (*pgxpool.Conn, bool, error) {
	actx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	waitRanOut := func() bool { return ctx.Err() == nil && actx.Err() != nil }

	conn, err := l.pool.Acquire(actx)
	if err != nil {
		if waitRanOut() {
			return nil, false, domain.ErrKeywordTargetingWriteInProgress
		}
		return nil, false, fmt.Errorf("acquire connection for keyword targeting lock: %w", err)
	}
	var got bool
	if err := conn.QueryRow(actx, l.lockSQL, keywordTargetingLockClass, key).Scan(&got); err != nil {
		// The client saw a failure, but Postgres may have granted the lock: returning this
		// connection to the pool would strand the lock on it. Destroy it instead — also when the
		// query was only cut short by the deadline.
		destroyKeywordTargetingLockConn(ctx, conn, scope, "failed to close keyword targeting lock connection after a failed try-lock")
		conn.Release()
		if waitRanOut() {
			return nil, false, domain.ErrKeywordTargetingWriteInProgress
		}
		return nil, false, fmt.Errorf("try keyword targeting lock: %w", err)
	}
	if !got {
		// A definite "held elsewhere": nothing is held on this session.
		conn.Release()
		return nil, false, nil
	}
	return conn, true, nil
}

// releaser returns the idempotent unlock for a granted lock held on conn.
func (l *KeywordTargetingWriteLock) releaser(ctx context.Context, conn *pgxpool.Conn, scope string, key int32) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), keywordTargetingUnlockTimeout)
			defer cancel()
			var unlocked bool
			err := conn.QueryRow(uctx, keywordTargetingUnlockSQL, keywordTargetingLockClass, key).Scan(&unlocked)
			if err != nil || !unlocked {
				// A session advisory lock survives the connection's return to the pool, so a
				// connection whose unlock did not succeed is destroyed, never reused: Postgres
				// drops the lock with the session.
				slog.WarnContext(ctx, "keyword targeting lock was not released cleanly; destroying its connection",
					"scope", scope, "unlocked", unlocked, "error", err)
				destroyKeywordTargetingLockConn(ctx, conn, scope, "failed to close keyword targeting lock connection after unlock failure")
			}
			conn.Release()
		})
	}
}

// destroyKeywordTargetingLockConn closes conn's underlying session so the pool discards it.
func destroyKeywordTargetingLockConn(ctx context.Context, conn *pgxpool.Conn, scope, msg string) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), keywordTargetingUnlockTimeout)
	defer cancel()
	if err := conn.Conn().Close(cctx); err != nil {
		slog.WarnContext(ctx, msg, "scope", scope, "error", err)
	}
}
