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

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
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

	t.Run("linkedin", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if strings.Contains(r.URL.Path, "adAnalytics") {
				_, _ = io.WriteString(w, `{"elements":[]}`)
				return
			}
			_, _ = io.WriteString(w, `{"elements":[{"id":1,"name":"c","status":"ACTIVE","dailyBudget":{"amount":"10","currencyCode":"USD"}}],"metadata":{}}`)
		}))
		t.Cleanup(srv.Close)
		d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{}, linkedin.WithBaseURL(srv.URL))
		read, err := d.ReadAccountMonitor(context.Background(), "cncf", model.ProviderLinkedInAds, "123456789", 30)
		if err != nil {
			t.Fatalf("ReadAccountMonitor: %v", err)
		}
		if read.Currency != "USD" || len(read.Rows) != 1 {
			t.Errorf("read = %+v, want one row in USD", read)
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
	})
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
			_, _ = io.WriteString(w, `{"Account":{"Id":1234567,"CurrencyCode":"Euro"}}`)
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
