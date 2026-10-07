// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

// Response bodies here follow the shapes accounts.go documents as UNVERIFIED against a live
// account ({"data":[...],"pagination":{"next_url":...}}): they prove the walk's handling of
// those shapes, not that Reddit sends them.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const (
	businessesPath = "/api/v3/me/businesses"
)

func adAccountsPath(businessID string) string {
	return "/api/v3/businesses/" + businessID + "/ad_accounts"
}

// discoveryStub answers each request from a per-path script. State is guarded by mu, and the
// test reads it only through the accessor methods — the handler runs on the server's goroutines.
type discoveryStub struct {
	mu       sync.Mutex
	srvURL   string
	requests []string
	// respond returns status and body for a request path+query; called under mu.
	respond func(s *discoveryStub, pathAndQuery string, n int) (int, string)
}

func (s *discoveryStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	key := r.URL.Path
	if r.URL.RawQuery != "" {
		key += "?" + r.URL.RawQuery
	}
	s.requests = append(s.requests, key)
	status, body := s.respond(s, key, len(s.requests))
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func (s *discoveryStub) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func (s *discoveryStub) next(path string) string {
	return fmt.Sprintf(`{"next_url":%q}`, s.srvURL+path)
}

// newDiscoveryClient wires a client built with a ZERO AccountConfig (discovery must not need
// one) to stub, with a pinned clock and a tiny backoff.
func newDiscoveryClient(t *testing.T, stub *discoveryStub) *Client {
	t.Helper()
	apiSrv := httptest.NewServer(stub)
	t.Cleanup(apiSrv.Close)
	stub.mu.Lock()
	stub.srvURL = apiSrv.URL
	stub.mu.Unlock()
	tokenSrv := httptest.NewServer(tokenHandlerReturning("tok"))
	t.Cleanup(tokenSrv.Close)
	return NewClient(testCreds, AccountConfig{},
		WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL),
		WithNowFunc(fixedRedditClock()), withRetryBaseDelay(tinyBackoff))
}

func TestListAdAccounts_WalksBusinessesThenTheirAccountsAcrossPages(t *testing.T) {
	stub := &discoveryStub{respond: func(s *discoveryStub, key string, _ int) (int, string) {
		switch key {
		case businessesPath:
			return 200, `{"data":[{"id":"biz-1","name":"The Linux Foundation"}],"pagination":` + s.next(businessesPath+"?page.token=b2") + `}`
		case businessesPath + "?page.token=b2":
			return 200, `{"data":[{"id":"biz_2","name":" CNCF "}],"pagination":{"next_url":null}}`
		case adAccountsPath("biz-1"):
			return 200, `{"data":[{"id":"t2_aaa","name":"LF Events","currency":"USD"}],"pagination":` + s.next(adAccountsPath("biz-1")+"?page.token=p2") + `}`
		case adAccountsPath("biz-1") + "?page.token=p2":
			return 200, `{"data":[{"id":"t2_bbb","name":""}],"pagination":{}}`
		case adAccountsPath("biz_2"):
			return 200, `{"data":[{"id":"t2_ccc","name":"KubeCon"}]}`
		}
		return 404, `{}`
	}}
	c := newDiscoveryClient(t, stub)

	got, err := c.ListAdAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAdAccounts: %v", err)
	}
	want := []AdAccount{
		{ID: "t2_aaa", Name: "LF Events", Currency: "USD", BusinessName: "The Linux Foundation"},
		{ID: "t2_bbb", BusinessName: "The Linux Foundation"},
		{ID: "t2_ccc", Name: "KubeCon", BusinessName: "CNCF"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("account %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	wantReqs := []string{businessesPath, businessesPath + "?page.token=b2", adAccountsPath("biz-1"),
		adAccountsPath("biz-1") + "?page.token=p2", adAccountsPath("biz_2")}
	if seen := stub.seen(); strings.Join(seen, ",") != strings.Join(wantReqs, ",") {
		t.Errorf("requests = %v, want %v", seen, wantReqs)
	}
}

func TestListAdAccounts_EmptyAnswersAreEmptyNotNil(t *testing.T) {
	cases := map[string]func(*discoveryStub, string, int) (int, string){
		"no businesses": func(_ *discoveryStub, key string, _ int) (int, string) {
			return 200, `{"data":[]}`
		},
		"a business with no accounts": func(_ *discoveryStub, key string, _ int) (int, string) {
			if key == businessesPath {
				return 200, `{"data":[{"id":"biz1"}]}`
			}
			return 200, `{"data":[]}`
		},
	}
	for name, respond := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := newDiscoveryClient(t, &discoveryStub{respond: respond}).ListAdAccounts(context.Background())
			if err != nil {
				t.Fatalf("ListAdAccounts: %v", err)
			}
			if got == nil || len(got) != 0 {
				t.Fatalf("got %#v, want a non-nil empty slice", got)
			}
		})
	}
}

