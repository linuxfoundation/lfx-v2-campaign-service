// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// The project-scoped account monitors (lfx-self-serve-ops#897): on a shared-tenancy platform the
// response carries only the requesting project's own campaigns, an empty scope never reaches the
// platform, and a scope that does not fit the monitored account is a 409.

// fakeMonitorScope stands in for the dispatch package's provenance helpers, which this package
// cannot import: an entry whose Result records {"account": X} with X != accountID is a mismatch
// that fails the whole scope, and the id "bad-id" is one the platform cannot read.
func fakeMonitorScope(scope []model.ProjectCampaignScope, accountID string) ([]string, error) {
	ids := make([]string, 0, len(scope))
	foreign := 0
	for _, s := range scope {
		if s.PlatformCampaignID == "bad-id" {
			return nil, fmt.Errorf("fake scope: malformed id: %w", domain.ErrAudienceScopeInvalid)
		}
		var blob struct {
			Account string `json:"account"`
		}
		_ = json.Unmarshal(s.Result, &blob)
		if blob.Account != "" && blob.Account != accountID {
			foreign++
			continue
		}
		ids = append(ids, s.PlatformCampaignID)
	}
	if foreign > 0 {
		return nil, fmt.Errorf("fake scope: %d campaigns under another account: %w", foreign, domain.ErrCampaignAccountMismatch)
	}
	return ids, nil
}

// scopedLiveDispatcher is a live-read platform whose account is shared: an AccountMetricsReader
// that also implements AccountMonitorScoper, counting every upstream read.
type scopedLiveDispatcher struct {
	mockAccountMetricsReaderDispatcher
	mu       sync.Mutex
	reads    int
	precheck error
}

func (d *scopedLiveDispatcher) MonitorScopePrecheck(string) error { return d.precheck }

func (d *scopedLiveDispatcher) ListAccountCampaignMetrics(ctx context.Context, projectID string, platform model.Provider, accountID string, days int) ([]model.AccountCampaignMetrics, error) {
	d.mu.Lock()
	d.reads++
	d.mu.Unlock()
	return append([]model.AccountCampaignMetrics{}, d.rows...), nil
}

func (d *scopedLiveDispatcher) MonitorScopeForAccount(scope []model.ProjectCampaignScope, accountID string) ([]string, error) {
	return fakeMonitorScope(scope, accountID)
}

// scopedReportDispatcher is X's report-backed read with the shared-tenancy capability, counting
// the live campaign list as well as fakeReportReader's own submit/check counts.
type scopedReportDispatcher struct {
	*fakeReportReader
	lists    int
	precheck error
}

func (d *scopedReportDispatcher) MonitorScopePrecheck(string) error { return d.precheck }

func (d *scopedReportDispatcher) ListAccountCampaigns(ctx context.Context, projectID string, platform model.Provider, accountID string) ([]model.AccountCampaignMetrics, error) {
	d.mu.Lock()
	d.lists++
	d.mu.Unlock()
	return d.fakeReportReader.ListAccountCampaigns(ctx, projectID, platform, accountID)
}

func (d *scopedReportDispatcher) MonitorScopeForAccount(scope []model.ProjectCampaignScope, accountID string) ([]string, error) {
	return fakeMonitorScope(scope, accountID)
}

// countingReportStore counts saved-report reads, so an empty scope can be shown not to touch it.
type countingReportStore struct {
	*fakeReportStore
	gets int
}

func (s *countingReportStore) GetAccountReport(ctx context.Context, key model.AccountReportKey) (*model.AccountReportSnapshot, error) {
	s.gets++
	return s.fakeReportStore.GetAccountReport(ctx, key)
}

// scopeRepo pins the project's campaign scope; accounts optionally records a creating account
// per id. A non-nil empty ids is the "no campaigns recorded" scope.
func scopeRepo(ids []string, accounts map[string]string) *fakeCampaignRepo {
	results := map[string]json.RawMessage{}
	for id, acct := range accounts {
		results[id] = json.RawMessage(fmt.Sprintf(`{"account":%q}`, acct))
	}
	return &fakeCampaignRepo{scopeIDs: ids, scopeResults: results}
}

