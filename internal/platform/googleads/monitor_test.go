// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestListAccountCampaigns_UsesInjectedClockNotWallClock pins a real defect found in round-14
// review: ListAccountCampaigns' [start, end] date window used to be computed by its caller
// (internal/dispatch/googleads.go) from the bare wall clock, unlike the equivalent Meta/LinkedIn
// windows, which already read the client's injected clock (see the sibling test of this name in
// internal/platform/meta and internal/platform/linkedin). That made Google's monitor window the
// only one of the four platforms that could not be pinned deterministically in a test. The
// window now comes from c.now, set here to a fixed value nowhere near the actual wall clock, so
// a test that failed to route through c.now would render a date range far outside what this
// test asserts.
func TestListAccountCampaigns_UsesInjectedClockNotWallClock(t *testing.T) {
	var mu sync.Mutex
	var gotBody string
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	t.Cleanup(apiSrv.Close)

	fixedNow := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	c := NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(func() time.Time { return fixedNow }))

	if _, err := c.ListAccountCampaigns(context.Background(), "1234567890", 7); err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}

	mu.Lock()
	body := gotBody
	mu.Unlock()
	// days=7 ending 2026-09-17 (inclusive) starts 2026-09-11 — the same days-1 convention the
	// LinkedIn/Meta/Reddit monitor dispatchers share. A wall-clock-derived window would not
	// contain this literal range regardless of when the test happens to run.
	if !strings.Contains(body, "2026-09-11") || !strings.Contains(body, "2026-09-17") {
		t.Errorf("query body = %s, want it to contain the injected-clock window 2026-09-11..2026-09-17", body)
	}
}
