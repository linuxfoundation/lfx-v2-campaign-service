// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
)

// LinkedIn settings readback (LFXV2-2665). The bid lever's tests are in linkedin_bid_test.go and
// reuse these fixtures; adoption is covered by the shared adopter table in adopt_test.go.

const (
	liCampaignPath = "/adAccounts/123456789/adCampaigns/777"
	liAccountPath  = "/adAccounts/123456789"
)

// liCampaignJSON is a campaign this service created: CPC, manual (NONE), a daily USD budget,
// and the flight the create path sends (start 00:00 UTC, end 23:59:59.999 UTC).
func liCampaignJSON(mutate ...func(string) string) string {
	s := `{"id":777,"account":"urn:li:sponsoredAccount:123456789","campaignGroup":"urn:li:sponsoredCampaignGroup:42",` +
		`"name":"KubeCon EU","status":"ACTIVE","objectiveType":"WEBSITE_CONVERSION","optimizationTargetType":"NONE",` +
		`"costType":"CPC","unitCost":{"amount":"5.25","currencyCode":"USD"},` +
		`"dailyBudget":{"amount":"100.00","currencyCode":"USD"},"runSchedule":{"start":1785542400000,"end":1786924799999}}`
	for _, m := range mutate {
		s = m(s)
	}
	return s
}

func replace(old, repl string) func(string) string {
	return func(s string) string { return strings.Replace(s, old, repl, 1) }
}

func liLeverDispatcher(t *testing.T, routes map[string]settingsRoute) (*LinkedInDispatcher, *settingsAPI) {
	t.Helper()
	api := newSettingsAPI(t, routes)
	d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{},
		linkedin.WithBaseURL(api.srv.URL))
	return d, api
}

func liRow() *model.Campaign {
	budget := 100.0
	daily := model.BudgetDaily
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC)
	return &model.Campaign{
		ID: "camp-1", Platform: model.ProviderLinkedInAds, PlatformCampaignID: "777", CampaignName: "KubeCon EU",
		BudgetAmount: &budget, BudgetType: &daily, StartDate: &start, EndDate: &end,
		CreatedAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		Result:    []byte(`{"accountId":"123456789","campaignId":"777","campaignGroupId":"42"}`),
	}
}

// ─── settings readback ───

func TestLinkedIn_ReadSettings_ComparesTheCampaign(t *testing.T) {
	d, api := liLeverDispatcher(t, map[string]settingsRoute{liCampaignPath: {status: 200, body: liCampaignJSON()}})
	rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderLinkedInAds, liRow())
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	if rb.PlatformCampaignID != "777" || rb.CampaignID != "camp-1" {
		t.Errorf("ids = %+v", rb)
	}
	assertSettingsFields(t, rb, map[string]fieldWant{
		settingsFieldBudgetAmount:    {"100.00", "100.00", model.SettingsMatch},
		settingsFieldBudgetType:      {"daily", "daily", model.SettingsMatch},
		settingsFieldName:            {"KubeCon EU", "KubeCon EU", model.SettingsMatch},
		settingsFieldStatus:          {"", "ACTIVE", model.SettingsUnknown},
		settingsFieldStartDate:       {"2026-08-01", "2026-08-01", model.SettingsMatch},
		settingsFieldEndDate:         {"2026-08-16", "2026-08-16", model.SettingsMatch},
		settingsFieldBiddingStrategy: {"", "NONE", model.SettingsUnknown},
		settingsFieldCostType:        {"", "CPC", model.SettingsUnknown},
		settingsFieldUnitCost:        {"", "5.25", model.SettingsUnknown},
		settingsFieldObjectiveType:   {"", "WEBSITE_CONVERSION", model.SettingsUnknown},
	})
	assertSettingsReadOnly(t, api.requests(), "")
}

func TestLinkedIn_ReadSettings_BudgetIsComparedOnlyWhenItsShapeIsUnambiguous(t *testing.T) {
	cases := map[string]func(string) string{
		"both daily and total": replace(`"dailyBudget":{"amount":"100.00","currencyCode":"USD"}`,
			`"dailyBudget":{"amount":"100.00","currencyCode":"USD"},"totalBudget":{"amount":"900.00","currencyCode":"USD"}`),
		"neither (shared group budget)": replace(`"dailyBudget":{"amount":"100.00","currencyCode":"USD"},`, ``),
		"not USD":                       replace(`"amount":"100.00","currencyCode":"USD"`, `"amount":"100.00","currencyCode":"EUR"`),
		"sub-micro amount":              replace(`"100.00"`, `"100.0000001"`),
	}
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			d, _ := liLeverDispatcher(t, map[string]settingsRoute{liCampaignPath: {status: 200, body: liCampaignJSON(m)}})
			rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderLinkedInAds, liRow())
			if err != nil {
				t.Fatalf("ReadSettings: %v", err)
			}
			for _, f := range []string{settingsFieldBudgetAmount, settingsFieldBudgetType} {
				if got := settingsField(t, rb, f); got.Upstream != nil || got.Comparison != model.SettingsUnknown {
					t.Errorf("%s = %+v, want an absent upstream side and `unknown`", f, got)
				}
			}
		})
	}
}