// scopedMonitorCase is one shared-tenancy platform under test, with rows for its own campaign c1
// and another foundation's c2 — c2 chosen so that platform's rules raise an action item for it.
type scopedMonitorCase struct {
	name     string
	provider model.Provider
	account  string
	live     []model.AccountCampaignMetrics
	call     func(svc *ConnectionService, account string) (*conn.AccountMonitor, error)
}

func scopedLiveCases() []scopedMonitorCase {
	return []scopedMonitorCase{
		{
			name: "google ads", provider: model.ProviderGoogleAds, account: "1234567890",
			live: []model.AccountCampaignMetrics{
				{PlatformCampaignID: "c1", Name: "own", Status: "enabled", BudgetDay: 50, Spend: 50, Impressions: 100, Clicks: 5},
				{PlatformCampaignID: "c2", Name: "other foundation", Status: "paused", BudgetDay: 20, Spend: 30},
			},
			call: func(svc *ConnectionService, account string) (*conn.AccountMonitor, error) {
				return svc.MonitorGoogleAdsAccount(context.Background(),
					&conn.MonitorGoogleAdsAccountPayload{ProjectID: "p", AccountID: account, Days: 30})
			},
		},
		{
			name: "meta", provider: model.ProviderMetaAds, account: "act_1001",
			live: []model.AccountCampaignMetrics{
				{PlatformCampaignID: "c1", Name: "own", Status: "ACTIVE", Spend: 5, Impressions: 100, Clicks: 2},
				{PlatformCampaignID: "c2", Name: "other foundation", Status: "ACTIVE"},
			},
			call: func(svc *ConnectionService, account string) (*conn.AccountMonitor, error) {
				return svc.MonitorMetaAdsAccount(context.Background(),
					&conn.MonitorMetaAdsAccountPayload{ProjectID: "p", AccountID: account, Days: 30})
			},
		},
		{
			name: "reddit", provider: model.ProviderRedditAds, account: "a2_owner",
			live: []model.AccountCampaignMetrics{
				{PlatformCampaignID: "c1", Name: "own", Status: "ACTIVE", Spend: 5, Impressions: 100, Clicks: 3},
				{PlatformCampaignID: "c2", Name: "other foundation", Status: "ACTIVE"},
			},
			call: func(svc *ConnectionService, account string) (*conn.AccountMonitor, error) {
				return svc.MonitorRedditAdsAccount(context.Background(),
					&conn.MonitorRedditAdsAccountPayload{ProjectID: "p", AccountID: account, Days: 30})
			},
		},
	}
}

func liveScopeService(provider model.Provider, d PlatformDispatcher, repo *fakeCampaignRepo) *ConnectionService {
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(NewOrchestrator(repo, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{provider: d}))
	return svc
}

// twitterScopeFixture is X's report-backed read with a finished report covering c1 (own) and c2
// (another foundation's, PAUSED so X's paused rule fires for it).
func twitterScopeFixture() (*scopedReportDispatcher, *countingReportStore) {
	reader := &scopedReportDispatcher{fakeReportReader: &fakeReportReader{
		campaigns: []model.AccountCampaignMetrics{
			{PlatformCampaignID: "c1", Name: "own", Status: "ACTIVE", BudgetDay: 10},
			{PlatformCampaignID: "c2", Name: "other foundation", Status: "PAUSED", BudgetDay: 20},
		},
		submitID: "111",
	}}
	store := &countingReportStore{fakeReportStore: &fakeReportStore{snap: &model.AccountReportSnapshot{Ready: &model.ReadyAccountReport{
		AsOf:        time.Date(2026, 10, 5, 14, 30, 0, 0, time.UTC),
		WindowStart: time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC),
		WindowEnd:   time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC),
		Rows: []model.AccountReportRow{
			{PlatformCampaignID: "c1", Spend: 70, Impressions: 1000, Clicks: 30},
			{PlatformCampaignID: "c2", Spend: 40, Impressions: 500, Clicks: 5},
		},
	}}}}
	return reader, store
}

