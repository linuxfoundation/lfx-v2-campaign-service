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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

var (
	_ service.GeoTargetSuggester     = (*GoogleAdsDispatcher)(nil)
	_ service.UserListReader         = (*GoogleAdsDispatcher)(nil)
	_ service.ConversionActionReader = (*GoogleAdsDispatcher)(nil)
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

// A project with no connection of its own reads the LF system account, exactly as its create
// would, so the lists it is offered are on the account the create uses (the route is
// global_marketing_ops only, so the LF account's lists are what its callers need).
func TestGoogleAdsListUserLists_UsesTheSystemFallback(t *testing.T) {
	rec := &pathRecorder{}
	opts, _ := keywordActionServers(t, func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.URL.Path)
		_, _ = io.WriteString(w, `{"results":[{"userList":{"resourceName":"customers/5555555555/userLists/7","id":"7","name":"HubSpot CRM","type":"CRM_BASED","membershipStatus":"OPEN"}}]}`)
	})
	d := NewGoogleAdsDispatcher(splitConnReader{system: googleAdsConnFor("5555555555")}, identityEncryptor{}, opts...)

	res, err := d.ListUserLists(context.Background(), "proj-1", model.ProviderGoogleAds, "")
	if err != nil {
		t.Fatalf("ListUserLists through the system fallback: %v", err)
	}
	if res.AccountID != "5555555555" || len(res.Lists) != 1 || res.Lists[0].ResourceName != "customers/5555555555/userLists/7" {
		t.Errorf("res = %+v, want the LF system account's list", res)
	}
	if got := rec.all(); len(got) != 1 || !strings.Contains(got[0], "/customers/5555555555/googleAds:search") {
		t.Errorf("paths = %v", got)
	}
}

// While the deployment forces the LF system account, a create ignores the project's own
// connection, so this read must too.
func TestGoogleAdsListUserLists_FollowsTheForcedSystemAccount(t *testing.T) {
	rec := &pathRecorder{}
	opts, _ := keywordActionServers(t, func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.URL.Path)
		_, _ = io.WriteString(w, `{"results":[]}`)
	})
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")
	d := NewGoogleAdsDispatcher(splitConnReader{own: googleAdsConnFor("1234567890"), system: googleAdsConnFor("5555555555")}, identityEncryptor{}, opts...)
	res, err := d.ListUserLists(context.Background(), "proj-1", model.ProviderGoogleAds, "")
	if err != nil || res.AccountID != "5555555555" || res.Lists == nil {
		t.Fatalf("res=%+v err=%v, want the forced system account and a non-nil empty list", res, err)
	}
	if got := rec.all(); len(got) != 1 || !strings.Contains(got[0], "/customers/5555555555/") {
		t.Errorf("paths = %v", got)
	}
}

