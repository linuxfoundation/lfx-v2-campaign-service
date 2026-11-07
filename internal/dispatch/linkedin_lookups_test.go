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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

var (
	_ service.AudienceReader         = (*LinkedInDispatcher)(nil)
	_ service.ConversionActionReader = (*LinkedInDispatcher)(nil)
)

func linkedInLookupServer(t *testing.T, path, body string, status int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != path {
			t.Errorf("path %s, want %s", r.URL.Path, path)
		}
		if !strings.Contains(r.URL.RawQuery, "sponsoredAccount%3A123456789") {
			t.Errorf("query %s does not name the connection's account", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestLinkedIn_ListAudiencesMapsAdSegments(t *testing.T) {
	srv, _ := linkedInLookupServer(t, "/adSegments", `{"elements":[{"id":16182906,"name":"Site visitors","type":"RETARGETING","status":"READY","approximateMemberCount":48000,"account":"urn:li:sponsoredAccount:123456789"}]}`, http.StatusOK)
	d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{}, linkedin.WithBaseURL(srv.URL))

	res, called, err := d.ListAudiences(context.Background(), "cncf", model.ProviderLinkedInAds, LinkedInAdSegmentKind)
	if err != nil || !called {
		t.Fatalf("ListAudiences = %v (called %v)", err, called)
	}
	if res.AccountID != "123456789" || res.Kind != LinkedInAdSegmentKind || len(res.Audiences) != 1 {
		t.Fatalf("result = %+v", res)
	}
	a := res.Audiences[0]
	if a.ResourceName != "urn:li:adSegment:16182906" || a.ID != "16182906" || a.Subtype != "RETARGETING" || a.Status != "READY" ||
		a.Facet != "dynamicSegments" || a.MemberCount == nil || *a.MemberCount != 48000 {
		t.Errorf("segment = %+v", a)
	}
}

func TestLinkedIn_ListConversionActionsMapsRules(t *testing.T) {
	srv, _ := linkedInLookupServer(t, "/conversions", `{"elements":[{"id":104012,"name":"KubeCon registration","type":"REGISTRATION","enabled":true},{"id":7,"name":"old","type":"LEAD","enabled":false}]}`, http.StatusOK)
	d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{}, linkedin.WithBaseURL(srv.URL))

	res, called, err := d.ListConversionActions(context.Background(), "cncf", model.ProviderLinkedInAds)
	if err != nil || !called {
		t.Fatalf("ListConversionActions = %v (called %v)", err, called)
	}
	got := map[string]model.ConversionAction{}
	for _, a := range res.Actions {
		got[a.ID] = a
	}
	if a := got["104012"]; a.ResourceName != "urn:lla:llaPartnerConversion:104012" || a.Status != "ENABLED" || a.Type != "REGISTRATION" {
		t.Errorf("enabled rule = %+v", a)
	}
	if a := got["7"]; a.Status != "DISABLED" {
		t.Errorf("disabled rule = %+v", a)
	}
}

func TestLinkedIn_LookupsRefuseLocallyAndClassifyDefects(t *testing.T) {
	d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{})
	if _, called, err := d.ListAudiences(context.Background(), "cncf", model.ProviderLinkedInAds, "custom_audience"); called || !errors.Is(err, domain.ErrTargetingLookupUnsupported) {
		t.Errorf("an unserved kind = %v (called %v), want ErrTargetingLookupUnsupported before any request", err, called)
	}
	none := NewLinkedInDispatcher(fakeConnReader{err: domain.ErrNotFound}, identityEncryptor{})
	if _, called, err := none.ListConversionActions(context.Background(), "cncf", model.ProviderLinkedInAds); called || err == nil {
		t.Errorf("no connection = %v (called %v), want a local refusal", err, called)
	}
	srv, calls := linkedInLookupServer(t, "/conversions", `{"serviceErrorCode":65601,"status":401}`, http.StatusUnauthorized)
	expired := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{}, linkedin.WithBaseURL(srv.URL))
	_, called, err := expired.ListConversionActions(context.Background(), "cncf", model.ProviderLinkedInAds)
	if !called || calls.Load() == 0 || !errors.Is(err, domain.ErrConnectionNotUsable) {
		t.Errorf("a 401 = %v (called %v), want a connection defect", err, called)
	}
}

// TestLinkedIn_LookupsRefuseAMalformedBoundAccount pins that a non-numeric stored account is a
// local connection defect: ErrConnectionNotUsable, not reported as an upstream call, no request.
func TestLinkedIn_LookupsRefuseAMalformedBoundAccount(t *testing.T) {
	srv, calls := linkedInLookupServer(t, "/conversions", `{"elements":[]}`, http.StatusOK)
	for _, acct := range []string{"acct-1", "0", "0123", "9223372036854775808"} {
		bad := activeLinkedInConn(goodLinkedInCreds)
		bad.AccountID = acct
		d := NewLinkedInDispatcher(fakeConnReader{conn: bad}, identityEncryptor{}, linkedin.WithBaseURL(srv.URL))
		if _, called, err := d.ListConversionActions(context.Background(), "cncf", model.ProviderLinkedInAds); called || !errors.Is(err, domain.ErrConnectionNotUsable) {
			t.Errorf("%s: conversions = %v (called %v), want a local connection defect", acct, err, called)
		}
		if _, called, err := d.ListAudiences(context.Background(), "cncf", model.ProviderLinkedInAds, LinkedInAdSegmentKind); called || !errors.Is(err, domain.ErrConnectionNotUsable) {
			t.Errorf("%s: audiences = %v (called %v), want a local connection defect", acct, err, called)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("%d requests for a malformed account", n)
	}
}
