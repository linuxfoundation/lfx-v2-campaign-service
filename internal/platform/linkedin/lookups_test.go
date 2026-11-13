// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// lookupServer answers one finder from pages, keyed by the start parameter, and records each
// request's raw query.
type lookupServer struct {
	mu      sync.Mutex
	queries []string
	pages   map[int]string
	path    string
}

func (s *lookupServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if r.URL.Path != s.path {
			t.Errorf("path %s, want %s", r.URL.Path, s.path)
		}
		s.queries = append(s.queries, r.URL.RawQuery)
		start, _ := strconv.Atoi(r.URL.Query().Get("start"))
		w.Header().Set("Content-Type", "application/json")
		body, ok := s.pages[start]
		if !ok {
			body = `{"elements":[]}`
		}
		_, _ = io.WriteString(w, body)
	}
}

func (s *lookupServer) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.queries...)
}

func segmentEl(id int, name, typ, status string) string {
	return fmt.Sprintf(`{"id":%d,"name":%q,"type":%q,"status":%q,"account":"urn:li:sponsoredAccount:123456789","approximateMemberCount":%d}`, id, name, typ, status, id*10)
}

func fullPage(mk func(i int) string, from int) string {
	els := make([]string, lookupPageSize)
	for i := range els {
		els[i] = mk(from + i)
	}
	return `{"elements":[` + strings.Join(els, ",") + `]}`
}

func TestListAdSegments(t *testing.T) {
	s := &lookupServer{path: "/adSegments", pages: map[int]string{
		0: fullPage(func(i int) string { return segmentEl(i+1, fmt.Sprintf("seg %03d", i+1), "BULK", "READY") }, 0),
		100: `{"elements":[` + strings.Join([]string{
			segmentEl(1001, "Site visitors", "RETARGETING", "READY"),
			segmentEl(1002, "a purged list", "BULK", "EXPIRED"),
			segmentEl(1003, "a failed list", "BULK", "FAILED"),
			segmentEl(1004, "archived list", "BULK", "ARCHIVED"),
			segmentEl(1005, "new kind", "LOOKALIKE", "READY"),
		}, ",") + `]}`,
	}}
	srv := httptest.NewServer(s.handler(t))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL))

	got, err := c.ListAdSegments(context.Background())
	if err != nil {
		t.Fatalf("ListAdSegments: %v", err)
	}
	if len(got) != 103 {
		t.Fatalf("%d segments, want 103 (EXPIRED and FAILED dropped)", len(got))
	}
	q := s.snapshot()
	if len(q) != 2 || q[0] != "accounts=List(urn%3Ali%3AsponsoredAccount%3A123456789)&count=100&q=accounts&start=0" || !strings.HasSuffix(q[1], "&start=100") {
		t.Errorf("queries = %q", q)
	}
	byName := map[string]AdSegment{}
	for _, seg := range got {
		byName[seg.Name] = seg
	}
	if v := byName["Site visitors"]; v.Facet != "dynamicSegments" || v.URN() != "urn:li:adSegment:1001" || v.ApproximateMemberCount == nil || *v.ApproximateMemberCount != 10010 {
		t.Errorf("retargeting segment = %+v", v)
	}
	if v := byName["archived list"]; v.Facet != "audienceMatchingSegments" || v.Status != "ARCHIVED" {
		t.Errorf("archived segment = %+v", v)
	}
	if v := byName["new kind"]; v.Facet != "" {
		t.Errorf("an unknown type was given a facet: %+v", v)
	}
	if got[0].Name != "archived list" || got[len(got)-1].Name != "Site visitors" {
		t.Errorf("not sorted by name ignoring case: first %q last %q", got[0].Name, got[len(got)-1].Name)
	}
}

func TestListConversionRules(t *testing.T) {
	s := &lookupServer{path: "/conversions", pages: map[int]string{
		0: `{"elements":[{"id":104012,"name":"KubeCon registration","type":"REGISTRATION","enabled":true,"account":"urn:li:sponsoredAccount:123456789"},` +
			`{"id":"7","name":"another","type":"LEAD","enabled":false},{"id":8,"name":"Absent flag"}]}`,
	}}
	srv := httptest.NewServer(s.handler(t))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL))

	got, err := c.ListConversionRules(context.Background())
	if err != nil {
		t.Fatalf("ListConversionRules: %v", err)
	}
	want := "Absent flag/8/false,another/7/false,KubeCon registration/104012/true"
	var parts []string
	for _, r := range got {
		parts = append(parts, fmt.Sprintf("%s/%s/%v", r.Name, r.ID, r.Enabled))
	}
	if strings.Join(parts, ",") != want {
		t.Errorf("rules = %s, want %s", strings.Join(parts, ","), want)
	}
	if got[2].URN() != "urn:lla:llaPartnerConversion:104012" {
		t.Errorf("URN = %s", got[2].URN())
	}
	if q := s.snapshot(); len(q) != 1 || q[0] != "account=urn%3Ali%3AsponsoredAccount%3A123456789&count=100&q=account&start=0" {
		t.Errorf("queries = %q", q)
	}
}

// TestLookupsFailRatherThanTruncate pins every arm that refuses a list instead of returning part
// of one.
func TestLookupsFailRatherThanTruncate(t *testing.T) {
	page := func(from int) string {
		return fullPage(func(i int) string { return segmentEl(i+1, "s", "BULK", "READY") }, from)
	}
	all := map[int]string{}
	for start := 0; start <= MaxLookupRows; start += lookupPageSize {
		all[start] = page(start)
	}
	for _, tc := range []struct {
		name  string
		pages map[int]string
		want  string
		is    error
	}{
		{"start ignored", map[int]string{0: page(0), 100: page(0)}, "repeats an earlier page", nil},
		{"duplicate inside a page", map[int]string{0: `{"elements":[` + segmentEl(1, "a", "BULK", "READY") + `,` + segmentEl(1, "a", "BULK", "READY") + `]}`}, "listed twice", nil},
		{"another account", map[int]string{0: `{"elements":[{"id":1,"name":"x","type":"BULK","status":"READY","account":"urn:li:sponsoredAccount:999"}]}`}, "another ad account", nil},
		{"unusable id", map[int]string{0: `{"elements":[{"id":"abc","name":"x"}]}`}, "unusable id", nil},
		{"too many", all, "more than 1000", ErrLookupTooLarge},
		{"no elements", map[int]string{0: `{}`}, "missing elements field", nil},
		{"short page with a next page token", map[int]string{0: `{"elements":[` + segmentEl(1, "a", "BULK", "READY") + `],"metadata":{"nextPageToken":"abc"}}`}, "advertises a next page token", nil},
		{"empty page with a next page token", map[int]string{0: `{"elements":[],"metadata":{"nextPageToken":"abc"}}`}, "advertises a next page token", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &lookupServer{path: "/adSegments", pages: tc.pages}
			srv := httptest.NewServer(s.handler(t))
			defer srv.Close()
			c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL))
			got, err := c.ListAdSegments(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %d segments, err %v; want an error containing %q", len(got), err, tc.want)
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Errorf("err %v is not %v", err, tc.is)
			}
		})
	}
}
