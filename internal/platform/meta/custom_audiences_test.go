// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// audiencePages serves the customaudiences edge one page per request, in order, and records each
// request's path+query.
func audiencePages(t *testing.T, pages ...string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := len(seen)
		seen = append(seen, r.URL.RequestURI())
		mu.Unlock()
		if r.Method != http.MethodGet || r.URL.Path != "/act_777/customaudiences" || n >= len(pages) {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.RequestURI())
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, pages[n])
	}))
	return srv, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), seen...) }
}

func audienceClient(url string) *Client {
	return NewClient(Credentials{AccessToken: "tok"}, AccountConfig{AccountID: "act_777", PageID: "987654321"}, WithBaseURL(url))
}

// Two pages are walked by the opaque cursor (never paging.next), every field is read, and the
// request asks for exactly the documented fields.
func TestListCustomAudiences_WalksPagesByCursor(t *testing.T) {
	srv, seen := audiencePages(t,
		`{"data":[{"id":"111","name":"KubeCon registrants","subtype":"CUSTOM","description":"HubSpot upload","approximate_count_lower_bound":12000,"approximate_count_upper_bound":14100,"account_id":"777"}],
		  "paging":{"cursors":{"after":"CUR1"},"next":"https://graph.facebook.com/v25.0/act_777/customaudiences?access_token=SECRET&after=CUR1"}}`,
		`{"data":[{"id":"222","name":"Site visitors","subtype":"WEBSITE","account_id":"act_777"}],"paging":{"cursors":{"after":"CUR2"}}}`)
	defer srv.Close()
	got, err := audienceClient(srv.URL).ListCustomAudiences(context.Background(), "act_777")
	if err != nil {
		t.Fatalf("ListCustomAudiences: %v", err)
	}
	if len(got) != 2 || got[0].ID != "111" || got[0].Name != "KubeCon registrants" || got[0].Subtype != "CUSTOM" || got[0].Description != "HubSpot upload" ||
		got[0].CountLower == nil || *got[0].CountLower != 12000 || got[0].CountUpper == nil || *got[0].CountUpper != 14100 {
		t.Fatalf("first = %+v", got)
	}
	if got[1].ID != "222" || got[1].CountLower != nil || got[1].Description != "" {
		t.Fatalf("second = %+v", got[1])
	}
	reqs := seen()
	if len(reqs) != 2 {
		t.Fatalf("requests = %v", reqs)
	}
	wantFirst := "/act_777/customaudiences?fields=id,name,subtype,description,approximate_count_lower_bound,approximate_count_upper_bound,account_id&limit=100"
	if reqs[0] != wantFirst || reqs[1] != wantFirst+"&after=CUR1" {
		t.Fatalf("requests = %v, want the field list then the cursor", reqs)
	}
	for _, r := range reqs {
		if strings.Contains(r, "SECRET") {
			t.Fatalf("a request followed paging.next: %s", r)
		}
	}
}

// Every malformed answer fails the whole read rather than returning part of it.
func TestListCustomAudiences_RefusesAPartialOrForeignList(t *testing.T) {
	row := func(id, subtype, acct string) string {
		return `{"id":"` + id + `","name":"n","subtype":"` + subtype + `","account_id":"` + acct + `"}`
	}
	for _, tc := range []struct {
		name  string
		pages []string
	}{
		{"another account", []string{`{"data":[` + row("111", "CUSTOM", "888") + `]}`}},
		{"no subtype (not an audience)", []string{`{"data":[{"id":"111","name":"n","account_id":"777"}]}`}},
		{"non-numeric id", []string{`{"data":[` + row("abc", "CUSTOM", "777") + `]}`}},
		{"listed twice", []string{`{"data":[` + row("111", "CUSTOM", "777") + `],"paging":{"cursors":{"after":"C"},"next":"x"}}`, `{"data":[` + row("111", "CUSTOM", "777") + `]}`}},
		{"no data field", []string{`{"paging":{}}`}},
		{"next without cursor", []string{`{"data":[],"paging":{"next":"x"}}`}},
		{"repeated cursor", []string{`{"data":[],"paging":{"cursors":{"after":"C"},"next":"x"}}`, `{"data":[],"paging":{"cursors":{"after":"C"},"next":"x"}}`}},
		{"negative count other than -1", []string{`{"data":[{"id":"111","name":"n","subtype":"CUSTOM","account_id":"777","approximate_count_lower_bound":-2}]}`}},
		{"oversized name", []string{`{"data":[{"id":"111","name":"` + strings.Repeat("x", maxCustomAudienceTextBytes+1) + `","subtype":"CUSTOM","account_id":"777"}]}`}},
		{"duplicated key", []string{`{"data":[{"id":"111","name":"n","subtype":"CUSTOM","account_id":"777","account_id":"888"}]}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := audiencePages(t, tc.pages...)
			defer srv.Close()
			got, err := audienceClient(srv.URL).ListCustomAudiences(context.Background(), "act_777")
			if err == nil || got != nil {
				t.Fatalf("ListCustomAudiences = %v, %v; want an error and no list", got, err)
			}
		})
	}
}

// More pages than the bound fails with ErrCustomAudienceListTooLarge rather than truncating.
func TestListCustomAudiences_FailsPastThePageBound(t *testing.T) {
	pages := make([]string, customAudienceListMaxPages)
	for i := range pages {
		pages[i] = `{"data":[{"id":"` + strconv.Itoa(1000+i) + `","name":"n","subtype":"CUSTOM","account_id":"777"}],"paging":{"cursors":{"after":"C` + strconv.Itoa(i) + `"},"next":"x"}}`
	}
	srv, seen := audiencePages(t, pages...)
	defer srv.Close()
	got, err := audienceClient(srv.URL).ListCustomAudiences(context.Background(), "act_777")
	if !errors.Is(err, ErrCustomAudienceListTooLarge) || got != nil || len(seen()) != customAudienceListMaxPages {
		t.Fatalf("got %v, err %v after %d requests; want ErrCustomAudienceListTooLarge after %d", got, err, len(seen()), customAudienceListMaxPages)
	}
}

// An account id that is not act_<digits> is refused before any request.
func TestListCustomAudiences_RefusesAMalformedAccount(t *testing.T) {
	srv, seen := audiencePages(t)
	defer srv.Close()
	if _, err := audienceClient(srv.URL).ListCustomAudiences(context.Background(), "act_77/x"); err == nil || len(seen()) != 0 {
		t.Fatalf("err = %v, requests = %v; want a refusal and none", err, seen())
	}
}

// Meta's -1 "size unavailable" bound (an inactive lookalike) reads as absent, not as a malformed row,
// so one such audience does not fail the whole listing.
func TestListCustomAudiences_MinusOneSizeIsAbsent(t *testing.T) {
	srv, _ := audiencePages(t, `{"data":[{"id":"111","name":"Inactive lookalike","subtype":"LOOKALIKE","account_id":"777","approximate_count_lower_bound":-1,"approximate_count_upper_bound":-1}]}`)
	defer srv.Close()
	got, err := audienceClient(srv.URL).ListCustomAudiences(context.Background(), "act_777")
	if err != nil || len(got) != 1 || got[0].CountLower != nil || got[0].CountUpper != nil {
		t.Fatalf("ListCustomAudiences = %+v, %v; want one audience with no size", got, err)
	}
}
