// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// #615 acceptance for LinkedIn, end to end through the real LinkedInDispatcher, the orchestrator
// and (for the omitted account_id) the ConnectionService handler: a project with no LinkedIn connection of its own monitors the campaigns it
// recorded on the LF system row's accounts (the default 538170226 or a grant), through the system
// credential, and sees only those. The unit tests in account_monitor_scope_test.go pin each rule
// on its own; these pin what a caller of the monitor actually gets back when two foundations share
// one account.

const systemLinkedInCreds = `{"AccessToken":"system-tok"}`

// systemLinkedInRow is grantedSystemRow (default 538170226, grant 500928401) with a token
// ("system-tok") that differs from the project fixture's ("tok"), so a test can tell which stored
// credential read the account.
func systemLinkedInRow() *model.Connection {
	c := grantedSystemRow()
	c.EncryptedCredentials = []byte(systemLinkedInCreds)
	return c
}

// linkedInMonitorFallbackDispatcher wires a LinkedIn dispatcher whose API records the bearer
// token and URL (path and unescaped query) of every request, and answers the same three campaigns on whichever account is
// read: 1001 (tlf's), 1002 (aswf's) and 1003, which no project records unless a test records
// it.
func linkedInMonitorFallbackDispatcher(t *testing.T, reader *scopedConnReader) (*LinkedInDispatcher, func() (tokens, paths []string)) {
	t.Helper()
	var mu sync.Mutex
	var tokens, paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tokens = append(tokens, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		query, _ := url.QueryUnescape(r.URL.RawQuery)
		paths = append(paths, r.URL.Path+"?"+query)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "adAnalytics") {
			_, _ = io.WriteString(w, `{"elements":[`+
				`{"pivotValues":["urn:li:sponsoredCampaign:1001"],"impressions":100,"clicks":4,"costInLocalCurrency":"10.00"},`+
				`{"pivotValues":["urn:li:sponsoredCampaign:1002"],"impressions":200,"clicks":8,"costInLocalCurrency":"20.00"},`+
				`{"pivotValues":["urn:li:sponsoredCampaign:1003"],"impressions":300,"clicks":9,"costInLocalCurrency":"30.00"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"elements":[`+
			`{"id":1001,"name":"tlf","status":"ACTIVE","dailyBudget":{"amount":"50.00","currencyCode":"USD"}},`+
			`{"id":1002,"name":"aswf","status":"ACTIVE","dailyBudget":{"amount":"50.00","currencyCode":"USD"}},`+
			`{"id":1003,"name":"unrecorded","status":"ACTIVE","dailyBudget":{"amount":"50.00","currencyCode":"USD"}}],"metadata":{}}`)
	}))
	t.Cleanup(srv.Close)
	d := NewLinkedInDispatcher(reader, identityEncryptor{}, linkedin.WithBaseURL(srv.URL))
	return d, func() ([]string, []string) {
		mu.Lock()
		defer mu.Unlock()
		out := [2][]string{append([]string(nil), tokens...), append([]string(nil), paths...)}
		tokens, paths = nil, nil
		return out[0], out[1]
	}
}

func linkedInMonitorOrchestrator(d *LinkedInDispatcher, live map[string][]model.ProjectCampaignScope) *service.Orchestrator {
	return service.NewOrchestrator(monitorScopeCampaigns{live: live}, nil,
		map[model.Provider]service.PlatformDispatcher{model.ProviderLinkedInAds: d})
}

// linkedInMonitorService is the ConnectionService serving monitor-linkedin-ads-account over o. The
// monitor reads no connection row of its own (the dispatcher resolves credentials), so the
// repository is a nil-embedded stub: any call to it panics, which keeps the test honest about that.
func linkedInMonitorService(o *service.Orchestrator) *service.ConnectionService {
	svc := service.NewConnectionService(struct{ domain.ConnectionRepository }{}, identityEncryptor{})
	svc.SetOrchestrator(o)
	return svc
}

// assertLinkedInReadOf fails unless every recorded request read account and an Ad Analytics read
// was among them: the campaign list on adAccounts/{account}, the analytics filtered to that
// account's URN, so neither half of the monitor can read another account unnoticed.
func assertLinkedInReadOf(t *testing.T, urls []string, account string) {
	t.Helper()
	analytics := 0
	for _, u := range urls {
		switch {
		case strings.Contains(u, "adAnalytics"):
			analytics++
			if !strings.Contains(u, "accounts=List(urn:li:sponsoredAccount:"+account+")") {
				t.Errorf("analytics read %s, want it filtered to account %s", u, account)
			}
		case !strings.Contains(u, "adAccounts/"+account+"/"):
			t.Errorf("request %s, want a read of account %s", u, account)
		}
	}
	if analytics == 0 {
		t.Errorf("requests %v include no Ad Analytics read", urls)
	}
}

