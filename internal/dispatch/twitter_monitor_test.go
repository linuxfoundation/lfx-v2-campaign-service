// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
)

// twitterMonitorServer stubs the X Ads API for the monitor, counting every call so a refusal
// test can prove nothing reached upstream, and recording paths so a test can prove WHICH account
// was read. The fixture connection (activeTwitterConn) is bound to account "acc1".
func twitterMonitorServer(t *testing.T, routes map[string]string) ([]twitter.Option, *atomic.Int32, func() []string) {
	t.Helper()
	var calls atomic.Int32
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		body, ok := routes[r.URL.Path]
		if !ok {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	seen := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	return []twitter.Option{twitter.WithBaseURL(srv.URL), twitter.WithWriteDelay(0), twitter.WithClock(func() time.Time { return now })}, &calls, seen
}

var twitterMonitorRoutes = map[string]string{
	"/12/accounts/acc1": `{"data":{"id":"acc1","timezone":"America/New_York"}}`,
	"/12/accounts/acc1/campaigns": `{"data":[
		{"id":"c1","name":"ok","entity_status":"ACTIVE","daily_budget_amount_local_micro":25000000,"total_budget_amount_local_micro":300000000},
		{"id":"c2","name":"broken budget","entity_status":"ACTIVE","daily_budget_amount_local_micro":-5},
		{"id":"c3","name":"broken flight","entity_status":"PAUSED","daily_budget_amount_local_micro":1000000}
	],"next_cursor":null}`,
	"/12/accounts/acc1/line_items": `{"data":[
		{"id":"l1","campaign_id":"c1","start_time":"2026-09-01T04:00:00Z","end_time":"2026-10-31T04:00:00Z"},
		{"id":"l3","campaign_id":"c3","start_time":"not a time"}
	],"next_cursor":null}`,
	"/12/stats/accounts/acc1/active_entities": `{"data":[{"entity_id":"c1"}]}`,
	"/12/stats/jobs/accounts/acc1":            `{"data":{"id_str":"555","status":"PROCESSING"}}`,
}

// A project with no X connection of its own must not be served from the LF system row, on any
// of the three methods.
func TestTwitterMonitor_RefusesSystemFallback(t *testing.T) {
	opts, calls, _ := twitterMonitorServer(t, twitterMonitorRoutes)
	d := NewTwitterDispatcher(&scopedConnReader{
		rows: map[string]*model.Connection{model.SystemProjectID: activeTwitterConn(goodTwitterCreds)},
	}, identityEncryptor{}, opts...)
	for name, call := range twitterMonitorCalls(d, "acc1", "555") {
		if err := call(); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("%s: err = %v, want domain.ErrNotFound", name, err)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("%d upstream calls, want none", n)
	}
}

func twitterMonitorCalls(d *TwitterDispatcher, accountID, reportID string) map[string]func() error {
	return map[string]func() error{
		"list": func() error {
			_, err := d.ListAccountCampaigns(context.Background(), "cncf", model.ProviderTwitterAds, accountID)
			return err
		},
		"submit": func() error {
			_, err := d.SubmitAccountReport(context.Background(), "cncf", model.ProviderTwitterAds, accountID, 7)
			return err
		},
		"check": func() error {
			_, err := d.CheckAccountReport(context.Background(), "cncf", model.ProviderTwitterAds, accountID, reportID)
			return err
		},
	}
}

// Every refusal arm stops before any upstream call, on ALL THREE methods — submit creates X
// stats jobs and check downloads account data. The permitted arm reads the stored account.
func TestTwitterMonitor_AccountScope(t *testing.T) {
	cases := []struct {
		name      string
		requested string
		wantErr   error
	}{
		{"another account is refused", "acc2", domain.ErrAccountNotManagedByConnection},
		{"padded id is malformed, not trimmed", " acc1", domain.ErrAccountIDMalformed},
		{"path characters are malformed", "acc1/x", domain.ErrAccountIDMalformed},
		{"over-long id is malformed", strings.Repeat("a", 65), domain.ErrAccountIDMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, calls, _ := twitterMonitorServer(t, twitterMonitorRoutes)
			d := NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{}, opts...)
			for name, call := range twitterMonitorCalls(d, tc.requested, "555") {
				if err := call(); !errors.Is(err, tc.wantErr) {
					t.Errorf("%s: err = %v, want %v", name, err, tc.wantErr)
				}
			}
			if n := calls.Load(); n != 0 {
				t.Errorf("%d upstream calls, want none for a refused request", n)
			}
		})
	}
}