// No connection anywhere is ErrNotFound (404), and Google is not contacted.
func TestGoogleAdsListUserLists_NoConnectionAnywhereIsNotFound(t *testing.T) {
	opts, reached := keywordActionServers(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, userListBody) })
	_, err := NewGoogleAdsDispatcher(splitConnReader{}, identityEncryptor{}, opts...).ListUserLists(context.Background(), "proj-1", model.ProviderGoogleAds, "")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if reached.Load() {
		t.Error("Google was contacted with no connection")
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

// On the shared customer the read is no longer refused (the route is global_marketing_ops
// only): through the orchestrator, a project whose own connection points at the shared customer
// gets that customer's lists.
func TestGoogleAdsUserLists_SharedCustomerIsListedThroughTheOrchestrator(t *testing.T) {
	rec := &pathRecorder{}
	opts, _ := keywordActionServers(t, func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.URL.Path)
		_, _ = io.WriteString(w, userListBody)
	})
	repo := &scopedConnReader{rows: map[string]*model.Connection{"proj-a": activeGoogleAdsConn(goodGoogleAdsCreds)}}
	d := NewGoogleAdsDispatcher(repo, identityEncryptor{}, opts...)
	orch := service.NewOrchestrator(nil, nil, map[model.Provider]service.PlatformDispatcher{model.ProviderGoogleAds: d})
	res, err := orch.ReadUserLists(context.Background(), "proj-a", model.ProviderGoogleAds, "")
	if err != nil {
		t.Fatalf("ReadUserLists on the shared customer: %v", err)
	}
	if res.AccountID != "1234567890" || len(res.Lists) != 2 {
		t.Errorf("res = %+v, want the shared customer's two lists", res)
	}
	if got := rec.all(); len(got) != 1 {
		t.Errorf("paths = %v, want one search", got)
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

const conversionActionBody = `{"results":[` +
	`{"conversionAction":{"resourceName":"customers/1234567890/conversionActions/111","id":"111","name":"Event Registrations","status":"ENABLED","type":"WEBPAGE","category":"SIGNUP","primaryForGoal":true}}` +
	`]}`

// splitConnReader holds a project's own Google Ads connection and the LF system row, so a test
// can tell which of the two a read resolved.
type splitConnReader struct{ own, system *model.Connection }

func (r splitConnReader) Get(_ context.Context, projectID string, _ model.Provider) (*model.Connection, error) {
	c := r.own
	if projectID == model.SystemProjectID {
		c = r.system
	}
	if c == nil {
		return nil, domain.ErrNotFound
	}
	return c, nil
}

func (splitConnReader) Disconnected(context.Context, string, model.Provider) (bool, error) {
	return false, nil
}

// googleAdsConnFor is activeGoogleAdsConn bound to accountID.
func googleAdsConnFor(accountID string) *model.Connection {
	c := activeGoogleAdsConn(goodGoogleAdsCreds)
	c.AccountID = accountID
	return c
}

// The read is one googleAds:search on the connection's account, filtered to ENABLED actions,
// and nothing else is sent: no mutate, no other path.
func TestGoogleAdsListConversionActions_OneSearchOnTheConnectionsAccount(t *testing.T) {
	var (
		mu       sync.Mutex
		requests []string
		queries  []string
	)
	opts, _ := keywordActionServers(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(b, &req)
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		queries = append(queries, req.Query)
		mu.Unlock()
		_, _ = io.WriteString(w, conversionActionBody)
	})
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	res, called, err := d.ListConversionActions(context.Background(), "proj-1", model.ProviderGoogleAds)
	if err != nil || !called {
		t.Fatalf("ListConversionActions: err=%v called=%v, want success with Google contacted", err, called)
	}
	if res.AccountID != "1234567890" || len(res.Actions) != 1 {
		t.Fatalf("res = %+v", res)
	}
	want := model.ConversionAction{ID: "111", ResourceName: "customers/1234567890/conversionActions/111", Name: "Event Registrations",
		Status: "ENABLED", Type: "WEBPAGE", Category: "SIGNUP", PrimaryForGoal: true}
	if res.Actions[0] != want {
		t.Errorf("action = %+v, want %+v", res.Actions[0], want)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 1 || !strings.HasPrefix(requests[0], http.MethodPost+" ") || !strings.HasSuffix(requests[0], "/customers/1234567890/googleAds:search") {
		t.Fatalf("requests = %v, want exactly one googleAds:search on customer 1234567890", requests)
	}
	if !strings.Contains(queries[0], "FROM conversion_action WHERE conversion_action.status = 'ENABLED'") {
		t.Errorf("query = %q, want the ENABLED-only conversion_action search", queries[0])
	}
}

// A project with no connection of its own reads the LF system account, exactly as its create
// would, so the ids it is offered are ones that create accepts.
func TestGoogleAdsListConversionActions_UsesTheSystemFallback(t *testing.T) {
	rec := &pathRecorder{}
	opts, _ := keywordActionServers(t, func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.URL.Path)
		_, _ = io.WriteString(w, `{"results":[]}`)
	})
	d := NewGoogleAdsDispatcher(splitConnReader{system: googleAdsConnFor("5555555555")}, identityEncryptor{}, opts...)

	res, _, err := d.ListConversionActions(context.Background(), "proj-1", model.ProviderGoogleAds)
	if err != nil {
		t.Fatalf("ListConversionActions through the system fallback: %v", err)
	}
	if res.AccountID != "5555555555" || res.Actions == nil || len(res.Actions) != 0 {
		t.Errorf("res = %#v, want the system account and a non-nil empty list", res)
	}
	if got := rec.all(); len(got) != 1 || !strings.Contains(got[0], "/customers/5555555555/googleAds:search") {
		t.Errorf("paths = %v", got)
	}
}

