// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// audienceSearchServer answers every googleAds:search with body and records each request's
// method, path and GAQL query.
func audienceSearchServer(t *testing.T, body string) (*httptest.Server, func() (paths, queries []string)) {
	t.Helper()
	var (
		mu      sync.Mutex
		paths   []string
		queries []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAccountsToken(w, r) {
			return
		}
		var req searchRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		queries = append(queries, req.Query)
		mu.Unlock()
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, func() ([]string, []string) {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...), append([]string(nil), queries...)
	}
}

// Each kind is one googleAds:search on the client's customer, selecting FROM its own resource
// with the usable-only filters, and its rows decode into the picker fields, sorted by name
// ignoring case.
func TestListAudiences_QueryAndParsePerKind(t *testing.T) {
	cases := []struct {
		kind      AudienceKind
		body      string
		wantQuery []string
		banned    []string
		want      []Audience
	}{
		{
			kind: AudienceKindCustomAudience,
			body: `{"results":[` +
				`{"customAudience":{"resourceName":"customers/1234567890/customAudiences/2","id":"2","name":"Zeta","type":"SEARCH","status":"ENABLED"}},` +
				`{"customAudience":{"resourceName":"customers/1234567890/customAudiences/1","id":"1","name":"alpha","type":"PURCHASE_INTENT","status":"ENABLED","description":"d"}}` +
				`]}`,
			wantQuery: []string{"FROM custom_audience", "custom_audience.type", "custom_audience.description", "WHERE custom_audience.status = 'ENABLED'"},
			want: []Audience{
				{ResourceName: "customers/1234567890/customAudiences/1", ID: "1", Name: "alpha", Subtype: "PURCHASE_INTENT", Status: "ENABLED", Description: "d"},
				{ResourceName: "customers/1234567890/customAudiences/2", ID: "2", Name: "Zeta", Subtype: "SEARCH", Status: "ENABLED"},
			},
		},
		{
			kind:      AudienceKindCombinedAudience,
			body:      `{"results":[{"combinedAudience":{"resourceName":"customers/1234567890/combinedAudiences/7","id":"7","name":"Both","status":"ENABLED","description":"x"}}]}`,
			wantQuery: []string{"FROM combined_audience", "WHERE combined_audience.status = 'ENABLED'"},
			want:      []Audience{{ResourceName: "customers/1234567890/combinedAudiences/7", ID: "7", Name: "Both", Status: "ENABLED", Description: "x"}},
		},
		{
			kind:      AudienceKindAudience,
			body:      `{"results":[{"audience":{"resourceName":"customers/1234567890/audiences/130134698","id":"130134698","name":"KubeCon EU","status":"ENABLED"}}]}`,
			wantQuery: []string{"FROM audience", "WHERE audience.status = 'ENABLED'", "audience.scope = 'CUSTOMER'"},
			want:      []Audience{{ResourceName: "customers/1234567890/audiences/130134698", ID: "130134698", Name: "KubeCon EU", Status: "ENABLED"}},
		},
		{
			kind: AudienceKindUserInterest,
			// A status Google never sends for user_interest is still not reported.
			body:      `{"results":[{"userInterest":{"resourceName":"customers/1234567890/userInterests/80276","userInterestId":"80276","name":"Software","taxonomyType":"IN_MARKET","status":"ENABLED"}}]}`,
			wantQuery: []string{"FROM user_interest", "user_interest.taxonomy_type IN ('AFFINITY', 'IN_MARKET')"},
			// launched_to_all is false on every AFFINITY and IN_MARKET row of the LF customer.
			banned: []string{"launched_to_all"},
			want:   []Audience{{ResourceName: "customers/1234567890/userInterests/80276", ID: "80276", Name: "Software", Subtype: "IN_MARKET"}},
		},
		{
			// #517: the legacy custom affinity segment customAffinities takes. Only CUSTOM_AFFINITY
			// is listed (Google refuses a CUSTOM_INTENT one in a customAffinity criterion); the
			// subtype is custom_interest.type, verbatim.
			kind: AudienceKindCustomInterest,
			body: `{"results":[` +
				`{"customInterest":{"resourceName":"customers/1234567890/customInterests/2808384","id":"2808384","name":"OSSNA - Cloud Native","type":"CUSTOM_AFFINITY","status":"ENABLED"}},` +
				`{"customInterest":{"resourceName":"customers/1234567890/customInterests/2913421","id":"2913421","name":"Google & Kubernetes","type":"CUSTOM_AFFINITY","status":"ENABLED","description":"d"}}` +
				`]}`,
			wantQuery: []string{"FROM custom_interest", "custom_interest.type", "custom_interest.description", "WHERE custom_interest.status = 'ENABLED'", "custom_interest.type = 'CUSTOM_AFFINITY'", "ORDER BY custom_interest.id"},
			want: []Audience{
				{ResourceName: "customers/1234567890/customInterests/2913421", ID: "2913421", Name: "Google & Kubernetes", Subtype: "CUSTOM_AFFINITY", Status: "ENABLED", Description: "d"},
				{ResourceName: "customers/1234567890/customInterests/2808384", ID: "2808384", Name: "OSSNA - Cloud Native", Subtype: "CUSTOM_AFFINITY", Status: "ENABLED"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			srv, seen := audienceSearchServer(t, tc.body)
			got, err := newConversionTestClient(t, srv).ListAudiences(context.Background(), tc.kind)
			if err != nil {
				t.Fatalf("ListAudiences: %v", err)
			}
			paths, queries := seen()
			if len(paths) != 1 || paths[0] != "POST /v23/customers/1234567890/googleAds:search" {
				t.Fatalf("requests = %v, want exactly one googleAds:search on the client's customer", paths)
			}
			for _, f := range tc.wantQuery {
				if !strings.Contains(queries[0], f) {
					t.Errorf("query %q is missing %s", queries[0], f)
				}
			}
			for _, f := range tc.banned {
				if strings.Contains(queries[0], f) {
					t.Errorf("query %q must not filter on %s", queries[0], f)
				}
			}
			if !strings.Contains(queries[0], fmt.Sprintf("LIMIT %d", MaxAudienceScanRows+1)) {
				t.Errorf("query %q is not bounded at MaxAudienceScanRows+1", queries[0])
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("row %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// A row naming another customer, another collection, or a non-canonical id is dropped: the
// create would refuse it.
func TestListAudiences_DropsRowsTheCreateWouldRefuse(t *testing.T) {
	srv, _ := audienceSearchServer(t, `{"results":[`+
		`{"audience":{"resourceName":"customers/9999999999/audiences/1","name":"other customer"}},`+
		`{"audience":{"resourceName":"customers/1234567890/customAudiences/2","name":"wrong collection"}},`+
		`{"audience":{"resourceName":"customers/1234567890/audiences/03","name":"leading zero"}},`+
		`{"audience":{"resourceName":"customers/1234567890/audiences/0","name":"zero"}},`+
		`{"customAudience":{"resourceName":"customers/1234567890/audiences/5","name":"wrong object"}},`+
		`{"audience":{"resourceName":"customers/1234567890/audiences/4","name":"kept"}}`+
		`]}`)
	got, err := newConversionTestClient(t, srv).ListAudiences(context.Background(), AudienceKindAudience)
	if err != nil {
		t.Fatalf("ListAudiences: %v", err)
	}
	if len(got) != 1 || got[0].ID != "4" {
		t.Errorf("got %+v, want only the row with id 4", got)
	}
}

// No entry of the kind is an empty, non-nil list.
func TestListAudiences_EmptyIsAnEmptyList(t *testing.T) {
	srv, _ := audienceSearchServer(t, `{}`)
	got, err := newConversionTestClient(t, srv).ListAudiences(context.Background(), AudienceKindCombinedAudience)
	if err != nil {
		t.Fatalf("ListAudiences: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("got %#v, want a non-nil empty list", got)
	}
}

// More rows than MaxAudienceScanRows fail the read rather than name-searching a partial set.
func TestListAudiences_TooManyRowsFailsTheRead(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"results":[`)
	for i := 0; i <= MaxAudienceScanRows; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"userInterest":{"resourceName":"customers/1234567890/userInterests/%d"}}`, i+1)
	}
	b.WriteString(`]}`)
	srv, _ := audienceSearchServer(t, b.String())
	if _, err := newConversionTestClient(t, srv).ListAudiences(context.Background(), AudienceKindUserInterest); err == nil {
		t.Fatal("want an error for more than MaxAudienceScanRows rows")
	}
}

// A custom_interest row is kept only under customInterests: the same id in another collection,
// or on another customer, is dropped, since customAffinities would refuse it (#517).
func TestListAudiences_CustomInterestDropsOtherCollections(t *testing.T) {
	srv, _ := audienceSearchServer(t, `{"results":[`+
		`{"customInterest":{"resourceName":"customers/1234567890/customAudiences/1","name":"wrong collection","type":"CUSTOM_AFFINITY"}},`+
		`{"customInterest":{"resourceName":"customers/9999999999/customInterests/2","name":"other customer","type":"CUSTOM_AFFINITY"}},`+
		`{"customAudience":{"resourceName":"customers/1234567890/customInterests/3","name":"wrong object","type":"SEARCH"}},`+
		`{"customInterest":{"resourceName":"customers/1234567890/customInterests/4","name":"kept","type":"CUSTOM_AFFINITY","status":"ENABLED"}}`+
		`]}`)
	got, err := newConversionTestClient(t, srv).ListAudiences(context.Background(), AudienceKindCustomInterest)
	if err != nil {
		t.Fatalf("ListAudiences: %v", err)
	}
	if len(got) != 1 || got[0].ID != "4" || got[0].Subtype != "CUSTOM_AFFINITY" {
		t.Errorf("got %+v, want only the customInterests row with id 4", got)
	}
}

// An unknown kind is refused without contacting Google.
func TestListAudiences_UnknownKindSendsNothing(t *testing.T) {
	srv, seen := audienceSearchServer(t, `{}`)
	if _, err := newConversionTestClient(t, srv).ListAudiences(context.Background(), AudienceKind("user_list")); err == nil {
		t.Fatal("want an error for an unknown kind")
	}
	if paths, _ := seen(); len(paths) != 0 {
		t.Errorf("requests = %v, want none", paths)
	}
}

func TestParseAudienceKind(t *testing.T) {
	for _, s := range []string{"custom_audience", "combined_audience", "audience", "user_interest", "custom_interest"} {
		if k, ok := ParseAudienceKind(s); !ok || string(k) != s {
			t.Errorf("ParseAudienceKind(%q) = %q, %v", s, k, ok)
		}
	}
	for _, s := range []string{"", "user_list", "Audience", " audience", "Custom_Interest", "custom_interests"} {
		if _, ok := ParseAudienceKind(s); ok {
			t.Errorf("ParseAudienceKind(%q) accepted", s)
		}
	}
}