// Every failure is an error with NOTHING returned — a partial list looks complete. Each case
// fails at the SECOND call (the accounts list) where that matters, so a walk that returned what
// the first call gathered would be caught.
func TestListAdAccounts_FailuresAreErrorsNeverShortLists(t *testing.T) {
	bizOK := `{"data":[{"id":"biz1","name":"LF"}]}`
	cases := []struct {
		name       string
		businesses func(n int) (int, string)
		accounts   func(n int) (int, string)
		wantStatus int // 0 when the failure is not an apiError
	}{
		{"401 on businesses", func(int) (int, string) { return 401, `{"error":"secret-upstream-text"}` }, nil, 401},
		{"403 on accounts", nil, func(int) (int, string) { return 403, `{"error":"secret-upstream-text"}` }, 403},
		{"404 on businesses", func(int) (int, string) { return 404, `{}` }, nil, 404},
		{"500 on accounts", nil, func(int) (int, string) { return 500, `secret-upstream-text` }, 500},
		{"503 on businesses", func(int) (int, string) { return 503, `` }, nil, 503},
		{"429 that outlasts the retry", nil, func(int) (int, string) { return 429, `{}` }, 429},
		{"non-JSON 2xx", nil, func(int) (int, string) { return 200, `<html>secret-upstream-text` }, 0},
		{"data absent", nil, func(int) (int, string) { return 200, `{"pagination":{}}` }, 0},
		{"data null", func(int) (int, string) { return 200, `{"data":null}` }, nil, 0},
		{"data not a list", nil, func(int) (int, string) { return 200, `{"data":{"id":"t2_a"}}` }, 0},
		{"account id with a path separator", nil, func(int) (int, string) { return 200, `{"data":[{"id":"t2_a/../b"}]}` }, 0},
		{"account id padded", nil, func(int) (int, string) { return 200, `{"data":[{"id":" t2_a"}]}` }, 0},
		{"account id over 64", nil, func(int) (int, string) {
			return 200, `{"data":[{"id":"` + strings.Repeat("a", 65) + `"}]}`
		}, 0},
		{"account listed twice in one business", nil, func(int) (int, string) { return 200, `{"data":[{"id":"t2_a"},{"id":"t2_a"}]}` }, 0},
		{"business id with a path separator", func(int) (int, string) { return 200, `{"data":[{"id":"b/../x"}]}` }, nil, 0},
		{"business listed twice", func(int) (int, string) { return 200, `{"data":[{"id":"b1"},{"id":"b1"}]}` }, nil, 0},
		{"next_url off origin", func(int) (int, string) {
			return 200, `{"data":[{"id":"b1"}],"pagination":{"next_url":"https://evil.example/api/v3/me/businesses"}}`
		}, nil, 0},
		{"next_url to another resource", nil, func(int) (int, string) {
			return 200, `{"data":[],"pagination":{"next_url":"/api/v3/businesses/other/ad_accounts"}}`
		}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &discoveryStub{respond: func(_ *discoveryStub, key string, n int) (int, string) {
				if strings.HasPrefix(key, businessesPath) {
					if tc.businesses != nil {
						return tc.businesses(n)
					}
					return 200, bizOK
				}
				if tc.accounts != nil {
					return tc.accounts(n)
				}
				return 200, `{"data":[{"id":"t2_ok"}]}`
			}}
			got, err := newDiscoveryClient(t, stub).ListAdAccounts(context.Background())
			if err == nil {
				t.Fatalf("got %+v with no error", got)
			}
			if got != nil {
				t.Errorf("got %+v alongside the error; a failed walk must return nothing", got)
			}
			if strings.Contains(err.Error(), "secret-upstream-text") {
				t.Errorf("error %q carries upstream body text", err)
			}
			var ae *apiError
			if tc.wantStatus != 0 {
				if !errors.As(err, &ae) || ae.StatusCode != tc.wantStatus {
					t.Errorf("error %v: want an apiError with status %d", err, tc.wantStatus)
				}
				return
			}
			if errors.As(err, &ae) {
				t.Errorf("error %v: a 2xx failure must not be reported as an upstream status", err)
			}
			// A body that decoded but cannot be believed is classified structurally. The
			// non-JSON and next_url cases fail earlier, in request() and nextPagePath.
			if !strings.Contains(tc.name, "non-JSON") && !strings.Contains(tc.name, "next_url") && !errors.Is(err, ErrDiscoveryMalformed) {
				t.Errorf("error %v: want ErrDiscoveryMalformed", err)
			}
		})
	}
}