func liRecorded(id, account string) model.ProjectCampaignScope {
	return model.ProjectCampaignScope{PlatformCampaignID: id, Result: []byte(`{"accountId":"` + account + `"}`)}
}

func monitorRowIDs(read *model.AccountMonitorRead) []string {
	ids := make([]string, 0, len(read.Rows))
	for _, r := range read.Rows {
		ids = append(ids, r.PlatformCampaignID)
	}
	sort.Strings(ids)
	return ids
}

// Two foundations with no LinkedIn connection of their own, tlf and aswf, both created campaigns
// on the LF system account 538170226, which also holds a campaign this service never recorded.
// Each calls monitor-linkedin-ads-account WITHOUT account_id: the response names 538170226 as the
// account read (how the monitor reports where the account came from, as Google's does), the read
// uses the system credential on that account for both the campaign list and the analytics, and
// each gets back only its own campaign with its own metrics: neither sees the other's campaign or
// spend, nor the unrecorded one.
func TestLinkedIn_AccountMonitor_TwoProjectsOnTheSharedSystemAccount(t *testing.T) {
	d, requests := linkedInMonitorFallbackDispatcher(t, &scopedConnReader{
		rows: map[string]*model.Connection{model.SystemProjectID: systemLinkedInRow()},
	})
	svc := linkedInMonitorService(linkedInMonitorOrchestrator(d, map[string][]model.ProjectCampaignScope{
		"tlf":  {liRecorded("1001", "538170226")},
		"aswf": {liRecorded("1002", "538170226")},
	}))
	type own struct {
		id          string
		spend       float64
		impressions int64
	}
	for project, want := range map[string]own{"tlf": {"1001", 10, 100}, "aswf": {"1002", 20, 200}} {
		t.Run(project, func(t *testing.T) {
			got, err := svc.MonitorLinkedinAdsAccount(context.Background(), &conn.MonitorLinkedinAdsAccountPayload{ProjectID: project, Days: 30})
			if err != nil {
				t.Fatalf("MonitorLinkedinAdsAccount without account_id through the LF system row: %v", err)
			}
			if got.AccountID != "538170226" {
				t.Errorf("account_id = %q, want the system account 538170226 its campaign records", got.AccountID)
			}
			if len(got.Campaigns) != 1 || got.Campaigns[0].PlatformCampaignID != want.id {
				ids := make([]string, 0, len(got.Campaigns))
				for _, c := range got.Campaigns {
					ids = append(ids, c.PlatformCampaignID)
				}
				t.Errorf("campaigns = %v, want only %s's own campaign %s", ids, project, want.id)
			} else if c := got.Campaigns[0]; c.Spend != want.spend || c.Impressions != want.impressions {
				t.Errorf("campaign %s spend=%v impressions=%d, want its own %v and %d", c.PlatformCampaignID, c.Spend, c.Impressions, want.spend, want.impressions)
			}
			if got.Totals == nil || got.Totals.Spend != want.spend || got.Totals.Impressions != want.impressions || got.Totals.CampaignCount != 1 {
				t.Errorf("totals = %+v, want only %s's own spend %v and impressions %d", got.Totals, project, want.spend, want.impressions)
			}
			// Action items name campaigns too: every campaign-specific one must be this project's,
			// and its own campaign (underspending against its 50/day budget) must have one.
			ownItem := false
			for _, item := range got.ActionItems {
				if item.CampaignID == nil {
					continue
				}
				if *item.CampaignID != want.id {
					name := ""
					if item.CampaignName != nil {
						name = *item.CampaignName
					}
					t.Errorf("action item for campaign %s (%q) leaked into %s's monitor: %s", *item.CampaignID, name, project, item.Issue)
					continue
				}
				ownItem = true
			}
			if !ownItem {
				t.Errorf("no action item for %s's own campaign %s among %d items", project, want.id, len(got.ActionItems))
			}
			tokens, urls := requests()
			if len(tokens) == 0 {
				t.Fatal("LinkedIn was never called")
			}
			for i, tok := range tokens {
				if tok != "system-tok" {
					t.Errorf("request %s used token %q, want the LF system credential", urls[i], tok)
				}
			}
			assertLinkedInReadOf(t, urls, "538170226")
		})
	}
}

// A project whose campaigns were created in one of the system row's GRANTED accounts is read the
// same way, on that account and still with the system credential.
func TestLinkedIn_AccountMonitor_GrantedAccountReadsThroughTheSystemRow(t *testing.T) {
	d, requests := linkedInMonitorFallbackDispatcher(t, &scopedConnReader{
		rows: map[string]*model.Connection{model.SystemProjectID: systemLinkedInRow()},
	})
	o := linkedInMonitorOrchestrator(d, map[string][]model.ProjectCampaignScope{"cncf": {liRecorded("1003", "500928401")}})
	read, err := o.ReadAccountCampaignMetrics(context.Background(), "cncf", model.ProviderLinkedInAds, "500928401", 30)
	if err != nil {
		t.Fatalf("ReadAccountCampaignMetrics on a granted account: %v", err)
	}
	if got := monitorRowIDs(read); len(got) != 1 || got[0] != "1003" {
		t.Errorf("rows = %v, want only cncf's campaign 1003", got)
	}
	tokens, urls := requests()
	if len(tokens) == 0 {
		t.Fatal("LinkedIn was never called")
	}
	assertLinkedInReadOf(t, urls, "500928401")
	for _, tok := range tokens {
		if tok != "system-tok" {
			t.Errorf("request used token %q, want the LF system credential", tok)
		}
	}
}

