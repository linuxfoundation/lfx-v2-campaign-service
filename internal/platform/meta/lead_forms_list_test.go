// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// leadFormPages serves the given pages in order (page i answers the i-th request) and records each
// request URL.
func leadFormPages(t *testing.T, pages ...string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		i := len(seen)
		seen = append(seen, r.URL.String())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if i >= len(pages) {
			t.Errorf("unexpected request %d: %s", i, r.URL)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, pages[i])
	}))
	return srv, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), seen...) }
}

// TestListLeadForms_WalksEveryPage: the edge is read with the field list and limit, the opaque
// cursor is followed, and every row of every page is returned with its status verbatim.
func TestListLeadForms_WalksEveryPage(t *testing.T) {
	srv, seen := leadFormPages(t,
		`{"data":[{"id":"1111","name":"KubeCon interest","status":"ACTIVE"}],"paging":{"cursors":{"after":"c1"},"next":"https://graph.facebook.com/x?access_token=secret"}}`,
		`{"data":[{"id":2222,"status":"ARCHIVED"}],"paging":{"cursors":{"after":"c2"}}}`)
	defer srv.Close()
	forms, err := biddingClient(srv.URL, 100).ListLeadForms(context.Background(), " 987654321 ")
	if err != nil {
		t.Fatalf("ListLeadForms: %v", err)
	}
	want := []LeadForm{{ID: "1111", Name: "KubeCon interest", Status: "ACTIVE"}, {ID: "2222", Status: "ARCHIVED"}}
	if !reflect.DeepEqual(forms, want) {
		t.Errorf("forms = %+v, want %+v", forms, want)
	}
	reqs := seen()
	if len(reqs) != 2 {
		t.Fatalf("requests = %v, want two pages", reqs)
	}
	first, _ := url.Parse(reqs[0])
	second, _ := url.Parse(reqs[1])
	if first.Path != "/987654321/leadgen_forms" || first.Query().Get("fields") != "id,name,status" || first.Query().Get("limit") != "100" || first.Query().Has("after") {
		t.Errorf("first request = %s", reqs[0])
	}
	if second.Query().Get("after") != "c1" || strings.Contains(reqs[1], "secret") {
		t.Errorf("second request = %s, want the opaque cursor and not the next URL", reqs[1])
	}
}

// TestListLeadForms_RefusesWhatItCannotBelieve: no data, a row without a numeric id or a status
// token, a repeated form, a next page without a cursor, a repeated cursor, a failed read, and more
// forms than the bound all fail the read; an empty Page is an empty, non-nil answer.
func TestListLeadForms_RefusesWhatItCannotBelieve(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pages []string
		want  string
	}{
		{"no data", []string{`{}`}, "no data field"},
		{"no id", []string{`{"data":[{"name":"x","status":"ACTIVE"}]}`}, "no canonical form id"},
		{"non-numeric id", []string{`{"data":[{"id":"abc","status":"ACTIVE"}]}`}, "no canonical form id"},
		{"no status", []string{`{"data":[{"id":"1"}]}`}, "no form status token"},
		{"lowercase status", []string{`{"data":[{"id":"1","status":"active"}]}`}, "no form status token"},
		{"repeated form", []string{`{"data":[{"id":"1","status":"ACTIVE"},{"id":"1","status":"ACTIVE"}]}`}, "listed twice"},
		{"next without cursor", []string{`{"data":[],"paging":{"next":"x"}}`}, "no cursor"},
		{"repeated cursor", []string{`{"data":[],"paging":{"cursors":{"after":"c"},"next":"x"}}`, `{"data":[],"paging":{"cursors":{"after":"c"},"next":"x"}}`}, "repeated cursor"},
		{"graph error", []string{`{"error":{"message":"(#200) Requires pages_manage_ads","code":200}}`}, "list meta lead forms"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := leadFormPages(t, tc.pages...)
			defer srv.Close()
			if forms, err := biddingClient(srv.URL, 100).ListLeadForms(context.Background(), "987654321"); err == nil || forms != nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ListLeadForms = %+v, %v; want an error containing %q", forms, err, tc.want)
			}
		})
	}
	t.Run("too many", func(t *testing.T) {
		pages := make([]string, leadFormListMaxPages)
		for p := range pages {
			rows := make([]string, leadFormListPageSize)
			for i := range rows {
				rows[i] = fmt.Sprintf(`{"id":"%d","status":"ACTIVE"}`, p*leadFormListPageSize+i+1)
			}
			pages[p] = fmt.Sprintf(`{"data":[%s],"paging":{"cursors":{"after":"c%d"},"next":"x"}}`, strings.Join(rows, ","), p)
		}
		srv, _ := leadFormPages(t, pages...)
		defer srv.Close()
		if _, err := biddingClient(srv.URL, 100).ListLeadForms(context.Background(), "987654321"); !errors.Is(err, ErrLeadFormListTooLarge) {
			t.Fatalf("err = %v, want ErrLeadFormListTooLarge", err)
		}
	})
	t.Run("oversized page", func(t *testing.T) {
		rows := make([]string, leadFormListPageSize*leadFormListMaxPages+1)
		for i := range rows {
			rows[i] = fmt.Sprintf(`{"id":"%d","status":"ACTIVE"}`, i+1)
		}
		srv, _ := leadFormPages(t, `{"data":[`+strings.Join(rows, ",")+`]}`)
		defer srv.Close()
		if _, err := biddingClient(srv.URL, 100).ListLeadForms(context.Background(), "987654321"); !errors.Is(err, ErrLeadFormListTooLarge) {
			t.Fatalf("err = %v, want ErrLeadFormListTooLarge", err)
		}
	})
	t.Run("empty page", func(t *testing.T) {
		srv, _ := leadFormPages(t, `{"data":[]}`)
		defer srv.Close()
		if forms, err := biddingClient(srv.URL, 100).ListLeadForms(context.Background(), "987654321"); err != nil || forms == nil || len(forms) != 0 {
			t.Fatalf("ListLeadForms = %#v, %v; want empty non-nil", forms, err)
		}
	})
	t.Run("bad page id", func(t *testing.T) {
		srv, seen := leadFormPages(t)
		defer srv.Close()
		for _, id := range []string{"", "my-page", strings.Repeat("1", maxMetaIDDigits+1)} {
			if _, err := biddingClient(srv.URL, 100).ListLeadForms(context.Background(), id); err == nil {
				t.Errorf("page id %q accepted", id)
			}
		}
		if n := len(seen()); n != 0 {
			t.Errorf("requests = %d, want none", n)
		}
	})
}
