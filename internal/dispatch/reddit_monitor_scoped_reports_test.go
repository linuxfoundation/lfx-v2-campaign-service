// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
)

// redditScopedReportsDispatcher wires a Reddit dispatcher on the LF system row for t2_acct, whose
// API lists two ACTIVE campaigns (tlf's t2_c1, another foundation's t2_c2) and counts the report
// requests it answers.
func redditScopedReportsDispatcher(t *testing.T) (*RedditDispatcher, *atomic.Int64) {
	t.Helper()
	var reports atomic.Int64
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	t.Cleanup(tok.Close)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/reports"):
			reports.Add(1)
			_, _ = w.Write([]byte(`{"data":{"metrics":[{"campaign_id":"t2_c1","impressions":1000,"clicks":150,"spend":25000000}]}}`))
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = w.Write([]byte(`{"data":{"campaigns":[{"id":"t2_c1","name":"tlf","configured_status":"ACTIVE"},{"id":"t2_c2","name":"other","configured_status":"ACTIVE"}]}}`))
		default:
			_, _ = w.Write([]byte(`{"data":{"id":"t2_acct","currency":"USD"}}`))
		}
	}))
	t.Cleanup(api.Close)
	d := NewRedditDispatcher(&scopedConnReader{
		rows: map[string]*model.Connection{model.SystemProjectID: systemRedditConnFor("t2_acct")},
	}, identityEncryptor{}, reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL))
	return d, &reports
}

// #659: the monitor sends a report only for the project's own campaign on the shared system
// account. Before, the other foundation's t2_c2 got one too and was dropped afterwards — on the
// real LF account that was every foundation's campaign, each spending the account's budget.
func TestReddit_AccountMonitor_ReportsOnlyTheProjectsCampaigns(t *testing.T) {
	d, reports := redditScopedReportsDispatcher(t)
	read, err := redditMonitorOrchestrator(d, tlfRedditScope).ReadAccountCampaignMetrics(
		context.Background(), "tlf", model.ProviderRedditAds, "t2_acct", 30)
	if err != nil {
		t.Fatalf("ReadAccountCampaignMetrics: %v", err)
	}
	if len(read.Rows) != 1 || read.Rows[0].PlatformCampaignID != "t2_c1" || read.Rows[0].FetchFailed {
		t.Fatalf("rows = %+v, want tlf's t2_c1, read", read.Rows)
	}
	if n := reports.Load(); n != 1 {
		t.Errorf("report requests = %d, want 1 (t2_c1 only, not the other foundation's t2_c2)", n)
	}
}

// A nil scope handed to ReadAccountMonitorIn reads nothing: it is never "every campaign".
func TestReddit_ReadAccountMonitorIn_NilScopeSendsNoReport(t *testing.T) {
	d, reports := redditScopedReportsDispatcher(t)
	read, err := d.ReadAccountMonitorIn(context.Background(), "tlf", model.ProviderRedditAds, "t2_acct", 30, nil)
	if err != nil {
		t.Fatalf("ReadAccountMonitorIn: %v", err)
	}
	if len(read.Rows) != 0 || reports.Load() != 0 {
		t.Errorf("rows = %+v, report requests = %d; want none and 0", read.Rows, reports.Load())
	}
}