// While the deployment forces the LF system account, a create ignores the project's own
// connection, so this read must too: it resolves like a create, not like a read of an
// existing campaign.
func TestGoogleAdsListConversionActions_FollowsTheForcedSystemAccount(t *testing.T) {
	rec := &pathRecorder{}
	opts, _ := keywordActionServers(t, func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.URL.Path)
		_, _ = io.WriteString(w, `{"results":[]}`)
	})
	// Set before the dispatcher is built: credsSource reads the flag at construction.
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")
	d := NewGoogleAdsDispatcher(splitConnReader{own: googleAdsConnFor("1234567890"), system: googleAdsConnFor("5555555555")}, identityEncryptor{}, opts...)

	res, _, err := d.ListConversionActions(context.Background(), "proj-1", model.ProviderGoogleAds)
	if err != nil {
		t.Fatalf("ListConversionActions: %v", err)
	}
	if res.AccountID != "5555555555" {
		t.Errorf("account = %q, want the forced system account 5555555555", res.AccountID)
	}
	if got := rec.all(); len(got) != 1 || !strings.Contains(got[0], "/customers/5555555555/") {
		t.Errorf("paths = %v", got)
	}
}

// Local refusals never reach Google and report called=false: no connection anywhere is
// ErrNotFound (404), and a project's own stored account id that is not digits only is a
// connection to fix (400), not an upstream failure.
func TestGoogleAdsListConversionActions_LocalRefusals(t *testing.T) {
	opts, reached := keywordActionServers(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, conversionActionBody) })

	_, called, err := NewGoogleAdsDispatcher(splitConnReader{}, identityEncryptor{}, opts...).
		ListConversionActions(context.Background(), "proj-1", model.ProviderGoogleAds)
	if !errors.Is(err, domain.ErrNotFound) || called {
		t.Errorf("no connection: err=%v called=%v, want ErrNotFound and called=false", err, called)
	}

	_, called, err = NewGoogleAdsDispatcher(fakeConnReader{conn: googleAdsConnFor("123-456-7890")}, identityEncryptor{}, opts...).
		ListConversionActions(context.Background(), "proj-1", model.ProviderGoogleAds)
	if !errors.Is(err, domain.ErrConnectionNotUsable) || !errors.Is(err, domain.ErrProviderConfigInvalid) || called {
		t.Errorf("dashed account id: err=%v called=%v, want ErrConnectionNotUsable and ErrProviderConfigInvalid, called=false", err, called)
	}
	if errors.Is(err, domain.ErrSystemConnectionNotUsable) {
		t.Errorf("dashed account id on the project's OWN row: err = %v, must not be tagged as the LF system row's defect", err)
	}

	if reached.Load() {
		t.Error("Google was contacted for a locally refused read")
	}
}

// An upstream failure is returned as one, never as a connection defect, so the service maps it
// to 503.
func TestGoogleAdsListConversionActions_UpstreamFailureIsNotAConnectionDefect(t *testing.T) {
	opts, _ := keywordActionServers(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":400,"message":"bad","status":"INVALID_ARGUMENT"}}`)
	})
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	_, called, err := d.ListConversionActions(context.Background(), "proj-1", model.ProviderGoogleAds)
	if err == nil || !called {
		t.Fatalf("err=%v called=%v, want an error with Google contacted", err, called)
	}
	if errors.Is(err, domain.ErrConnectionNotUsable) || errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want an upstream error, not a local connection state", err)
	}
}

// A non-digit account id on the LF system row a project fell back to is the LF row's defect, not
// the project's: it carries ErrSystemConnectionNotUsable, which the service answers 500 (paging
// the operator) rather than the 400 that would tell the project to edit a connection it lacks.
func TestGoogleAdsListConversionActions_BadSystemRowAccountIsTheSystemRowsDefect(t *testing.T) {
	opts, reached := keywordActionServers(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, conversionActionBody) })
	d := NewGoogleAdsDispatcher(splitConnReader{system: googleAdsConnFor("123-456-7890")}, identityEncryptor{}, opts...)

	_, called, err := d.ListConversionActions(context.Background(), "proj-1", model.ProviderGoogleAds)
	if !errors.Is(err, domain.ErrSystemConnectionNotUsable) || !errors.Is(err, domain.ErrProviderConfigInvalid) || called {
		t.Errorf("err=%v called=%v, want ErrSystemConnectionNotUsable and ErrProviderConfigInvalid, called=false", err, called)
	}
	if reached.Load() {
		t.Error("Google was contacted for a locally refused read")
	}
}

var _ service.AudienceReader = (*GoogleAdsDispatcher)(nil)