// A 429 is retried (request()'s bounded throttle retry); a throttle that clears is a success.
func TestListAdAccounts_ThrottleThatClearsIsRetried(t *testing.T) {
	stub := &discoveryStub{respond: func(_ *discoveryStub, key string, n int) (int, string) {
		if key == businessesPath && n == 1 {
			return 429, `{}`
		}
		if key == businessesPath {
			return 200, `{"data":[{"id":"b1"}]}`
		}
		return 200, `{"data":[{"id":"t2_a"}]}`
	}}
	got, err := newDiscoveryClient(t, stub).ListAdAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAdAccounts: %v", err)
	}
	if len(got) != 1 || got[0].ID != "t2_a" {
		t.Fatalf("got %+v", got)
	}
	if n := len(stub.seen()); n != 3 {
		t.Errorf("requests = %d, want 3 (one throttled, one retried, one accounts read)", n)
	}
}

func TestListAdAccounts_PageCapIsAnErrorNotATruncation(t *testing.T) {
	stub := &discoveryStub{respond: func(s *discoveryStub, _ string, n int) (int, string) {
		return 200, fmt.Sprintf(`{"data":[{"id":"b%d"}],"pagination":%s}`, n, s.next(fmt.Sprintf("%s?page.token=%d", businessesPath, n)))
	}}
	got, err := newDiscoveryClient(t, stub).ListAdAccounts(context.Background())
	if !errors.Is(err, errBusinessPageCap) {
		t.Fatalf("err = %v, want errBusinessPageCap", err)
	}
	if got != nil {
		t.Errorf("got %+v alongside the cap error", got)
	}
	if n := len(stub.seen()); n != discoveryBusinessMaxPages {
		t.Errorf("requests = %d, want exactly %d pages before refusing", n, discoveryBusinessMaxPages)
	}
}

func TestListAdAccounts_AccountPageCapIsAnErrorNotATruncation(t *testing.T) {
	stub := &discoveryStub{respond: func(s *discoveryStub, key string, n int) (int, string) {
		if key == businessesPath {
			return 200, `{"data":[{"id":"b1"}]}`
		}
		return 200, fmt.Sprintf(`{"data":[{"id":"t2_%d"}],"pagination":%s}`, n, s.next(fmt.Sprintf("%s?page.token=%d", adAccountsPath("b1"), n)))
	}}
	_, err := newDiscoveryClient(t, stub).ListAdAccounts(context.Background())
	if !errors.Is(err, errAccountPageCap) {
		t.Fatalf("err = %v, want errAccountPageCap", err)
	}
}

// The item bound is read plus one: exactly the bound is an answer, one more is an error.
func TestListAdAccounts_ItemBoundIsReadPlusOne(t *testing.T) {
	businesses := func(n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = fmt.Sprintf(`{"id":"b%d"}`, i)
		}
		return `{"data":[` + strings.Join(parts, ",") + `]}`
	}
	for _, tc := range []struct {
		n       int
		wantErr bool
	}{{maxDiscoveredBusinesses, false}, {maxDiscoveredBusinesses + 1, true}} {
		t.Run(fmt.Sprint(tc.n), func(t *testing.T) {
			stub := &discoveryStub{respond: func(_ *discoveryStub, key string, _ int) (int, string) {
				if key == businessesPath {
					return 200, businesses(tc.n)
				}
				return 200, `{"data":[]}`
			}}
			_, err := newDiscoveryClient(t, stub).ListAdAccounts(context.Background())
			if (err != nil) != tc.wantErr {
				t.Fatalf("n=%d: err = %v, wantErr %v", tc.n, err, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, ErrDiscoveryMalformed) {
				t.Errorf("err = %v, want ErrDiscoveryMalformed", err)
			}
		})
	}
}

