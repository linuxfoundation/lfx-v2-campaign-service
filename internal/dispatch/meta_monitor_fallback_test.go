// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// monitorScopeCampaigns is the slice of domain.CampaignRepository an account-monitor read
// consults: each project's recorded campaigns on the platform, with none soft-deleted. Any other
// method panics on the nil embedded interface, which is what keeps a test honest about what the
// read touches.
type monitorScopeCampaigns struct {
	domain.CampaignRepository
	live map[string][]model.ProjectCampaignScope
}

func (m monitorScopeCampaigns) ListProjectPlatformCampaignIDs(_ context.Context, projectID string, _ model.Provider) ([]model.ProjectCampaignScope, error) {
	return m.live[projectID], nil
}

func (m monitorScopeCampaigns) ListProjectPlatformDeletedCampaignIDs(context.Context, string, model.Provider) ([]model.ProjectCampaignScope, error) {
	return nil, nil
}

func (m monitorScopeCampaigns) ListProjectPlatformMonitorCampaignIDs(_ context.Context, projectID string, _ model.Provider) ([]model.ProjectCampaignScope, error) {
	return m.live[projectID], nil
}

const systemMetaCreds = `{"AccessToken":"system-tok"}`

// systemMetaConnFor is the LF system Meta row configured for accountID, with an access token
// ("system-tok") that differs from the project fixture's ("tok").
func systemMetaConnFor(accountID string) *model.Connection {
	c := activeMetaConn(systemMetaCreds)
	c.AccountID = accountID
	return c
}

// metaMonitorFallbackDispatcher wires a Meta dispatcher whose API records the bearer token of
// every request, so a test can tell WHICH stored credential read the account, and answers two
// campaigns on act_777: 111 (tlf's) and 222 (another foundation's on the same shared account).
func metaMonitorFallbackDispatcher(t *testing.T, reader *scopedConnReader) (*MetaDispatcher, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var tokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tokens = append(tokens, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = w.Write([]byte(`{"data":[{"id":"111","name":"tlf","status":"ACTIVE","daily_budget":"1000"},{"id":"222","name":"other","status":"ACTIVE","daily_budget":"1000"}],"paging":{}}`))
		case strings.HasSuffix(r.URL.Path, "/insights"):
			_, _ = w.Write([]byte(`{"data":[{"campaign_id":"111","impressions":"10","clicks":"1","spend":"5"},{"campaign_id":"222","impressions":"20","clicks":"2","spend":"9"}],"paging":{}}`))
		default: // the account currency read
			_, _ = w.Write([]byte(`{"currency":"USD","id":"act_777"}`))
		}
	}))
	t.Cleanup(srv.Close)
	d := NewMetaDispatcher(reader, identityEncryptor{}, meta.WithBaseURL(srv.URL))
	return d, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), tokens...)
	}
}

func metaMonitorOrchestrator(d *MetaDispatcher, live map[string][]model.ProjectCampaignScope) *service.Orchestrator {
	return service.NewOrchestrator(monitorScopeCampaigns{live: live}, nil,
		map[model.Provider]service.PlatformDispatcher{model.ProviderMetaAds: d})
}

// tlfMetaScope is tlf's one recorded campaign, created on the LF system account act_777.
var tlfMetaScope = map[string][]model.ProjectCampaignScope{
	"tlf": {{PlatformCampaignID: "111", Result: []byte(`{"AccountID":"act_777"}`)}},
}

// #615: a project with no Meta connection of its own monitors the account the LF system row is
// configured for — the account its campaigns were created in — on the system credential, and sees
// only its own recorded campaigns, not the other foundation's campaign on the same account.
// Before #615 this was a 404, so nothing created through the system account could be monitored.
func TestMeta_AccountMonitor_FallsBackToSystemForItsAccount(t *testing.T) {
	d, tokens := metaMonitorFallbackDispatcher(t, &scopedConnReader{
		rows: map[string]*model.Connection{model.SystemProjectID: systemMetaConnFor("act_777")},
	})
	read, err := metaMonitorOrchestrator(d, tlfMetaScope).ReadAccountCampaignMetrics(
		context.Background(), "tlf", model.ProviderMetaAds, "act_777", 30)
	if err != nil {
		t.Fatalf("ReadAccountCampaignMetrics on the LF system account: %v", err)
	}
	if len(read.Rows) != 1 || read.Rows[0].PlatformCampaignID != "111" {
		t.Errorf("rows = %+v, want only tlf's recorded campaign 111", read.Rows)
	}
	got := tokens()
	if len(got) == 0 {
		t.Fatal("Meta was never called")
	}
	for _, tok := range got {
		if tok != "system-tok" {
			t.Errorf("request used token %q, want the LF system credential (all: %v)", tok, got)
		}
	}
}

