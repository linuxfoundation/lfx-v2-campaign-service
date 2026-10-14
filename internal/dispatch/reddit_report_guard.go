// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// redditReportGuard de-duplicates the Reddit project-scoped report reads, which each spend up to
// 24 attempts of an ad account's reporting budget (reddit/report_budget.go). A dashboard refreshing,
// or two operators on one project's panel, would otherwise spend it once per request. Keyed by
// account + window + the UTC day + the sorted, de-duplicated campaign scope — so a result is only
// ever reused for exactly the same campaigns on the same account, never a wider scope:
//
//   - identical concurrent reads share ONE upstream read (single flight). A joiner whose own
//     context ends first gets its own context error; a joiner whose LEADER's context ended re-leads
//     (at most redditReportMaxReLeads times) instead of inheriting another caller's cancellation;
//   - a SUCCESSFUL result is reused for redditReportCacheTTL. The UTC day is in the key, because
//     every window is anchored to it (dateRangeForWindow): "today" is never served across midnight.
//     Failures are never cached.
//
// One guard per dispatcher, so per process, like the budget it protects.
type redditReportGuard[T any] struct {
	now func() time.Time

	mu       sync.Mutex
	cache    map[string]redditReportCached[T]
	inflight map[string]*redditReportCall[T]
	// onJoin fires when a caller joins an in-flight read. Tests only.
	onJoin func()
}

const (
	// redditReportCacheTTL is how long a successful read is reused. Short: the numbers are live, and
	// the reuse is for refresh bursts, not for serving stale data.
	redditReportCacheTTL = 60 * time.Second
	// redditReportMaxReLeads bounds how often one caller takes over after its leader's context
	// ended — enough for a run of disconnecting clients, and it guarantees termination.
	redditReportMaxReLeads = 2
)

type redditReportCached[T any] struct {
	v        *T
	storedAt time.Time
}

type redditReportCall[T any] struct {
	done chan struct{}
	v    *T
	err  error
}

// redditLeaderContextError tags a shared failure caused only by the LEADING caller's own context.
type redditLeaderContextError struct{ err error }

func (e *redditLeaderContextError) Error() string { return e.err.Error() }
func (e *redditLeaderContextError) Unwrap() error { return e.err }

func newRedditReportGuard[T any]() *redditReportGuard[T] {
	return &redditReportGuard[T]{now: time.Now, cache: map[string]redditReportCached[T]{}, inflight: map[string]*redditReportCall[T]{}}
}

// redditReportKey is the guard key: account, window, UTC day and the sorted distinct scope.
func redditReportKey(accountID string, window model.MetricsWindow, day time.Time, campaignIDs []string) string {
	ids := append([]string(nil), campaignIDs...)
	sort.Strings(ids)
	uniq := ids[:0]
	for i, id := range ids {
		if i == 0 || id != ids[i-1] {
			uniq = append(uniq, id)
		}
	}
	return accountID + "\x00" + string(window) + "\x00" + day.UTC().Format("2006-01-02") + "\x00" + strings.Join(uniq, ",")
}

// read serves the key from the cache, joins an identical in-flight read, or runs fetch. The result
// is shared and must be treated as read-only.
func (g *redditReportGuard[T]) read(ctx context.Context, accountID string, window model.MetricsWindow, campaignIDs []string, fetch func(context.Context) (*T, error)) (*T, error) {
	for reLeads := 0; ; reLeads++ {
		key := redditReportKey(accountID, window, g.now(), campaignIDs)
		v, joined, err := g.readOnce(ctx, key, fetch)
		var lce *redditLeaderContextError
		if joined && ctx.Err() == nil && reLeads < redditReportMaxReLeads && errors.As(err, &lce) {
			continue
		}
		return v, err
	}
}

func (g *redditReportGuard[T]) readOnce(ctx context.Context, key string, fetch func(context.Context) (*T, error)) (v *T, joined bool, err error) {
	g.mu.Lock()
	if c, ok := g.cache[key]; ok {
		if age := g.now().Sub(c.storedAt); age >= 0 && age < redditReportCacheTTL {
			g.mu.Unlock()
			return c.v, false, nil
		}
		delete(g.cache, key)
	}
	if call, ok := g.inflight[key]; ok {
		g.mu.Unlock()
		if g.onJoin != nil {
			g.onJoin()
		}
		select {
		case <-call.done:
			return call.v, true, call.err
		case <-ctx.Done():
			return nil, false, fmt.Errorf("reddit report read: waiting for an identical read in flight: %w", ctx.Err())
		}
	}
	call := &redditReportCall[T]{done: make(chan struct{})}
	g.inflight[key] = call
	g.mu.Unlock()

	defer func() {
		if call.err != nil && ctx.Err() != nil {
			call.err = &redditLeaderContextError{err: call.err}
		}
		g.mu.Lock()
		delete(g.inflight, key)
		if call.err == nil && call.v != nil {
			now := g.now()
			for k, c := range g.cache { // drop expired entries so the cache does not grow unbounded
				if now.Sub(c.storedAt) >= redditReportCacheTTL {
					delete(g.cache, k)
				}
			}
			g.cache[key] = redditReportCached[T]{v: call.v, storedAt: now}
		}
		g.mu.Unlock()
		close(call.done)
	}()
	call.v, call.err = fetch(ctx)
	return call.v, false, call.err
}
