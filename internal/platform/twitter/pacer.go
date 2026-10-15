// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"sync"
	"time"
)

// writePacer is the write-pacing state pace and reserveStatsJobSlots share: next is the earliest
// instant the next write may be issued, guarded by mu (held across a pace wait, and across a
// whole stats-job batch reservation, so no other writer can interleave).
type writePacer struct {
	mu   sync.Mutex
	next time.Time
}

// AccountPacers hands out ONE writePacer per X ad account, so every Client built with it for the
// same account paces against the same state. X's 1-write/sec limit and its stats-job slots are
// properties of the ACCOUNT, but a process can hold several clients for one account — two
// projects whose connections point at the shared LF account (the client cache is keyed by
// project and connection row), or a cache replacement while a caller still holds the
// predecessor. Without a shared pacer, one of them could reserve and send mid-way through
// another's stats-job batch.
//
// The dispatcher owns one registry for the life of the process (TwitterDispatcher), so the scope
// is the PROCESS: replicas do not share it, which is why only the pod holding the per-account
// stats-job lease (domain.StatsJobLease) creates stats jobs, and the chart also refuses more than
// one replica while the stats-job features are on (templates/deployment.yaml). Entries are never evicted:
// there is one per ad account this process has written to, a small fixed set, and evicting one
// while a client still holds it would split the account's pacing again.
type AccountPacers struct {
	mu sync.Mutex
	m  map[string]*writePacer
	// media holds the per-(account, image) Media Library locks (mediaLocks); created lazily.
	media *mediaLocks
}

// NewAccountPacers returns an empty registry.
func NewAccountPacers() *AccountPacers {
	return &AccountPacers{m: map[string]*writePacer{}}
}

// forAccount returns the account's pacer, creating it on first use. key is the API base URL plus
// the account id: the base URL keeps two X endpoints (tests' stand-ins) from sharing state.
func (r *AccountPacers) forAccount(key string) *writePacer {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.m[key]
	if !ok {
		p = &writePacer{}
		r.m[key] = p
	}
	return p
}

// WithAccountPacers makes the client pace against the registry's pacer for its ad account
// instead of a private one. A client with no account id (account discovery, which never writes)
// keeps a private pacer. A nil registry is ignored.
func WithAccountPacers(r *AccountPacers) Option {
	return func(c *Client) {
		if r != nil {
			c.pacers = r
		}
	}
}

// mediaLocks serializes the Media Library find-or-upload for ONE (ad account, image) pair, so
// two dispatches carrying identical bytes to one account cannot both miss the lookup and both
// upload and add (media.go's ensureImageMedia). The lock spans lookup -> upload -> library add;
// the second holder's lookup then finds the first's entry.
//
// A semaphore (a 1-buffered channel) rather than a sync.Mutex so a waiter's context bounds its
// wait. Entries are reference-counted and removed when the last holder or waiter leaves, so the
// map holds only pairs with a dispatch in flight. The scope is the PROCESS, as for the pacer:
// replicas do not share it.
type mediaLocks struct {
	mu sync.Mutex
	m  map[string]*mediaLock
}

type mediaLock struct {
	sem  chan struct{}
	refs int
}

func newMediaLocks() *mediaLocks { return &mediaLocks{m: map[string]*mediaLock{}} }

// acquire blocks until key is held or ctx is done, returning the release func on success.
func (l *mediaLocks) acquire(ctx context.Context, key string) (func(), error) {
	l.mu.Lock()
	e, ok := l.m[key]
	if !ok {
		e = &mediaLock{sem: make(chan struct{}, 1)}
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

// forMedia returns the registry's shared media locks, so every client for one account in this
// process serializes against the same entries.
func (r *AccountPacers) forMedia() *mediaLocks {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.media == nil {
		r.media = newMediaLocks()
	}
	return r.media
}