// The ACCOUNT bound is read plus one too, and counts unique accounts across every business and
// page: exactly maxDiscoveredAccounts is an answer, one more is an error and never a truncation.
// The ids are spread over two businesses of two pages each, and the second business re-lists one
// of the first's accounts — that repeat is deduplicated, so it must not count toward the bound.
func TestListAdAccounts_AccountItemBoundIsReadPlusOne(t *testing.T) {
	const businesses, pagesPerBusiness = 2, 2
	// accountsPage renders one page of the spread: business b, page p of n unique accounts, with
	// t2_0 (already listed by b0) prepended to b1's first page as a cross-business repeat.
	accountsPage := func(s *discoveryStub, n, b, p int) string {
		chunks := businesses * pagesPerBusiness
		per := (n + chunks - 1) / chunks
		lo, hi := (b*pagesPerBusiness+p)*per, (b*pagesPerBusiness+p+1)*per
		hi = min(hi, n)
		parts := make([]string, 0, per+1)
		if b == 1 && p == 0 {
			parts = append(parts, `{"id":"t2_0"}`)
		}
		for i := lo; i < hi; i++ {
			parts = append(parts, fmt.Sprintf(`{"id":"t2_%d"}`, i))
		}
		body := `{"data":[` + strings.Join(parts, ",") + `]`
		if p+1 < pagesPerBusiness {
			body += `,"pagination":` + s.next(fmt.Sprintf("%s?page.token=%d", adAccountsPath(fmt.Sprintf("b%d", b)), p+1))
		}
		return body + `}`
	}
	for _, tc := range []struct {
		n       int
		wantErr bool
	}{{maxDiscoveredAccounts, false}, {maxDiscoveredAccounts + 1, true}} {
		t.Run(fmt.Sprint(tc.n), func(t *testing.T) {
			stub := &discoveryStub{respond: func(s *discoveryStub, key string, _ int) (int, string) {
				if key == businessesPath {
					return 200, `{"data":[{"id":"b0"},{"id":"b1"}]}`
				}
				for b := 0; b < businesses; b++ {
					for p := 0; p < pagesPerBusiness; p++ {
						want := adAccountsPath(fmt.Sprintf("b%d", b))
						if p > 0 {
							want += fmt.Sprintf("?page.token=%d", p)
						}
						if key == want {
							return 200, accountsPage(s, tc.n, b, p)
						}
					}
				}
				return 404, `{}`
			}}
			got, err := newDiscoveryClient(t, stub).ListAdAccounts(context.Background())
			if (err != nil) != tc.wantErr {
				t.Fatalf("n=%d: err = %v, wantErr %v", tc.n, err, tc.wantErr)
			}
			if tc.wantErr {
				if !errors.Is(err, ErrDiscoveryMalformed) {
					t.Errorf("err = %v, want ErrDiscoveryMalformed", err)
				}
				if got != nil {
					t.Errorf("got %d accounts alongside the bound error; want nil", len(got))
				}
				return
			}
			if len(got) != tc.n {
				t.Fatalf("got %d accounts, want all %d", len(got), tc.n)
			}
			if wantReqs := 1 + businesses*pagesPerBusiness; len(stub.seen()) != wantReqs {
				t.Errorf("requests = %d, want %d (every page of both businesses read)", len(stub.seen()), wantReqs)
			}
		})
	}
}

// One account reachable through two businesses is offered once, under the first business.
func TestListAdAccounts_AnAccountUnderTwoBusinessesIsListedOnce(t *testing.T) {
	stub := &discoveryStub{respond: func(_ *discoveryStub, key string, _ int) (int, string) {
		switch key {
		case businessesPath:
			return 200, `{"data":[{"id":"b1","name":"One"},{"id":"b2","name":"Two"}]}`
		case adAccountsPath("b1"):
			return 200, `{"data":[{"id":"t2_shared"}]}`
		default:
			return 200, `{"data":[{"id":"t2_shared"},{"id":"t2_own"}]}`
		}
	}}
	got, err := newDiscoveryClient(t, stub).ListAdAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAdAccounts: %v", err)
	}
	if len(got) != 2 || got[0].ID != "t2_shared" || got[0].BusinessName != "One" || got[1].ID != "t2_own" {
		t.Fatalf("got %+v", got)
	}
}