func twitterScopeService(reader *scopedReportDispatcher, store domain.AccountReportRepository, repo *fakeCampaignRepo) *ConnectionService {
	o := NewOrchestrator(repo, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{model.ProviderTwitterAds: reader})
	o.SetAccountReportStore(store)
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(o)
	return svc
}

func monitorTwitter(svc *ConnectionService, account string) (*conn.AccountMonitor, error) {
	return svc.MonitorTwitterAdsAccount(context.Background(),
		&conn.MonitorTwitterAdsAccountPayload{ProjectID: "p", AccountID: account, Days: 7})
}

func campaignIDs(m *conn.AccountMonitor) []string {
	out := make([]string, 0, len(m.Campaigns))
	for _, c := range m.Campaigns {
		out = append(out, c.PlatformCampaignID)
	}
	return out
}

func hasActionItemFor(m *conn.AccountMonitor, id string) bool {
	for _, it := range m.ActionItems {
		if it.CampaignID != nil && *it.CampaignID == id {
			return true
		}
	}
	return false
}

// assertOnlyOwnCampaign checks a scoped response: c1 only, totals over c1 alone, nothing about c2.
func assertOnlyOwnCampaign(t *testing.T, got *conn.AccountMonitor, ownSpend float64) {
	t.Helper()
	if ids := campaignIDs(got); len(ids) != 1 || ids[0] != "c1" {
		t.Fatalf("campaigns = %v, want only the project's own c1", ids)
	}
	if got.Totals.CampaignCount != 1 || got.Totals.Spend != ownSpend {
		t.Errorf("totals = %+v, want campaign_count 1 and spend %v (c1 alone)", got.Totals, ownSpend)
	}
	if hasActionItemFor(got, "c2") {
		t.Errorf("action_items = %+v, want none about another foundation's c2", got.ActionItems)
	}
}

// AC-1/AC-2: an account carrying another foundation's campaign returns only the project's own.
// The control read, with c2 also in scope, proves each fixture's c2 really raises an action item,
// so its absence in the scoped read is the filter's doing.
func TestMonitorAccount_ScopedToProjectCampaigns(t *testing.T) {
	for _, tc := range scopedLiveCases() {
		t.Run(tc.name, func(t *testing.T) {
			control, err := tc.call(liveScopeService(tc.provider,
				&scopedLiveDispatcher{mockAccountMetricsReaderDispatcher: mockAccountMetricsReaderDispatcher{rows: tc.live}},
				scopeRepo([]string{"c1", "c2"}, nil)), tc.account)
			if err != nil {
				t.Fatalf("control read: %v", err)
			}
			if len(control.Campaigns) != 2 || !hasActionItemFor(control, "c2") {
				t.Fatalf("control read: campaigns %v, items %+v — fixture c2 must be returned and raise an action item",
					campaignIDs(control), control.ActionItems)
			}

			got, err := tc.call(liveScopeService(tc.provider,
				&scopedLiveDispatcher{mockAccountMetricsReaderDispatcher: mockAccountMetricsReaderDispatcher{rows: tc.live}},
				scopeRepo([]string{"c1"}, nil)), tc.account)
			if err != nil {
				t.Fatalf("scoped read: %v", err)
			}
			assertOnlyOwnCampaign(t, got, tc.live[0].Spend)
		})
	}
	t.Run("x (report-backed)", func(t *testing.T) {
		reader, store := twitterScopeFixture()
		control, err := monitorTwitter(twitterScopeService(reader, store, scopeRepo([]string{"c1", "c2"}, nil)), "a1")
		if err != nil {
			t.Fatalf("control read: %v", err)
		}
		if len(control.Campaigns) != 2 || !hasActionItemFor(control, "c2") {
			t.Fatalf("control read: campaigns %v, items %+v — fixture c2 must be returned and raise an action item",
				campaignIDs(control), control.ActionItems)
		}

		reader, store = twitterScopeFixture()
		got, err := monitorTwitter(twitterScopeService(reader, store, scopeRepo([]string{"c1"}, nil)), "a1")
		if err != nil {
			t.Fatalf("scoped read: %v", err)
		}
		assertOnlyOwnCampaign(t, got, 70)
		// Only the rows are scoped: the saved report's metadata still describes the read.
		if got.MetricsAsOf == nil || *got.MetricsAsOf != "2026-10-05T14:30:00Z" {
			t.Errorf("metrics_as_of = %v, want the saved report's as-of", got.MetricsAsOf)
		}
		if got.MetricsWindowStart == nil || *got.MetricsWindowStart != "2026-09-29" ||
			got.MetricsWindowEnd == nil || *got.MetricsWindowEnd != "2026-10-05" {
			t.Errorf("metrics window = %v..%v, want the saved report's 2026-09-29..2026-10-05", got.MetricsWindowStart, got.MetricsWindowEnd)
		}
		if got.MetricsPending == nil {
			t.Errorf("metrics_pending absent, want it set on a report-backed platform")
		}
	})
}

