// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// #615 acceptance for LinkedIn, end to end through the real LinkedInDispatcher and the
// orchestrator: a project with no LinkedIn connection of its own monitors the campaigns it
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
// token and path of every request, and answers the same three campaigns on whichever account is
// read: 1001 (tlf's), 1002 (aswf's) and 1003, which no project records unless a test records
// it.
func linkedInMonitorFallbackDispatcher(t *testing.T, reader *scopedConnReader) (*LinkedInDispatcher, func() (tokens, paths []string)) {
	t.Helper()
	var mu sync.Mutex
	var tokens, paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tokens = append(tokens, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		paths = append(paths, r.URL.Path)
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
// Each monitors it without naming the account (DefaultMonitorAccount names 538170226, as the
// response's account_id then reports it), through the system credential, and each gets back only
// its own campaign: neither sees the other's, nor the unrecorded one.
func TestLinkedIn_AccountMonitor_TwoProjectsOnTheSharedSystemAccount(t *testing.T) {
	d, requests := linkedInMonitorFallbackDispatcher(t, &scopedConnReader{
		rows: map[string]*model.Connection{model.SystemProjectID: systemLinkedInRow()},
	})
	o := linkedInMonitorOrchestrator(d, map[string][]model.ProjectCampaignScope{
		"tlf":  {liRecorded("1001", "538170226")},
		"aswf": {liRecorded("1002", "538170226")},
	})
	for project, want := range map[string]string{"tlf": "1001", "aswf": "1002"} {
		t.Run(project, func(t *testing.T) {
			ctx := context.Background()
			account, err := o.DefaultMonitorAccount(ctx, project, model.ProviderLinkedInAds)
			if err != nil || account != "538170226" {
				t.Fatalf("DefaultMonitorAccount = %q, %v; want the system account 538170226 its campaign records", account, err)
			}
			if tokens, _ := requests(); len(tokens) != 0 {
				t.Fatalf("naming the account contacted LinkedIn: %v", tokens)
			}
			read, err := o.ReadAccountCampaignMetrics(ctx, project, model.ProviderLinkedInAds, account, 30)
			if err != nil {
				t.Fatalf("ReadAccountCampaignMetrics through the LF system row: %v", err)
			}
			if got := monitorRowIDs(read); len(got) != 1 || got[0] != want {
				t.Errorf("rows = %v, want only %s's own campaign %s", got, project, want)
			}
			tokens, paths := requests()
			if len(tokens) == 0 {
				t.Fatal("LinkedIn was never called")
			}
			for i, tok := range tokens {
				if tok != "system-tok" {
					t.Errorf("request %s used token %q, want the LF system credential", paths[i], tok)
				}
			}
			if !strings.Contains(paths[0], "538170226") {
				t.Errorf("first request %s, want a read of the system account 538170226", paths[0])
			}
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
	tokens, paths := requests()
	if len(tokens) == 0 || !strings.Contains(paths[0], "500928401") {
		t.Fatalf("requests %v, want a read of the granted account 500928401", paths)
	}
	for _, tok := range tokens {
		if tok != "system-tok" {
			t.Errorf("request used token %q, want the LF system credential", tok)
		}
	}
}

// With nothing recorded on LinkedIn the monitor is an empty 200 and LinkedIn is never asked, on
// the system fallback, whether or not the caller names the account.
func TestLinkedIn_AccountMonitor_NoRecordedCampaignsIsEmptyWithoutUpstreamCall(t *testing.T) {
	d, requests := linkedInMonitorFallbackDispatcher(t, &scopedConnReader{
		rows: map[string]*model.Connection{model.SystemProjectID: systemLinkedInRow()},
	})
	o := linkedInMonitorOrchestrator(d, nil)
	account, err := o.DefaultMonitorAccount(context.Background(), "tlf", model.ProviderLinkedInAds)
	if err != nil || account != "" {
		t.Fatalf("DefaultMonitorAccount = %q, %v; want no account to name", account, err)
	}
	read, err := o.ReadAccountCampaignMetrics(context.Background(), "tlf", model.ProviderLinkedInAds, "538170226", 30)
	if err != nil {
		t.Fatalf("ReadAccountCampaignMetrics: %v", err)
	}
	if read.Rows == nil || len(read.Rows) != 0 {
		t.Errorf("rows = %#v, want a non-nil empty slice", read.Rows)
	}
	if tokens, paths := requests(); len(tokens) != 0 {
		t.Errorf("requests %v, want none", paths)
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
			if tokens, paths := requests(); len(tokens) != 0 {
				t.Errorf("requests %v, want none: the refusal must come before any upstream call", paths)
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
	tokens, paths := requests()
	if len(tokens) == 0 || !strings.Contains(paths[0], "538170226") {
		t.Fatalf("requests %v, want a read of the system account 538170226", paths)
	}
	for _, tok := range tokens {
		if tok != "system-tok" {
			t.Errorf("request used token %q, want the LF system credential", tok)
		}
	}
}
