// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service/rules"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// The live-read dispatchers hand the account's currency through with the rows; their
// ListAccountCampaignMetrics (the same read) still returns the rows alone.
func TestReadAccountMonitor_CarriesCurrency(t *testing.T) {
	t.Run("meta", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if strings.HasSuffix(r.URL.Path, "/insights") {
				_, _ = io.WriteString(w, `{"data":[{"campaign_id":"1","impressions":"10","clicks":"1","spend":"2","account_currency":"EUR"}],"paging":{}}`)
				return
			}
			_, _ = io.WriteString(w, `{"data":[{"id":"1","name":"c","status":"ACTIVE","daily_budget":"1000"}],"paging":{}}`)
		}))
		t.Cleanup(srv.Close)
		d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{}, meta.WithBaseURL(srv.URL))
		read, err := d.ReadAccountMonitor(context.Background(), "cncf", model.ProviderMetaAds, "act_777", 30)
		if err != nil {
			t.Fatalf("ReadAccountMonitor: %v", err)
		}
		if read.Currency != "EUR" || len(read.Rows) != 1 {
			t.Errorf("read = %+v, want one row in EUR", read)
		}
	})

	t.Run("reddit", func(t *testing.T) {
		var accountReads atomic.Int32
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case strings.HasSuffix(r.URL.Path, "/reports"):
				_, _ = io.WriteString(w, `{"data":{"metrics":[{"campaign_id":"t2_c1","impressions":10,"clicks":1,"spend":1000000}]}}`)
			case strings.HasSuffix(r.URL.Path, "/campaigns"):
				_, _ = io.WriteString(w, `{"data":[{"id":"t2_c1","name":"c","configured_status":"ACTIVE"}]}`)
			case strings.HasSuffix(r.URL.Path, "/ad_accounts/t2_acct"):
				accountReads.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"data":{"id":"t2_acct","currency":"CAD"}}`)
			default:
				t.Errorf("unexpected request %s", r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		t.Cleanup(api.Close)
		tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
		}))
		t.Cleanup(tok.Close)
		d := NewRedditDispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
			reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL))
		read, err := d.ReadAccountMonitor(context.Background(), "proj", model.ProviderRedditAds, "t2_acct", 30)
		if err != nil {
			t.Fatalf("ReadAccountMonitor: %v", err)
		}
		if read.Currency != "CAD" || len(read.Rows) != 1 {
			t.Errorf("read = %+v, want one row in CAD", read)
		}
		if n := accountReads.Load(); n != 1 {
			t.Errorf("ad-account reads = %d, want 1", n)
		}
		// The metrics-reader path does not pay for the currency: no ad-account read.
		if _, err := d.ListAccountCampaignMetrics(context.Background(), "proj", model.ProviderRedditAds, "t2_acct", 30); err != nil {
			t.Fatalf("ListAccountCampaignMetrics: %v", err)
		}
		if n := accountReads.Load(); n != 1 {
			t.Errorf("ad-account reads = %d after ListAccountCampaignMetrics, want still 1", n)
		}
	})
}

// LinkedIn's monitor reads spend in the account's own currency (costInLocalCurrency), so a EUR
// account paces EUR spend against its EUR budget and names EUR; a USD account's numbers are what
// they were (costInLocalCurrency is USD there); and budgets that disagree give no currency and
// unknown pacing for the rows whose budget named one.
func TestLinkedInMonitor_LocalCurrency(t *testing.T) {
	now := time.Now()
	serve := func(t *testing.T, list string) *LinkedInDispatcher {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if strings.Contains(r.URL.Path, "adAnalytics") {
				if !strings.Contains(r.URL.RawQuery, "costInLocalCurrency") || strings.Contains(r.URL.RawQuery, "costInUsd") {
					t.Errorf("analytics query %q, want costInLocalCurrency and not costInUsd", r.URL.RawQuery)
				}
				// costInUsd is present and different, so a read of the wrong field shows.
				_, _ = io.WriteString(w, `{"elements":[`+
					`{"pivotValues":["urn:li:sponsoredCampaign:1"],"impressions":5000,"clicks":40,"costInLocalCurrency":"600.00","costInUsd":"650.00"},`+
					`{"pivotValues":["urn:li:sponsoredCampaign:2"],"impressions":5000,"clicks":40,"costInLocalCurrency":"600.00","costInUsd":"650.00"}]}`)
				return
			}
			_, _ = io.WriteString(w, list)
		}))
		t.Cleanup(srv.Close)
		return NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{}, linkedin.WithBaseURL(srv.URL))
	}
	list := func(code1, code2 string) string {
		return `{"elements":[` +
			`{"id":1,"name":"a","status":"ACTIVE","dailyBudget":{"amount":"100.00","currencyCode":"` + code1 + `"}},` +
			`{"id":2,"name":"b","status":"ACTIVE","dailyBudget":{"amount":"100.00","currencyCode":"` + code2 + `"}}],"metadata":{}}`
	}
	for _, tc := range []struct {
		name, code1, code2, wantCurrency string
		wantPaced                        bool
	}{
		{"a EUR account paces EUR spend against its EUR budget", "EUR", "EUR", "EUR", true},
		{"a USD account is unchanged", "USD", "USD", "USD", true},
		{"mixed currencies: no currency, no pacing", "USD", "EUR", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			read, err := serve(t, list(tc.code1, tc.code2)).ReadAccountMonitor(context.Background(), "cncf", model.ProviderLinkedInAds, "123456789", 30)
			if err != nil {
				t.Fatalf("ReadAccountMonitor: %v", err)
			}
			if read.Currency != tc.wantCurrency {
				t.Errorf("currency = %q, want %q", read.Currency, tc.wantCurrency)
			}
			rows, _ := rules.EvaluateLinkedInMonitor(read.Rows, 30, now)
			for _, r := range rows {
				if r.Metrics.Spend != 600 || r.Metrics.BudgetDay != 100 {
					t.Errorf("row %s spend=%v budget=%v, want 600 local spend against 100/day", r.Metrics.PlatformCampaignID, r.Metrics.Spend, r.Metrics.BudgetDay)
				}
				if r.Metrics.PacingUnknown == tc.wantPaced {
					t.Errorf("row %s PacingUnknown = %v, want paced=%v", r.Metrics.PlatformCampaignID, r.Metrics.PacingUnknown, tc.wantPaced)
				}
				// 600 spent against 100/day over the 29 days LinkedIn's rules expect for a 30-day
				// window: 600/2900 ≈ 20.69%, the same figure on a EUR and a USD account because both
				// sides are in the account's own currency (costInUsd would give 650/2900 ≈ 22.41%).
				if want := 600.0 / 2900 * 100; tc.wantPaced && (r.PacingPct < want-0.01 || r.PacingPct > want+0.01) {
					t.Errorf("row %s pacing = %v%%, want %v%%", r.Metrics.PlatformCampaignID, r.PacingPct, want)
				}
			}
		})
	}
}

// One X monitor read — the campaign list, then the currency — makes exactly ONE account GET: the
// currency rides on the account read the list made for the timezone.
func TestTwitterMonitor_CurrencyReusesTheListsAccountRead(t *testing.T) {
	t.Setenv(constants.EnvTwitterMetricsEnabled, "true")
	routes := map[string]string{}
	for k, v := range twitterMonitorRoutes {
		routes[k] = v
	}
	routes["/12/accounts/acc1"] = `{"data":{"id":"acc1","timezone":"America/New_York","currency":"EUR"}}`
	opts, _, seen := twitterMonitorServer(t, routes)
	d := NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{}, opts...)
	if _, err := d.ListAccountCampaigns(context.Background(), "cncf", model.ProviderTwitterAds, "acc1"); err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	got, err := d.ReadAccountCurrency(context.Background(), "cncf", model.ProviderTwitterAds, "acc1")
	if err != nil || got != "EUR" {
		t.Fatalf("ReadAccountCurrency = %q, %v; want EUR", got, err)
	}
	n := 0
	for _, p := range seen() {
		if p == "/12/accounts/acc1" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d account GETs across the list and the currency read, want exactly 1", n)
	}
}

// X's currency comes from the account read the monitor shares with the timezone, through the same
// trust boundary as the other monitor methods: refused arms reach nothing upstream.
func TestTwitterMonitor_ReadAccountCurrency(t *testing.T) {
	t.Setenv(constants.EnvTwitterMetricsEnabled, "true")
	routes := map[string]string{"/12/accounts/acc1": `{"data":{"id":"acc1","timezone":"America/New_York","currency":"EUR"}}`}

	opts, _, _ := twitterMonitorServer(t, routes)
	d := NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{}, opts...)
	got, err := d.ReadAccountCurrency(context.Background(), "cncf", model.ProviderTwitterAds, "acc1")
	if err != nil || got != "EUR" {
		t.Fatalf("ReadAccountCurrency = %q, %v; want EUR", got, err)
	}

	for _, tc := range []struct {
		name      string
		requested string
		wantErr   error
	}{
		{"another account is refused", "acc2", domain.ErrAccountNotManagedByConnection},
		{"padded id is malformed", " acc1", domain.ErrAccountIDMalformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, calls, _ := twitterMonitorServer(t, routes)
			d := NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{}, opts...)
			if _, err := d.ReadAccountCurrency(context.Background(), "cncf", model.ProviderTwitterAds, tc.requested); !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
			if n := calls.Load(); n != 0 {
				t.Errorf("%d upstream calls, want none", n)
			}
		})
	}
}

// Microsoft's currency is one Customer Management read for the connection's own account; the
// refused arms and the disabled flag reach nothing upstream.
func TestMicrosoftMonitor_ReadAccountCurrency(t *testing.T) {
	servers := func(t *testing.T) ([]microsoft.Option, *atomic.Int32) {
		t.Helper()
		var calls atomic.Int32
		tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"access_token":"at-123","expires_in":3600,"token_type":"Bearer"}`)
		}))
		t.Cleanup(tokenSrv.Close)
		custSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if !strings.HasSuffix(r.URL.Path, "/Account/Query") {
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"Account":{"Id":1234567,"CurrencyCode":"EUR"}}`)
		}))
		t.Cleanup(custSrv.Close)
		return []microsoft.Option{microsoft.WithTokenURL(tokenSrv.URL), microsoft.WithCustomerBaseURL(custSrv.URL),
			microsoft.WithBaseURL("http://campaign.invalid")}, &calls
	}

	t.Run("disabled", func(t *testing.T) {
		t.Setenv(constants.EnvMicrosoftMetricsEnabled, "")
		opts, calls := servers(t)
		d := NewMicrosoftDispatcher(fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{}, opts...)
		if _, err := d.ReadAccountCurrency(context.Background(), "cncf", model.ProviderMicrosoftAds, "1234567"); !errors.Is(err, domain.ErrAccountMetricsUnsupported) {
			t.Errorf("err = %v, want ErrAccountMetricsUnsupported", err)
		}
		if n := calls.Load(); n != 0 {
			t.Errorf("%d upstream calls, want none", n)
		}
	})

	t.Setenv(constants.EnvMicrosoftMetricsEnabled, "true")
	t.Run("the connection's account", func(t *testing.T) {
		opts, calls := servers(t)
		d := NewMicrosoftDispatcher(fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{}, opts...)
		got, err := d.ReadAccountCurrency(context.Background(), "cncf", model.ProviderMicrosoftAds, "1234567")
		if err != nil || got != "EUR" {
			t.Fatalf("ReadAccountCurrency = %q, %v; want EUR", got, err)
		}
		if n := calls.Load(); n != 1 {
			t.Errorf("%d upstream calls, want 1", n)
		}
	})
	t.Run("a second monitor read is served from the client's cache", func(t *testing.T) {
		opts, calls := servers(t)
		d := NewMicrosoftDispatcher(fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{}, opts...)
		for i := 0; i < 3; i++ {
			got, err := d.ReadAccountCurrency(context.Background(), "cncf", model.ProviderMicrosoftAds, "1234567")
			if err != nil || got != "EUR" {
				t.Fatalf("ReadAccountCurrency #%d = %q, %v; want EUR", i, got, err)
			}
		}
		if n := calls.Load(); n != 1 {
			t.Errorf("%d Account/Query calls over three reads, want 1", n)
		}
	})
	for _, tc := range []struct {
		name      string
		requested string
		wantErr   error
	}{
		{"another account is refused", "7654321", domain.ErrAccountNotManagedByConnection},
		{"leading zero is malformed", "01234567", domain.ErrAccountIDMalformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, calls := servers(t)
			d := NewMicrosoftDispatcher(fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{}, opts...)
			if _, err := d.ReadAccountCurrency(context.Background(), "cncf", model.ProviderMicrosoftAds, tc.requested); !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
			if n := calls.Load(); n != 0 {
				t.Errorf("%d upstream calls, want none", n)
			}
		})
	}
}
