// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// staticTime is a fixed clock used to make OAuth signing deterministic in tests.
func staticTime() time.Time { return time.Unix(1600000000, 0).UTC() }

// TestGenerateOAuthSignature verifies HMAC-SHA1 signing against a fixed input.
// This is the highest-value test: the signature must be byte-for-byte correct
// or every X Ads API call fails. Inputs are modeled on the RFC 5849 OAuth 1.0a
// worked example (single-valued form of its parameter set). The golden digest
// below is the HMAC-SHA1/base64 over the canonical signature base string these
// exact inputs produce; any conformant OAuth 1.0a implementation reproduces it.
func TestGenerateOAuthSignature(t *testing.T) {
	method := "POST"
	baseURL := "http://example.com/request"
	params := map[string]string{
		"b5":                     "=%3D",
		"a3":                     "a",
		"c@":                     "",
		"a2":                     "r b",
		"oauth_consumer_key":     "9djdj82h48djs9d2",
		"oauth_token":            "kkk9d7dh3k39sjv7",
		"oauth_signature_method": "HMAC-SHA1",
		"oauth_timestamp":        "137131201",
		"oauth_nonce":            "7d8f3e4a",
		"c2":                     "",
	}
	consumerSecret := "j49sk3j29djd"
	tokenSecret := "dh893hdasih9"

	got := generateOAuthSignature(method, baseURL, params, consumerSecret, tokenSecret)

	const want = "0WEKYr+OUMQLH1La8byKezhwJpc="
	if got != want {
		t.Fatalf("signature mismatch:\n got=%q\nwant=%q", got, want)
	}
}

// TestBuildOAuthHeaderDeterministic verifies the full Authorization header with
// injected nonce + timestamp, so the whole signing path is assertable.
func TestBuildOAuthHeaderDeterministic(t *testing.T) {
	c := NewClient(
		Credentials{
			ConsumerKey:       "ck",
			ConsumerSecret:    "cs",
			AccessToken:       "at",
			AccessTokenSecret: "ats",
		},
		AccountConfig{AccountID: "acc1"},
	)
	c.nonceFn = func() string { return "fixednonce" }
	c.timeFn = staticTime

	hdr, err := c.buildOAuthHeader("GET", "https://ads-api.x.com/12/accounts/acc1", nil)
	if err != nil {
		t.Fatalf("buildOAuthHeader: %v", err)
	}
	if !strings.HasPrefix(hdr, "OAuth ") {
		t.Fatalf("header missing OAuth prefix: %q", hdr)
	}
	for _, want := range []string{
		`oauth_consumer_key="ck"`,
		`oauth_nonce="fixednonce"`,
		`oauth_signature_method="HMAC-SHA1"`,
		`oauth_timestamp="1600000000"`,
		`oauth_token="at"`,
		`oauth_version="1.0"`,
		`oauth_signature="`,
	} {
		if !strings.Contains(hdr, want) {
			t.Errorf("header missing %q\nfull: %s", want, hdr)
		}
	}
}

