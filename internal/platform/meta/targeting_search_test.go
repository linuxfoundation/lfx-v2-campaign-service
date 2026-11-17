// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

// searchServer answers every request with status and body, recording the request URLs.
func searchServer(t *testing.T, status int, body string) (*httptest.Server, *atomic.Value, *atomic.Int32) {
	t.Helper()
	var last atomic.Value
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		last.Store(r.URL.String())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	return srv, &last, &count
}

// TestSearchTargeting_RequestShapes: each kind sends Meta's documented search: adinterest and
// adworkposition with q and a limit, behaviors as the adTargetingCategory browse without q.
func TestSearchTargeting_RequestShapes(t *testing.T) {
	for _, tc := range []struct {
		kind string
		want url.Values
	}{
		{TargetingSearchInterest, url.Values{"type": {"adinterest"}, "q": {"cloud native"}, "limit": {"50"}}},
		{TargetingSearchWorkPosition, url.Values{"type": {"adworkposition"}, "q": {"cloud native"}, "limit": {"50"}}},
		{TargetingSearchBehavior, url.Values{"type": {"adTargetingCategory"}, "class": {"behaviors"}}},
	} {
		srv, last, _ := searchServer(t, http.StatusOK, `{"data":[]}`)
		rows, _, err := biddingClient(srv.URL, 100).SearchTargeting(context.Background(), tc.kind, "  cloud native ")
		srv.Close()
		if err != nil || rows == nil || len(rows) != 0 {
			t.Fatalf("%s: rows=%v err=%v, want an empty non-nil answer", tc.kind, rows, err)
		}
		u, _ := url.Parse(last.Load().(string))
		if u.Path != "/search" || !reflect.DeepEqual(u.Query(), tc.want) {
			t.Errorf("%s: request = %s, want /search?%s", tc.kind, u, tc.want.Encode())
		}
	}
}

// TestSearchTargeting_ParsesRows: ids given as numbers or strings are kept, rows without a numeric
// id or a name are dropped and counted, repeats are dropped, a negative size is absent, and a work
// position's coverage and subtext are carried.
func TestSearchTargeting_ParsesRows(t *testing.T) {
	srv, _, _ := searchServer(t, http.StatusOK, `{"data":[
		{"id":6003107902433,"name":"Kubernetes","path":["Interests","Technology"],"audience_size_lower_bound":1200000,"audience_size_upper_bound":-1},
		{"id":"6003107902433","name":"Kubernetes again"},
		{"id":"abc","name":"Not an id"},
		{"id":6003,"name":"  "},
		{"id":{"x":1},"name":"Object id"},
		{"id":"105763692790962","name":"Engineer","coverage":56000,"subtext":"Software"}]}`)
	defer srv.Close()
	rows, dropped, err := biddingClient(srv.URL, 100).SearchTargeting(context.Background(), TargetingSearchInterest, "ku")
	if err != nil {
		t.Fatalf("SearchTargeting: %v", err)
	}
	lower, cov := int64(1200000), int64(56000)
	want := []TargetingOption{
		{ID: "6003107902433", Name: "Kubernetes", Path: []string{"Interests", "Technology"}, AudienceLower: &lower},
		{ID: "105763692790962", Name: "Engineer", Description: "Software", Coverage: &cov},
	}
	if !reflect.DeepEqual(rows, want) || dropped != 3 {
		t.Errorf("rows = %+v dropped %d, want %+v dropped 3", rows, dropped, want)
	}
}

// TestSearchTargeting_BehaviorsAreFilteredByName: the behavior browse is kept by name (ignoring
// case), and a row of another class is dropped; SearchTargetingAll keeps the whole class.
func TestSearchTargeting_BehaviorsAreFilteredByName(t *testing.T) {
	body := `{"data":[{"id":6002714895372,"name":"Frequent Travelers","type":"behaviors"},{"id":6002714898572,"name":"Small business owners","type":"behaviors"},{"id":6002714898573,"name":"Travel lovers","type":"interests"}]}`
	srv, _, _ := searchServer(t, http.StatusOK, body)
	defer srv.Close()
	c := biddingClient(srv.URL, 100)
	rows, dropped, err := c.SearchTargeting(context.Background(), TargetingSearchBehavior, "TRAVEL")
	if err != nil || len(rows) != 1 || rows[0].ID != "6002714895372" || dropped != 1 {
		t.Fatalf("rows = %+v dropped %d err %v, want only Frequent Travelers", rows, dropped, err)
	}
	all, _, err := c.SearchTargetingAll(context.Background())
	if err != nil || len(all) != 2 {
		t.Fatalf("SearchTargetingAll = %+v, %v; want both behaviors", all, err)
	}
	if got := FilterTargetingOptions(all, "zzz"); got == nil || len(got) != 0 {
		t.Errorf("FilterTargetingOptions with no match = %#v, want empty non-nil", got)
	}
}

