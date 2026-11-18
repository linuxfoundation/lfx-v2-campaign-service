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
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

var _ service.TargetingEntitySearcher = (*LinkedInDispatcher)(nil)

func linkedInTypeaheadServer(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int32, *atomic.Value) {
	t.Helper()
	var calls atomic.Int32
	var lastQuery atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		lastQuery.Store(r.URL.RawQuery)
		if r.URL.Path != "/adTargetingEntities" {
			t.Errorf("path %s, want /adTargetingEntities", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization %q, want the connection's token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls, &lastQuery
}

func TestLinkedIn_SearchTargetingEntitiesMapsAndCaches(t *testing.T) {
	srv, calls, last := linkedInTypeaheadServer(t, http.StatusOK,
		`{"elements":[{"urn":"urn:li:geo:103644278","facetUrn":"urn:li:adTargetingFacet:locations","name":"United States"},{"urn":"urn:li:skill:1","name":"wrong facet"}]}`)
	d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{}, linkedin.WithBaseURL(srv.URL))

	rows, called, err := d.SearchTargetingEntities(context.Background(), "cncf", model.ProviderLinkedInAds, model.TargetingEntityQuery{Facet: "locations", Query: "United"})
	if err != nil || !called {
		t.Fatalf("SearchTargetingEntities = %v (called %v)", err, called)
	}
	if len(rows) != 1 || rows[0] != (model.TargetingEntity{URN: "urn:li:geo:103644278", Name: "United States"}) {
		t.Errorf("rows = %+v, want only the geo URN", rows)
	}
	if q, _ := last.Load().(string); !strings.Contains(q, "facet=urn%3Ali%3AadTargetingFacet%3Alocations") || !strings.Contains(q, "query=United") {
		t.Errorf("query string %s", q)
	}
	// The same search, folded for case and space, is served from the cache: no request, and
	// called false so the orchestrator records no upstream call.
	again, called, err := d.SearchTargetingEntities(context.Background(), "other-project", model.ProviderLinkedInAds, model.TargetingEntityQuery{Facet: "locations", Query: " united "})
	if err != nil || called || len(again) != 1 || calls.Load() != 1 {
		t.Errorf("cached search = %+v (called %v, err %v, requests %d), want a hit", again, called, err, calls.Load())
	}
	// Another facet is another entry.
	if _, called, _ := d.SearchTargetingEntities(context.Background(), "cncf", model.ProviderLinkedInAds, model.TargetingEntityQuery{Facet: "skills", Query: "united"}); !called || calls.Load() != 2 {
		t.Errorf("a different facet must miss the cache (called %v, requests %d)", called, calls.Load())
	}
}

// The finder takes no ad account, so a connection that has not selected one can still search.
func TestLinkedIn_SearchTargetingEntitiesNeedsNoAccount(t *testing.T) {
	srv, _, _ := linkedInTypeaheadServer(t, http.StatusOK, `{"elements":[{"urn":"urn:li:title:9","name":"Software Engineer"}]}`)
	c := activeLinkedInConn(goodLinkedInCreds)
	c.AccountID = ""
	d := NewLinkedInDispatcher(fakeConnReader{conn: c}, identityEncryptor{}, linkedin.WithBaseURL(srv.URL))
	rows, called, err := d.SearchTargetingEntities(context.Background(), "cncf", model.ProviderLinkedInAds, model.TargetingEntityQuery{Facet: "titles", Query: "software"})
	if err != nil || !called || len(rows) != 1 {
		t.Errorf("account-less search = %+v (called %v, err %v)", rows, called, err)
	}
}

func TestLinkedIn_SearchTargetingEntitiesRefusesLocallyAndClassifies(t *testing.T) {
	srv, calls, _ := linkedInTypeaheadServer(t, http.StatusOK, `{"elements":[]}`)
	d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{}, linkedin.WithBaseURL(srv.URL))
	for _, q := range []model.TargetingEntityQuery{{Facet: "dynamicSegments", Query: "go"}, {Facet: "skills", Query: " "}, {Facet: "skills", Query: " g "}} {
		if _, called, err := d.SearchTargetingEntities(context.Background(), "cncf", model.ProviderLinkedInAds, q); called || !errors.Is(err, domain.ErrTargetingQueryInvalid) {
			t.Errorf("%+v = %v (called %v), want ErrTargetingQueryInvalid before any request", q, err, called)
		}
	}
	none := NewLinkedInDispatcher(fakeConnReader{err: domain.ErrNotFound}, identityEncryptor{}, linkedin.WithBaseURL(srv.URL))
	if _, called, err := none.SearchTargetingEntities(context.Background(), "cncf", model.ProviderLinkedInAds, model.TargetingEntityQuery{Facet: "skills", Query: "go"}); called || err == nil {
		t.Errorf("no connection = %v (called %v), want a local refusal", err, called)
	}
	inactive := activeLinkedInConn(goodLinkedInCreds)
	inactive.Status = model.StatusInactive
	off := NewLinkedInDispatcher(fakeConnReader{conn: inactive}, identityEncryptor{}, linkedin.WithBaseURL(srv.URL))
	if _, called, err := off.SearchTargetingEntities(context.Background(), "cncf", model.ProviderLinkedInAds, model.TargetingEntityQuery{Facet: "skills", Query: "go"}); called || !errors.Is(err, domain.ErrConnectionNotUsable) {
		t.Errorf("inactive connection = %v (called %v), want ErrConnectionNotUsable", err, called)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("%d requests for refused searches", n)
	}

	expiredSrv, expiredCalls, _ := linkedInTypeaheadServer(t, http.StatusUnauthorized, `{"serviceErrorCode":65601,"status":401}`)
	expired := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{}, linkedin.WithBaseURL(expiredSrv.URL))
	_, called, err := expired.SearchTargetingEntities(context.Background(), "cncf", model.ProviderLinkedInAds, model.TargetingEntityQuery{Facet: "skills", Query: "go"})
	if !called || expiredCalls.Load() == 0 || !errors.Is(err, domain.ErrConnectionNotUsable) {
		t.Errorf("a 401 = %v (called %v), want a connection defect", err, called)
	}

	// An upstream failure is not cached: the next search asks again.
	failSrv, failCalls, _ := linkedInTypeaheadServer(t, http.StatusInternalServerError, `{}`)
	fail := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{}, linkedin.WithBaseURL(failSrv.URL))
	for i := 0; i < 2; i++ {
		if _, called, err := fail.SearchTargetingEntities(context.Background(), "cncf", model.ProviderLinkedInAds, model.TargetingEntityQuery{Facet: "skills", Query: "go"}); !called || err == nil {
			t.Errorf("attempt %d: a 500 = %v (called %v), want an upstream failure", i, err, called)
		}
	}
	if n := failCalls.Load(); n != 2 {
		t.Errorf("%d requests, want 2: a failure must not be cached", n)
	}
}

func TestTargetingEntityCache_ExpiresAndBounds(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := newTargetingEntityCache()
	c.now = func() time.Time { return now }
	c.put("k", []model.TargetingEntity{})
	rows, ok := c.get("k")
	if !ok || rows == nil {
		t.Fatalf("an empty answer must be cached as a non-nil empty slice: %v %v", rows, ok)
	}
	now = now.Add(targetingEntityTTL)
	if _, ok := c.get("k"); ok {
		t.Error("an entry at its TTL must expire")
	}
	for i := 0; i < targetingEntityMaxEntries+10; i++ {
		c.put(strings.Repeat("x", i+1), nil)
	}
	if n := len(c.entries); n > targetingEntityMaxEntries {
		t.Errorf("cache holds %d entries, cap %d", n, targetingEntityMaxEntries)
	}
	var nilCache *targetingEntityCache
	nilCache.put("k", nil)
	if _, ok := nilCache.get("k"); ok {
		t.Error("a nil cache caches nothing")
	}
}
