// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// mockAccountMonitorReaderDispatcher implements AccountMonitorReader: a live-read platform whose
// read also reports the account's currency.
type mockAccountMonitorReaderDispatcher struct {
	rows     []model.AccountCampaignMetrics
	currency string
}

func (m *mockAccountMonitorReaderDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, nil
}

func (m *mockAccountMonitorReaderDispatcher) ReadAccountMonitor(context.Context, string, model.Provider, string, int) (*model.AccountMonitorRead, error) {
	return &model.AccountMonitorRead{Rows: m.rows, Currency: m.currency}, nil
}

// currencyReportReader is fakeReportReader plus AccountCurrencyReader.
type currencyReportReader struct {
	*fakeReportReader
	currency string
	err      error
	calls    int
	// block holds the read until its context ends — a hung or throttled account read.
	block bool
}

func (c *currencyReportReader) ReadAccountCurrency(ctx context.Context, _ string, _ model.Provider, _ string) (string, error) {
	c.calls++
	if c.block {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return c.currency, c.err
}

func currencyOf(got *conn.AccountMonitor) string {
	if got.Currency == nil {
		return "<absent>"
	}
	return *got.Currency
}

// A live-read platform's currency reaches the response when well-formed and is absent otherwise;
// a dispatcher with only AccountMetricsReader (Google Ads) never sets it.
func TestMonitorAccount_Currency(t *testing.T) {
	rows := []model.AccountCampaignMetrics{{PlatformCampaignID: "1", Name: "c", Status: "ACTIVE", BudgetDay: 50, Spend: 50, Impressions: 100, Clicks: 5}}
	for _, tc := range []struct {
		name     string
		currency string
		want     string
	}{
		{"present", "EUR", "EUR"},
		{"absent", "", "<absent>"},
		{"malformed dropped", "eur", "<absent>"},
		{"malformed (symbol) dropped", "€", "<absent>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
			svc.SetOrchestrator(&Orchestrator{dispatchers: map[model.Provider]PlatformDispatcher{
				model.ProviderMetaAds: &mockAccountMonitorReaderDispatcher{rows: rows, currency: tc.currency},
			}})
			got, err := svc.MonitorMetaAdsAccount(context.Background(),
				&conn.MonitorMetaAdsAccountPayload{ProjectID: "p", AccountID: "act_1", Days: 30})
			if err != nil {
				t.Fatalf("MonitorMetaAdsAccount: %v", err)
			}
			if c := currencyOf(got); c != tc.want {
				t.Errorf("currency = %s, want %s", c, tc.want)
			}
			if len(got.Campaigns) != 1 {
				t.Errorf("campaigns = %d, want 1", len(got.Campaigns))
			}
		})
	}

	t.Run("rows-only reader leaves it absent", func(t *testing.T) {
		svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
		svc.SetOrchestrator(&Orchestrator{dispatchers: map[model.Provider]PlatformDispatcher{
			model.ProviderGoogleAds: &mockAccountMetricsReaderDispatcher{rows: rows},
		}})
		got, err := svc.MonitorGoogleAdsAccount(context.Background(),
			&conn.MonitorGoogleAdsAccountPayload{ProjectID: "p", AccountID: "a", Days: 30})
		if err != nil {
			t.Fatalf("MonitorGoogleAdsAccount: %v", err)
		}
		if got.Currency != nil {
			t.Errorf("currency = %q, want absent for a reader that reports none", *got.Currency)
		}
	})
}

