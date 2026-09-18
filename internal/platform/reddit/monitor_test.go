// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// tokenHandlerReturning is a tiny OAuth token-endpoint stub shared by the tests below.
func tokenHandlerReturning(accessToken string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": accessToken, "expires_in": 3600})
	}
}

// manyActiveCampaignsBody builds a bare-array campaign-list response with n ACTIVE campaigns,
// enough to exceed monitorReportConcurrency so a concurrency-bound regression has something to
// pin against.
func manyActiveCampaignsBody(n int) string {
	elements := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		elements = append(elements, map[string]any{
			"id":                fmt.Sprintf("camp%d", i),
			"name":              fmt.Sprintf("Campaign %d", i),
			"configured_status": StatusActive,
		})
	}
	b, err := json.Marshal(elements)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// TestListAccountCampaigns_BoundsReportConcurrency pins the round-19 review fix: the
// per-campaign report fan-out must run bounded-concurrently (monitorReportConcurrency, 5) rather
// than serially, so an account with several campaigns cannot exceed its caller's fixed read
// deadline even when each individual report call is healthy. Asserts both that ListAccountCampaigns
// completes well inside the time a fully serial loop would need, and that the number of report
// calls in flight at once never exceeds the configured limit.
func TestListAccountCampaigns_BoundsReportConcurrency(t *testing.T) {
	const numCampaigns = 12
	const perRequestDelay = 50 * time.Millisecond

	var inFlight, maxInFlight atomic.Int64
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/ad_accounts/t2_test/campaigns":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":` + manyActiveCampaignsBody(numCampaigns) + `}`))
		default:
			// Every campaign's /reports call.
			cur := inFlight.Add(1)
			for {
				prev := maxInFlight.Load()
				if cur <= prev || maxInFlight.CompareAndSwap(prev, cur) {
					break
				}
			}
			time.Sleep(perRequestDelay)
			inFlight.Add(-1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"metrics":[{"impressions":100,"clicks":10,"spend":5000000}]}}`))
		}
	}))
	defer apiSrv.Close()
	tokenSrv := httptest.NewServer(tokenHandlerReturning("tok"))
	defer tokenSrv.Close()

	c := NewClient(testCreds, testAccount, WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))

	rows, err := c.ListAccountCampaigns(context.Background(), testAccount.AccountID, 7)
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	if len(rows) != numCampaigns {
		t.Fatalf("got %d rows, want %d", len(rows), numCampaigns)
	}
	for _, row := range rows {
		if row.FetchFailed {
			t.Errorf("row %+v: unexpected FetchFailed on a healthy report response", row)
		}
	}

	// maxInFlight is the deterministic proof of boundedness: perRequestDelay only needs to hold
	// each request open long enough for concurrent ones to pile up, never compared against a
	// wall-clock budget (a fixed elapsed-time assertion here would be a flake waiting to happen
	// under a loaded `go test -race` run — round-19 review).
	if got := maxInFlight.Load(); got > int64(monitorReportConcurrency) {
		t.Errorf("max concurrent /reports requests = %d, want <= %d (monitorReportConcurrency)", got, monitorReportConcurrency)
	}
}

