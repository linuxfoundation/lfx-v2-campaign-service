// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

var (
	_ service.GeoTargetSuggester = (*GoogleAdsDispatcher)(nil)
	_ service.UserListReader     = (*GoogleAdsDispatcher)(nil)
)

// pathRecorder records the API paths a fake Google server was asked for.
type pathRecorder struct {
	mu    sync.Mutex
	paths []string
}

func (p *pathRecorder) add(s string) { p.mu.Lock(); p.paths = append(p.paths, s); p.mu.Unlock() }
func (p *pathRecorder) all() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.paths...)
}

const geoSuggestBody = `{"geoTargetConstantSuggestions":[{"geoTargetConstant":{"resourceName":"geoTargetConstants/9031000","id":"9031000","name":"78701","countryCode":"US","targetType":"Postal Code","status":"ENABLED","canonicalName":"78701,Texas,United States"},"reach":"40000"}]}`

// The geo search is public reference data, so a project with no connection of its own reads
// it through the LF system credential, exactly as account discovery does.
func TestGoogleAdsSuggestGeoTargets_UsesTheSystemFallback(t *testing.T) {
	rec := &pathRecorder{}
	opts, _ := keywordActionServers(t, func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.URL.Path)
		_, _ = io.WriteString(w, geoSuggestBody)
	})
	d := NewGoogleAdsDispatcher(ownOnlyConnReader{system: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	rows, _, err := d.SuggestGeoTargets(context.Background(), "proj-1", model.ProviderGoogleAds, model.GeoTargetQuery{Query: "78701", CountryCode: "US"})
	if err != nil {
		t.Fatalf("SuggestGeoTargets through the system fallback: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "9031000" || rows[0].TargetType != "Postal Code" || rows[0].Reach == nil || *rows[0].Reach != 40000 {
		t.Fatalf("rows = %+v", rows)
	}
	if got := rec.all(); len(got) != 1 || !strings.HasSuffix(got[0], "/geoTargetConstants:suggest") {
		t.Errorf("paths = %v, want one geoTargetConstants:suggest call", got)
	}
}

const userListBody = `{"results":[` +
	`{"userList":{"resourceName":"customers/1234567890/userLists/22","id":"22","name":"beta","type":"RULE_BASED","membershipStatus":"OPEN","sizeForSearch":"900","eligibleForSearch":true}},` +
	`{"userList":{"resourceName":"customers/1234567890/userLists/11","id":"11","name":"Alpha","type":"CRM_BASED","membershipStatus":"CLOSED"}}` +
	`]}`

// User lists are account data on a customer every foundation shares: the system fallback is
// refused outright, and Google is never contacted.
func TestGoogleAdsListUserLists_RefusesTheSystemFallback(t *testing.T) {
	opts, reached := keywordActionServers(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, userListBody)
	})
	d := NewGoogleAdsDispatcher(ownOnlyConnReader{system: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	_, err := d.ListUserLists(context.Background(), "proj-1", model.ProviderGoogleAds, "")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound (no connection of the project's own)", err)
	}
	if reached.Load() {
		t.Error("Google was contacted with the LF system credential")
	}
}

func TestGoogleAdsListUserLists_ReadsTheConnectionsAccountByDefault(t *testing.T) {
	rec := &pathRecorder{}
	opts, _ := keywordActionServers(t, func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.URL.Path)
		_, _ = io.WriteString(w, userListBody)
	})
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	res, err := d.ListUserLists(context.Background(), "proj-1", model.ProviderGoogleAds, "")
	if err != nil {
		t.Fatalf("ListUserLists: %v", err)
	}
	if res.AccountID != "1234567890" {
		t.Errorf("account = %q, want the connection's 1234567890", res.AccountID)
	}
	if len(res.Lists) != 2 || res.Lists[0].Name != "Alpha" || res.Lists[1].Name != "beta" {
		t.Fatalf("lists = %+v, want both, sorted by name case-insensitively", res.Lists)
	}
	if !res.Lists[1].EligibleForSearch || res.Lists[1].SizeForSearch == nil || *res.Lists[1].SizeForSearch != 900 {
		t.Errorf("beta = %+v", res.Lists[1])
	}
	if res.Lists[0].EligibleForSearch || res.Lists[0].SizeForSearch != nil {
		t.Errorf("an omitted eligibleForSearch is false and an omitted size is absent: %+v", res.Lists[0])
	}
	if got := rec.all(); len(got) != 1 || !strings.Contains(got[0], "/customers/1234567890/googleAds:search") {
		t.Errorf("paths = %v", got)
	}
}

