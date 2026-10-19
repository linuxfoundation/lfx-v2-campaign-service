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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
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
func (d *RedditDispatcher) lockRedditTargeting(ctx context.Context, op, accountID, adGroupID string) (domain.KeywordTargetingLockHold, error) {
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

// releaseRedditTargeting ends a write's hold on its ad group's lock, by the write's OUTCOME:
//
//   - SETTLED (nothing was sent, a definite refusal, every item APPLIED or ALREADY_PRESENT): release
//     now.
//   - UNSETTLED (the PATCH was sent and its outcome is UNCONFIRMED — a transport error, timeout or
//     cancel after the send, or a re-read that could not confirm): keep the lock for the
//     cooldown, detached from the request (ReleaseAfterCooldown). Reddit may still be applying the
//     write; released at once, the next LFX write would read the targeting as it is now, pass its
//     revision check, write — and then be overwritten by this one landing late from ITS stale
//     snapshot (a delayed negative add restoring a removed keyword, or a delayed removal erasing a
//     confirmed negative). The cooldown narrows that window; it cannot close it, because nothing
//     tells this service when, or whether, Reddit finishes a request it stopped waiting for.
func (d *RedditDispatcher) releaseRedditTargeting(hold domain.KeywordTargetingLockHold, unsettled bool) {
	if unsettled {
		hold.ReleaseAfterCooldown(d.targetingCooldown)
		return
	}
	hold.Release()
}

// redditWriteUnsettled reports whether a whole-targeting write's result leaves its outcome
// UNCONFIRMED: the error says so, or any per-item outcome does (the negative add answers per item
// with a nil error when some keywords were ALREADY_PRESENT, so the error alone is not enough).
func redditWriteUnsettled(err error, outcomes ...string) bool {
	var u interface{ Unconfirmed() bool }
	if errors.As(err, &u) && u.Unconfirmed() {
		return true
	}
	for _, o := range outcomes {
		if o == model.KeywordOutcomeUnconfirmed {
			return true
		}
	}
	return false
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
func (l *localTargetingLock) Lock(ctx context.Context, scope string) (domain.KeywordTargetingLockHold, error) {
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
		return &localTargetingHold{release: func() {
			<-s.ch
			l.drop(scope, s)
		}}, nil
	case <-t.C:
		l.drop(scope, s)
		return nil, domain.ErrKeywordTargetingWriteInProgress
	case <-ctx.Done():
		l.drop(scope, s)
		return nil, fmt.Errorf("wait for keyword targeting lock: %w", ctx.Err())
	}
}

// localTargetingHold is one granted in-process lock; exactly one release takes effect. A cooling
// hold ends with the process at the latest — the lock is in memory.
type localTargetingHold struct {
	once    sync.Once
	release func()
}

func (h *localTargetingHold) Release() { h.once.Do(h.release) }

func (h *localTargetingHold) ReleaseAfterCooldown(cooldown time.Duration) {
	time.AfterFunc(cooldown, h.Release)
}

func (l *localTargetingLock) drop(scope string, s *localTargetingSlot) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s.refs--
	if s.refs == 0 && l.slots[scope] == s {
		delete(l.slots, scope)
	}
}
