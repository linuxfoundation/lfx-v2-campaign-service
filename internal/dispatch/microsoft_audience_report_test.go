// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// The audience read reuses the keyword read's fake Microsoft (newMSKeywordServer: a pinned
// clock, every captured body handed over under its mutex, every Reporting call counted).

// Gate off: the keyword read's 400 sentinel from every method, and nothing reaches Microsoft.
func TestMicrosoftAudience_DisabledByDefault(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftMetricsEnabled, "")
	m, opts := newMSKeywordServer(t)
	d := msKeywordDispatcher(opts)
	scope := []model.ProjectCampaignScope{msScope("111", "1234567")}
	for name, call := range map[string]func() error{
		"enabled": func() error { return d.AudienceReportEnabled(model.MetricsWindowLast30Days) },
		"account": func() error {
			_, err := d.AudienceReportAccount(context.Background(), "cncf", model.ProviderMicrosoftAds, model.MetricsWindowLast30Days, scope)
			return err
		},
		"submit": func() error {
			_, err := d.SubmitAudienceReport(context.Background(), "cncf", model.ProviderMicrosoftAds, "1234567", model.MetricsWindowLast30Days, scope)
			return err
		},
		"check": func() error {
			_, err := d.CheckAudienceReport(context.Background(), "cncf", model.ProviderMicrosoftAds, "1234567", "kr-1")
			return err
		},
	} {
		if err := call(); !errors.Is(err, domain.ErrKeywordInsightsUnsupported) {
			t.Errorf("%s: err = %v, want ErrKeywordInsightsUnsupported", name, err)
		}
	}
	if n := m.calls.Load(); n != 0 {
		t.Errorf("%d upstream calls with the gate off, want none", n)
	}
}

func TestMicrosoftAudience_EnabledChecksGateAndWindow(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftMetricsEnabled, "true")
	_, opts := newMSKeywordServer(t)
	d := msKeywordDispatcher(opts)
	if err := d.AudienceReportEnabled(model.MetricsWindowThisMonth); err != nil {
		t.Errorf("this_month with the gate on: err = %v", err)
	}
	if err := d.AudienceReportEnabled(model.MetricsWindowLast14Days); !errors.Is(err, domain.ErrMetricsWindowUnsupported) {
		t.Errorf("last_14_days: err = %v, want ErrMetricsWindowUnsupported", err)
	}
}

// Every refusal stops before any upstream call, with the AUDIENCE read's scope sentinels.
func TestMicrosoftAudience_RefusalsMakeNoUpstreamCall(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftMetricsEnabled, "true")
	tooMany := make([]model.ProjectCampaignScope, microsoft.MaxKeywordReportCampaigns+1)
	for i := range tooMany {
		tooMany[i] = msScope(fmt.Sprint(i+1), "")
	}
	own := []model.ProjectCampaignScope{msScope("111", "1234567")}
	cases := []struct {
		name    string
		window  model.MetricsWindow
		account string
		scope   []model.ProjectCampaignScope
		want    error
	}{
		{"empty scope", model.MetricsWindowLast30Days, "1234567", nil, microsoft.ErrAudienceReportScope},
		{"scope past the report ceiling", model.MetricsWindowLast30Days, "1234567", tooMany, domain.ErrAudienceScopeTooLarge},
		{"one campaign from another account", model.MetricsWindowLast30Days, "1234567",
			[]model.ProjectCampaignScope{msScope("111", "1234567"), msScope("222", "7654321")}, domain.ErrCampaignAccountMismatch},
		{"unsupported window", model.MetricsWindowYesterday, "1234567", own, domain.ErrMetricsWindowUnsupported},
		{"malformed stored campaign id", model.MetricsWindowLast30Days, "1234567",
			[]model.ProjectCampaignScope{msScope("111", "1234567"), msScope("0222", "")}, domain.ErrAudienceScopeInvalid},
		{"account the connection is not bound to", model.MetricsWindowLast30Days, "7654321", own, domain.ErrAccountNotManagedByConnection},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, opts := newMSKeywordServer(t)
			d := msKeywordDispatcher(opts)
			_, serr := d.SubmitAudienceReport(context.Background(), "cncf", model.ProviderMicrosoftAds, tc.account, tc.window, tc.scope)
			if !errors.Is(serr, tc.want) {
				t.Errorf("submit: err = %v, want %v", serr, tc.want)
			}
			if errors.Is(serr, domain.ErrKeywordReportScopeInvalid) || errors.Is(serr, domain.ErrKeywordReportScopeTooLarge) {
				t.Errorf("submit: the audience read must not answer with the keyword read's sentinel: %v", serr)
			}
			if tc.account == "1234567" {
				_, aerr := d.AudienceReportAccount(context.Background(), "cncf", model.ProviderMicrosoftAds, tc.window, tc.scope)
				if !errors.Is(aerr, tc.want) {
					t.Errorf("account: err = %v, want %v", aerr, tc.want)
				}
			} else if _, cerr := d.CheckAudienceReport(context.Background(), "cncf", model.ProviderMicrosoftAds, tc.account, "kr-1"); !errors.Is(cerr, tc.want) {
				t.Errorf("check: err = %v, want %v", cerr, tc.want)
			}
			if n := m.calls.Load(); n != 0 {
				t.Errorf("%d upstream calls, want none for a refused read", n)
			}
		})
	}
}

