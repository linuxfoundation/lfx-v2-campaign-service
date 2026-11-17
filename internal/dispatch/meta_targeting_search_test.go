// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
)

// metaSearchFake answers Targeting Search requests and counts them by type.
type metaSearchFake struct {
	*httptest.Server
	mu     sync.Mutex
	byType map[string]int
}

func newMetaSearchFake(t *testing.T) *metaSearchFake {
	t.Helper()
	f := &metaSearchFake{byType: map[string]int{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.byType[r.URL.Query().Get("type")]++
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/search" {
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.URL.Query().Get("type") {
		case "adTargetingCategory":
			_, _ = io.WriteString(w, `{"data":[{"id":6002714895372,"name":"Frequent Travelers","type":"behaviors","path":["Travel"]},{"id":6002714898572,"name":"Small business owners","type":"behaviors"}]}`)
		default:
			_, _ = io.WriteString(w, `{"data":[{"id":6003107902433,"name":"Kubernetes","audience_size_lower_bound":10,"audience_size_upper_bound":20}]}`)
		}
	}))
	return f
}

func (f *metaSearchFake) count(typ string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.byType[typ]
}

// TestMetaDispatcher_SearchMetaTargeting_CachesAfterResolution: an interest search reaches Meta once
// and a repeat (any case) is a cache hit; behaviors are one browse filtered per query; called tracks
// whether Meta was reached; the cache expires.
func TestMetaDispatcher_SearchMetaTargeting_CachesAfterResolution(t *testing.T) {
	f := newMetaSearchFake(t)
	defer f.Close()
	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{}, meta.WithBaseURL(f.URL))
	now := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	d.searchCache().now = func() time.Time { return now }
	ctx := context.Background()

	rows, called, err := d.SearchMetaTargeting(ctx, "cncf", model.ProviderMetaAds, model.MetaTargetingQuery{Kind: "interest", Query: " Kube "})
	if err != nil || !called || len(rows) != 1 || rows[0].ID != "6003107902433" || *rows[0].AudienceUpper != 20 {
		t.Fatalf("first interest search = %+v, %v, %v", rows, called, err)
	}
	if _, called, err := d.SearchMetaTargeting(ctx, "cncf", model.ProviderMetaAds, model.MetaTargetingQuery{Kind: "interest", Query: "KUBE"}); err != nil || called {
		t.Fatalf("repeat = called %v, %v; want a cache hit", called, err)
	}
	if n := f.count("adinterest"); n != 1 {
		t.Fatalf("adinterest requests = %d, want 1", n)
	}

	travel, called, err := d.SearchMetaTargeting(ctx, "cncf", model.ProviderMetaAds, model.MetaTargetingQuery{Kind: "behavior", Query: "travel"})
	if err != nil || !called || len(travel) != 1 || travel[0].Name != "Frequent Travelers" || travel[0].Path[0] != "Travel" {
		t.Fatalf("behavior search = %+v, %v, %v", travel, called, err)
	}
	owners, called, err := d.SearchMetaTargeting(ctx, "cncf", model.ProviderMetaAds, model.MetaTargetingQuery{Kind: "behavior", Query: "owners"})
	if err != nil || called || len(owners) != 1 || owners[0].ID != "6002714898572" {
		t.Fatalf("second behavior search = %+v, %v, %v; want the cached browse filtered", owners, called, err)
	}
	if n := f.count("adTargetingCategory"); n != 1 {
		t.Fatalf("behavior browses = %d, want 1", n)
	}
	if none, _, err := d.SearchMetaTargeting(ctx, "cncf", model.ProviderMetaAds, model.MetaTargetingQuery{Kind: "behavior", Query: "zzz"}); err != nil || none == nil || len(none) != 0 {
		t.Fatalf("no match = %#v, %v; want empty non-nil", none, err)
	}

	now = now.Add(metaTargetingTTL)
	if _, called, err := d.SearchMetaTargeting(ctx, "cncf", model.ProviderMetaAds, model.MetaTargetingQuery{Kind: "interest", Query: "kube"}); err != nil || !called {
		t.Fatalf("after the TTL = called %v, %v; want Meta reached again", called, err)
	}
}

// TestMetaDispatcher_SearchMetaTargeting_RefusesLocally: a bad kind or query is
// ErrMetaTargetingSearchInvalid and a missing connection its own error, all without reaching Meta.
func TestMetaDispatcher_SearchMetaTargeting_RefusesLocally(t *testing.T) {
	f := newMetaSearchFake(t)
	defer f.Close()
	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{}, meta.WithBaseURL(f.URL))
	for _, q := range []model.MetaTargetingQuery{{Kind: "interests", Query: "go"}, {Kind: "interest", Query: "g"}, {Kind: "interest", Query: "go\x00"}} {
		if _, called, err := d.SearchMetaTargeting(context.Background(), "cncf", model.ProviderMetaAds, q); called || !errors.Is(err, domain.ErrMetaTargetingSearchInvalid) {
			t.Errorf("%+v: called %v err %v, want a local ErrMetaTargetingSearchInvalid", q, called, err)
		}
	}
	gone := NewMetaDispatcher(fakeConnReader{err: domain.ErrNotFound}, identityEncryptor{}, meta.WithBaseURL(f.URL))
	if _, called, err := gone.SearchMetaTargeting(context.Background(), "cncf", model.ProviderMetaAds, model.MetaTargetingQuery{Kind: "interest", Query: "go"}); called || err == nil {
		t.Errorf("no connection: called %v err %v, want a local error", called, err)
	}
	if n := f.count("adinterest"); n != 0 {
		t.Errorf("requests = %d, want none", n)
	}
}

// TestMetaDispatcher_SearchMetaTargeting_FailureIsNotCached: an upstream failure reaches the caller
// with called true and is not cached.
func TestMetaDispatcher_SearchMetaTargeting_FailureIsNotCached(t *testing.T) {
	var n int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		n++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"slow down","code":4}}`)
	}))
	defer srv.Close()
	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{}, meta.WithBaseURL(srv.URL))
	for i := 0; i < 2; i++ {
		if _, called, err := d.SearchMetaTargeting(context.Background(), "cncf", model.ProviderMetaAds, model.MetaTargetingQuery{Kind: "work_position", Query: "engineer"}); !called || err == nil {
			t.Fatalf("attempt %d: called %v err %v, want an upstream error", i, called, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if n != 2 {
		t.Errorf("requests = %d, want one per attempt (sent once, never cached)", n)
	}
}