func TestGoogleAdsListUserLists_ExplicitAccountAndItsGuards(t *testing.T) {
	t.Run("explicit account is read", func(t *testing.T) {
		rec := &pathRecorder{}
		opts, _ := keywordActionServers(t, func(w http.ResponseWriter, r *http.Request) {
			rec.add(r.URL.Path)
			_, _ = io.WriteString(w, `{"results":[{"userList":{"resourceName":"customers/5555555555/userLists/1","id":"1","name":"x","type":"CRM_BASED","membershipStatus":"OPEN"}},{"userList":{"resourceName":"customers/1234567890/userLists/2","id":"2","name":"foreign"}}]}`)
		})
		d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
		res, err := d.ListUserLists(context.Background(), "proj-1", model.ProviderGoogleAds, "5555555555")
		if err != nil {
			t.Fatalf("ListUserLists: %v", err)
		}
		if res.AccountID != "5555555555" || len(res.Lists) != 1 || res.Lists[0].ResourceName != "customers/5555555555/userLists/1" {
			t.Errorf("res = %+v; a row naming another customer must be dropped", res)
		}
		if got := rec.all(); len(got) != 1 || !strings.Contains(got[0], "/customers/5555555555/") {
			t.Errorf("paths = %v", got)
		}
	})
	t.Run("malformed account never resolves a credential", func(t *testing.T) {
		d := NewGoogleAdsDispatcher(fakeConnReader{err: errors.New("must not be consulted")}, identityEncryptor{})
		_, err := d.ListUserLists(context.Background(), "proj-1", model.ProviderGoogleAds, "866-674-6580")
		if !errors.Is(err, domain.ErrAccountIDMalformed) {
			t.Fatalf("err = %v, want ErrAccountIDMalformed", err)
		}
	})
	t.Run("no account anywhere is a not-usable connection", func(t *testing.T) {
		opts, reached := keywordActionServers(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) })
		c := activeGoogleAdsConn(goodGoogleAdsCreds)
		c.AccountID = ""
		d := NewGoogleAdsDispatcher(fakeConnReader{conn: c}, identityEncryptor{}, opts...)
		_, err := d.ListUserLists(context.Background(), "proj-1", model.ProviderGoogleAds, "")
		if !errors.Is(err, domain.ErrConnectionNotUsable) || !errors.Is(err, domain.ErrAccountNotSelected) {
			t.Fatalf("err = %v, want ErrConnectionNotUsable + ErrAccountNotSelected", err)
		}
		if reached.Load() {
			t.Error("Google was contacted with no account to read")
		}
	})
}

// Regression for the cross-tenant read: two projects whose OWN connections point at the same
// shared Google Ads customer. Neither may list user lists through the orchestrator — the read
// is refused (ErrUserListsSharedTenancy) before any credential is used or Google contacted, so
// project A can never see lists project B created.
func TestGoogleAdsUserLists_TwoProjectsOnOneSharedCustomerAreRefused(t *testing.T) {
	opts, reached := keywordActionServers(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, userListBody)
	})
	a := activeGoogleAdsConn(goodGoogleAdsCreds)
	b := activeGoogleAdsConn(goodGoogleAdsCreds) // same customer 1234567890
	repo := &scopedConnReader{rows: map[string]*model.Connection{"proj-a": a, "proj-b": b}}
	d := NewGoogleAdsDispatcher(repo, identityEncryptor{}, opts...)
	orch := service.NewOrchestrator(nil, nil, map[model.Provider]service.PlatformDispatcher{model.ProviderGoogleAds: d})
	for _, p := range []string{"proj-a", "proj-b"} {
		for _, acct := range []string{"", "1234567890"} {
			_, err := orch.ReadUserLists(context.Background(), p, model.ProviderGoogleAds, acct)
			if !errors.Is(err, domain.ErrUserListsSharedTenancy) {
				t.Fatalf("%s/%q: err = %v, want ErrUserListsSharedTenancy", p, acct, err)
			}
		}
	}
	if reached.Load() {
		t.Error("Google was contacted for a shared-customer user-list read")
	}
	if len(repo.gets) != 0 {
		t.Errorf("connections were resolved (%v); the refusal must come first", repo.gets)
	}
}