// The audience read is one googleAds:search on the connection's account, for the kind asked,
// and nothing else is sent; the kind and account are echoed on the result.
func TestGoogleAdsListAudiences_OneSearchOnTheConnectionsAccount(t *testing.T) {
	var (
		mu       sync.Mutex
		requests []string
		queries  []string
	)
	opts, _ := keywordActionServers(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(b, &req)
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		queries = append(queries, req.Query)
		mu.Unlock()
		_, _ = io.WriteString(w, `{"results":[{"customAudience":{"resourceName":"customers/1234567890/customAudiences/9","id":"9","name":"KubeCon","type":"SEARCH","status":"ENABLED"}}]}`)
	})
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	res, called, err := d.ListAudiences(context.Background(), "proj-1", model.ProviderGoogleAds, "custom_audience")
	if err != nil || !called {
		t.Fatalf("ListAudiences: err=%v called=%v, want success with Google contacted", err, called)
	}
	want := model.AudienceResource{ResourceName: "customers/1234567890/customAudiences/9", ID: "9", Name: "KubeCon", Subtype: "SEARCH", Status: "ENABLED"}
	if res.AccountID != "1234567890" || res.Kind != "custom_audience" || len(res.Audiences) != 1 || res.Audiences[0] != want {
		t.Fatalf("res = %+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 1 || !strings.HasPrefix(requests[0], http.MethodPost+" ") || !strings.HasSuffix(requests[0], "/customers/1234567890/googleAds:search") {
		t.Fatalf("requests = %v, want exactly one googleAds:search on customer 1234567890", requests)
	}
	if !strings.Contains(queries[0], "FROM custom_audience") {
		t.Errorf("query = %q, want the custom_audience search", queries[0])
	}
}

// Resolution is the create's: the LF system fallback for a project with no connection, and
// the forced system account over the project's own row.
func TestGoogleAdsListAudiences_ResolvesLikeACreate(t *testing.T) {
	rec := &pathRecorder{}
	opts, _ := keywordActionServers(t, func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.URL.Path)
		_, _ = io.WriteString(w, `{"results":[]}`)
	})
	d := NewGoogleAdsDispatcher(splitConnReader{system: googleAdsConnFor("5555555555")}, identityEncryptor{}, opts...)
	res, _, err := d.ListAudiences(context.Background(), "proj-1", model.ProviderGoogleAds, "audience")
	if err != nil {
		t.Fatalf("ListAudiences through the system fallback: %v", err)
	}
	if res.AccountID != "5555555555" || res.Audiences == nil || len(res.Audiences) != 0 {
		t.Errorf("res = %#v, want the system account and a non-nil empty list", res)
	}

	t.Setenv(constants.EnvForceSystemAdsAccount, "true")
	d = NewGoogleAdsDispatcher(splitConnReader{own: googleAdsConnFor("1234567890"), system: googleAdsConnFor("5555555555")}, identityEncryptor{}, opts...)
	res, _, err = d.ListAudiences(context.Background(), "proj-1", model.ProviderGoogleAds, "audience")
	if err != nil || res.AccountID != "5555555555" {
		t.Errorf("forced system: res=%+v err=%v, want the system account 5555555555", res, err)
	}
	for _, p := range rec.all() {
		if !strings.Contains(p, "/customers/5555555555/googleAds:search") {
			t.Errorf("path %s, want only the system account searched", p)
		}
	}
}

// Local refusals never reach Google and report called=false: an unknown kind (before any
// credential), no connection anywhere, and a stored account id that is not digits only.
func TestGoogleAdsListAudiences_LocalRefusals(t *testing.T) {
	opts, reached := keywordActionServers(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) })

	_, called, err := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...).
		ListAudiences(context.Background(), "proj-1", model.ProviderGoogleAds, "user_list")
	if !errors.Is(err, domain.ErrTargetingLookupUnsupported) || called {
		t.Errorf("unknown kind: err=%v called=%v, want ErrTargetingLookupUnsupported and called=false", err, called)
	}

	_, called, err = NewGoogleAdsDispatcher(splitConnReader{}, identityEncryptor{}, opts...).
		ListAudiences(context.Background(), "proj-1", model.ProviderGoogleAds, "audience")
	if !errors.Is(err, domain.ErrNotFound) || called {
		t.Errorf("no connection: err=%v called=%v, want ErrNotFound and called=false", err, called)
	}

	_, called, err = NewGoogleAdsDispatcher(fakeConnReader{conn: googleAdsConnFor("123-456-7890")}, identityEncryptor{}, opts...).
		ListAudiences(context.Background(), "proj-1", model.ProviderGoogleAds, "audience")
	if !errors.Is(err, domain.ErrConnectionNotUsable) || !errors.Is(err, domain.ErrProviderConfigInvalid) || called {
		t.Errorf("dashed account id: err=%v called=%v, want ErrConnectionNotUsable and ErrProviderConfigInvalid, called=false", err, called)
	}

	if reached.Load() {
		t.Error("Google was contacted for a locally refused read")
	}
}

