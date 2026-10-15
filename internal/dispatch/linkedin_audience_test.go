// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
)

// linkedInAudienceUpstream counts adAnalytics requests and answers every pivot with one element
// for campaign 555.
type linkedInAudienceUpstream struct {
	mu      sync.Mutex
	calls   int
	status  int
	queries []string
}

func (u *linkedInAudienceUpstream) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.calls++
		u.queries = append(u.queries, r.URL.RawQuery)
		u.mu.Unlock()
		if u.status != 0 {
			w.WriteHeader(u.status)
			_, _ = io.WriteString(w, `{"message":"upstream-secret-text"}`)
			return
		}
		value := "urn:li:function:8"
		switch {
		case strings.Contains(r.URL.RawQuery, "MEMBER_SENIORITY"):
			value = "urn:li:seniority:5"
		case strings.Contains(r.URL.RawQuery, "MEMBER_INDUSTRY"):
			value = "urn:li:industry:4"
		case strings.Contains(r.URL.RawQuery, "MEMBER_COMPANY_SIZE"):
			value = "SIZE_11_TO_50"
		case strings.Contains(r.URL.RawQuery, "MEMBER_COUNTRY_V2"):
			value = "urn:li:geo:103644278"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"elements":[{"pivotValues":[%q],"impressions":100,"clicks":5,"costInUsd":"2.5"}]}`, value)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (u *linkedInAudienceUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

func linkedInAudienceDispatcher(reader connReader, srvURL string) *LinkedInDispatcher {
	return NewLinkedInDispatcher(reader, identityEncryptor{},
		linkedin.WithBaseURL(srvURL), linkedin.WithClock(func() time.Time { return time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC) }))
}

func ownScope(results ...string) []model.ProjectCampaignScope {
	out := make([]model.ProjectCampaignScope, 0, len(results))
	for _, r := range results {
		out = append(out, model.ProjectCampaignScope{PlatformCampaignID: "555", Result: json.RawMessage(r)})
	}
	return out
}

func TestLinkedIn_ReadAudience_HappyPathMapsEveryPivot(t *testing.T) {
	up := &linkedInAudienceUpstream{}
	d := linkedInAudienceDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, up.server(t).URL)
	ai, err := d.ReadLinkedInAudienceInsights(context.Background(), "proj", model.ProviderLinkedInAds,
		model.MetricsWindowLast30Days, ownScope(`{"campaignId":"555","accountId":"123456789"}`))
	if err != nil {
		t.Fatalf("ReadLinkedInAudienceInsights: %v", err)
	}
	if ai.Window != model.MetricsWindowLast30Days || len(ai.Buckets) != 5 {
		t.Fatalf("result = %+v", ai)
	}
	wantDims := []string{"job_function", "seniority", "industry", "company_size", "country"}
	for i, b := range ai.Buckets {
		if b.Dimension != wantDims[i] || b.Impressions != 100 || b.Clicks != 5 || b.CostMicros != 2_500_000 || b.Ctr != 0.05 || b.Value == "" {
			t.Errorf("bucket %d = %+v", i, b)
		}
	}
	if up.count() != 5 {
		t.Errorf("requests = %d, want 5", up.count())
	}
	// Every request is confined to the connection's BOUND account (activeLinkedInConn's
	// 123456789) as well as to the scoped campaign: campaign ids are unique only per account.
	up.mu.Lock()
	defer up.mu.Unlock()
	for i, q := range up.queries {
		if !strings.Contains(q, "accounts=List("+url.QueryEscape("urn:li:sponsoredAccount:123456789")+")") {
			t.Errorf("request %d is not confined to the bound account: %s", i, q)
		}
		if !strings.Contains(q, "campaigns=List("+url.QueryEscape("urn:li:sponsoredCampaign:555")+")") {
			t.Errorf("request %d does not name the scoped campaign: %s", i, q)
		}
		if !strings.Contains(q, "q=analytics") {
			t.Errorf("request %d does not use the single-pivot Analytics finder: %s", i, q)
		}
	}
}

// Trust boundary: the project's OWN connection only. A connection that exists only under the LF
// system scope must NOT serve the read — the result is ErrNotFound (404), and LinkedIn is never
// contacted.
func TestLinkedIn_ReadAudience_RefusesSystemFallback(t *testing.T) {
	up := &linkedInAudienceUpstream{}
	d := linkedInAudienceDispatcher(&scopedConnReader{
		rows: map[string]*model.Connection{model.SystemProjectID: activeLinkedInConn(goodLinkedInCreds)},
	}, up.server(t).URL)
	_, err := d.ReadLinkedInAudienceInsights(context.Background(), "cncf", model.ProviderLinkedInAds,
		model.MetricsWindowLast30Days, ownScope(`{"campaignId":"555"}`))
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want domain.ErrNotFound (no own connection; the LF system row must not be borrowed)", err)
	}
	if up.count() != 0 {
		t.Errorf("LinkedIn contacted %d time(s) on a refused fallback", up.count())
	}
}

// Bound account only: ANY campaign created under another LinkedIn ad account refuses the whole
// read (409) before LinkedIn is contacted, including the mixed case.
func TestLinkedIn_ReadAudience_ForeignAccountIs409AndNeverQueries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		scope []model.ProjectCampaignScope
	}{
		{"accountId field", ownScope(`{"campaignId":"555","accountId":"999888777"}`)},
		{"legacy url", ownScope(`{"linkedInUrl":"https://www.linkedin.com/campaignmanager/accounts/999888777/campaigns/555"}`)},
		{"mixed scope", ownScope(`{"accountId":"123456789"}`, `{"accountId":"999888777"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := &linkedInAudienceUpstream{}
			d := linkedInAudienceDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, up.server(t).URL)
			ai, err := d.ReadLinkedInAudienceInsights(context.Background(), "proj", model.ProviderLinkedInAds, model.MetricsWindowLast30Days, tc.scope)
			if !errors.Is(err, domain.ErrCampaignAccountMismatch) {
				t.Fatalf("err = %v, want ErrCampaignAccountMismatch", err)
			}
			if ai != nil {
				t.Errorf("a refused read returned %+v", ai)
			}
			if up.count() != 0 {
				t.Errorf("LinkedIn contacted %d time(s)", up.count())
			}
		})
	}
}

