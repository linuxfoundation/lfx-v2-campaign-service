// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// warningCapture collects records so a test can COUNT the near-expiry warning.
type warningCapture struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *warningCapture) Enabled(context.Context, slog.Level) bool { return true }
func (h *warningCapture) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, r.Clone())
	return nil
}
func (h *warningCapture) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *warningCapture) WithGroup(string) slog.Handler      { return h }

func (h *warningCapture) countFor(connection string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, rec := range h.recs {
		if !strings.Contains(rec.Message, "refresh token is nearing expiry") {
			continue
		}
		var got string
		rec.Attrs(func(a slog.Attr) bool {
			if a.Key == "connection" {
				got = a.Value.String()
			}
			return true
		})
		if got == connection {
			n++
		}
	}
	return n
}

// nearExpiryTokenServer mints a token whose REFRESH token is inside the warning window.
func nearExpiryTokenServer(t *testing.T, refreshTTL time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":"a","expires_in":3600,"refresh_token":"r","refresh_token_expires_in":%d}`,
			int(refreshTTL.Seconds()))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A 30-day warning window must not produce one WARN per OPERATION for thirty days.
//
// internal/dispatch/linkedin.go constructs a Client PER OPERATION (four call sites), and no
// access-token expiry is persisted, so every client exchanges on its first request and
// re-evaluates this window — see the "EVERY refresh-capable client performs a token exchange
// on its first request" note in accessTokenValue. A brief-level fan-out is therefore one
// exchange, and one warning, PER CAMPAIGN. Over the final thirty days of a refresh token's
// life that is thousands of identical lines, which is how an operator learns to filter the
// one line this feature exists to deliver.
//
// The property is per PROCESS and per CONNECTION: the operator has to be told once that this
// credential is expiring, and a second connection expiring must not be masked by the first.
// Asserted by COUNT across independently constructed clients — the shape the dispatcher
// actually produces. A test using one client would pass against the defect, because a single
// client exchanges only once anyway.
func TestRefreshTokenNearExpiryWarnsOncePerConnectionNotPerOperation(t *testing.T) {
	h := &warningCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	t.Cleanup(resetRefreshExpiryWarnStateForTest)
	resetRefreshExpiryWarnStateForTest()

	// Well inside the 30-day window, so every exchange evaluates it as "warn".
	srv := nearExpiryTokenServer(t, 5*24*time.Hour)

	const operations = 25
	for i := 0; i < operations; i++ {
		creds := refreshableCreds()
		creds.ConnectionName = "LF LinkedIn"
		c := NewClient(creds, RuntimeConfig{})
		c.tokenURL = srv.URL
		if _, err := c.accessTokenValue(context.Background()); err != nil {
			t.Fatalf("operation %d: accessTokenValue: %v", i, err)
		}
	}

	// Bind the fixture to the warning path first: if no warning fired at all, a
	// "count <= 1" assertion would pass against a feature that had been deleted.
	got := h.countFor("LF LinkedIn")
	if got == 0 {
		t.Fatal("no near-expiry warning fired at all — the fixture is not reaching the " +
			"warning path, so a count assertion would prove nothing")
	}
	if got > 1 {
		t.Errorf("the near-expiry warning fired %d times across %d per-operation clients, want 1: "+
			"a 30-day window that re-fires on every refresh-capable operation buries the one "+
			"line the operator needs under thousands of identical copies", got, operations)
	}

	// A DIFFERENT connection must still be able to warn — deduping must not silence the
	// second expiring credential just because the first already reported.
	other := refreshableCreds()
	other.ConnectionName = "CNCF LinkedIn"
	c2 := NewClient(other, RuntimeConfig{})
	c2.tokenURL = srv.URL
	if _, err := c2.accessTokenValue(context.Background()); err != nil {
		t.Fatalf("second connection: accessTokenValue: %v", err)
	}
	if n := h.countFor("CNCF LinkedIn"); n != 1 {
		t.Errorf("second connection warned %d times, want 1 — dedupe must be per connection, "+
			"or one expiring credential hides every other", n)
	}
}