// The list maps budgets, flights and the unreadable-field flags onto the rule engine's inputs,
// and every request addresses the connection's own account.
func TestTwitterMonitor_ListMapsCampaigns(t *testing.T) {
	opts, _, seen := twitterMonitorServer(t, twitterMonitorRoutes)
	d := NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{}, opts...)
	rows, err := d.ListAccountCampaigns(context.Background(), "cncf", model.ProviderTwitterAds, "acc1")
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	byID := map[string]model.AccountCampaignMetrics{}
	for _, r := range rows {
		byID[r.PlatformCampaignID] = r
	}
	c1 := byID["c1"]
	if c1.BudgetDay != 25 || c1.TotalBudget != 300 || c1.StartDate != "2026-09-01" || c1.EndDate != "2026-10-30" || c1.FetchFailed || c1.Status != "ACTIVE" {
		t.Errorf("c1 = %+v, want budgets 25/300, flight 2026-09-01..2026-10-30 (New York days)", c1)
	}
	if !byID["c2"].FetchFailed {
		t.Errorf("c2 = %+v, want FetchFailed for a negative budget", byID["c2"])
	}
	if !byID["c3"].FetchFailed {
		t.Errorf("c3 = %+v, want FetchFailed for an unreadable flight", byID["c3"])
	}
	for _, p := range seen() {
		if !strings.Contains(p, "/accounts/acc1") {
			t.Errorf("request %s did not address the connection's account", p)
		}
	}
}

// Submit returns the composite job id and the account-timezone calendar window; check maps X's
// job states, and folds spend from micros. The sentinel is a finished empty report.
func TestTwitterMonitor_SubmitAndCheck(t *testing.T) {
	opts, _, _ := twitterMonitorServer(t, twitterMonitorRoutes)
	d := NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{}, opts...)
	sub, err := d.SubmitAccountReport(context.Background(), "cncf", model.ProviderTwitterAds, "acc1", 7)
	if err != nil {
		t.Fatalf("SubmitAccountReport: %v", err)
	}
	if sub.ReportID != "555" {
		t.Errorf("report id = %q, want 555", sub.ReportID)
	}
	if want := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC); !sub.WindowStart.Equal(want) {
		t.Errorf("window start = %v, want %v", sub.WindowStart, want)
	}
	if want := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC); !sub.WindowEnd.Equal(want) {
		t.Errorf("window end = %v, want %v", sub.WindowEnd, want)
	}

	check, err := d.CheckAccountReport(context.Background(), "cncf", model.ProviderTwitterAds, "acc1", twitter.NoActiveCampaignsReportID)
	if err != nil {
		t.Fatalf("CheckAccountReport(sentinel): %v", err)
	}
	if check.Status != model.AccountReportReady || check.Rows == nil || len(check.Rows) != 0 {
		t.Errorf("sentinel check = %+v, want ready with a non-nil empty row set", check)
	}
	if _, err := d.CheckAccountReport(context.Background(), "cncf", model.ProviderTwitterAds, "acc1", " "); err == nil {
		t.Error("a blank report id was accepted")
	}
	if _, err := d.SubmitAccountReport(context.Background(), "cncf", model.ProviderTwitterAds, "acc1", 6); !errors.Is(err, domain.ErrMonitorDaysInvalid) {
		t.Errorf("days=6: err = %v, want ErrMonitorDaysInvalid", err)
	}
}

func TestTwitterMonitor_CheckMapsStatusesAndSpend(t *testing.T) {
	cases := []struct {
		name   string
		status string
		want   model.AccountReportStatus
	}{
		{"pending", `{"data":[{"id_str":"555","status":"PROCESSING"}]}`, model.AccountReportPending},
		{"failed", `{"data":[{"id_str":"555","status":"FAILED"}]}`, model.AccountReportFailed},
		{"ready", `{"data":[{"id_str":"555","status":"SUCCESS","url":"FILE"}]}`, model.AccountReportReady},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var srvURL string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/12/stats/jobs/accounts/acc1":
					_, _ = io.WriteString(w, strings.Replace(tc.status, "FILE", srvURL+"/f.json", 1))
				case "/f.json":
					_, _ = io.WriteString(w, `{"data":[{"id":"c1","id_data":[{"metrics":{"impressions":[100],"clicks":[4],"billed_charge_local_micro":[12340000]}}]}]}`)
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(srv.Close)
			srvURL = srv.URL
			d := NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{}, twitter.WithBaseURL(srv.URL))
			got, err := d.CheckAccountReport(context.Background(), "cncf", model.ProviderTwitterAds, "acc1", "555")
			if err != nil {
				t.Fatalf("CheckAccountReport: %v", err)
			}
			if got.Status != tc.want {
				t.Errorf("status = %q, want %q", got.Status, tc.want)
			}
			if tc.want != model.AccountReportReady {
				return
			}
			if !got.Partial || len(got.Rows) != 1 {
				t.Fatalf("check = %+v, want one partial row", got)
			}
			r := got.Rows[0]
			if r.PlatformCampaignID != "c1" || r.Spend != 12.34 || r.Impressions != 100 || r.Clicks != 4 || r.Conversions != nil {
				t.Errorf("row = %+v, want c1 spend 12.34, 100 impressions, 4 clicks, conversions nil", r)
			}
		})
	}
}
