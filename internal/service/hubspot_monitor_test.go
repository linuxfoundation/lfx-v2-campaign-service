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

// mockEmailMonitorDispatcher implements EmailMonitorReader. calls records every read so a test
// can assert the dispatcher was (or was not) reached and with which campaigns.
type mockEmailMonitorDispatcher struct {
	mu    sync.Mutex
	read  *model.HubSpotEmailMonitorRead
	err   error
	calls [][]*model.Campaign
	days  []int
}

func (m *mockEmailMonitorDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, nil
}

func (m *mockEmailMonitorDispatcher) ReadEmailMonitor(_ context.Context, _ string, _ model.Provider, campaigns []*model.Campaign, days int) (*model.HubSpotEmailMonitorRead, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, campaigns)
	m.days = append(m.days, days)
	return m.read, m.err
}

func (m *mockEmailMonitorDispatcher) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

// The pinned instants a read "happened" at. The dispatcher owns the clock (the HubSpot client's
// injected one); these tests pin what it reports.
var (
	monAsOf  = time.Date(2026, 10, 8, 14, 30, 0, 0, time.UTC)
	monStart = time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	monEnd   = time.Date(2026, 10, 8, 23, 59, 59, int(999*time.Millisecond), time.UTC)
)

func hubspotMonitorService(repo *fakeCampaignRepo, d PlatformDispatcher) *ConnectionService {
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(&Orchestrator{
		campaigns:   repo,
		dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderHubSpot: d},
	})
	return svc
}

// recordedCampaigns returns n campaigns newest first, one recorded per hour back from newest.
func recordedCampaigns(n int) []*model.Campaign {
	return recordedCampaignsFrom(n, monAsOf)
}

func recordedCampaignsFrom(n int, newest time.Time) []*model.Campaign {
	out := make([]*model.Campaign, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, &model.Campaign{ID: fmt.Sprintf("c%d", i), Platform: model.ProviderHubSpot,
			PlatformCampaignID: fmt.Sprintf("%d", 1000+i), CreatedAt: newest.Add(-time.Duration(i) * time.Hour)})
	}
	return out
}

func TestMonitorHubspotAccount_TotalsAndRatesAreFromTheSums(t *testing.T) {
	d := &mockEmailMonitorDispatcher{read: &model.HubSpotEmailMonitorRead{
		Emails: []model.HubSpotMonitorEmail{
			{CampaignID: "c0", EmailID: "1000", Name: "big", Counters: model.HubSpotEmailCounters{Sent: 1000, Delivered: 1000, Opens: 300, Clicks: 40, Bounces: 0, Unsubscribes: 2}},
			{CampaignID: "c0", EmailID: "1001", Name: "small B", ABVariant: true, Deleted: true, Counters: model.HubSpotEmailCounters{Sent: 100, Delivered: 0, Bounces: 100}},
		},
		SpanStart: monStart, SpanEnd: monEnd, AsOf: monAsOf,
		EmailsChecked: 3, EmailsNotSentInWindow: 1, EmailsUnattributable: 2,
	}}
	svc := hubspotMonitorService(&fakeCampaignRepo{recent: recordedCampaigns(1)}, d)

	out, err := svc.MonitorHubspotAccount(context.Background(), &conn.MonitorHubspotAccountPayload{ProjectID: "p", Days: 30})
	if err != nil {
		t.Fatalf("MonitorHubspotAccount: %v", err)
	}
	if out.Days != 30 || len(out.Emails) != 2 {
		t.Fatalf("days/emails = %d/%d", out.Days, len(out.Emails))
	}
	tot := out.Totals
	if tot.EmailCount != 2 || tot.Sent != 1100 || tot.Delivered != 1000 || tot.Bounces != 100 || tot.Opens != 300 {
		t.Errorf("totals = %+v", tot)
	}
	// From the SUMS: 300/1000 opens, 100/1100 bounces — not the average of 30% and (absent).
	if tot.OpenRate == nil || *tot.OpenRate != 0.3 || tot.BounceRate == nil || *tot.BounceRate != 100.0/1100.0 {
		t.Errorf("total rates = open %v bounce %v", tot.OpenRate, tot.BounceRate)
	}
	// The undelivered variant has no delivered-based rates: absent, not 0.
	b := out.Emails[1]
	if !b.Deleted || out.Emails[0].Deleted {
		t.Errorf("deleted flags = %v/%v, want only the second row marked", out.Emails[0].Deleted, b.Deleted)
	}
	if !b.AbVariant || b.OpenRate != nil || b.ClickRate != nil || b.UnsubscribeRate != nil || b.BounceRate == nil || *b.BounceRate != 1 {
		t.Errorf("variant row = %+v", b)
	}
	if out.MetricsAsOf == nil || *out.MetricsAsOf != "2026-10-08T14:30:00Z" ||
		out.MetricsWindowStart == nil || *out.MetricsWindowStart != "2026-09-09" ||
		out.MetricsWindowEnd == nil || *out.MetricsWindowEnd != "2026-10-08" {
		t.Errorf("as-of/window = %v %v %v", out.MetricsAsOf, out.MetricsWindowStart, out.MetricsWindowEnd)
	}
	if out.EmailsChecked != 3 || out.EmailsNotSentInWindow != 1 || out.EmailsUnattributable != 2 || out.EmailsTruncated {
		t.Errorf("completeness = %d/%d/%d/%v", out.EmailsChecked, out.EmailsNotSentInWindow, out.EmailsUnattributable, out.EmailsTruncated)
	}
	// The variant sent 100 and delivered none: the HIGH finding, first, naming that email.
	// campaign_id is the service UUID, as on the rows; email_id is the join key to the row.
	if len(out.ActionItems) == 0 || out.ActionItems[0].Priority != "HIGH" ||
		out.ActionItems[0].CampaignID == nil || *out.ActionItems[0].CampaignID != "c0" ||
		out.ActionItems[0].EmailID == nil || *out.ActionItems[0].EmailID != "1001" {
		t.Errorf("action items = %+v", out.ActionItems)
	}
}