// The report-backed monitor reads the currency best-effort: present when the reader returns a
// well-formed code; absent when it returns none, a malformed one, or fails — and a failure never
// fails the monitor.
func TestMonitorReportedAccount_Currency(t *testing.T) {
	for _, tc := range []struct {
		name     string
		currency string
		err      error
		want     string
	}{
		{"present", "GBP", nil, "GBP"},
		{"absent", "", nil, "<absent>"},
		{"malformed dropped", "GBPX", nil, "<absent>"},
		{"the read fails", "GBP", errors.New("boom"), "<absent>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &currencyReportReader{fakeReportReader: &fakeReportReader{campaigns: twoCampaigns(), submitID: "r1"}, currency: tc.currency, err: tc.err}
			o := NewOrchestrator(&fakeCampaignRepo{}, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{model.ProviderMicrosoftAds: reader})
			o.SetAccountReportStore(&fakeReportStore{})
			svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
			svc.SetOrchestrator(o)
			got, err := svc.MonitorMicrosoftAdsAccount(context.Background(),
				&conn.MonitorMicrosoftAdsAccountPayload{ProjectID: "p", AccountID: "123", Days: 7})
			if err != nil {
				t.Fatalf("MonitorMicrosoftAdsAccount: %v — a currency read must never fail the monitor", err)
			}
			if c := currencyOf(got); c != tc.want {
				t.Errorf("currency = %s, want %s", c, tc.want)
			}
			if len(got.Campaigns) != 2 {
				t.Errorf("campaigns = %d, want both", len(got.Campaigns))
			}
			if reader.calls != 1 {
				t.Errorf("currency reads = %d, want 1", reader.calls)
			}
		})
	}

	t.Run("a reader without the capability leaves it absent", func(t *testing.T) {
		reader := &fakeReportReader{campaigns: twoCampaigns(), submitID: "r1"}
		got, err := microsoftMonitorService(reader, &fakeReportStore{}).MonitorMicrosoftAdsAccount(context.Background(),
			&conn.MonitorMicrosoftAdsAccountPayload{ProjectID: "p", AccountID: "123", Days: 7})
		if err != nil {
			t.Fatalf("MonitorMicrosoftAdsAccount: %v", err)
		}
		if got.Currency != nil {
			t.Errorf("currency = %q, want absent", *got.Currency)
		}
	})
}

// The service normalizes whatever a dispatcher hands it, so a dispatcher that forgot cannot
// publish a malformed code.
func TestReadAccountCampaignMetrics_NormalizesCurrency(t *testing.T) {
	o := &Orchestrator{dispatchers: map[model.Provider]PlatformDispatcher{
		model.ProviderRedditAds: &mockAccountMonitorReaderDispatcher{rows: []model.AccountCampaignMetrics{}, currency: " USD"},
	}}
	read, err := o.ReadAccountCampaignMetrics(context.Background(), "p", model.ProviderRedditAds, "t2_a", 30)
	if err != nil {
		t.Fatalf("ReadAccountCampaignMetrics: %v", err)
	}
	if read.Currency != "" {
		t.Errorf("currency = %q, want it dropped", read.Currency)
	}
}

// buildAccountMonitor is the last stop before the wire, so it refuses a malformed code itself
// rather than trusting every caller to have normalized it.
func TestBuildAccountMonitor_Currency(t *testing.T) {
	for in, want := range map[string]string{"EUR": "EUR", "": "<absent>", "eur": "<absent>", "EURO": "<absent>"} {
		if got := currencyOf(buildAccountMonitor("a", 30, nil, nil, in)); got != want {
			t.Errorf("buildAccountMonitor(currency %q) = %s, want %s", in, got, want)
		}
	}
}

// A currency read that hangs costs the report-backed monitor at most accountCurrencyReadTimeout:
// the response is served, with its rows and without a currency, well inside the call budget.
func TestMonitorReportedAccount_SlowCurrencyReadIsBounded(t *testing.T) {
	reader := &currencyReportReader{fakeReportReader: &fakeReportReader{campaigns: twoCampaigns(), submitID: "r1"}, block: true}
	o := NewOrchestrator(&fakeCampaignRepo{}, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{model.ProviderMicrosoftAds: reader})
	o.SetAccountReportStore(&fakeReportStore{})
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(o)
	start := time.Now()
	got, err := svc.MonitorMicrosoftAdsAccount(context.Background(),
		&conn.MonitorMicrosoftAdsAccountPayload{ProjectID: "p", AccountID: "123", Days: 7})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("MonitorMicrosoftAdsAccount: %v", err)
	}
	if got.Currency != nil || len(got.Campaigns) != 2 {
		t.Errorf("currency=%v campaigns=%d, want absent and both rows", got.Currency, len(got.Campaigns))
	}
	if elapsed > accountCurrencyReadTimeout+2*time.Second || elapsed >= accountsCallTimeout {
		t.Errorf("took %v, want about accountCurrencyReadTimeout (%v), far inside the %v call budget", elapsed, accountCurrencyReadTimeout, accountsCallTimeout)
	}
}