// With nothing recorded on LinkedIn the monitor is an empty 200 and LinkedIn is never asked, on
// the system fallback, whether or not the caller names the account. Without account_id the
// response names none.
func TestLinkedIn_AccountMonitor_NoRecordedCampaignsIsEmptyWithoutUpstreamCall(t *testing.T) {
	d, requests := linkedInMonitorFallbackDispatcher(t, &scopedConnReader{
		rows: map[string]*model.Connection{model.SystemProjectID: systemLinkedInRow()},
	})
	o := linkedInMonitorOrchestrator(d, nil)
	got, err := linkedInMonitorService(o).MonitorLinkedinAdsAccount(context.Background(), &conn.MonitorLinkedinAdsAccountPayload{ProjectID: "tlf", Days: 30})
	if err != nil {
		t.Fatalf("MonitorLinkedinAdsAccount without account_id: %v", err)
	}
	if got.AccountID != "" || len(got.Campaigns) != 0 {
		t.Errorf("account_id = %q, campaigns = %d; want an empty monitor naming no account", got.AccountID, len(got.Campaigns))
	}
	read, err := o.ReadAccountCampaignMetrics(context.Background(), "tlf", model.ProviderLinkedInAds, "538170226", 30)
	if err != nil {
		t.Fatalf("ReadAccountCampaignMetrics naming the system account: %v", err)
	}
	if read.Rows == nil || len(read.Rows) != 0 {
		t.Errorf("rows = %#v, want a non-nil empty slice", read.Rows)
	}
	if tokens, urls := requests(); len(tokens) != 0 {
		t.Errorf("requests %v, want none", urls)
	}
}

// A project whose recorded campaign names an account the LF system row is neither configured for
// nor granted keeps the 404 a project without a connection of its own always got, and nothing is
// sent, with forced-system mode on and off.
func TestLinkedIn_AccountMonitor_SystemFallbackRefusesOtherAccounts(t *testing.T) {
	for _, forced := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary fallback", true: "forced-system mode"}[forced], func(t *testing.T) {
			d, requests := linkedInMonitorFallbackDispatcher(t, &scopedConnReader{
				rows: map[string]*model.Connection{model.SystemProjectID: systemLinkedInRow()},
			})
			d.creds.forceSystemPaidAds = forced
			o := linkedInMonitorOrchestrator(d, map[string][]model.ProjectCampaignScope{"tlf": {liRecorded("1001", "111111111")}})
			_, err := o.ReadAccountCampaignMetrics(context.Background(), "tlf", model.ProviderLinkedInAds, "111111111", 30)
			if !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("err = %v, want domain.ErrNotFound for an account the LF system row does not serve", err)
			}
			if tokens, urls := requests(); len(tokens) != 0 {
				t.Errorf("requests %v, want none: the refusal must come before any upstream call", urls)
			}
		})
	}
}

// A project that DISCONNECTED its own LinkedIn connection still monitors the campaigns it created
// on the LF system account, as it can still pause them (resolveExisting's error arm).
func TestLinkedIn_AccountMonitor_DisconnectedProjectReadsItsSystemAccount(t *testing.T) {
	d, requests := linkedInMonitorFallbackDispatcher(t, &scopedConnReader{
		rows:       map[string]*model.Connection{model.SystemProjectID: systemLinkedInRow()},
		tombstoned: map[string]bool{"tlf": true},
	})
	o := linkedInMonitorOrchestrator(d, map[string][]model.ProjectCampaignScope{"tlf": {liRecorded("1001", "538170226")}})
	read, err := o.ReadAccountCampaignMetrics(context.Background(), "tlf", model.ProviderLinkedInAds, "538170226", 30)
	if err != nil {
		t.Fatalf("disconnected project reading its system account: %v", err)
	}
	if got := monitorRowIDs(read); len(got) != 1 || got[0] != "1001" {
		t.Errorf("rows = %v, want only tlf's campaign 1001", got)
	}
	tokens, urls := requests()
	if len(tokens) == 0 {
		t.Fatal("LinkedIn was never called")
	}
	assertLinkedInReadOf(t, urls, "538170226")
	for _, tok := range tokens {
		if tok != "system-tok" {
			t.Errorf("request used token %q, want the LF system credential", tok)
		}
	}
}
