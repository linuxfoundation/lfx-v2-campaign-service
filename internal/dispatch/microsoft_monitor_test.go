// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// microsoftMonitorServers stubs the token and Campaign Management hosts for the monitor's
// campaign list, counting every API call so a refusal test can prove nothing reached upstream.
func microsoftMonitorServers(t *testing.T, campaignsBody string) ([]microsoft.Option, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"at-123","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if !strings.HasSuffix(r.URL.Path, "/Campaigns/QueryByAccountId") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if got := r.Header.Get("CustomerAccountId"); got != "1234567" {
			t.Errorf("CustomerAccountId = %q, want the connection's 1234567", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, campaignsBody)
	}))
	t.Cleanup(apiSrv.Close)
	return []microsoft.Option{microsoft.WithTokenURL(tokenSrv.URL), microsoft.WithBaseURL(apiSrv.URL)}, &calls
}

// The monitor rides MICROSOFT_METRICS_ENABLED, like ReadMetrics: off, it is the same 400 as a
// platform with no monitor, and no credential is resolved.
func TestMicrosoftMonitor_DisabledByDefault(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftMetricsEnabled, "")
	d := NewMicrosoftDispatcher(fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{})
	_, err := d.ListAccountCampaigns(context.Background(), "cncf", model.ProviderMicrosoftAds, "1234567")
	if !errors.Is(err, domain.ErrAccountMetricsUnsupported) {
		t.Fatalf("err = %v, want ErrAccountMetricsUnsupported", err)
	}
}

// A project with no Microsoft connection of its own must not be served from the LF system row:
// scopedConnReader holds a valid connection ONLY under the system project, so success here
// would mean the fallback was consulted.
func TestMicrosoftMonitor_RefusesSystemFallback(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftMetricsEnabled, "true")
	d := NewMicrosoftDispatcher(&scopedConnReader{
		rows: map[string]*model.Connection{model.SystemProjectID: activeMicrosoftConn(goodMicrosoftCreds)},
	}, identityEncryptor{})
	for name, call := range map[string]func() error{
		"list": func() error {
			_, err := d.ListAccountCampaigns(context.Background(), "cncf", model.ProviderMicrosoftAds, "1234567")
			return err
		},
		"submit": func() error {
			_, err := d.SubmitAccountReport(context.Background(), "cncf", model.ProviderMicrosoftAds, "1234567", 7)
			return err
		},
		"check": func() error {
			_, err := d.CheckAccountReport(context.Background(), "cncf", model.ProviderMicrosoftAds, "1234567", "r1")
			return err
		},
	} {
		if err := call(); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("%s: err = %v, want domain.ErrNotFound", name, err)
		}
	}
}

// Every refusal arm stops before any upstream call, on ALL THREE methods — submit spends a
// Microsoft report build and check downloads account data, so a refactor that bypassed
// resolveMicrosoftMonitorClient on either must fail here, not only on the list. The permitted
// arm (list only) reaches the stored account.
func TestMicrosoftMonitor_AccountScope(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftMetricsEnabled, "true")
	cases := []struct {
		name      string
		requested string
		wantErr   error
	}{
		{"matching account is read", "1234567", nil},
		{"another account is refused", "7654321", domain.ErrAccountNotManagedByConnection},
		{"padded id is malformed, not trimmed", " 1234567", domain.ErrAccountIDMalformed},
		{"leading zero is malformed", "01234567", domain.ErrAccountIDMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, calls := microsoftMonitorServers(t, `{"Campaigns":[]}`)
			d := NewMicrosoftDispatcher(fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{}, opts...)
			if tc.wantErr != nil {
				for name, call := range map[string]func() error{
					"list": func() error {
						_, err := d.ListAccountCampaigns(context.Background(), "cncf", model.ProviderMicrosoftAds, tc.requested)
						return err
					},
					"submit": func() error {
						_, err := d.SubmitAccountReport(context.Background(), "cncf", model.ProviderMicrosoftAds, tc.requested, 7)
						return err
					},
					"check": func() error {
						_, err := d.CheckAccountReport(context.Background(), "cncf", model.ProviderMicrosoftAds, tc.requested, "r1")
						return err
					},
				} {
					if err := call(); !errors.Is(err, tc.wantErr) {
						t.Errorf("%s: err = %v, want %v", name, err, tc.wantErr)
					}
				}
				if n := calls.Load(); n != 0 {
					t.Errorf("%d upstream calls, want none for a refused request", n)
				}
				return
			}
			rows, err := d.ListAccountCampaigns(context.Background(), "cncf", model.ProviderMicrosoftAds, tc.requested)
			if err != nil {
				t.Fatalf("ListAccountCampaigns: %v", err)
			}
			if rows == nil || calls.Load() != 1 {
				t.Errorf("rows=%v calls=%d, want a non-nil empty list from one call", rows, calls.Load())
			}
		})
	}
}

