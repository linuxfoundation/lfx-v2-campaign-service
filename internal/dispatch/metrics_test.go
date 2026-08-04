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
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
)

// googleAdsSearchServer wires a token endpoint plus an API server that answers the
// GAQL :search path, which is the only endpoint the metrics read touches.
func googleAdsSearchServer(t *testing.T, searchH http.HandlerFunc) []googleads.Option {
	t.Helper()
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "googleAds:search") {
			searchH(w, r)
			return
		}
		http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
	}))
	t.Cleanup(apiSrv.Close)
	return []googleads.Option{googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL)}
}

func metricsCampaign() *model.Campaign {
	return &model.Campaign{
		ID:                 "11111111-1111-1111-1111-111111111111",
		ProjectID:          "cncf",
		Platform:           model.ProviderGoogleAds,
		PlatformCampaignID: "555",
	}
}

func testMetricsRange(t *testing.T) (time.Time, time.Time) {
	t.Helper()
	from, _ := time.Parse("2006-01-02", "2026-07-01")
	to, _ := time.Parse("2006-01-02", "2026-07-02")
	return from, to
}

// TestGoogleAdsFetchMetrics_MapsRowsAndStampsAttributionBasis is the mapping contract:
// every row must carry the campaign id, the platform, and — critically — the
// attribution basis, without which a rollup cannot tell comparable rows apart.
func TestGoogleAdsFetchMetrics_MapsRowsAndStampsAttributionBasis(t *testing.T) {
	opts := googleAdsSearchServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"results":[
			{"segments":{"date":"2026-07-01"},
			 "metrics":{"impressions":"1000","clicks":"25","costMicros":"5500000","conversions":1.5},
			 "customer":{"currencyCode":"EUR"}}
		]}`)
	})
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	from, to := testMetricsRange(t)
	rows, err := d.FetchMetrics(context.Background(), metricsCampaign(), from, to)
	if err != nil {
		t.Fatalf("FetchMetrics: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	r := rows[0]

	if r.CampaignID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("CampaignID = %q, want the LFX campaign UUID (not the platform id)", r.CampaignID)
	}
	if r.Platform != model.ProviderGoogleAds {
		t.Errorf("Platform = %q, want %q", r.Platform, model.ProviderGoogleAds)
	}
	// THE load-bearing field: without a basis, SummariseMetrics cannot refuse to add
	// incomparable conversions, and a cross-platform total silently becomes wrong.
	if r.AttributionBasis != model.AttributionGoogleAdsClickTime {
		t.Errorf("AttributionBasis = %q, want %q — an unstamped row makes a rollup silently incomparable",
			r.AttributionBasis, model.AttributionGoogleAdsClickTime)
	}
	if r.Spend != "5.500000" {
		t.Errorf("Spend = %s, want 5.500000 (5500000 micros)", r.Spend)
	}
	if r.Conversions != "1.500000" {
		t.Errorf("Conversions = %s, want 1.500000", r.Conversions)
	}
	if r.Currency != "EUR" {
		t.Errorf("Currency = %q, want EUR — spend must never be stored without its unit", r.Currency)
	}
	if r.Impressions != 1000 || r.Clicks != 25 {
		t.Errorf("Impressions/Clicks = %d/%d, want 1000/25", r.Impressions, r.Clicks)
	}
	if len(r.Raw) == 0 {
		t.Error("Raw is empty; the platform's own response must be retained for auditability")
	}
}

// TestGoogleAdsFetchMetrics_NoUpstreamIDIsNotProvisioned: a campaign that was never
// created upstream has nothing to report, and must say so as a state error rather than
// calling Google with an empty id.
func TestGoogleAdsFetchMetrics_NoUpstreamIDIsNotProvisioned(t *testing.T) {
	var called bool
	opts := googleAdsSearchServer(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		_, _ = io.WriteString(w, `{"results":[]}`)
	})
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	c := metricsCampaign()
	c.PlatformCampaignID = ""
	from, to := testMetricsRange(t)
	_, err := d.FetchMetrics(context.Background(), c, from, to)
	if !errors.Is(err, domain.ErrCampaignNotProvisioned) {
		t.Errorf("err = %v, want ErrCampaignNotProvisioned", err)
	}
	if called {
		t.Error("Google was called for a campaign with no upstream id; the guard must short-circuit first")
	}
}

func TestGoogleAdsFetchMetrics_NilCampaignIsAnError(t *testing.T) {
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{})
	from, to := testMetricsRange(t)
	if _, err := d.FetchMetrics(context.Background(), nil, from, to); err == nil {
		t.Error("a nil campaign must be an error, not a panic or an empty result")
	}
}

// TestGoogleAdsFetchMetrics_BadConnectionSurfaces confirms the read path applies the
// SAME connection validation as create/toggle, so the three cannot drift.
func TestGoogleAdsFetchMetrics_BadConnectionSurfaces(t *testing.T) {
	cases := []struct {
		name string
		repo connReader
		enc  domain.Encryptor
	}{
		{"missing connection", fakeConnReader{err: domain.ErrNotFound}, identityEncryptor{}},
		{"decrypt fails", fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, errEncryptor{}},
		{"incomplete credentials", fakeConnReader{conn: activeGoogleAdsConn(`{"ClientID":"cid"}`)}, identityEncryptor{}},
		{"inactive connection", fakeConnReader{conn: &model.Connection{
			Provider: model.ProviderGoogleAds, AccountID: "1",
			EncryptedCredentials: []byte(goodGoogleAdsCreds), Status: model.StatusInactive,
		}}, identityEncryptor{}},
	}
	from, to := testMetricsRange(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewGoogleAdsDispatcher(tc.repo, tc.enc)
			if _, err := d.FetchMetrics(context.Background(), metricsCampaign(), from, to); err == nil {
				t.Error("an unusable connection must surface an error, not an empty metric set")
			}
		})
	}
}

// TestGoogleAdsFetchMetrics_APIErrorIsNotAnEmptyResult: a failed read must NOT look
// like "this campaign had no activity", which would overwrite real history with zeros.
func TestGoogleAdsFetchMetrics_APIErrorIsNotAnEmptyResult(t *testing.T) {
	opts := googleAdsSearchServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
	})
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	from, to := testMetricsRange(t)
	rows, err := d.FetchMetrics(context.Background(), metricsCampaign(), from, to)
	if err == nil {
		t.Fatalf("a 500 from Google returned %d rows and no error; that is indistinguishable from a campaign with no activity", len(rows))
	}
	if rows != nil {
		t.Errorf("rows = %v on error, want nil", rows)
	}
}

// TestDeferredPlatformsReturnUnsupported pins the deferral contract: every
// unimplemented platform must return the SENTINEL, never a fabricated empty result.
// An empty-and-nil-error response would report a campaign that spent real money as
// having spent nothing.
func TestDeferredPlatformsReturnUnsupported(t *testing.T) {
	from, to := testMetricsRange(t)
	enc := identityEncryptor{}
	repo := fakeConnReader{err: domain.ErrNotFound}

	cases := []struct {
		name    string
		fetcher metricsFetcher
	}{
		{"meta", NewMetaDispatcher(repo, enc)},
		{"linkedin", NewLinkedInDispatcher(repo, enc)},
		{"microsoft", NewMicrosoftDispatcher(repo, enc)},
		{"reddit", NewRedditDispatcher(repo, enc)},
		{"twitter", NewTwitterDispatcher(repo, enc)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := tc.fetcher.FetchMetrics(context.Background(), metricsCampaign(), from, to)
			if !errors.Is(err, domain.ErrMetricsUnsupported) {
				t.Errorf("err = %v, want ErrMetricsUnsupported — a deferred platform must say so explicitly, never return a plausible zero", err)
			}
			if len(rows) != 0 {
				t.Errorf("got %d rows from a deferred platform, want 0", len(rows))
			}
			// The message must name the platform so an operator can act on it.
			if err != nil && !strings.Contains(strings.ToLower(err.Error()), tc.name) {
				t.Errorf("error %q should name the platform %q", err, tc.name)
			}
		})
	}
}

// TestGoogleAdsIsNotDeferred guards against the whole feature silently regressing into
// "everything unsupported".
func TestGoogleAdsIsNotDeferred(t *testing.T) {
	opts := googleAdsSearchServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"results":[]}`)
	})
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	from, to := testMetricsRange(t)
	if _, err := d.FetchMetrics(context.Background(), metricsCampaign(), from, to); errors.Is(err, domain.ErrMetricsUnsupported) {
		t.Error("Google Ads reports ErrMetricsUnsupported; it is the one platform that IS implemented")
	}
}