// An empty scope is a 200 with nothing in it, and HubSpot is never reached — the dispatcher is
// not even called, so no connection is resolved either.
func TestMonitorHubspotAccount_EmptyScopeAnswersEmptyWithoutCallingHubSpot(t *testing.T) {
	d := &mockEmailMonitorDispatcher{err: errors.New("must not be called")}
	svc := hubspotMonitorService(&fakeCampaignRepo{}, d)
	out, err := svc.MonitorHubspotAccount(context.Background(), &conn.MonitorHubspotAccountPayload{ProjectID: "p", Days: 7})
	if err != nil {
		t.Fatalf("MonitorHubspotAccount: %v", err)
	}
	if d.callCount() != 0 {
		t.Error("the dispatcher was called for a project with no recorded email")
	}
	if out.Emails == nil || len(out.Emails) != 0 || out.ActionItems == nil || len(out.ActionItems) != 0 || out.Totals.EmailCount != 0 {
		t.Errorf("empty answer = %+v", out)
	}
	if out.MetricsAsOf != nil || out.MetricsWindowStart != nil || out.MetricsWindowEnd != nil {
		t.Error("an as-of or window was stated for a read that did not happen")
	}
	if out.Totals.OpenRate != nil || out.Totals.BounceRate != nil {
		t.Error("rates over nothing must be absent")
	}
}

