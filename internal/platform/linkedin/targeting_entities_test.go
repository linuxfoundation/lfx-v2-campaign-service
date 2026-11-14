// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func typeaheadServer(t *testing.T, status int, body string, gotQuery *string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/adTargetingEntities" {
			t.Errorf("path %s, want /adTargetingEntities", r.URL.Path)
		}
		if v := r.Header.Get("LinkedIn-Version"); v != apiVersion {
			t.Errorf("LinkedIn-Version %q, want %q", v, apiVersion)
		}
		if gotQuery != nil {
			*gotQuery = r.URL.RawQuery
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestSearchTargetingEntities_RequestShape(t *testing.T) {
	var raw string
	srv, _ := typeaheadServer(t, http.StatusOK, `{"elements":[]}`, &raw)
	c := NewClient(Credentials{AccessToken: "t"}, RuntimeConfig{}, WithBaseURL(srv.URL))

	got, dropped, err := c.SearchTargetingEntities(context.Background(), "skills", "  C++ (dev) & R&D, 50%  ")
	if err != nil {
		t.Fatalf("SearchTargetingEntities: %v", err)
	}
	if got == nil || len(got) != 0 || dropped != 0 {
		t.Errorf("an empty answer = %v (dropped %d), want a non-nil empty slice", got, dropped)
	}
	// Keys sorted by buildRawQuery. The query is trimmed, every reserved character is
	// percent-encoded (so & ( , cannot split the parameter or break the Rest.li parser), and a
	// space is %20, never "+".
	want := "facet=urn%3Ali%3AadTargetingFacet%3Askills&q=typeahead&query=C%2B%2B%20%28dev%29%20%26%20R%26D%2C%2050%25&queryVersion=QUERY_USES_URNS"
	if raw != want {
		t.Errorf("query string\n got %s\nwant %s", raw, want)
	}
}

func TestSearchTargetingEntities_KeepsOnlyUsableURNsInOrder(t *testing.T) {
	body := `{"elements":[
		{"urn":"urn:li:skill:300","facetUrn":"urn:li:adTargetingFacet:skills","name":"Kubernetes"},
		{"urn":"urn:li:skill:12","facetUrn":"urn:li:adTargetingFacet:skills","name":" Kubernetes Operators "},
		{"urn":"urn:li:skill:300","name":"Kubernetes (again)"},
		{"urn":"urn:li:title:9","name":"wrong namespace"},
		{"urn":"urn:li:skill:0","name":"leading zero"},
		{"urn":"urn:li:skill:44","name":"   "},
		{"name":"no urn"}
	]}`
	srv, _ := typeaheadServer(t, http.StatusOK, body, nil)
	c := NewClient(Credentials{AccessToken: "t"}, RuntimeConfig{}, WithBaseURL(srv.URL))

	got, dropped, err := c.SearchTargetingEntities(context.Background(), "skills", "kube")
	if err != nil {
		t.Fatalf("SearchTargetingEntities: %v", err)
	}
	want := []TargetingEntity{{URN: "urn:li:skill:300", Name: "Kubernetes"}, {URN: "urn:li:skill:12", Name: "Kubernetes Operators"}}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entity %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if dropped != 4 {
		t.Errorf("dropped = %d, want 4 (wrong namespace, leading zero, blank name, no urn)", dropped)
	}
}

// Every URN the typeahead keeps for a targeting facet is one ValidateTargeting accepts, and a
// location URN is one CreateCampaign's geoURNRE accepts.
func TestTypeaheadFacets_MatchWhatACreateAccepts(t *testing.T) {
	samples := map[string]string{
		"skills": "urn:li:skill:1", "groups": "urn:li:group:2", "titles": "urn:li:title:3",
		"interests": "urn:li:interest:4", "industries": "urn:li:industry:5", "employers": "urn:li:organization:6",
	}
	for _, facet := range TypeaheadFacetKeys() {
		if facet == FacetLocations {
			if !typeaheadFacets[facet].MatchString("urn:li:geo:103644278") || !geoURNRE.MatchString("urn:li:geo:103644278") {
				t.Errorf("locations must keep a geo URN")
			}
			continue
		}
		urn, ok := samples[facet]
		if !ok {
			t.Fatalf("no sample for typeahead facet %s", facet)
		}
		if !typeaheadFacets[facet].MatchString(urn) {
			t.Errorf("%s does not keep %s", facet, urn)
		}
		clause := map[string][]string{facet: {urn}}
		if _, err := ValidateTargeting(&Targeting{Include: []map[string][]string{clause}}, nil); err != nil {
			t.Errorf("ValidateTargeting refuses a %s URN the typeahead would offer: %v", facet, err)
		}
	}
	if got, want := strings.Join(TypeaheadFacetKeys(), ","), "employers,groups,industries,interests,locations,skills,titles"; got != want {
		t.Errorf("TypeaheadFacetKeys = %s, want %s", got, want)
	}
}

func TestSearchTargetingEntities_RefusesBeforeAnyRequest(t *testing.T) {
	srv, calls := typeaheadServer(t, http.StatusOK, `{"elements":[]}`, nil)
	c := NewClient(Credentials{AccessToken: "t"}, RuntimeConfig{}, WithBaseURL(srv.URL))
	cases := []struct{ facet, query string }{
		{"seniorities", "senior"},
		{"audienceMatchingSegments", "x"},
		{"Skills", "go"},
		{"skills", "   "},
		{"skills", strings.Repeat("a", MaxTypeaheadQueryLen+1)},
		{"skills", "go\x00lang"},
		{"skills", "go\nlang"},
		{"skills", "\xff\xfe"},
	}
	for _, tc := range cases {
		if _, _, err := c.SearchTargetingEntities(context.Background(), tc.facet, tc.query); !errors.Is(err, ErrTypeaheadInvalid) {
			t.Errorf("(%q, %q) = %v, want ErrTypeaheadInvalid", tc.facet, tc.query, err)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("%d requests sent for refused searches, want 0", n)
	}
	// The longest accepted query is MaxTypeaheadQueryLen characters, counted as runes.
	if _, err := NormalizeTypeaheadQuery(strings.Repeat("é", MaxTypeaheadQueryLen)); err != nil {
		t.Errorf("a %d-rune query was refused: %v", MaxTypeaheadQueryLen, err)
	}
}

func TestSearchTargetingEntities_UpstreamFailures(t *testing.T) {
	t.Run("non-2xx", func(t *testing.T) {
		srv, _ := typeaheadServer(t, http.StatusBadRequest, `{"message":"bad query secret-text"}`, nil)
		c := NewClient(Credentials{AccessToken: "t"}, RuntimeConfig{}, WithBaseURL(srv.URL))
		_, _, err := c.SearchTargetingEntities(context.Background(), "titles", "secret-text")
		if err == nil || strings.Contains(err.Error(), "secret-text") {
			t.Errorf("err = %v, want a failure that does not quote the query", err)
		}
	})
	t.Run("no elements field", func(t *testing.T) {
		srv, _ := typeaheadServer(t, http.StatusOK, `{}`, nil)
		c := NewClient(Credentials{AccessToken: "t"}, RuntimeConfig{}, WithBaseURL(srv.URL))
		if _, _, err := c.SearchTargetingEntities(context.Background(), "titles", "eng"); err == nil {
			t.Error("a 2xx with no elements field must fail, not read as no matches")
		}
	})
	t.Run("transport failure does not quote the query", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("no hijacker")
			}
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		}))
		defer srv.Close()
		c := NewClient(Credentials{AccessToken: "t"}, RuntimeConfig{}, WithBaseURL(srv.URL))
		_, _, err := c.SearchTargetingEntities(context.Background(), "titles", "secret-text")
		if err == nil {
			t.Fatal("a dropped connection must fail")
		}
		if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "query=") {
			t.Errorf("the error quotes the search text: %v", err)
		}
	})
}