// user_interest is cached per account: a second read is served without Google (called=false)
// and returns an independent copy; the account kinds are never cached; an expired entry is
// re-read; and a local refusal still wins over a cached answer.
func TestGoogleAdsListAudiences_CachesUserInterestsOnly(t *testing.T) {
	rec := &pathRecorder{}
	opts, _ := keywordActionServers(t, func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.URL.Path)
		_, _ = io.WriteString(w, `{"results":[{"userInterest":{"resourceName":"customers/1234567890/userInterests/80276","name":"Software","taxonomyType":"IN_MARKET"}},`+
			`{"audience":{"resourceName":"customers/1234567890/audiences/5","name":"A","status":"ENABLED"}}]}`)
	})
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	d.interests.now = func() time.Time { return now }
	ctx := context.Background()

	first, called, err := d.ListAudiences(ctx, "proj-1", model.ProviderGoogleAds, "user_interest")
	if err != nil || !called || len(first.Audiences) != 1 {
		t.Fatalf("first read: res=%+v called=%v err=%v", first, called, err)
	}
	first.Audiences[0].Name = "mutated"
	second, called, err := d.ListAudiences(ctx, "proj-1", model.ProviderGoogleAds, "user_interest")
	if err != nil || called || len(second.Audiences) != 1 || second.Audiences[0].Name != "Software" || second.AccountID != "1234567890" {
		t.Fatalf("cached read: res=%+v called=%v err=%v, want the cached copy without Google", second, called, err)
	}
	second.Audiences[0].Name = "mutated"
	if third, _, _ := d.ListAudiences(ctx, "proj-1", model.ProviderGoogleAds, "user_interest"); third.Audiences[0].Name != "Software" {
		t.Fatalf("a caller's edit reached the cache: %+v", third.Audiences)
	}
	if n := len(rec.all()); n != 1 {
		t.Fatalf("%d searches after a cache hit, want 1", n)
	}

	for i := 0; i < 2; i++ {
		if _, called, err := d.ListAudiences(ctx, "proj-1", model.ProviderGoogleAds, "audience"); err != nil || !called {
			t.Fatalf("audience read %d: called=%v err=%v, want every account-kind read to reach Google", i, called, err)
		}
	}
	if n := len(rec.all()); n != 3 {
		t.Fatalf("%d searches, want 3 (account kinds are not cached)", n)
	}

	now = now.Add(userInterestTTL)
	if _, called, err := d.ListAudiences(ctx, "proj-1", model.ProviderGoogleAds, "user_interest"); err != nil || !called {
		t.Errorf("after the TTL: called=%v err=%v, want a fresh read", called, err)
	}

	d.creds = newCredsSource(splitConnReader{}, identityEncryptor{})
	if _, called, err := d.ListAudiences(ctx, "proj-1", model.ProviderGoogleAds, "user_interest"); !errors.Is(err, domain.ErrNotFound) || called {
		t.Errorf("no connection with a warm cache: called=%v err=%v, want ErrNotFound", called, err)
	}
}

// An upstream failure is returned as one, with called=true, never as a connection defect.
func TestGoogleAdsListAudiences_UpstreamFailureIsNotAConnectionDefect(t *testing.T) {
	opts, _ := keywordActionServers(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":400,"message":"bad","status":"INVALID_ARGUMENT"}}`)
	})
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	_, called, err := d.ListAudiences(context.Background(), "proj-1", model.ProviderGoogleAds, "user_interest")
	if err == nil || !called {
		t.Fatalf("err=%v called=%v, want an error with Google contacted", err, called)
	}
	if errors.Is(err, domain.ErrConnectionNotUsable) || errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want an upstream error, not a local connection state", err)
	}
}