// AC-3: a project with no campaigns recorded on the platform gets an empty monitor, and the
// platform is never contacted — an empty scope must never become an account-wide read.
func TestMonitorAccount_EmptyScopeMakesNoUpstreamCall(t *testing.T) {
	for _, tc := range scopedLiveCases() {
		t.Run(tc.name, func(t *testing.T) {
			d := &scopedLiveDispatcher{mockAccountMetricsReaderDispatcher: mockAccountMetricsReaderDispatcher{rows: tc.live}}
			got, err := tc.call(liveScopeService(tc.provider, d, scopeRepo([]string{}, nil)), tc.account)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if got.Campaigns == nil || len(got.Campaigns) != 0 || got.Totals.CampaignCount != 0 || got.Totals.Spend != 0 {
				t.Errorf("campaigns = %v, totals = %+v, want an empty monitor", campaignIDs(got), got.Totals)
			}
			if len(got.ActionItems) != 0 {
				t.Errorf("action_items = %+v, want none", got.ActionItems)
			}
			if d.reads != 0 {
				t.Errorf("upstream reads = %d, want 0", d.reads)
			}
		})
	}
	t.Run("x (report-backed)", func(t *testing.T) {
		reader, store := twitterScopeFixture()
		got, err := monitorTwitter(twitterScopeService(reader, store, scopeRepo([]string{}, nil)), "a1")
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(got.Campaigns) != 0 || got.Totals.CampaignCount != 0 {
			t.Errorf("campaigns = %v, totals = %+v, want an empty monitor", campaignIDs(got), got.Totals)
		}
		if reader.lists != 0 || reader.submits != 0 || reader.checks != 0 || store.gets != 0 {
			t.Errorf("lists=%d submits=%d checks=%d store gets=%d, want no upstream or store call",
				reader.lists, reader.submits, reader.checks, store.gets)
		}
	})
}

// AC-4: a campaign recorded under a different account than the monitored one fails the read with
// a 409 before any upstream call, rather than returning the project's picture minus that campaign.
func TestMonitorAccount_ScopeOnAnotherAccountIs409(t *testing.T) {
	for _, tc := range scopedLiveCases() {
		t.Run(tc.name, func(t *testing.T) {
			d := &scopedLiveDispatcher{mockAccountMetricsReaderDispatcher: mockAccountMetricsReaderDispatcher{rows: tc.live}}
			repo := scopeRepo([]string{"c1", "c2"}, map[string]string{"c2": "another-account"})
			_, err := tc.call(liveScopeService(tc.provider, d, repo), tc.account)
			var ce *conn.AccountMonitorScopeConflictError
			if !errors.As(err, &ce) || ce.Code != "409" || ce.Reason != "campaign_account_mismatch" {
				t.Fatalf("err = %T %v, want a 409 AccountMonitorScopeConflictError with reason campaign_account_mismatch", err, err)
			}
			if !strings.Contains(ce.Message, "to monitor this account") {
				t.Errorf("message = %q, want the monitor's remedy", ce.Message)
			}
			if d.reads != 0 {
				t.Errorf("upstream reads = %d, want 0", d.reads)
			}
		})
	}
	for _, x := range []struct {
		name     string
		ids      []string
		accounts map[string]string
		reason   string
	}{
		{"x (report-backed) campaign on another account", []string{"c1"}, map[string]string{"c1": "another-account"}, "campaign_account_mismatch"},
		{"x (report-backed) campaign id unreadable", []string{"bad-id"}, nil, "campaign_scope_invalid"},
	} {
		t.Run(x.name, func(t *testing.T) {
			reader, store := twitterScopeFixture()
			_, err := monitorTwitter(twitterScopeService(reader, store, scopeRepo(x.ids, x.accounts)), "a1")
			var ce *conn.AccountMonitorConflictError
			if !errors.As(err, &ce) || ce.Code != "409" || ce.Reason != x.reason {
				t.Fatalf("err = %T %v, want a 409 AccountMonitorConflictError with reason %q", err, err, x.reason)
			}
			if reader.lists != 0 || reader.submits != 0 || store.gets != 0 {
				t.Errorf("lists=%d submits=%d store gets=%d, want no upstream or store call", reader.lists, reader.submits, store.gets)
			}
		})
	}
}

