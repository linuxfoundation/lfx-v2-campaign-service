// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"sync"
)

// keyedLocks serializes work per key within the process (see groupScheduleLocks). A semaphore (a
// 1-buffered channel) rather than a sync.Mutex so a waiter's context bounds its wait. Entries are
// reference-counted and removed when the last holder or waiter leaves.
type keyedLocks struct {
	mu sync.Mutex
	m  map[string]*keyedLock
}

type keyedLock struct {
	sem  chan struct{}
	refs int
}

func newKeyedLocks() *keyedLocks { return &keyedLocks{m: map[string]*keyedLock{}} }

// acquire blocks until key is held or ctx is done, returning the release func on success.
func (l *keyedLocks) acquire(ctx context.Context, key string) (func(), error) {
	l.mu.Lock()
	e, ok := l.m[key]
	if !ok {
		e = &keyedLock{sem: make(chan struct{}, 1)}
		l.m[key] = e
	}
	e.refs++
	l.mu.Unlock()

	drop := func() {
		l.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(l.m, key)
		}
		l.mu.Unlock()
	}
	select {
	case e.sem <- struct{}{}:
		return func() {
			<-e.sem
			drop()
		}, nil
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
}