// The scope is read with cap plus one; an extra row truncates to the cap and says so.
func TestMonitorHubspotAccount_TruncatesToTheCapAndSaysSo(t *testing.T) {
	repo := &fakeCampaignRepo{recent: recordedCampaigns(hubspotMonitorMaxCampaigns + 5)}
	d := &mockEmailMonitorDispatcher{read: &model.HubSpotEmailMonitorRead{Emails: []model.HubSpotMonitorEmail{}, SpanStart: monStart, SpanEnd: monEnd, AsOf: monAsOf}}
	svc := hubspotMonitorService(repo, d)
	out, err := svc.MonitorHubspotAccount(context.Background(), &conn.MonitorHubspotAccountPayload{ProjectID: "p", Days: 30})
	if err != nil {
		t.Fatalf("MonitorHubspotAccount: %v", err)
	}
	if !out.EmailsTruncated {
		t.Error("emails_truncated is false with more campaigns than the cap")
	}
	if len(repo.recentLimits) != 1 || repo.recentLimits[0] != hubspotMonitorMaxCampaigns+1 {
		t.Errorf("scope limits = %v, want one read of cap+1", repo.recentLimits)
	}
	if got := len(d.calls[0]); got != hubspotMonitorMaxCampaigns {
		t.Errorf("dispatcher got %d campaigns, want the cap %d", got, hubspotMonitorMaxCampaigns)
	}
	if d.calls[0][0].ID != "c0" {
		t.Errorf("first campaign = %s, want the newest (c0)", d.calls[0][0].ID)
	}

	// More rows than the cap is truncation EVEN WHEN every checked row predates the window: an
	// unchecked older draft could have been sent late, inside the window, and nothing stored
	// says otherwise.
	repo3 := &fakeCampaignRepo{recent: recordedCampaignsFrom(hubspotMonitorMaxCampaigns+5, monStart.Add(-100*24*time.Hour))}
	out3, err := hubspotMonitorService(repo3, d).MonitorHubspotAccount(context.Background(), &conn.MonitorHubspotAccountPayload{ProjectID: "p", Days: 30})
	if err != nil || !out3.EmailsTruncated {
		t.Errorf("every row older than the window: truncated=%v err=%v, want true", out3 != nil && out3.EmailsTruncated, err)
	}

	// Exactly the cap is not truncated.
	repo2 := &fakeCampaignRepo{recent: recordedCampaigns(hubspotMonitorMaxCampaigns)}
	out2, err := hubspotMonitorService(repo2, d).MonitorHubspotAccount(context.Background(), &conn.MonitorHubspotAccountPayload{ProjectID: "p", Days: 30})
	if err != nil || out2.EmailsTruncated {
		t.Errorf("exactly the cap: truncated=%v err=%v", out2 != nil && out2.EmailsTruncated, err)
	}
}

func TestMonitorHubspotAccount_ClassifiesFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"no connection of its own", domain.ErrNotFound, "404"},
		{"unusable project-owned connection", fmt.Errorf("%w: %w", domain.ErrConnectionNotUsable, domain.ErrConnectionInactive), "400"},
		{"unusable LF system fallback connection", fmt.Errorf("%w: %w: %w", domain.ErrSystemConnectionNotUsable, domain.ErrConnectionNotUsable, domain.ErrConnectionInactive), "500"},
		{"days re-checked by the dispatcher", domain.ErrMonitorDaysInvalid, "400"},
		{"decryption failure", domain.ErrCredentialDecryptionFailed, "500"},
		{"upstream failure", errors.New("hubspot: GET /marketing/v3/emails/statistics/list: 403"), "503"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := hubspotMonitorService(&fakeCampaignRepo{recent: recordedCampaigns(1)}, &mockEmailMonitorDispatcher{err: tc.err})
			out, err := svc.MonitorHubspotAccount(context.Background(), &conn.MonitorHubspotAccountPayload{ProjectID: "p", Days: 30})
			if out != nil {
				t.Fatalf("a partial result accompanied the error: %+v", out)
			}
			switch tc.want {
			case "404":
				if _, ok := err.(*conn.NotFoundError); !ok {
					t.Fatalf("err = %T %v, want 404", err, err)
				}
			case "400":
				e, ok := err.(*conn.BadRequestError)
				if !ok {
					t.Fatalf("err = %T %v, want 400", err, err)
				}
				// The remedy names the field a caller can send, and the token's validity and
				// scopes — what a 401/403 needs fixed.
				if errors.Is(tc.err, domain.ErrConnectionNotUsable) &&
					(!strings.Contains(e.Message, "private_app_token") || strings.Contains(e.Message, "privateAppToken") || !strings.Contains(e.Message, "scopes")) {
					t.Errorf("400 remedy = %q", e.Message)
				}
			case "500":
				if _, ok := err.(*conn.InternalServerError); !ok {
					t.Fatalf("err = %T %v, want 500", err, err)
				}
			case "503":
				e, ok := err.(*conn.ConnServiceUnavailableError)
				if !ok {
					t.Fatalf("err = %T %v, want 503", err, err)
				}
				// No upstream text reaches the client: the message is the fixed label.
				if !strings.Contains(e.Message, "account monitor") || strings.Contains(e.Message, "403") || strings.Contains(e.Message, "statistics") {
					t.Errorf("503 message = %q", e.Message)
				}
			}
		})
	}
}