// Two UNNAMED connections must each warn.
//
// ConnectionLabel() falls back to a shared constant ("the LinkedIn connection") whenever the
// operator set no ConnectionName, so a dedupe key built from the label ALONE collapses every
// unnamed connection into one entry: the first to warn permanently silences the rest, and a
// second credential expiring is never reported. That is the same masking the per-connection
// requirement exists to prevent, reachable through the DEFAULT configuration rather than an
// exotic one.
//
// This case is separate from the named-connection test above because that test cannot see it:
// with distinct ConnectionNames a label-only key and the full key behave identically, so the
// label-only implementation passes it. Both connections here are deliberately unnamed, which
// is the only fixture that can tell the two keys apart.
func TestRefreshTokenNearExpiryWarnsPerConnectionEvenWhenUnnamed(t *testing.T) {
	h := &warningCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	t.Cleanup(resetRefreshExpiryWarnStateForTest)
	resetRefreshExpiryWarnStateForTest()

	srv := nearExpiryTokenServer(t, 5*24*time.Hour)

	// Distinct connections, neither named: they differ only by the credential and account
	// that actually identify them upstream.
	for i, clientID := range []string{"client-a", "client-b"} {
		creds := refreshableCreds()
		creds.ConnectionName = ""
		creds.ClientID = clientID
		c := NewClient(creds, RuntimeConfig{DefaultAccountID: fmt.Sprintf("acct-%d", i)})
		c.tokenURL = srv.URL
		if _, err := c.accessTokenValue(context.Background()); err != nil {
			t.Fatalf("connection %q: accessTokenValue: %v", clientID, err)
		}
	}

	// Both fall back to the same label, which is the whole point: the COUNT under that label
	// is what distinguishes a per-connection key from a per-label one.
	if got := h.countFor("the LinkedIn connection"); got != 2 {
		t.Errorf("two distinct UNNAMED connections produced %d near-expiry warnings, want 2 — "+
			"a dedupe keyed on the connection LABEL collapses every unnamed connection into "+
			"one entry, so the first to warn silences the rest and a second expiring "+
			"credential is never reported", got)
	}
}

// Concurrent dispatches for ONE connection must still warn once.
//
// This is the shape production actually produces: a brief-level fan-out reads every campaign
// concurrently, each through its own Client, and every one of them exchanges a token and
// evaluates this window in parallel. A sequential test cannot distinguish `LoadOrStore` from a
// `Load`-then-`Store` pair — both yield exactly one warning when callers arrive one at a time —
// so a check-then-set implementation, in which several goroutines all observe "not yet warned"
// before any of them records it, passes the sequential test while emitting one line per
// concurrent campaign at runtime.
//
// HONEST LIMIT of this test: it pins the property, but it does not reliably KILL the
// check-then-set mutation. Measured directly, `Load`-then-`Store` still yields exactly one
// warning here across repeated -race runs — the two map operations are adjacent enough that
// the goroutines do not interleave between them. Forcing a `runtime.Gosched()` into that gap
// makes the mutant emit up to 28 duplicates for 64 callers, which establishes the window is
// REAL rather than theoretical; it is simply too narrow for a test to hit on demand without
// instrumenting production code. So `LoadOrStore` is the correct construction on the argument,
// and this test guards the observable property and the -race cleanliness rather than claiming
// to prove atomicity.
func TestRefreshTokenNearExpiryWarnsOnceUnderConcurrentDispatch(t *testing.T) {
	h := &warningCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	t.Cleanup(resetRefreshExpiryWarnStateForTest)
	resetRefreshExpiryWarnStateForTest()

	srv := nearExpiryTokenServer(t, 5*24*time.Hour)

	const fanout = 32
	// Release every goroutine at once, so they contend on the guard rather than arriving in
	// sequence — the ordering a check-then-set implementation survives.
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, fanout)
	for i := 0; i < fanout; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			creds := refreshableCreds()
			creds.ConnectionName = "LF LinkedIn"
			c := NewClient(creds, RuntimeConfig{})
			c.tokenURL = srv.URL
			<-start
			if _, err := c.accessTokenValue(context.Background()); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("accessTokenValue: %v", err)
	}

	got := h.countFor("LF LinkedIn")
	if got == 0 {
		t.Fatal("no near-expiry warning fired at all — the fixture is not reaching the " +
			"warning path, so the count assertion would prove nothing")
	}
	if got != 1 {
		t.Errorf("%d concurrent dispatches for one connection produced %d warnings, want 1 — "+
			"the guard must be atomic (LoadOrStore), or every goroutine that reads the map "+
			"before any of them writes it logs its own copy", fanout, got)
	}
}
