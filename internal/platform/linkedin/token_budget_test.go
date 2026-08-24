// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// deadlineTap is a RoundTripper that records the deadline carried by each
// outbound request's context, keyed by whether the request is the OAuth token
// exchange or a Marketing API call.
//
// The deadline is read CLIENT-side, inside the transport: an HTTP request does
// not transmit its context deadline, so a server handler's context never carries
// it. This tap is therefore the only place the two budgets can be compared.
type deadlineTap struct {
	next     http.RoundTripper
	tokenURL string

	mu         sync.Mutex
	tokenDL    time.Time
	tokenHasDL bool
	tokenSeen  bool
	apiDL      time.Time
	apiHasDL   bool
	apiSeen    bool
}

func (d *deadlineTap) RoundTrip(r *http.Request) (*http.Response, error) {
	dl, ok := r.Context().Deadline()
	d.mu.Lock()
	if strings.HasPrefix(r.URL.String(), d.tokenURL) {
		d.tokenDL, d.tokenHasDL, d.tokenSeen = dl, ok, true
	} else {
		d.apiDL, d.apiHasDL, d.apiSeen = dl, ok, true
	}
	d.mu.Unlock()
	return d.next.RoundTrip(r)
}

func (d *deadlineTap) snapshot() (tokenDL time.Time, tokenHasDL, tokenSeen bool, apiDL time.Time, apiHasDL, apiSeen bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tokenDL, d.tokenHasDL, d.tokenSeen, d.apiDL, d.apiHasDL, d.apiSeen
}

// assertBudgetsAreSiblings encodes the property both call paths must hold.
//
// Two independently-bounded operations each receive a fresh requestTimeout, so
// the API deadline lands strictly LATER than the exchange deadline (the exchange
// starts first). A NESTED budget — the defect — makes the exchange context a
// descendant of the API attempt context, so the exchange deadline can never fall
// later than the API one, and the API request keeps only the remainder.
//
// The comparison is between two observed VALUES; it involves no elapsed-time
// measurement and no sleep, so it cannot pass merely because a goroutine was
// unscheduled.
func assertBudgetsAreSiblings(t *testing.T, tap *deadlineTap, what string) {
	t.Helper()
	tokenDL, tokenHasDL, tokenSeen, apiDL, apiHasDL, apiSeen := tap.snapshot()

	if !tokenSeen {
		t.Fatal("no token exchange was performed: the test did not exercise the refresh path it claims to cover")
	}
	if !apiSeen {
		t.Fatalf("%s request was never sent", what)
	}
	if !tokenHasDL {
		t.Fatal("token exchange carried no deadline: the exchange must stay independently bounded")
	}
	if !apiHasDL {
		t.Fatalf("%s request carried no deadline: the per-attempt bound must still apply", what)
	}
	if !apiDL.After(tokenDL) {
		t.Fatalf("%s deadline (%v) is not after the token-exchange deadline (%v): the request "+
			"budget is carved out of token acquisition, so a slow but successful exchange "+
			"leaves the real request little or no time",
			what, apiDL, tokenDL)
	}
}

func refreshOnlyCreds() Credentials {
	return Credentials{RefreshToken: "r", ClientID: "cid", ClientSecret: "sec"}
}

// newTappedClient builds a client whose transport records per-request deadlines.
func newTappedClient(t *testing.T, apiURL, tokenURL string) (*Client, *deadlineTap) {
	t.Helper()
	tap := &deadlineTap{next: http.DefaultTransport, tokenURL: tokenURL}
	c := NewClient(refreshOnlyCreds(), testConfig(),
		WithBaseURL(apiURL),
		withTokenURL(tokenURL),
		WithHTTPClient(&http.Client{Transport: tap}),
	)
	return c, tap
}

func tokenExchangeServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"refreshed","expires_in":3600}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRequestBudgetSurvivesTokenExchange covers the doRequest write/read path.
func TestRequestBudgetSurvivesTokenExchange(t *testing.T) {
	t.Parallel()

	tokenSrv := tokenExchangeServer(t)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"elements":[]}`))
	}))
	t.Cleanup(apiSrv.Close)

	c, tap := newTappedClient(t, apiSrv.URL, tokenSrv.URL)

	// The parent context carries NO deadline, so every deadline observed by the
	// tap was constructed inside the client.
	if _, err := c.doRequest(context.Background(), http.MethodGet, "/adAccounts", nil, nil, nil); err != nil {
		t.Fatalf("doRequest: %v", err)
	}
	assertBudgetsAreSiblings(t, tap, "API")
}

// TestMetricsRequestBudgetSurvivesTokenExchange covers the Ad Analytics metrics
// read path, which constructs its own attempt context.
func TestMetricsRequestBudgetSurvivesTokenExchange(t *testing.T) {
	t.Parallel()

	tokenSrv := tokenExchangeServer(t)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"elements":[]}`))
	}))
	t.Cleanup(apiSrv.Close)

	c, tap := newTappedClient(t, apiSrv.URL, tokenSrv.URL)

	if _, _, _, err := c.doAdAnalyticsAttempt(context.Background(), apiSrv.URL+"/adAnalytics"); err != nil {
		t.Fatalf("doAdAnalyticsAttempt: %v", err)
	}
	assertBudgetsAreSiblings(t, tap, "Ad Analytics")
}