func TestLinkedIn_ReadSettings_SubCentAmountCannotRoundIntoAMatch(t *testing.T) {
	d, _ := liLeverDispatcher(t, map[string]settingsRoute{liCampaignPath: {status: 200, body: liCampaignJSON(replace(`"100.00"`, `"100.004"`))}})
	rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderLinkedInAds, liRow())
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	if f := settingsField(t, rb, settingsFieldBudgetAmount); f.Comparison != model.SettingsDiverged || derefOr(f.Upstream) != "100.004" {
		t.Errorf("budget = %+v, want 100.004 diverged from 100.00", f)
	}
}

func TestLinkedIn_ReadSettings_TrustBoundary(t *testing.T) {
	t.Run("unknown provenance fails closed before any request", func(t *testing.T) {
		d, api := liLeverDispatcher(t, nil)
		row := liRow()
		row.Result = []byte(`{}`)
		_, err := d.ReadSettings(context.Background(), "proj", model.ProviderLinkedInAds, row)
		assertProvenanceUnknown(t, err)
		if n := len(api.requests()); n != 0 {
			t.Fatalf("%d request(s) for a row of unknown provenance", n)
		}
	})
	t.Run("a row created under another account is a mismatch, before any request", func(t *testing.T) {
		d, api := liLeverDispatcher(t, nil)
		row := liRow()
		row.Result = []byte(`{"accountId":"555"}`)
		_, err := d.ReadSettings(context.Background(), "proj", model.ProviderLinkedInAds, row)
		assertMismatch(t, err)
		if n := len(api.requests()); n != 0 {
			t.Fatalf("%d request(s) for a mismatched row", n)
		}
	})
	t.Run("an upstream account URN naming another account is an identity mismatch", func(t *testing.T) {
		d, _ := liLeverDispatcher(t, map[string]settingsRoute{liCampaignPath: {status: 200,
			body: liCampaignJSON(replace("sponsoredAccount:123456789", "sponsoredAccount:999"))}})
		_, err := d.ReadSettings(context.Background(), "proj", model.ProviderLinkedInAds, liRow())
		if !errors.Is(err, domain.ErrCampaignUpstreamIdentityMismatch) {
			t.Fatalf("err = %v, want ErrCampaignUpstreamIdentityMismatch", err)
		}
	})
	t.Run("a 404 with the account confirmed is absent", func(t *testing.T) {
		d, _ := liLeverDispatcher(t, map[string]settingsRoute{
			liCampaignPath: {status: 404, body: `{}`},
			liAccountPath:  {status: 200, body: `{"id":123456789}`},
		})
		_, err := d.ReadSettings(context.Background(), "proj", model.ProviderLinkedInAds, liRow())
		if !errors.Is(err, domain.ErrPlatformCampaignAbsent) {
			t.Fatalf("err = %v, want ErrPlatformCampaignAbsent", err)
		}
	})
	t.Run("a 404 without the account confirmed is NOT absent", func(t *testing.T) {
		d, _ := liLeverDispatcher(t, map[string]settingsRoute{
			liCampaignPath: {status: 404, body: `{}`},
			liAccountPath:  {status: 404, body: `{}`},
		})
		_, err := d.ReadSettings(context.Background(), "proj", model.ProviderLinkedInAds, liRow())
		if err == nil || errors.Is(err, domain.ErrPlatformCampaignAbsent) {
			t.Fatalf("err = %v, want an unverifiable error", err)
		}
	})
	t.Run("a 401 is a connection defect, not a 503", func(t *testing.T) {
		d, _ := liLeverDispatcher(t, map[string]settingsRoute{liCampaignPath: {status: 401, body: `{"status":401}`}})
		_, err := d.ReadSettings(context.Background(), "proj", model.ProviderLinkedInAds, liRow())
		if !errors.Is(err, domain.ErrConnectionNotUsable) || !errors.Is(err, domain.ErrCredentialsExpired) {
			t.Fatalf("err = %v, want ErrConnectionNotUsable + ErrCredentialsExpired", err)
		}
	})
}
