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
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
)

// metaAudienceServer answers both audience breakdowns with one row for campaign "555" and
// records every request URI under a mutex (the handler runs on the server's goroutines).
type metaAudienceServer struct {
	mu   sync.Mutex
	uris []string
	srv  *httptest.Server
}

func newMetaAudienceServer(t *testing.T) *metaAudienceServer {
	t.Helper()
	s := &metaAudienceServer{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.uris = append(s.uris, r.URL.RequestURI())
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("breakdowns") == "age,gender" {
			_, _ = io.WriteString(w, `{"data":[{"campaign_id":"555","age":"25-34","gender":"female","impressions":"1000","clicks":"40","spend":"25.00","account_currency":"USD"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"campaign_id":"555","publisher_platform":"instagram","platform_position":"feed","impressions":"900","clicks":"9","spend":"1.5","account_currency":"USD"}]}`)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *metaAudienceServer) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.uris...)
}

func metaScope(ids ...string) []model.ProjectCampaignScope {
	out := make([]model.ProjectCampaignScope, 0, len(ids))
	for _, id := range ids {
		out = append(out, model.ProjectCampaignScope{PlatformCampaignID: id})
	}
	return out
}

func TestMeta_ReadMetaAudienceInsights_MapsBucketsAndKeepsRequestWindow(t *testing.T) {
	s := newMetaAudienceServer(t)
	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{}, meta.WithBaseURL(s.srv.URL))

	ai, err := d.ReadMetaAudienceInsights(context.Background(), "proj", model.ProviderMetaAds, model.MetricsWindowLast7Days, metaScope("555"))
	if err != nil {
		t.Fatalf("ReadMetaAudienceInsights: %v", err)
	}
	if ai.Window != model.MetricsWindowLast7Days || ai.Currency != "USD" {
		t.Errorf("window/currency = %q/%q", ai.Window, ai.Currency)
	}
	want := []model.MetaAudienceBucket{
		{Dimension: model.MetaAudienceDimensionAgeGender, Age: "25-34", Gender: "female", Impressions: 1000, Clicks: 40, CostMicros: 25_000_000, Ctr: 0.04},
		{Dimension: model.MetaAudienceDimensionPlacement, PublisherPlatform: "instagram", PlatformPosition: "feed", Impressions: 900, Clicks: 9, CostMicros: 1_500_000, Ctr: 0.01},
	}
	if len(ai.Buckets) != len(want) {
		t.Fatalf("buckets = %+v", ai.Buckets)
	}
	for i := range want {
		if ai.Buckets[i] != want[i] {
			t.Errorf("bucket %d = %+v, want %+v", i, ai.Buckets[i], want[i])
		}
	}
	// The model dimension tokens must be the client's; a drift would publish an enum value the
	// design does not declare.
	if model.MetaAudienceDimensionAgeGender != meta.AudienceDimensionAgeGender || model.MetaAudienceDimensionPlacement != meta.AudienceDimensionPlacement {
		t.Error("model and client dimension tokens diverged")
	}
	for _, uri := range s.requests() {
		u, _ := url.Parse(uri)
		if u.Path != "/act_777/insights" {
			t.Errorf("path = %q, want the connection's own account", u.Path)
		}
		if got := u.Query().Get("filtering"); got != `[{"field":"campaign.id","operator":"IN","value":["555"]}]` {
			t.Errorf("filtering = %s, want the project's own campaign ids", got)
		}
		if got := u.Query().Get("date_preset"); got != "last_7d" {
			t.Errorf("date_preset = %q", got)
		}
	}
}