// A locally deleted campaign keeps running upstream (DeleteCampaign is local-only), so the monitor
// still shows it — even as the project's only one, which must therefore reach the platform. A
// deleted row the scoper refuses for the monitored account is dropped, never a 409:
// it cannot be re-dispatched or reconciled, so the refusal would have no remedy.
func TestMonitorAccount_DeletedCampaignsStayVisible(t *testing.T) {
	for _, tc := range scopedLiveCases() {
		t.Run(tc.name+"/deleted-only scope reads the platform and shows it", func(t *testing.T) {
			d := &scopedLiveDispatcher{mockAccountMetricsReaderDispatcher: mockAccountMetricsReaderDispatcher{rows: tc.live}}
			repo := scopeRepo([]string{}, nil)
			repo.deletedScopeIDs = []string{"c1"}
			got, err := tc.call(liveScopeService(tc.provider, d, repo), tc.account)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			assertOnlyOwnCampaign(t, got, tc.live[0].Spend)
			if d.reads != 1 {
				t.Errorf("upstream reads = %d, want 1", d.reads)
			}
		})
		t.Run(tc.name+"/deleted row on another account is dropped, not a 409", func(t *testing.T) {
			d := &scopedLiveDispatcher{mockAccountMetricsReaderDispatcher: mockAccountMetricsReaderDispatcher{rows: tc.live}}
			repo := scopeRepo([]string{"c1"}, map[string]string{"c2": "another-account"})
			repo.deletedScopeIDs = []string{"c2"}
			got, err := tc.call(liveScopeService(tc.provider, d, repo), tc.account)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			assertOnlyOwnCampaign(t, got, tc.live[0].Spend)
		})
	}
	t.Run("x (report-backed) deleted-only scope", func(t *testing.T) {
		reader, store := twitterScopeFixture()
		repo := scopeRepo([]string{}, nil)
		repo.deletedScopeIDs = []string{"c1"}
		got, err := monitorTwitter(twitterScopeService(reader, store, repo), "a1")
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		assertOnlyOwnCampaign(t, got, 70)
	})
	t.Run("deleted scope lookup failure fails the read", func(t *testing.T) {
		d := &scopedLiveDispatcher{mockAccountMetricsReaderDispatcher: mockAccountMetricsReaderDispatcher{rows: scopedLiveCases()[0].live}}
		repo := scopeRepo([]string{"c1"}, nil)
		repo.deletedScopeErr = errors.New("db down")
		got, err := liveScopeService(model.ProviderGoogleAds, d, repo).MonitorGoogleAdsAccount(context.Background(),
			&conn.MonitorGoogleAdsAccountPayload{ProjectID: "p", AccountID: "1234567890", Days: 30})
		if err == nil || got != nil {
			t.Fatalf("got %v, err %v; want the read to fail", got, err)
		}
		if d.reads != 0 {
			t.Errorf("upstream reads = %d, want 0", d.reads)
		}
	})
}