func TestPercentEncode(t *testing.T) {
	cases := map[string]string{
		"a b":        "a%20b",
		"r b":        "r%20b",
		"=%3D":       "%3D%253D",
		"AZaz09-._~": "AZaz09-._~",
		"!'()*":      "%21%27%28%29%2A",
		"a3":         "a3",
		"c@":         "c%40",
	}
	for in, want := range cases {
		if got := percentEncode(in); got != want {
			t.Errorf("percentEncode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMicroCurrencyRoundTrip(t *testing.T) {
	cases := []struct {
		usd   float64
		micro int64
	}{
		{1, 1_000_000},
		{0.01, 10_000},
		{100.50, 100_500_000},
		{1234.56, 1_234_560_000},
	}
	for _, tc := range cases {
		if got := toMicroCurrency(tc.usd); got != tc.micro {
			t.Errorf("toMicroCurrency(%v) = %d, want %d", tc.usd, got, tc.micro)
		}
		if got := fromMicroCurrency(tc.micro); got != tc.usd {
			t.Errorf("fromMicroCurrency(%d) = %v, want %v", tc.micro, got, tc.usd)
		}
	}

	// Rounding: 0.1 * 1_000_000 must round cleanly to 100000.
	if got := toMicroCurrency(0.1); got != 100_000 {
		t.Errorf("toMicroCurrency(0.1) = %d, want 100000", got)
	}
}

func TestToIso8601Utc(t *testing.T) {
	if got := toIso8601Utc("2026-07-09"); got != "2026-07-09T00:00:00Z" {
		t.Errorf("toIso8601Utc = %q", got)
	}
}

func TestBuildTwitterCampaignName(t *testing.T) {
	got := buildTwitterCampaignName(CampaignInput{EventName: "KubeCon | EU", Project: ""})
	want := "Events | KubeCon - EU | Global | Awareness | Prospecting | Promoted Post | Linux Foundation | MoFU"
	if got != want {
		t.Errorf("campaign name = %q, want %q", got, want)
	}
}

func TestBuildTwitterUtmURL(t *testing.T) {
	got := buildTwitterUtmURL(CampaignInput{
		EventName:       "Open Source Summit",
		RegistrationURL: "https://events.lf.org/oss/",
	})
	if !strings.HasPrefix(got, "https://events.lf.org/oss?") {
		t.Errorf("trailing slash not stripped / bad base: %q", got)
	}
	for _, want := range []string{
		"utm_source=twitter",
		"utm_medium=paid-social",
		"utm_campaign=open-source-summit",
		"utm_term=open-source-summit",
		"utm_content=promoted-tweet",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("utm url missing %q: %s", want, got)
		}
	}

	// Existing query string uses & separator.
	got2 := buildTwitterUtmURL(CampaignInput{
		EventName:       "Event",
		RegistrationURL: "https://x.com/reg?ref=1",
	})
	if !strings.Contains(got2, "?ref=1&") {
		t.Errorf("expected & separator when query already present: %s", got2)
	}
}

// TestRetryOn429 verifies the client retries after a 429 then succeeds.
func TestRetryOn429(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"id":"cmp123"}}`))
	}))
	defer srv.Close()

	c := NewClient(
		Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
		AccountConfig{AccountID: "acc1"},
		WithBaseURL(srv.URL),
		WithAPIVersion("12"),
	)
	c.nonceFn = func() string { return "n" }
	c.timeFn = staticTime

	resp, err := c.request(context.Background(), http.MethodPost, "campaigns", map[string]any{"name": "x"})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if calls != 2 {
		t.Errorf("expected 2 calls (1 retry), got %d", calls)
	}
	if id := extractID(resp); id != "cmp123" {
		t.Errorf("extractID = %q, want cmp123", id)
	}
}

// TestParseRetryAfter covers both headers: Retry-After is a delay in seconds;
// X-Rate-Limit-Reset is a Unix epoch that must be converted to time-until-reset
// (not treated as a raw delay, which would always saturate the cap).
func TestParseRetryAfter(t *testing.T) {
	c := NewClient(Credentials{}, AccountConfig{})
	c.timeFn = staticTime // now = epoch 1600000000

	mk := func(h map[string]string) *http.Response {
		r := &http.Response{Header: http.Header{}}
		for k, v := range h {
			r.Header.Set(k, v)
		}
		return r
	}

	// Retry-After: 5 -> 5s.
	if got := c.parseRetryAfter(mk(map[string]string{"Retry-After": "5"})); got != 5*time.Second {
		t.Errorf("Retry-After=5: got %v, want 5s", got)
	}
	// X-Rate-Limit-Reset 30s in the future -> ~30s, NOT a decades-long duration.
	reset := strconv.FormatInt(staticTime().Unix()+30, 10)
	if got := c.parseRetryAfter(mk(map[string]string{"X-Rate-Limit-Reset": reset})); got != 30*time.Second {
		t.Errorf("X-Rate-Limit-Reset(+30s): got %v, want 30s", got)
	}
	// A reset already in the past -> 0 (fall back to backoff), never negative.
	past := strconv.FormatInt(staticTime().Unix()-10, 10)
	if got := c.parseRetryAfter(mk(map[string]string{"X-Rate-Limit-Reset": past})); got != 0 {
		t.Errorf("X-Rate-Limit-Reset(past): got %v, want 0", got)
	}
	// No headers -> 0.
	if got := c.parseRetryAfter(mk(nil)); got != 0 {
		t.Errorf("no headers: got %v, want 0", got)
	}
}

// TestRetryExhausted verifies persistent 429s exhaust retries and error out.
func TestRetryExhausted(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := NewClient(
		Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
		AccountConfig{AccountID: "acc1"},
		WithBaseURL(srv.URL),
	)
	c.nonceFn = func() string { return "n" }
	c.timeFn = staticTime

	_, err := c.request(context.Background(), http.MethodGet, "campaigns", nil)
	if err == nil {
		t.Fatal("expected error after exhausted retries")
	}
	// retryMax=3 -> attempts 0..3 that all 429; last attempt (==retryMax) does
	// not sleep/retry, so total server hits = retryMax+1 = 4.
	if calls != retryMax+1 {
		t.Errorf("expected %d calls, got %d", retryMax+1, calls)
	}
	if !strings.Contains(err.Error(), "429") && !strings.Contains(err.Error(), "exhausted") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestContextCancellationDuringRetry verifies ctx cancellation aborts the
// backoff sleep rather than blocking.
func TestContextCancellationDuringRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	// Serve a 429 with a long Retry-After so the client enters the backoff
	// sleep, and cancel the context as the first request lands — so cancellation
	// is observed inside sleepCtx (not at the initial httpClient.Do).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		cancel()
	}))
	defer srv.Close()

	c := NewClient(
		Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
		AccountConfig{AccountID: "acc1"},
		WithBaseURL(srv.URL),
	)
	c.nonceFn = func() string { return "n" }
	c.timeFn = staticTime

	start := time.Now()
	_, err := c.request(ctx, http.MethodGet, "campaigns", nil)
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
	// The 30s Retry-After sleep must have been aborted by cancellation, not slept.
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("expected fast return via cancelled backoff, took %v", elapsed)
	}
}

// TestRequestSetsAuthHeader verifies each request carries an OAuth header.
func TestRequestSetsAuthHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"id":"1"}}`))
	}))
	defer srv.Close()

	c := NewClient(
		Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
		AccountConfig{AccountID: "acc1"},
		WithBaseURL(srv.URL),
	)
	c.nonceFn = func() string { return "n" }
	c.timeFn = staticTime

	if _, err := c.request(context.Background(), http.MethodGet, "campaigns", nil); err != nil {
		t.Fatalf("request: %v", err)
	}
	if !strings.HasPrefix(gotAuth, "OAuth ") {
		t.Errorf("missing OAuth authorization header: %q", gotAuth)
	}
}