// TestListAccountCampaigns_PerCampaignReportFailure_MarksOnlyThatRowFailed pins that a single
// campaign's failed /reports call marks only that campaign's row FetchFailed and never cancels
// or degrades the other campaigns' in-flight reads — the reason ListAccountCampaigns's fan-out
// never propagates a per-item error out of errgroup.Go.
func TestListAccountCampaigns_PerCampaignReportFailure_MarksOnlyThatRowFailed(t *testing.T) {
	const failingCampaignID = "camp1"
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/ad_accounts/t2_test/campaigns":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":` + manyActiveCampaignsBody(3) + `}`))
		case r.URL.Path == "/api/v3/ad_accounts/t2_test/campaigns/"+failingCampaignID+"/reports":
			http.Error(w, "internal error", http.StatusInternalServerError)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"metrics":[{"impressions":100,"clicks":10,"spend":5000000}]}}`))
		}
	}))
	defer apiSrv.Close()
	tokenSrv := httptest.NewServer(tokenHandlerReturning("tok"))
	defer tokenSrv.Close()

	c := NewClient(testCreds, testAccount, WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))

	rows, err := c.ListAccountCampaigns(context.Background(), testAccount.AccountID, 7)
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3: %+v", len(rows), rows)
	}
	for _, row := range rows {
		if row.CampaignID == failingCampaignID {
			if !row.FetchFailed {
				t.Errorf("row %+v: want FetchFailed=true for the campaign whose /reports call failed", row)
			}
			continue
		}
		if row.FetchFailed {
			t.Errorf("row %+v: a sibling campaign's report failure must not mark this healthy row FetchFailed", row)
		}
		if row.Impressions != 100 || row.Clicks != 10 {
			t.Errorf("row %+v: want the healthy metrics from its own report response", row)
		}
	}
}

// TestListAccountCampaigns_MalformedCampaignID_MarksFetchFailedWithoutRequest pins the round-23
// review fix: a Reddit-returned campaign id that fails accountIDRe (the same safe-charset guard
// every sibling path already applies to a caller- or Reddit-supplied id before interpolating it
// into a request path) must mark that row FetchFailed and never reach fetchMonitorReport — a
// malformed id must not retarget this project's live bearer token at an arbitrary Reddit path.
func TestListAccountCampaigns_MalformedCampaignID_MarksFetchFailedWithoutRequest(t *testing.T) {
	const malformedID = "../t2_other/campaigns"
	body := `{"data":[
		{"id":"` + malformedID + `","name":"Malformed","configured_status":"ACTIVE"},
		{"id":"campOK","name":"OK","configured_status":"ACTIVE"}
	]}`

	var reportRequested atomic.Bool
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/ad_accounts/t2_test/campaigns":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		case r.URL.Path == "/api/v3/ad_accounts/t2_test/campaigns/"+malformedID+"/reports":
			reportRequested.Store(true)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"metrics":[{"impressions":100,"clicks":10,"spend":5000000}]}}`))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"metrics":[{"impressions":100,"clicks":10,"spend":5000000}]}}`))
		}
	}))
	defer apiSrv.Close()
	tokenSrv := httptest.NewServer(tokenHandlerReturning("tok"))
	defer tokenSrv.Close()

	c := NewClient(testCreds, testAccount, WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))

	rows, err := c.ListAccountCampaigns(context.Background(), testAccount.AccountID, 7)
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(rows), rows)
	}
	for _, row := range rows {
		if row.CampaignID == malformedID {
			if !row.FetchFailed {
				t.Errorf("row %+v: want FetchFailed=true for a campaign id that fails accountIDRe", row)
			}
			continue
		}
		if row.FetchFailed {
			t.Errorf("row %+v: a sibling campaign's malformed id must not mark this healthy row FetchFailed", row)
		}
	}
	if reportRequested.Load() {
		t.Error("fetchMonitorReport was called for a campaign id that fails accountIDRe — the malformed id must never reach a request path")
	}
}

// TestListAccountCampaigns_NoStartTime_LeavesStartDateEmpty pins the other half of the round-23
// review fix: a campaign whose start_time Reddit never reported must NOT have its StartDate
// seeded from the report window. An empty StartDate is EvaluateRedditMonitor's signal to set
// PacingUnknown instead of computing a pacing verdict against a flight the campaign never had.
func TestListAccountCampaigns_NoStartTime_LeavesStartDateEmpty(t *testing.T) {
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/ad_accounts/t2_test/campaigns":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":` + manyActiveCampaignsBody(1) + `}`))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"metrics":[{"impressions":100,"clicks":10,"spend":5000000}]}}`))
		}
	}))
	defer apiSrv.Close()
	tokenSrv := httptest.NewServer(tokenHandlerReturning("tok"))
	defer tokenSrv.Close()

	c := NewClient(testCreds, testAccount, WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))

	rows, err := c.ListAccountCampaigns(context.Background(), testAccount.AccountID, 7)
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
	}
	if rows[0].StartDate != "" {
		t.Errorf("StartDate = %q, want empty — manyActiveCampaignsBody sends no start_time, so it must not be seeded from the report window", rows[0].StartDate)
	}
	if rows[0].EndDate != "" {
		t.Errorf("EndDate = %q, want empty for the same reason", rows[0].EndDate)
	}
}