// The LF system credential reads only the account the LF system row is configured for. Any other
// account keeps the 404 a project without a connection of its own always got, and nothing is sent.
func TestMeta_AccountMonitor_SystemFallbackRefusesOtherAccounts(t *testing.T) {
	for _, forced := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary fallback", true: "forced-system mode"}[forced], func(t *testing.T) {
			d, tokens := metaMonitorFallbackDispatcher(t, &scopedConnReader{
				rows: map[string]*model.Connection{model.SystemProjectID: systemMetaConnFor("act_777")},
			})
			d.creds.forceSystemPaidAds = forced
			_, err := d.ReadAccountMonitor(context.Background(), "tlf", model.ProviderMetaAds, "act_222", 30)
			if !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("err = %v, want domain.ErrNotFound for an account the LF system row is not configured for", err)
			}
			if got := tokens(); len(got) != 0 {
				t.Errorf("requests %v, want none: the refusal must come before any upstream call", got)
			}
		})
	}
}

// A project with a connection of its own reads its own account with its own credential, as
// before #615, and the system account with the system credential; an account neither row is
// configured for is still refused by requireMetaManagedAccount against the project's own row.
func TestMeta_AccountMonitor_ReadsWithTheRowConfiguredForTheAccount(t *testing.T) {
	own := activeMetaConn(goodMetaCreds) // token "tok"
	own.AccountID = "act_555"
	system := systemMetaConnFor("act_777")
	cases := []struct {
		name      string
		accountID string
		wantToken string
		wantErr   error
	}{
		{"the project's own account", "act_555", "tok", nil},
		{"the LF system account, with a connection of its own elsewhere", "act_777", "system-tok", nil},
		{"an account neither row is configured for", "act_999", "", domain.ErrAccountNotManagedByConnection},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, tokens := metaMonitorFallbackDispatcher(t, &scopedConnReader{
				rows: map[string]*model.Connection{"tlf": own, model.SystemProjectID: system},
			})
			_, err := d.ReadAccountMonitor(context.Background(), "tlf", model.ProviderMetaAds, tc.accountID, 30)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				if got := tokens(); len(got) != 0 {
					t.Errorf("requests %v, want none", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadAccountMonitor: %v", err)
			}
			if got := tokens(); len(got) == 0 || got[0] != tc.wantToken {
				t.Errorf("requests used tokens %v, want %q", got, tc.wantToken)
			}
		})
	}
}

// With nothing recorded on Meta the monitor is an empty 200 and Meta is never asked, even on the
// system fallback.
func TestMeta_AccountMonitor_NoRecordedCampaignsIsEmptyWithoutUpstreamCall(t *testing.T) {
	d, tokens := metaMonitorFallbackDispatcher(t, &scopedConnReader{
		rows: map[string]*model.Connection{model.SystemProjectID: systemMetaConnFor("act_777")},
	})
	read, err := metaMonitorOrchestrator(d, nil).ReadAccountCampaignMetrics(
		context.Background(), "tlf", model.ProviderMetaAds, "act_777", 30)
	if err != nil {
		t.Fatalf("ReadAccountCampaignMetrics: %v", err)
	}
	if read.Rows == nil || len(read.Rows) != 0 {
		t.Errorf("rows = %#v, want a non-nil empty slice", read.Rows)
	}
	if got := tokens(); len(got) != 0 {
		t.Errorf("requests %v, want none", got)
	}
}

// With no connection of its own and no LF system row, the read is still the 404 it always was.
func TestMeta_AccountMonitor_NoConnectionAnywhereIsNotFound(t *testing.T) {
	d, tokens := metaMonitorFallbackDispatcher(t, &scopedConnReader{rows: map[string]*model.Connection{}})
	_, err := d.ReadAccountMonitor(context.Background(), "tlf", model.ProviderMetaAds, "act_777", 30)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want domain.ErrNotFound", err)
	}
	if got := tokens(); len(got) != 0 {
		t.Errorf("requests %v, want none", got)
	}
}

// A project that DISCONNECTED its own Meta connection still monitors the campaigns it created on
// the LF system account: resolveExisting's error arm rescues the read through the system row when
// that row is configured for the account (systemConn alone refuses a disconnected project). Any
// other account stays a 404 with nothing sent.
func TestMeta_AccountMonitor_DisconnectedProjectReadsItsSystemAccount(t *testing.T) {
	reader := func() *scopedConnReader {
		return &scopedConnReader{
			rows:       map[string]*model.Connection{model.SystemProjectID: systemMetaConnFor("act_777")},
			tombstoned: map[string]bool{"tlf": true},
		}
	}

	d, tokens := metaMonitorFallbackDispatcher(t, reader())
	read, err := metaMonitorOrchestrator(d, tlfMetaScope).ReadAccountCampaignMetrics(
		context.Background(), "tlf", model.ProviderMetaAds, "act_777", 30)
	if err != nil {
		t.Fatalf("disconnected project reading its system account: %v", err)
	}
	if len(read.Rows) != 1 || read.Rows[0].PlatformCampaignID != "111" {
		t.Errorf("rows = %+v, want only tlf's recorded campaign 111", read.Rows)
	}
	for _, tok := range tokens() {
		if tok != "system-tok" {
			t.Errorf("request used token %q, want the LF system credential", tok)
		}
	}

	d, tokens = metaMonitorFallbackDispatcher(t, reader())
	if _, err := d.ReadAccountMonitor(context.Background(), "tlf", model.ProviderMetaAds, "act_222", 30); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want domain.ErrNotFound for an account the LF system row is not configured for", err)
	}
	if got := tokens(); len(got) != 0 {
		t.Errorf("requests %v, want none", got)
	}
}