// The list maps each campaign's budget facts onto the flags the rule engine reads: a shared
// budget is PacingUnknown (never paced, never a placeholder), an unparseable budget is
// FetchFailed, and Search is recognised from Microsoft's flags-style CampaignType.
func TestMicrosoftMonitor_ListMapsBudgetFlags(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftMetricsEnabled, "true")
	opts, _ := microsoftMonitorServers(t, `{"Campaigns":[
		{"Id":11,"Name":"own","Status":"Active","CampaignType":"Search","BudgetType":"DailyBudgetStandard","DailyBudget":25.5,"BudgetId":null},
		{"Id":12,"Name":"shared","Status":"Active","CampaignType":"Audience","BudgetType":"DailyBudgetStandard","DailyBudget":null,"BudgetId":4455},
		{"Id":13,"Name":"broken","Status":"Paused","CampaignType":"Search","DailyBudget":null,"BudgetId":null}
	]}`)
	d := NewMicrosoftDispatcher(fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{}, opts...)
	rows, err := d.ListAccountCampaigns(context.Background(), "cncf", model.ProviderMicrosoftAds, "1234567")
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	byID := map[string]model.AccountCampaignMetrics{}
	for _, r := range rows {
		byID[r.PlatformCampaignID] = r
	}
	if own := byID["11"]; own.BudgetDay != 25.5 || own.PacingUnknown || own.FetchFailed || !own.IsSearchChannel {
		t.Errorf("own-budget campaign = %+v", own)
	}
	if shared := byID["12"]; !shared.PacingUnknown || shared.FetchFailed || shared.IsSearchChannel {
		t.Errorf("shared-budget campaign = %+v, want PacingUnknown, not FetchFailed, not search", shared)
	}
	if broken := byID["13"]; !broken.FetchFailed {
		t.Errorf("unparseable-budget campaign = %+v, want FetchFailed", broken)
	}
}

func TestMicrosoftIsSearchCampaign(t *testing.T) {
	for in, want := range map[string]bool{"Search": true, "search": true, "Search DynamicSearchAds": true, "Audience": false, "": false, "PerformanceMax": false} {
		if got := microsoftIsSearchCampaign(in); got != want {
			t.Errorf("microsoftIsSearchCampaign(%q) = %v, want %v", in, got, want)
		}
	}
}

// days outside 7..90 is refused before any credential is resolved — the defense-in-depth every
// monitor dispatcher re-runs for a caller that bypasses Goa.
func TestMicrosoftMonitor_SubmitValidatesDays(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftMetricsEnabled, "true")
	d := NewMicrosoftDispatcher(fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{})
	if _, err := d.SubmitAccountReport(context.Background(), "cncf", model.ProviderMicrosoftAds, "1234567", 6); !errors.Is(err, domain.ErrMonitorDaysInvalid) {
		t.Errorf("err = %v, want ErrMonitorDaysInvalid", err)
	}
}
