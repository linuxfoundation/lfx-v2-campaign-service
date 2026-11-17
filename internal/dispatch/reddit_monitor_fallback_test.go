// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

const systemRedditCreds = `{"ClientID":"system-cid","ClientSecret":"sec","RefreshToken":"rt"}`

// systemRedditConnFor is the LF system Reddit row configured for accountID, with a client id
// ("system-cid") that differs from the project fixture's ("cid").
func systemRedditConnFor(accountID string) *model.Connection {
	c := activeRedditConn(systemRedditCreds)
	c.AccountID = accountID
	return c
}

// redditMonitorFallbackDispatcher wires a Reddit dispatcher whose token endpoint records the
// client id of every refresh, so a test can tell WHICH stored credential read the account, and
// whose API counts requests and answers two campaigns on the account: t2_c1 (tlf's) and t2_c2
// (another foundation's on the same shared account).
func redditMonitorFallbackDispatcher(t *testing.T, reader *scopedConnReader) (*RedditDispatcher, func() []string, func() int) {
	t.Helper()
	var mu sync.Mutex
	var clientIDs []string
	apiCalls := 0
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _, _ := r.BasicAuth()
		mu.Lock()
		clientIDs = append(clientIDs, id)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	t.Cleanup(tok.Close)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		apiCalls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/reports"):
			_, _ = w.Write([]byte(`{"data":{"metrics":[{"campaign_id":"t2_c1","impressions":1000,"clicks":150,"spend":25000000},{"campaign_id":"t2_c2","impressions":10,"clicks":1,"spend":1000000}]}}`))
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = w.Write([]byte(`{"data":{"campaigns":[{"id":"t2_c1","name":"tlf","configured_status":"ACTIVE"},{"id":"t2_c2","name":"other","configured_status":"ACTIVE"}]}}`))
		default: // the account currency read
			_, _ = w.Write([]byte(`{"data":{"id":"t2_acct","currency":"USD"}}`))
		}
	}))
	t.Cleanup(api.Close)
	d := NewRedditDispatcher(reader, identityEncryptor{},
		reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL))
	return d, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), clientIDs...)
		}, func() int {
			mu.Lock()
			defer mu.Unlock()
			return apiCalls
		}
}

func redditMonitorOrchestrator(d *RedditDispatcher, live map[string][]model.ProjectCampaignScope) *service.Orchestrator {
	return service.NewOrchestrator(monitorScopeCampaigns{live: live}, nil,
		map[model.Provider]service.PlatformDispatcher{model.ProviderRedditAds: d})
}

// tlfRedditScope is tlf's one recorded campaign, created on the LF system account t2_acct.
var tlfRedditScope = map[string][]model.ProjectCampaignScope{
	"tlf": {{PlatformCampaignID: "t2_c1", Result: []byte(`{"accountId":"t2_acct"}`)}},
}

// #615: a project with no Reddit connection of its own monitors the account the LF system row is
// configured for — the account its campaigns were created in — on the system credential, and sees
// only its own recorded campaigns, not the other foundation's campaign on the same account.
// Before #615 this was a 404, so nothing created through the system account could be monitored.
func TestReddit_AccountMonitor_FallsBackToSystemForItsAccount(t *testing.T) {
	d, clientIDs, _ := redditMonitorFallbackDispatcher(t, &scopedConnReader{
		rows: map[string]*model.Connection{model.SystemProjectID: systemRedditConnFor("t2_acct")},
	})
	read, err := redditMonitorOrchestrator(d, tlfRedditScope).ReadAccountCampaignMetrics(
		context.Background(), "tlf", model.ProviderRedditAds, "t2_acct", 30)
	if err != nil {
		t.Fatalf("ReadAccountCampaignMetrics on the LF system account: %v", err)
	}
	if len(read.Rows) != 1 || read.Rows[0].PlatformCampaignID != "t2_c1" {
		t.Errorf("rows = %+v, want only tlf's recorded campaign t2_c1", read.Rows)
	}
	if got := clientIDs(); len(got) == 0 || got[0] != "system-cid" {
		t.Errorf("token refreshes used client ids %v, want the LF system credential", got)
	}
}