func TestMicrosoftAudience_AccountAndSubmitScope(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftMetricsEnabled, "true")
	m, opts := newMSKeywordServer(t)
	d := msKeywordDispatcher(opts)
	scope := []model.ProjectCampaignScope{msScope("111", "1234567"), msScope("111", ""), msScope("222", "")}
	acct, err := d.AudienceReportAccount(context.Background(), "cncf", model.ProviderMicrosoftAds, model.MetricsWindowLast7Days, scope)
	if err != nil || acct != "1234567" {
		t.Fatalf("AudienceReportAccount = %q, %v", acct, err)
	}
	if n := m.calls.Load(); n != 0 {
		t.Fatalf("AudienceReportAccount made %d upstream calls, want none", n)
	}
	sub, err := d.SubmitAudienceReport(context.Background(), "cncf", model.ProviderMicrosoftAds, acct, model.MetricsWindowLast7Days, scope)
	if err != nil {
		t.Fatalf("SubmitAudienceReport: %v", err)
	}
	if sub.ReportID != "kr-1" || strings.Join(sub.CampaignIDs, ",") != "111,222" {
		t.Errorf("submission = %+v (scope must be de-duplicated)", sub)
	}
	if got := sub.WindowStart.Format("2006-01-02") + ".." + sub.WindowEnd.Format("2006-01-02"); got != "2026-09-29..2026-10-05" {
		t.Errorf("window = %s", got)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !bytes.Contains(m.submitRaw, []byte(`"Type":"AgeGenderAudienceReportRequest"`)) ||
		!bytes.Contains(m.submitRaw, []byte(`"Campaigns":[{"AccountId":"1234567","CampaignId":"111"},{"AccountId":"1234567","CampaignId":"222"}]`)) ||
		bytes.Contains(m.submitRaw, []byte(`AccountIds`)) {
		t.Errorf("submit must be an age/gender report scoped to exactly the project's campaigns: %s", m.submitRaw)
	}
}

func TestMicrosoftAudience_RefusesSystemFallback(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftMetricsEnabled, "true")
	d := NewMicrosoftDispatcher(&scopedConnReader{
		rows: map[string]*model.Connection{model.SystemProjectID: activeMicrosoftConn(goodMicrosoftCreds)},
	}, identityEncryptor{})
	_, err := d.AudienceReportAccount(context.Background(), "cncf", model.ProviderMicrosoftAds, model.MetricsWindowLast30Days, []model.ProjectCampaignScope{msScope("111", "")})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want domain.ErrNotFound", err)
	}
}