// A dispatcher that answers (nil, nil) — or a nil Emails slice — has broken the contract; the
// read fails rather than reporting an authoritative empty answer.
func TestMonitorHubspotAccount_NilResultIsAContractViolation(t *testing.T) {
	for name, read := range map[string]*model.HubSpotEmailMonitorRead{"nil read": nil, "nil emails": {}} {
		t.Run(name, func(t *testing.T) {
			svc := hubspotMonitorService(&fakeCampaignRepo{recent: recordedCampaigns(1)}, &mockEmailMonitorDispatcher{read: read})
			_, err := svc.MonitorHubspotAccount(context.Background(), &conn.MonitorHubspotAccountPayload{ProjectID: "p", Days: 30})
			if _, ok := err.(*conn.ConnServiceUnavailableError); !ok {
				t.Fatalf("err = %T %v, want 503", err, err)
			}
		})
	}
}

// The reserved system scope is refused before anything else (no orchestrator is wired here, so a
// 503 would mean the backend was resolved first); days is validated before the backend too.
func TestMonitorHubspotAccount_GuardsRunFirst(t *testing.T) {
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	_, err := svc.MonitorHubspotAccount(context.Background(), &conn.MonitorHubspotAccountPayload{ProjectID: model.SystemProjectID, Days: 30})
	if _, ok := err.(*conn.NotFoundError); !ok {
		t.Fatalf("system scope err = %T %v, want 404", err, err)
	}
	_, err = svc.MonitorHubspotAccount(context.Background(), &conn.MonitorHubspotAccountPayload{ProjectID: "p", Days: 91})
	if _, ok := err.(*conn.BadRequestError); !ok {
		t.Fatalf("days err = %T %v, want 400", err, err)
	}
}

// A platform whose dispatcher lacks the capability is "not supported" (400), and the scope
// lookup failing is a 503 — never an unscoped read.
func TestReadHubSpotEmailMonitor_CapabilityAndScopeFailures(t *testing.T) {
	o := &Orchestrator{campaigns: &fakeCampaignRepo{}, dispatchers: map[model.Provider]PlatformDispatcher{
		model.ProviderHubSpot: &mockAccountMetricsReaderDispatcher{},
	}}
	if _, err := o.ReadHubSpotEmailMonitor(context.Background(), "p", model.ProviderHubSpot, 30); !errors.Is(err, ErrAccountMetricsUnsupported) {
		t.Errorf("err = %v, want ErrAccountMetricsUnsupported", err)
	}
	if _, err := o.ReadHubSpotEmailMonitor(context.Background(), "p", model.ProviderMetaAds, 30); !errors.Is(err, ErrAccountMetricsUnsupported) {
		t.Errorf("unregistered platform err = %v, want ErrAccountMetricsUnsupported", err)
	}
	d := &mockEmailMonitorDispatcher{}
	o = &Orchestrator{campaigns: &fakeCampaignRepo{recentErr: errors.New("db down")}, dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderHubSpot: d}}
	if _, err := o.ReadHubSpotEmailMonitor(context.Background(), "p", model.ProviderHubSpot, 30); err == nil {
		t.Error("a failed scope lookup was not an error")
	}
	if d.callCount() != 0 {
		t.Error("the dispatcher was called without a scope")
	}
}

// A project whose only recorded campaign was deleted locally still has an email in HubSpot that
// may be sending: the read reaches the dispatcher rather than answering empty.
func TestReadHubSpotEmailMonitor_DeletedOnlyScopeIsStillRead(t *testing.T) {
	d := &mockEmailMonitorDispatcher{read: &model.HubSpotEmailMonitorRead{Emails: []model.HubSpotMonitorEmail{}, SpanStart: monStart, SpanEnd: monEnd, AsOf: monAsOf}}
	o := &Orchestrator{campaigns: &fakeCampaignRepo{recent: []*model.Campaign{{ID: "c1", PlatformCampaignID: "1", Status: model.CampaignStatusDeleted}}},
		dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderHubSpot: d}}
	if _, err := o.ReadHubSpotEmailMonitor(context.Background(), "p", model.ProviderHubSpot, 30); err != nil {
		t.Fatalf("ReadHubSpotEmailMonitor: %v", err)
	}
	if d.callCount() != 1 {
		t.Error("a deleted-only scope was answered empty without asking HubSpot")
	}
}