// TestCreateCampaignValidation covers the input validation guards.
func TestCreateCampaignValidation(t *testing.T) {
	c := NewClient(Credentials{}, AccountConfig{})
	cases := []CampaignInput{
		{BudgetUsd: 0, StartDate: "2026-01-01", EndDate: "2026-01-02"},
		{BudgetUsd: 100, StartDate: "bad", EndDate: "2026-01-02"},
		{BudgetUsd: 100, StartDate: "2026-01-01", EndDate: "bad"},
		{BudgetUsd: 100, StartDate: "2026-01-02", EndDate: "2026-01-01"},
	}
	for i, in := range cases {
		if _, err := c.CreateCampaign(context.Background(), in); err == nil {
			t.Errorf("case %d: expected validation error", i)
		}
	}
}

// TestCreateCampaignFlow exercises the full create flow against a fake server:
// account verify -> lookup (empty) -> create campaign -> create line item ->
// create promoted tweet.
func TestCreateCampaignFlow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/acc1"):
			_, _ = w.Write([]byte(`{"data":{"name":"LF Events"}}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "campaigns"):
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "line_items"):
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "campaigns"):
			_, _ = w.Write([]byte(`{"data":{"id":"cmp1"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "line_items"):
			_, _ = w.Write([]byte(`{"data":{"id":"li1"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "promoted_tweets"):
			_, _ = w.Write([]byte(`{"data":[{"id":"pt1"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewClient(
		Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
		AccountConfig{AccountID: "acc1", FundingInstrumentID: "fi1"},
		WithBaseURL(srv.URL),
		// Speed up: no real write delay needed since server is instant; the
		// 1s delay still runs but is acceptable for a single test.
	)
	c.nonceFn = func() string { return "n" }
	c.timeFn = staticTime

	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "KubeCon EU",
		Project:         "CNCF",
		BudgetUsd:       500,
		StartDate:       "2026-03-01",
		EndDate:         "2026-03-10",
		TweetID:         "1234567890",
		RegistrationURL: "https://events.lf.org/kubecon",
	})
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if res.CampaignID != "cmp1" || res.LineItemID != "li1" || res.PromotedTweetID != "pt1" {
		t.Errorf("unexpected ids: %+v", res)
	}
	if res.Platform != "twitter-ads" || res.TwitterURL != AdsManagerURL {
		t.Errorf("unexpected metadata: %+v", res)
	}
	if len(res.Steps) == 0 {
		t.Error("expected step log entries")
	}
}

// TestCreateCampaignIdempotent verifies existing entities are reused by name.
func TestCreateCampaignIdempotent(t *testing.T) {
	campaignName := buildTwitterCampaignName(CampaignInput{EventName: "KubeCon EU", Project: "CNCF"})
	lineItemName := "Events | KubeCon EU | Promoted Tweets | AUTO"

	var postCampaign, postLineItem int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/acc1"):
			_, _ = w.Write([]byte(`{"data":{"name":"LF Events"}}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "campaigns"):
			b, _ := json.Marshal(map[string]any{"data": []map[string]string{{"id": "existingCmp", "name": campaignName}}})
			_, _ = w.Write(b)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "line_items"):
			b, _ := json.Marshal(map[string]any{"data": []map[string]string{{"id": "existingLi", "name": lineItemName}}})
			_, _ = w.Write(b)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "campaigns"):
			postCampaign++
			_, _ = w.Write([]byte(`{"data":{"id":"new"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "line_items"):
			postLineItem++
			_, _ = w.Write([]byte(`{"data":{"id":"new"}}`))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[{"id":"pt1"}]}`))
		}
	}))
	defer srv.Close()

	c := NewClient(
		Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
		AccountConfig{AccountID: "acc1", FundingInstrumentID: "fi1"},
		WithBaseURL(srv.URL),
	)
	c.nonceFn = func() string { return "n" }
	c.timeFn = staticTime

	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName: "KubeCon EU", Project: "CNCF", BudgetUsd: 500,
		StartDate: "2026-03-01", EndDate: "2026-03-10",
	})
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if res.CampaignID != "existingCmp" || res.LineItemID != "existingLi" {
		t.Errorf("expected reuse, got %+v", res)
	}
	if postCampaign != 0 || postLineItem != 0 {
		t.Errorf("expected no create POSTs, got campaign=%d lineItem=%d", postCampaign, postLineItem)
	}
}

// TestFindByNamePagination verifies name lookups follow next_cursor so a match
// on the second page is found (idempotency must not break past page 1).
func TestFindByNamePagination(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("cursor") == "" {
			// page 1: no match, hand back a cursor.
			_, _ = w.Write([]byte(`{"data":[{"id":"c1","name":"other"}],"next_cursor":"CURSOR2"}`))
			return
		}
		// page 2: the match, no further cursor.
		_, _ = w.Write([]byte(`{"data":[{"id":"c2","name":"target"}]}`))
	}))
	defer srv.Close()

	c := NewClient(
		Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
		AccountConfig{AccountID: "acc1"},
		WithBaseURL(srv.URL),
	)
	c.nonceFn = func() string { return "n" }
	c.timeFn = staticTime

	if id := c.findCampaignByName(context.Background(), "target"); id != "c2" {
		t.Errorf("findCampaignByName across pages = %q, want c2", id)
	}
	if calls != 2 {
		t.Errorf("expected 2 pages fetched, got %d", calls)
	}
}