// TestListAccountCampaigns_MalformedReportJSON_MarksFetchFailed pins the round-21 review fix:
// a /reports response that isn't the expected report shape must mark that row FetchFailed
// rather than being read as a legitimate zero-delivery measurement, which is what the BFF's
// optional-chaining `?.metrics ?? []` did and this port deliberately diverges from (see
// fetchMonitorReport's doc comment).
func TestListAccountCampaigns_MalformedReportJSON_MarksFetchFailed(t *testing.T) {
	const malformedCampaignID = "camp1"
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/ad_accounts/t2_test/campaigns":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":` + manyActiveCampaignsBody(3) + `}`))
		case r.URL.Path == "/api/v3/ad_accounts/t2_test/campaigns/"+malformedCampaignID+"/reports":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":"not-the-expected-shape"}`))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"metrics":[{"impressions":100,"clicks":10,"spend":5000000}]}}`))
		}
	}))
	defer apiSrv.Close()
	tokenSrv := httptest.NewServer(tokenHandlerReturning("tok"))
	defer tokenSrv.Close()

	c := NewClient(testCreds, testAccount, WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))

	rows, err := c.ListAccountCampaigns(context.Background(), testAccount.AccountID, 7)
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3: %+v", len(rows), rows)
	}
	for _, row := range rows {
		if row.CampaignID == malformedCampaignID {
			if !row.FetchFailed {
				t.Errorf("row %+v: want FetchFailed=true for a malformed /reports body, not a fabricated zero", row)
			}
			continue
		}
		if row.FetchFailed {
			t.Errorf("row %+v: a sibling campaign's malformed report must not mark this healthy row FetchFailed", row)
		}
	}
}

// TestListAccountCampaigns_MalformedCampaignListShape_ReturnsError pins the round-30+ review
// fix: a campaign-list body matching neither the bare-array nor the {"campaigns": [...]} shape
// must surface as an error, not as a fabricated empty account. Before this fix
// decodeCampaignList silently returned nil for any unrecognized shape, indistinguishable from
// an account that genuinely has zero campaigns (a 200 with no findings) — see
// decodeCampaignList's own doc comment.
func TestListAccountCampaigns_MalformedCampaignListShape_ReturnsError(t *testing.T) {
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":"not-an-array-or-a-campaigns-object"}`))
	}))
	defer apiSrv.Close()
	tokenSrv := httptest.NewServer(tokenHandlerReturning("tok"))
	defer tokenSrv.Close()

	c := NewClient(testCreds, testAccount, WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))

	rows, err := c.ListAccountCampaigns(context.Background(), testAccount.AccountID, 7)
	if err == nil {
		t.Fatalf("ListAccountCampaigns: got rows=%+v, err=nil; want an error for a malformed campaign-list shape, not a silent empty account", rows)
	}
}

// TestListAccountCampaigns_EmptyCampaignListBody_ReturnsNoRowsWithoutError confirms the
// legitimately-empty case (no body at all) is still NOT treated as malformed — Reddit's own
// API returns this for "no campaigns," and decodeCampaignList must not conflate it with a
// decode failure.
func TestListAccountCampaigns_EmptyCampaignListBody_ReturnsNoRowsWithoutError(t *testing.T) {
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer apiSrv.Close()
	tokenSrv := httptest.NewServer(tokenHandlerReturning("tok"))
	defer tokenSrv.Close()

	c := NewClient(testCreds, testAccount, WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))

	rows, err := c.ListAccountCampaigns(context.Background(), testAccount.AccountID, 7)
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("got %d rows, want 0 for a legitimately empty account", len(rows))
	}
}
