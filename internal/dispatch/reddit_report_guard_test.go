// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

type redditGuardClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *redditGuardClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *redditGuardClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestGuard() (*redditReportGuard[int], *redditGuardClock) {
	clk := &redditGuardClock{t: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}
	g := newRedditReportGuard[int]()
	g.now = clk.now
	return g, clk
}

// Identical concurrent reads share ONE upstream read.
func TestRedditReportGuard_SingleFlightDedupes(t *testing.T) {
	g, _ := newTestGuard()
	joined := make(chan struct{}, 4)
	g.onJoin = func() { joined <- struct{}{} }
	release := make(chan struct{})
	var calls atomic.Int32
	fetch := func(context.Context) (*int, error) {
		calls.Add(1)
		<-release
		v := 7
		return &v, nil
	}
	var wg sync.WaitGroup
	results := make([]*int, 4)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := g.read(context.Background(), "t2_acct", model.MetricsWindowLast7Days, []string{"t3_b", "t3_a"}, fetch)
			if err != nil {
				t.Errorf("read %d: %v", i, err)
			}
			results[i] = v
		}()
	}
	for i := 0; i < 3; i++ {
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatalf("only %d of 3 callers joined the in-flight read", i)
		}
	}
	close(release)
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream reads = %d, want 1", n)
	}
	for i, v := range results {
		if v == nil || *v != 7 {
			t.Errorf("result %d = %v", i, v)
		}
	}
}

// A success is reused inside the TTL for exactly the same key; a different scope, window or UTC
// day, an expired entry, and a failure are not reused.
func TestRedditReportGuard_CachesSuccessForTheSameKeyOnly(t *testing.T) {
	g, clk := newTestGuard()
	var calls int
	ok := func(context.Context) (*int, error) { calls++; v := calls; return &v, nil }
	read := func(w model.MetricsWindow, ids ...string) {
		t.Helper()
		if _, err := g.read(context.Background(), "t2_acct", w, ids, ok); err != nil {
			t.Fatal(err)
		}
	}
	read(model.MetricsWindowToday, "t3_a", "t3_b")
	read(model.MetricsWindowToday, "t3_b", "t3_a", "t3_a") // same scope, reordered and repeated
	if calls != 1 {
		t.Fatalf("calls = %d, want the second read served from cache", calls)
	}
	read(model.MetricsWindowToday, "t3_a")             // narrower scope
	read(model.MetricsWindowLast7Days, "t3_a", "t3_b") // other window
	if calls != 3 {
		t.Fatalf("calls = %d, want a different scope or window to read upstream", calls)
	}
	clk.add(redditReportCacheTTL)
	read(model.MetricsWindowToday, "t3_a", "t3_b")
	if calls != 4 {
		t.Fatalf("calls = %d, want an expired entry to read upstream", calls)
	}
	// Across UTC midnight INSIDE the TTL: "today" names another day, so it is read again.
	clk.mu.Lock()
	clk.t = time.Date(2026, 10, 13, 23, 59, 40, 0, time.UTC)
	clk.mu.Unlock()
	read(model.MetricsWindowToday, "t3_a", "t3_b")
	clk.add(30 * time.Second) // 00:00:10 the next day, well inside the TTL
	read(model.MetricsWindowToday, "t3_a", "t3_b")
	if calls != 6 {
		t.Fatalf("calls = %d, want a read after UTC midnight not served from the previous day's entry", calls)
	}
	failing := 0
	fail := func(context.Context) (*int, error) { failing++; return nil, errors.New("upstream 503") }
	for i := 0; i < 2; i++ {
		if _, err := g.read(context.Background(), "t2_acct", model.MetricsWindowLastMonth, []string{"t3_a"}, fail); err == nil {
			t.Fatal("expected the failure")
		}
	}
	if failing != 2 {
		t.Errorf("failed reads upstream = %d, want 2 (failures are never cached)", failing)
	}
}

// A joiner whose LEADER's context ended re-leads instead of inheriting that cancellation.
func TestRedditReportGuard_JoinerReLeadsAfterLeaderCancels(t *testing.T) {
	g, _ := newTestGuard()
	joined := make(chan struct{}, 1)
	g.onJoin = func() { joined <- struct{}{} }
	leaderCtx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	var calls atomic.Int32
	fetch := func(ctx context.Context) (*int, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		v := 9
		return &v, nil
	}
	leaderDone := make(chan error, 1)
	go func() {
		_, err := g.read(leaderCtx, "t2_acct", model.MetricsWindowToday, []string{"t3_a"}, fetch)
		leaderDone <- err
	}()
	redditWaitFor(t, started, "the leader's read to start")
	joinerDone := make(chan *int, 1)
	go func() {
		v, err := g.read(context.Background(), "t2_acct", model.MetricsWindowToday, []string{"t3_a"}, fetch)
		if err != nil {
			t.Errorf("joiner: %v", err)
		}
		joinerDone <- v
	}()
	redditWaitFor(t, joined, "the joiner to join")
	cancel()
	select {
	case err := <-leaderDone:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("leader error = %v, want its own cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the leader never returned")
	}
	select {
	case v := <-joinerDone:
		if v == nil || *v != 9 {
			t.Errorf("joiner result = %v, want the re-led read's", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the joiner never returned")
	}
}

// Through the dispatcher: a refresh inside the TTL spends no further reporting requests.
func TestReddit_ReadRedditAudienceInsights_RefreshReusesTheRead(t *testing.T) {
	t.Setenv(constants.EnvRedditMetricsEnabled, "true")
	s := newRedditAudienceServer(t)
	d := s.dispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)})
	for i := 0; i < 3; i++ {
		if _, err := d.ReadRedditAudienceInsights(context.Background(), "proj", model.ProviderRedditAds, model.MetricsWindowLast30Days, redditScope("t3_c")); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
	}
	if n := len(s.requests()); n != 3 {
		t.Errorf("reporting requests = %d, want 3 (one read's breakdowns)", n)
	}
}

// redditWaitFor bounds a wait, so a broken guard fails the test instead of hanging it.
func redditWaitFor[T any](t *testing.T, ch <-chan T, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}
