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
	// keywordTargetingLockWait bounds how long a write waits for another LFX write on the same
	// scope before it is refused (domain.ErrKeywordTargetingWriteInProgress, 409). The holder's
	// own critical section is three platform requests inside the mutation timeout; a waiter that
	// outlasted this would rarely still fit its own write inside that timeout.
	keywordTargetingLockWait = 10 * time.Second
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
// (the holder's), not one per waiting request. The wait is bounded (wait); when it runs out with
// the lock still held elsewhere the write is refused with ErrKeywordTargetingWriteInProgress and
// nothing is sent.
//
// POOL COST. The holder keeps its connection checked out across its platform calls (bounded by the
// orchestrator's mutation timeout), exactly as a toggle's campaign claim does; concurrent writes
// to DIFFERENT ad groups cost one pool slot each.
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
}

// NewKeywordTargetingWriteLock returns the lock on pool with the production wait and poll.
func NewKeywordTargetingWriteLock(pool *Pool) *KeywordTargetingWriteLock {
	return &KeywordTargetingWriteLock{pool: pool, wait: keywordTargetingLockWait, poll: keywordTargetingLockPoll}
}

// NewKeywordTargetingWriteLockWithWait is NewKeywordTargetingWriteLock with the acquisition wait
// and poll interval supplied, for the live tests.
func NewKeywordTargetingWriteLockWithWait(pool *Pool, wait, poll time.Duration) *KeywordTargetingWriteLock {
	return &KeywordTargetingWriteLock{pool: pool, wait: wait, poll: poll}
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
		conn, got, err := l.try(ctx, scope, key)
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

// try makes one pg_try_advisory_lock attempt on a freshly checked-out connection. It returns the
// connection only when the lock was granted; otherwise the connection is back in the pool.
func (l *KeywordTargetingWriteLock) try(ctx context.Context, scope string, key int32) (*pgxpool.Conn, bool, error) {
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquire connection for keyword targeting lock: %w", err)
	}
	var got bool
	if err := conn.QueryRow(ctx, keywordTargetingLockSQL, keywordTargetingLockClass, key).Scan(&got); err != nil {
		// The client saw a failure, but Postgres may have granted the lock: returning this
		// connection to the pool would strand the lock on it. Destroy it instead.
		destroyKeywordTargetingLockConn(ctx, conn, scope, "failed to close keyword targeting lock connection after a failed try-lock")
		conn.Release()
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