// The dispatcher's credential-free refusals answer before the scope is read, so a disabled monitor
// or a malformed account id is the same 400 whether the project has no campaigns (which would
// otherwise be 200 empty) or a campaign on another account (otherwise 409).
func TestMonitorAccount_PrecheckAnswersBeforeTheScope(t *testing.T) {
	disabled := fmt.Errorf("x account monitor is disabled: %w", domain.ErrAccountMetricsUnsupported)
	malformed := fmt.Errorf("bad shape: %w", domain.ErrAccountIDMalformed)
	scopes := []struct {
		name     string
		ids      []string
		accounts map[string]string
	}{
		{"empty scope", []string{}, nil},
		{"scope on another account", []string{"c1"}, map[string]string{"c1": "another-account"}},
	}
	for _, sc := range scopes {
		t.Run("x disabled/"+sc.name, func(t *testing.T) {
			reader, store := twitterScopeFixture()
			reader.precheck = disabled
			_, err := monitorTwitter(twitterScopeService(reader, store, scopeRepo(sc.ids, sc.accounts)), "a1")
			if _, ok := err.(*conn.BadRequestError); !ok {
				t.Fatalf("err = %T %v, want the disabled monitor's 400", err, err)
			}
			if reader.lists != 0 || reader.submits != 0 || store.gets != 0 {
				t.Errorf("lists=%d submits=%d store gets=%d, want no upstream or store call", reader.lists, reader.submits, store.gets)
			}
		})
		for _, tc := range scopedLiveCases() {
			t.Run(tc.name+" malformed account/"+sc.name, func(t *testing.T) {
				d := &scopedLiveDispatcher{mockAccountMetricsReaderDispatcher: mockAccountMetricsReaderDispatcher{rows: tc.live}, precheck: malformed}
				_, err := tc.call(liveScopeService(tc.provider, d, scopeRepo(sc.ids, sc.accounts)), tc.account)
				if _, ok := err.(*conn.BadRequestError); !ok {
					t.Fatalf("err = %T %v, want the malformed account id's 400", err, err)
				}
				if d.reads != 0 {
					t.Errorf("upstream reads = %d, want 0", d.reads)
				}
			})
		}
	}
	t.Run("precheck runs before the scope lookup", func(t *testing.T) {
		d := &scopedLiveDispatcher{mockAccountMetricsReaderDispatcher: mockAccountMetricsReaderDispatcher{rows: scopedLiveCases()[0].live}, precheck: malformed}
		repo := &fakeCampaignRepo{scopeErr: errors.New("db down")}
		_, err := liveScopeService(model.ProviderGoogleAds, d, repo).MonitorGoogleAdsAccount(context.Background(),
			&conn.MonitorGoogleAdsAccountPayload{ProjectID: "p", AccountID: "1234567890", Days: 30})
		if _, ok := err.(*conn.BadRequestError); !ok {
			t.Fatalf("err = %T %v, want the precheck's 400 rather than the scope lookup's failure", err, err)
		}
	})
}

// A scope that cannot be established fails the read; it never falls back to the account-wide one.
func TestMonitorAccount_ScopeLookupFailureMakesNoUpstreamCall(t *testing.T) {
	d := &scopedLiveDispatcher{mockAccountMetricsReaderDispatcher: mockAccountMetricsReaderDispatcher{rows: scopedLiveCases()[0].live}}
	repo := &fakeCampaignRepo{scopeErr: errors.New("db down")}
	got, err := liveScopeService(model.ProviderGoogleAds, d, repo).MonitorGoogleAdsAccount(context.Background(),
		&conn.MonitorGoogleAdsAccountPayload{ProjectID: "p", AccountID: "1234567890", Days: 30})
	if err == nil || got != nil {
		t.Fatalf("got %v, err %v; want the read to fail", got, err)
	}
	if d.reads != 0 {
		t.Errorf("upstream reads = %d, want 0", d.reads)
	}
}

