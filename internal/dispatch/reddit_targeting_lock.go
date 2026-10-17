// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// SetKeywordTargetingWriteLock binds the cross-replica lock that serializes Reddit's
// whole-targeting writes per ad group (see domain.KeywordTargetingWriteLock). The container binds
// the Postgres advisory lock on both wiring paths; until then — and in direct-construction tests —
// the dispatcher uses an in-process lock, which serializes writes within this process only.
func (d *RedditDispatcher) SetKeywordTargetingWriteLock(l domain.KeywordTargetingWriteLock) {
	if l != nil {
		d.targetingLock = l
	}
}

// redditTargetingLockScope is the lock scope of one ad group's targeting. The ad account is part
// of it because every ad group request is addressed under it (/ad_accounts/{account}/ad_groups/
// {id}); Reddit does not document ad group ids as unique across accounts, and a narrower scope
// costs nothing.
func redditTargetingLockScope(accountID, adGroupID string) string {
	return "reddit/" + accountID + "/ad_groups/" + adGroupID
}

// lockRedditTargeting takes the ad group's write lock. The caller holds it across the fresh read,
// the revision compare, the PATCH and the confirming re-read, and defers the returned unlock. A
// refusal here is definite: nothing was read for the write and nothing was sent.
//
// The wait is reported to the caller's lock-wait recorder (domain.RecordLockWait), so the
// orchestrator can keep it out of Reddit's upstream latency. Any other lock failure — the database,
// or the request ending mid-wait (its context's error stays wrapped) — is tagged
// domain.ErrKeywordTargetingLockUnavailable, so no refusal at the lock is mistaken for a Reddit
// failure.
func (d *RedditDispatcher) lockRedditTargeting(ctx context.Context, op, accountID, adGroupID string) (func(), error) {
	start := time.Now()
	unlock, err := d.targetingLock.Lock(ctx, redditTargetingLockScope(accountID, adGroupID))
	domain.RecordLockWait(ctx, time.Since(start))
	if err != nil {
		if !errors.Is(err, domain.ErrKeywordTargetingWriteInProgress) {
			err = fmt.Errorf("%w: %w", domain.ErrKeywordTargetingLockUnavailable, err)
		}
		return nil, fmt.Errorf("%s: ad group %s: %w", op, adGroupID, err)
	}
	return unlock, nil
}

// localTargetingLock is the in-process KeywordTargetingWriteLock: one one-slot semaphore per
// scope, created on first use and dropped when nobody holds or waits on it.
type localTargetingLock struct {
	mu    sync.Mutex
	slots map[string]*localTargetingSlot
	wait  time.Duration
}

type localTargetingSlot struct {
	ch   chan struct{}
	refs int
}

func newLocalTargetingLock(wait time.Duration) *localTargetingLock {
	return &localTargetingLock{slots: map[string]*localTargetingSlot{}, wait: wait}
}

var _ domain.KeywordTargetingWriteLock = (*localTargetingLock)(nil)

// Lock implements domain.KeywordTargetingWriteLock within this process.
func (l *localTargetingLock) Lock(ctx context.Context, scope string) (func(), error) {
	l.mu.Lock()
	s := l.slots[scope]
	if s == nil {
		s = &localTargetingSlot{ch: make(chan struct{}, 1)}
		l.slots[scope] = s
	}
	s.refs++
	l.mu.Unlock()

	t := time.NewTimer(l.wait)
	defer t.Stop()
	select {
	case s.ch <- struct{}{}:
		var once sync.Once
		return func() {
			once.Do(func() {
				<-s.ch
				l.drop(scope, s)
			})
		}, nil
	case <-t.C:
		l.drop(scope, s)
		return nil, domain.ErrKeywordTargetingWriteInProgress
	case <-ctx.Done():
		l.drop(scope, s)
		return nil, fmt.Errorf("wait for keyword targeting lock: %w", ctx.Err())
	}
}

func (l *localTargetingLock) drop(scope string, s *localTargetingSlot) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s.refs--
	if s.refs == 0 && l.slots[scope] == s {
		delete(l.slots, scope)
	}
}