func TestMicrosoftAudience_CheckStates(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftMetricsEnabled, "true")
	for reply, want := range map[string]model.AccountReportStatus{
		`{"ReportRequestStatus":{"Status":"Pending"}}`: model.AccountReportPending,
		`{"ReportRequestStatus":{"Status":"Error"}}`:   model.AccountReportFailed,
	} {
		m, opts := newMSKeywordServer(t)
		m.pollReply = reply
		got, err := msKeywordDispatcher(opts).CheckAudienceReport(context.Background(), "cncf", model.ProviderMicrosoftAds, "1234567", "kr-1")
		if err != nil || got.Status != want || got.Rows != nil {
			t.Errorf("%s: got %+v, %v", reply, got, err)
		}
	}

	m, opts := newMSKeywordServer(t)
	m.pollReply = `{"ReportRequestStatus":{"Status":"Success","ReportDownloadUrl":"%URL%"}}`
	m.zip = keywordZip(t, `"Potential Incomplete Data: true"
"CampaignId","AgeGroup","Gender","Impressions","Clicks","Spend"
"111","25-34","Female","100","5","2.50"
"111","65+","Male","10","0","0"
`)
	got, err := msKeywordDispatcher(opts).CheckAudienceReport(context.Background(), "cncf", model.ProviderMicrosoftAds, "1234567", "kr-1")
	if err != nil {
		t.Fatalf("CheckAudienceReport: %v", err)
	}
	if got.Status != model.AccountReportReady || !got.Partial || len(got.Rows) != 2 {
		t.Fatalf("got %+v", got)
	}
	if got.Rows[0] != (model.AudienceReportRow{CampaignID: "111", AgeGroup: "25-34", Gender: "Female", Impressions: 100, Clicks: 5, Spend: 2.5}) {
		t.Errorf("row 1 = %+v", got.Rows[0])
	}

	// A malformed report fails the check (no partial rows).
	m, opts = newMSKeywordServer(t)
	m.pollReply = `{"ReportRequestStatus":{"Status":"Success","ReportDownloadUrl":"%URL%"}}`
	m.zip = keywordZip(t, `"CampaignId","AgeGroup","Gender","Impressions","Clicks","Spend"
"111","","Female","100","5","2.50"
`)
	if _, err := msKeywordDispatcher(opts).CheckAudienceReport(context.Background(), "cncf", model.ProviderMicrosoftAds, "1234567", "kr-1"); err == nil {
		t.Errorf("a row with a blank age group must fail the check")
	}
}

func TestMicrosoftAudience_ScopeRejectionIsPermanent(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftMetricsEnabled, "true")
	m, opts := newMSKeywordServer(t)
	m.submitReject = `{"Code":2027,"Message":"scope rejected"}`
	_, err := msKeywordDispatcher(opts).SubmitAudienceReport(context.Background(), "cncf", model.ProviderMicrosoftAds, "1234567",
		model.MetricsWindowLast7Days, []model.ProjectCampaignScope{msScope("111", "")})
	if !errors.Is(err, domain.ErrServiceDefect) || !errors.Is(err, microsoft.ErrAudienceReportScopeRejected) {
		t.Errorf("err = %v, want ErrServiceDefect wrapping ErrAudienceReportScopeRejected", err)
	}
	m, opts = newMSKeywordServer(t)
	m.submitReject = `{"Code":105,"Message":"other"}`
	_, err = msKeywordDispatcher(opts).SubmitAudienceReport(context.Background(), "cncf", model.ProviderMicrosoftAds, "1234567",
		model.MetricsWindowLast7Days, []model.ProjectCampaignScope{msScope("111", "")})
	if err == nil || errors.Is(err, domain.ErrServiceDefect) {
		t.Errorf("err = %v, want a non-defect error", err)
	}
}
