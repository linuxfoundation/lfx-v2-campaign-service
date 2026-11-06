// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// recordingReportLedger is a domain.RedditReportLedger that grants every Take and records it.
type recordingReportLedger struct {
	mu       sync.Mutex
	accounts []string
}

func (l *recordingReportLedger) Take(_ context.Context, accountID string, _ bool, _ domain.RedditReportLimits) (domain.RedditReportTake, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.accounts = append(l.accounts, accountID)
	return domain.RedditReportTake{SlotID: int64(len(l.accounts)), Taken: true}, nil
}

func (l *recordingReportLedger) Release(context.Context, int64) error { return nil }

func (l *recordingReportLedger) takes() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.accounts...)
}

// The dispatcher's production budget is the SHARED ledger: until the container binds one, a
// report read is refused before anything is sent (fail closed); once bound — even on a client the
// dispatcher already built and cached — every report attempt is recorded in it.
func TestReddit_ReportBudgetIsTheSharedLedger(t *testing.T) {
	t.Setenv(constants.EnvRedditMetricsEnabled, "true")
	var sent atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v3/ad_accounts/t2_acct/reports" {
			sent.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"metrics":[{"campaign_id":"t3_c","impressions":1,"clicks":1,"spend":1}]}}`))
	}))
	defer api.Close()
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tok.Close()

	d := NewRedditDispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL))
	// What production builds (this test binary's newRedditReportBudgets is unlimited).
	d.reportBudgets = reddit.NewSharedReportBudgets()
	read := func() error {
		_, err := d.ReadMetrics(context.Background(), "proj", model.ProviderRedditAds,
			toggleCampaign("t3_c", "t5_ag", "t6_ad"), model.MetricsWindowToday)
		return err
	}

	if err := read(); !errors.Is(err, reddit.ErrReportBudgetUnavailable) {
		t.Fatalf("ReadMetrics with no ledger bound: err = %v, want ErrReportBudgetUnavailable", err)
	}
	if n := sent.Load(); n != 0 {
		t.Fatalf("sent %d report requests with no ledger bound", n)
	}

	l := &recordingReportLedger{}
	d.SetRedditReportLedger(l)
	if err := read(); err != nil {
		t.Fatalf("ReadMetrics with the ledger bound: %v", err)
	}
	if got := l.takes(); sent.Load() != 1 || len(got) != 1 || got[0] != "t2_acct" {
		t.Errorf("sent %d, ledger takes %v; want 1 send recorded for t2_acct", sent.Load(), got)
	}
}