// AC-5: LinkedIn and Microsoft do not implement AccountMonitorScoper, so an empty campaign scope
// leaves their account-wide read unchanged.
func TestMonitorAccount_UnscopedPlatformsKeepTheAccountRead(t *testing.T) {
	t.Run("linkedin", func(t *testing.T) {
		rows := []model.AccountCampaignMetrics{
			{PlatformCampaignID: "1", Name: "a", Status: "ACTIVE", BudgetDay: 10},
			{PlatformCampaignID: "2", Name: "b", Status: "ACTIVE", BudgetDay: 20},
		}
		svc := liveScopeService(model.ProviderLinkedInAds, &mockAccountMetricsReaderDispatcher{rows: rows}, scopeRepo([]string{}, nil))
		got, err := svc.MonitorLinkedinAdsAccount(context.Background(),
			&conn.MonitorLinkedinAdsAccountPayload{ProjectID: "p", AccountID: "512345678", Days: 30})
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(got.Campaigns) != 2 {
			t.Errorf("campaigns = %v, want both account rows", campaignIDs(got))
		}
	})
	t.Run("microsoft", func(t *testing.T) {
		o := NewOrchestrator(scopeRepo([]string{}, nil), newFakeJobRepo(), map[model.Provider]PlatformDispatcher{
			model.ProviderMicrosoftAds: &fakeReportReader{campaigns: twoCampaigns(), submitID: "r1"},
		})
		o.SetAccountReportStore(&fakeReportStore{})
		svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
		svc.SetOrchestrator(o)
		got, err := svc.MonitorMicrosoftAdsAccount(context.Background(),
			&conn.MonitorMicrosoftAdsAccountPayload{ProjectID: "p", AccountID: "123", Days: 7})
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(got.Campaigns) != 2 {
			t.Errorf("campaigns = %v, want both account rows", campaignIDs(got))
		}
	})
}

// classifyMonitorError's arm order follows classifyInsightsErrorFor: the mixed fault first, then
// unknown provenance, then a plain mismatch — each joined error carries the later sentinels too.
// A platform whose monitor declares no 409 never receives one.
func TestClassifyMonitorError(t *testing.T) {
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	mixed := errors.Join(domain.ErrCampaignProvenanceMixed, domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch)
	unknown := errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch)
	cases := []struct {
		name     string
		err      error
		contains string
		reason   string
	}{
		{"mixed names both remedies", mixed, "others do not record", "campaign_account_mismatch"},
		{"unknown provenance asks for re-dispatch", unknown, "do not record which ad account", "campaign_account_mismatch"},
		{"mismatch", fmt.Errorf("wrapped: %w", domain.ErrCampaignAccountMismatch), "than the one being monitored — monitor the account that owns them", "campaign_account_mismatch"},
		{"unreadable id", fmt.Errorf("wrapped: %w", domain.ErrAudienceScopeInvalid), "an id the platform cannot read", "campaign_scope_invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Google, Meta and Reddit declare AccountMonitorScopeConflictError, never the shared
			// ConflictError whose published example is a connection collision; every 409 carries
			// its reason.
			err := svc.classifyMonitorError(context.Background(), "p", model.ProviderRedditAds, redditAdsAccountDiscovery, tc.err)
			var ce *conn.AccountMonitorScopeConflictError
			if !errors.As(err, &ce) || ce.Code != "409" || ce.Reason != tc.reason || !strings.Contains(ce.Message, tc.contains) {
				t.Fatalf("err = %T %v, want a 409 AccountMonitorScopeConflictError with reason %q containing %q", err, err, tc.reason, tc.contains)
			}
		})
	}
	t.Run("linkedin is delegated unchanged", func(t *testing.T) {
		err := svc.classifyMonitorError(context.Background(), "p", model.ProviderLinkedInAds, linkedInAdsMonitorDiscovery, domain.ErrCampaignAccountMismatch)
		var ce *conn.ConflictError
		var sce *conn.AccountMonitorScopeConflictError
		if errors.As(err, &ce) || errors.As(err, &sce) {
			t.Fatalf("err = %v, want classifyDiscoveryError's answer, not a 409 the method does not declare", err)
		}
	})
}