// A repeated geo search is served from the cache: Google is asked once.
func TestGoogleAdsSuggestGeoTargets_CachesRepeatedSearches(t *testing.T) {
	rec := &pathRecorder{}
	opts, _ := keywordActionServers(t, func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.URL.Path)
		_, _ = io.WriteString(w, geoSuggestBody)
	})
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	for i, q := range []string{"78701", " 78701 ", "78701"} {
		rows, called, err := d.SuggestGeoTargets(context.Background(), "proj-1", model.ProviderGoogleAds, model.GeoTargetQuery{Query: q, CountryCode: "US"})
		if err != nil || len(rows) != 1 {
			t.Fatalf("rows=%v err=%v", rows, err)
		}
		if called != (i == 0) {
			t.Errorf("call %d: called=%v, want %v (only the first reaches Google)", i, called, i == 0)
		}
		rows[0].ID = "mutated" // must not corrupt the cached copy
	}
	if n := len(rec.all()); n != 1 {
		t.Errorf("Google was asked %d times, want 1", n)
	}
	// A different country is a different question.
	if _, _, err := d.SuggestGeoTargets(context.Background(), "proj-1", model.ProviderGoogleAds, model.GeoTargetQuery{Query: "78701", CountryCode: "CA"}); err != nil {
		t.Fatal(err)
	}
	if n := len(rec.all()); n != 2 {
		t.Errorf("Google was asked %d times after a new country, want 2", n)
	}
}

// The cache never answers for a project that resolves no credential: its 404 stands.
func TestGoogleAdsSuggestGeoTargets_CacheDoesNotMaskAMissingConnection(t *testing.T) {
	opts, _ := keywordActionServers(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, geoSuggestBody) })
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	q := model.GeoTargetQuery{Query: "78701"}
	if _, _, err := d.SuggestGeoTargets(context.Background(), "proj-1", model.ProviderGoogleAds, q); err != nil {
		t.Fatal(err)
	}
	d.creds = newCredsSource(ownOnlyConnReader{}, identityEncryptor{})
	if _, _, err := d.SuggestGeoTargets(context.Background(), "proj-2", model.ProviderGoogleAds, q); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound despite a cached answer", err)
	}
}

func TestGeoSuggestCache_ExpiresAndStaysBounded(t *testing.T) {
	c := newGeoSuggestCache()
	now := time.Unix(0, 0)
	c.now = func() time.Time { return now }
	c.put("k", []model.GeoTargetSuggestion{{ID: "1"}})
	if _, ok := c.get("k"); !ok {
		t.Fatal("fresh entry missing")
	}
	now = now.Add(geoSuggestTTL)
	if _, ok := c.get("k"); ok {
		t.Error("expired entry served")
	}
	for i := 0; i < geoSuggestMaxEntries+10; i++ {
		c.put(fmt.Sprint(i), nil)
	}
	if n := len(c.entries); n > geoSuggestMaxEntries {
		t.Errorf("cache holds %d entries, cap is %d", n, geoSuggestMaxEntries)
	}
	var nilCache *geoSuggestCache
	nilCache.put("k", nil)
	if _, ok := nilCache.get("k"); ok {
		t.Error("a nil cache must cache nothing")
	}
}

// A no-match search is cached as an EMPTY slice, never nil: the repeat must still be a 200 with
// [] through the orchestrator, not the nil-result 503.
func TestGoogleAdsSuggestGeoTargets_RepeatedEmptySearchStaysEmpty(t *testing.T) {
	rec := &pathRecorder{}
	opts, _ := keywordActionServers(t, func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.URL.Path)
		_, _ = io.WriteString(w, `{}`)
	})
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	orch := service.NewOrchestrator(nil, nil, map[model.Provider]service.PlatformDispatcher{model.ProviderGoogleAds: d})
	for i := 0; i < 2; i++ {
		rows, err := orch.ReadGeoTargetSuggestions(context.Background(), "proj-1", model.ProviderGoogleAds, model.GeoTargetQuery{Query: "nowhere"})
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if rows == nil || len(rows) != 0 {
			t.Fatalf("call %d: rows = %#v, want a non-nil empty slice", i, rows)
		}
	}
	if n := len(rec.all()); n != 1 {
		t.Errorf("Google was asked %d times, want 1 (the empty answer is cached)", n)
	}
}

// A missing or unusable connection is refused locally: Google is not reached, and the
// dispatcher says so, so no upstream call is recorded for it.
func TestGoogleAdsSuggestGeoTargets_LocalRefusalReportsNotCalled(t *testing.T) {
	opts, reached := keywordActionServers(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, geoSuggestBody) })
	inactive := activeGoogleAdsConn(goodGoogleAdsCreds)
	inactive.Status = model.StatusInactive
	for name, repo := range map[string]connReader{
		"no connection":       ownOnlyConnReader{},
		"unusable connection": fakeConnReader{conn: inactive},
	} {
		d := NewGoogleAdsDispatcher(repo, identityEncryptor{}, opts...)
		_, called, err := d.SuggestGeoTargets(context.Background(), "proj-1", model.ProviderGoogleAds, model.GeoTargetQuery{Query: "78701"})
		if err == nil || called {
			t.Errorf("%s: err=%v called=%v, want a local error and called=false", name, err, called)
		}
	}
	if reached.Load() {
		t.Error("Google was contacted for a locally refused search")
	}
}