// The LF system credential reads only the account the LF system row is configured for. Any other
// account keeps the 404 a project without a connection of its own always got, and nothing is sent.
func TestReddit_AccountMonitor_SystemFallbackRefusesOtherAccounts(t *testing.T) {
	for _, forced := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary fallback", true: "forced-system mode"}[forced], func(t *testing.T) {
			d, clientIDs, apiCalls := redditMonitorFallbackDispatcher(t, &scopedConnReader{
				rows: map[string]*model.Connection{model.SystemProjectID: systemRedditConnFor("t2_acct")},
			})
			d.creds.forceSystemPaidAds = forced
			_, err := d.ReadAccountMonitor(context.Background(), "tlf", model.ProviderRedditAds, "t2_other", 30)
			if !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("err = %v, want domain.ErrNotFound for an account the LF system row is not configured for", err)
			}
			if got, n := clientIDs(), apiCalls(); len(got) != 0 || n != 0 {
				t.Errorf("token refreshes %v, api calls %d; want none: the refusal must come before any upstream call", got, n)
			}
		})
	}
}

// A project with a connection of its own reads its own account with its own credential, as
// before #615, and the system account with the system credential; an account neither row is
// configured for is still refused by resolveMonitorClient's equality check against the project's
// own row.
func TestReddit_AccountMonitor_ReadsWithTheRowConfiguredForTheAccount(t *testing.T) {
	own := activeRedditConn(goodRedditCreds) // client id "cid"
	own.AccountID = "t2_own"
	system := systemRedditConnFor("t2_acct")
	cases := []struct {
		name      string
		accountID string
		want      string
		wantErr   error
	}{
		{"the project's own account", "t2_own", "cid", nil},
		{"the LF system account, with a connection of its own elsewhere", "t2_acct", "system-cid", nil},
		{"an account neither row is configured for", "t2_other", "", domain.ErrAccountNotManagedByConnection},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, clientIDs, apiCalls := redditMonitorFallbackDispatcher(t, &scopedConnReader{
				rows: map[string]*model.Connection{"tlf": own, model.SystemProjectID: system},
			})
			_, err := d.ReadAccountMonitor(context.Background(), "tlf", model.ProviderRedditAds, tc.accountID, 30)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				if n := apiCalls(); n != 0 {
					t.Errorf("api calls = %d, want none", n)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadAccountMonitor: %v", err)
			}
			if got := clientIDs(); len(got) == 0 || got[0] != tc.want {
				t.Errorf("token refreshes used client ids %v, want %q", got, tc.want)
			}
		})
	}
}

// With nothing recorded on Reddit the monitor is an empty 200 and Reddit is never asked, even on
// the system fallback.
func TestReddit_AccountMonitor_NoRecordedCampaignsIsEmptyWithoutUpstreamCall(t *testing.T) {
	d, clientIDs, apiCalls := redditMonitorFallbackDispatcher(t, &scopedConnReader{
		rows: map[string]*model.Connection{model.SystemProjectID: systemRedditConnFor("t2_acct")},
	})
	read, err := redditMonitorOrchestrator(d, nil).ReadAccountCampaignMetrics(
		context.Background(), "tlf", model.ProviderRedditAds, "t2_acct", 30)
	if err != nil {
		t.Fatalf("ReadAccountCampaignMetrics: %v", err)
	}
	if read.Rows == nil || len(read.Rows) != 0 {
		t.Errorf("rows = %#v, want a non-nil empty slice", read.Rows)
	}
	if got, n := clientIDs(), apiCalls(); len(got) != 0 || n != 0 {
		t.Errorf("token refreshes %v, api calls %d; want none", got, n)
	}
}

// With no connection of its own and no LF system row, the read is still the 404 it always was.
func TestReddit_AccountMonitor_NoConnectionAnywhereIsNotFound(t *testing.T) {
	d, clientIDs, apiCalls := redditMonitorFallbackDispatcher(t, &scopedConnReader{rows: map[string]*model.Connection{}})
	_, err := d.ReadAccountMonitor(context.Background(), "tlf", model.ProviderRedditAds, "t2_acct", 30)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want domain.ErrNotFound", err)
	}
	if got, n := clientIDs(), apiCalls(); len(got) != 0 || n != 0 {
		t.Errorf("token refreshes %v, api calls %d; want none", got, n)
	}
}