// Every refusal below is decided before Meta is contacted.
func TestMeta_ReadMetaAudienceInsights_RefusedBeforeAnyRequest(t *testing.T) {
	noAccount := activeMetaConn(goodMetaCreds)
	noAccount.AccountID = ""
	bareDigits := activeMetaConn(goodMetaCreds)
	bareDigits.AccountID = "777"
	tooMany := make([]string, meta.MaxAudienceCampaigns+1)
	for i := range tooMany {
		tooMany[i] = strconv.Itoa(1000 + i)
	}

	for _, tc := range []struct {
		name   string
		reader fakeConnReader
		window model.MetricsWindow
		scope  []model.ProjectCampaignScope
		want   []error
	}{
		{"unsupported window", fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, "last_year", metaScope("555"), []error{domain.ErrMetricsWindowUnsupported}},
		{"no connection", fakeConnReader{err: domain.ErrNotFound}, model.MetricsWindowLast30Days, metaScope("555"), []error{domain.ErrNotFound}},
		{"no account selected", fakeConnReader{conn: noAccount}, model.MetricsWindowLast30Days, metaScope("555"), []error{domain.ErrConnectionNotUsable, domain.ErrAccountNotSelected}},
		{"bare-digit stored account", fakeConnReader{conn: bareDigits}, model.MetricsWindowLast30Days, metaScope("555"), []error{domain.ErrConnectionNotUsable}},
		{"incomplete credential", fakeConnReader{conn: activeMetaConn(`{}`)}, model.MetricsWindowLast30Days, metaScope("555"), []error{domain.ErrConnectionNotUsable}},
		{"malformed stored campaign id", fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, model.MetricsWindowLast30Days, metaScope("555", "12x"), []error{domain.ErrAudienceScopeInvalid}},
		{"empty scope never widens", fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, model.MetricsWindowLast30Days, nil, []error{domain.ErrAudienceScopeInvalid}},
		{"scope too large", fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, model.MetricsWindowLast30Days, metaScope(tooMany...), []error{domain.ErrAudienceScopeTooLarge}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newMetaAudienceServer(t)
			d := NewMetaDispatcher(tc.reader, identityEncryptor{}, meta.WithBaseURL(s.srv.URL))
			ai, err := d.ReadMetaAudienceInsights(context.Background(), "proj", model.ProviderMetaAds, tc.window, tc.scope)
			for _, w := range tc.want {
				if !errors.Is(err, w) {
					t.Errorf("error = %v, want it to wrap %v", err, w)
				}
			}
			if ai != nil {
				t.Errorf("a refused read returned %+v", ai)
			}
			if n := len(s.requests()); n != 0 {
				t.Errorf("Meta was contacted %d time(s)", n)
			}
		})
	}
}

// ANY campaign created under a different ad account refuses the whole read (409) rather than
// returning the rest as though it were the project's whole picture.
func TestMeta_ReadMetaAudienceInsights_AccountMismatchRefusesWholeRead(t *testing.T) {
	foreign := model.ProjectCampaignScope{PlatformCampaignID: "666", Result: json.RawMessage(`{"CampaignID":"666","AccountID":"act_999"}`)}
	for name, scope := range map[string][]model.ProjectCampaignScope{
		"all foreign": {foreign},
		"partial":     append(metaScope("555"), foreign),
	} {
		t.Run(name, func(t *testing.T) {
			s := newMetaAudienceServer(t)
			d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{}, meta.WithBaseURL(s.srv.URL))
			_, err := d.ReadMetaAudienceInsights(context.Background(), "proj", model.ProviderMetaAds, model.MetricsWindowLast30Days, scope)
			if !errors.Is(err, domain.ErrCampaignAccountMismatch) {
				t.Fatalf("error = %v, want ErrCampaignAccountMismatch", err)
			}
			if n := len(s.requests()); n != 0 {
				t.Errorf("Meta was contacted %d time(s)", n)
			}
		})
	}
}

// A row that cannot PROVE a mismatch is read: matching provenance in either recorded form, or
// none at all.
func TestMeta_ReadMetaAudienceInsights_MatchingOrUnknownProvenanceReads(t *testing.T) {
	scope := []model.ProjectCampaignScope{
		{PlatformCampaignID: "555", Result: json.RawMessage(`{"CampaignID":"555","AccountID":"act_777"}`)},
		{PlatformCampaignID: "556", Result: json.RawMessage(`{"CampaignID":"556","MetaURL":"https://adsmanager.facebook.com/adsmanager/manage/campaigns?act=777"}`)},
		{PlatformCampaignID: "557"},
	}
	s := newMetaAudienceServer(t)
	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{}, meta.WithBaseURL(s.srv.URL))
	if _, err := d.ReadMetaAudienceInsights(context.Background(), "proj", model.ProviderMetaAds, model.MetricsWindowLast30Days, scope); err != nil {
		t.Fatalf("ReadMetaAudienceInsights: %v", err)
	}
	reqs := s.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d", len(reqs))
	}
	u, _ := url.Parse(reqs[0])
	if got := u.Query().Get("filtering"); !strings.Contains(got, `["555","556","557"]`) {
		t.Errorf("filtering = %s, want all three owned campaigns", got)
	}
}

// An upstream failure is returned unclassified (→ 503 at the service), never as a domain
// sentinel that would turn it into a 4xx.
func TestMeta_ReadMetaAudienceInsights_UpstreamFailureIsUnclassified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"no permission","type":"OAuthException","code":200}}`)
	}))
	t.Cleanup(srv.Close)
	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{}, meta.WithBaseURL(srv.URL))
	_, err := d.ReadMetaAudienceInsights(context.Background(), "proj", model.ProviderMetaAds, model.MetricsWindowLast30Days, metaScope("555"))
	var apiErr *meta.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden {
		t.Fatalf("error = %v, want the 403 *meta.APIError", err)
	}
	for _, s := range []error{domain.ErrConnectionNotUsable, domain.ErrNotFound, domain.ErrCampaignAccountMismatch, domain.ErrAudienceScopeInvalid} {
		if errors.Is(err, s) {
			t.Errorf("an upstream refusal must not carry %v", s)
		}
	}
}