// Unknown provenance (no recorded account, unparseable blob) is not a mismatch: the read proceeds.
func TestLinkedIn_ReadAudience_UnknownProvenanceStillReads(t *testing.T) {
	up := &linkedInAudienceUpstream{}
	d := linkedInAudienceDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, up.server(t).URL)
	if _, err := d.ReadLinkedInAudienceInsights(context.Background(), "proj", model.ProviderLinkedInAds,
		model.MetricsWindowLast30Days, ownScope(`{"campaignId":"555"}`, `not json`)); err != nil {
		t.Fatalf("unknown provenance must proceed: %v", err)
	}
	if up.count() != 5 {
		t.Errorf("requests = %d, want 5", up.count())
	}
}

// Permanent request/row faults win over the connection's state and never decrypt or query.
func TestLinkedIn_ReadAudience_LocalRefusalsBeatConnectionState(t *testing.T) {
	inactive := activeLinkedInConn(goodLinkedInCreds)
	inactive.Status = model.StatusInactive
	tooMany := make([]model.ProjectCampaignScope, linkedin.MaxAudienceCampaigns+1)
	for i := range tooMany {
		tooMany[i] = model.ProjectCampaignScope{PlatformCampaignID: fmt.Sprint(1000 + i)}
	}
	for _, tc := range []struct {
		name   string
		window model.MetricsWindow
		scope  []model.ProjectCampaignScope
		want   error
	}{
		{"yesterday", model.MetricsWindowYesterday, ownScope(`{}`), domain.ErrMetricsWindowUnsupported},
		{"last_14_days", model.MetricsWindowLast14Days, ownScope(`{}`), domain.ErrMetricsWindowUnsupported},
		{"malformed stored id", model.MetricsWindowLast30Days, []model.ProjectCampaignScope{{PlatformCampaignID: "urn:li:sponsoredCampaign:5"}}, domain.ErrAudienceScopeInvalid},
		{"empty scope", model.MetricsWindowLast30Days, nil, domain.ErrAudienceScopeInvalid},
		{"over the cap", model.MetricsWindowLast30Days, tooMany, domain.ErrAudienceScopeTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := &linkedInAudienceUpstream{}
			d := linkedInAudienceDispatcher(fakeConnReader{conn: inactive}, up.server(t).URL)
			_, err := d.ReadLinkedInAudienceInsights(context.Background(), "proj", model.ProviderLinkedInAds, tc.window, tc.scope)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if errors.Is(err, domain.ErrConnectionNotUsable) {
				t.Errorf("a permanent request fault was reported as a connection fault: %v", err)
			}
			if up.count() != 0 {
				t.Errorf("LinkedIn contacted %d time(s)", up.count())
			}
		})
	}
}

func TestLinkedIn_ReadAudience_ConnectionStatesAndUpstreamFaults(t *testing.T) {
	inactive := activeLinkedInConn(goodLinkedInCreds)
	inactive.Status = model.StatusInactive
	noAccount := activeLinkedInConn(goodLinkedInCreds)
	noAccount.AccountID = "  "
	for _, tc := range []struct {
		name     string
		conn     *model.Connection
		status   int
		want     []error
		wantCall bool
	}{
		{"inactive connection", inactive, 0, []error{domain.ErrConnectionNotUsable, domain.ErrConnectionInactive}, false},
		{"no account selected", noAccount, 0, []error{domain.ErrConnectionNotUsable, domain.ErrAccountNotSelected}, false},
		{"401 is an expired credential", activeLinkedInConn(goodLinkedInCreds), http.StatusUnauthorized, []error{domain.ErrConnectionNotUsable, domain.ErrCredentialsExpired}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := &linkedInAudienceUpstream{status: tc.status}
			d := linkedInAudienceDispatcher(fakeConnReader{conn: tc.conn}, up.server(t).URL)
			_, err := d.ReadLinkedInAudienceInsights(context.Background(), "proj", model.ProviderLinkedInAds, model.MetricsWindowLast30Days, ownScope(`{}`))
			for _, w := range tc.want {
				if !errors.Is(err, w) {
					t.Errorf("err = %v, want it to wrap %v", err, w)
				}
			}
			if (up.count() > 0) != tc.wantCall {
				t.Errorf("LinkedIn contacted %d time(s), wantCall=%v", up.count(), tc.wantCall)
			}
		})
	}
	// A 5xx is neither a connection fault nor a scope fault: it reaches the service's 503 arm,
	// and carries no upstream body.
	up := &linkedInAudienceUpstream{status: http.StatusBadGateway}
	d := linkedInAudienceDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, up.server(t).URL)
	_, err := d.ReadLinkedInAudienceInsights(context.Background(), "proj", model.ProviderLinkedInAds, model.MetricsWindowLast30Days, ownScope(`{}`))
	if err == nil || errors.Is(err, domain.ErrConnectionNotUsable) || errors.Is(err, domain.ErrAudienceScopeInvalid) {
		t.Fatalf("5xx: err = %v, want an unclassified upstream failure", err)
	}
	if strings.Contains(err.Error(), "upstream-secret-text") {
		t.Errorf("5xx error carries the upstream body: %v", err)
	}
}