// TestSearchTargeting_RefusesBeforeAnyRequest: an unknown kind and an unusable query send nothing.
func TestSearchTargeting_RefusesBeforeAnyRequest(t *testing.T) {
	srv, _, count := searchServer(t, http.StatusOK, `{"data":[]}`)
	defer srv.Close()
	c := biddingClient(srv.URL, 100)
	for _, tc := range []struct{ kind, query string }{
		{"interests", "go"}, {TargetingSearchInterest, " g "}, {TargetingSearchInterest, "go\nlang"},
		{TargetingSearchInterest, strings.Repeat("é", MaxTargetingQueryLen+1)}, {TargetingSearchInterest, "\xff\xfe"},
	} {
		if _, _, err := c.SearchTargeting(context.Background(), tc.kind, tc.query); !errors.Is(err, ErrTargetingSearchInvalid) {
			t.Errorf("SearchTargeting(%q, %q) = %v, want ErrTargetingSearchInvalid", tc.kind, tc.query, err)
		}
	}
	if n := count.Load(); n != 0 {
		t.Errorf("requests = %d, want none", n)
	}
}

// TestSearchTargeting_ThrottleIsNotRetriedAndNoErrorQuotesTheQuery: a 429 or a rate-limit 400 is
// sent once; every failure (Graph error, throttle, no data, unreachable host) keeps the query out
// of the error text while the chain still carries the APIError.
func TestSearchTargeting_ThrottleIsNotRetriedAndNoErrorQuotesTheQuery(t *testing.T) {
	const secret = "zebracorn"
	for _, tc := range []struct {
		name, body, want string
		status           int
		apiError         bool
	}{
		{"429", `{"error":{"message":"rate limited","code":4}}`, "rate-limiting the search; it was not retried", http.StatusTooManyRequests, true},
		{"rate-limit 400", `{"error":{"message":"too many calls","code":17}}`, "rate-limiting the search", http.StatusBadRequest, true},
		{"graph 500", `{"error":{"message":"unknown","code":1}}`, "Meta answered HTTP 500", http.StatusInternalServerError, true},
		{"no data", `{}`, "the answer had no data field", http.StatusOK, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, count := searchServer(t, tc.status, tc.body)
			defer srv.Close()
			_, _, err := biddingClient(srv.URL, 100).SearchTargeting(context.Background(), TargetingSearchInterest, secret)
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), secret) {
				t.Fatalf("err = %v, want one containing %q and not the query", err, tc.want)
			}
			var ae *APIError
			if errors.As(err, &ae) != tc.apiError || (tc.apiError && strings.Contains(ae.Path, secret)) {
				t.Errorf("APIError in chain = %v (path %q), want %v without the query", errors.As(err, &ae), func() string {
					if ae != nil {
						return ae.Path
					}
					return ""
				}(), tc.apiError)
			}
			if n := count.Load(); n != 1 {
				t.Errorf("requests = %d, want exactly one", n)
			}
		})
	}
	t.Run("unreachable", func(t *testing.T) {
		srv, _, _ := searchServer(t, http.StatusOK, `{"data":[]}`)
		srv.Close()
		_, _, err := biddingClient(srv.URL, 100).SearchTargeting(context.Background(), TargetingSearchWorkPosition, secret)
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Fatalf("err = %v, want an error without the query", err)
		}
		var ue *url.Error
		if errors.As(err, &ue) && strings.Contains(ue.URL, secret) {
			t.Errorf("url.Error URL = %q still carries the query", ue.URL)
		}
	})
}
