// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	if !strings.HasPrefix(got, "https://events.lf.org/oss/?") {
		t.Errorf("bad base: %q", got)
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

	// Existing query params are preserved (merged into the query).
	got2 := buildTwitterUtmURL(CampaignInput{
		EventName:       "Event",
		RegistrationURL: "https://x.com/reg?ref=1",
	})
	if !strings.Contains(got2, "ref=1") || !strings.Contains(got2, "utm_source=twitter") {
		t.Errorf("existing query param not preserved alongside utm: %s", got2)
	}

	// A URL fragment must stay at the END (query before #, not inside it).
	got3 := buildTwitterUtmURL(CampaignInput{
		EventName:       "Event",
		RegistrationURL: "https://x.com/reg#details",
	})
	if !strings.HasSuffix(got3, "#details") || strings.Contains(got3, "#details?") {
		t.Errorf("fragment not preserved at end: %s", got3)
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

	resp, err := c.createRequest(context.Background(), "campaigns", map[string]string{"name": "x"})
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

	_, err := c.request(context.Background(), http.MethodGet, "campaigns")
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
	_, err := c.request(ctx, http.MethodGet, "campaigns")
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

	if _, err := c.request(context.Background(), http.MethodGet, "campaigns"); err != nil {
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
		// Well-shaped but impossible calendar dates must be rejected before any
		// mutating call (the regex alone would let these through).
		{BudgetUsd: 100, StartDate: "2026-99-99", EndDate: "2026-12-31"},
		{BudgetUsd: 100, StartDate: "2026-01-01", EndDate: "2026-99-99"},
		{BudgetUsd: 100, StartDate: "2026-02-30", EndDate: "2026-12-31"},
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

	id, err := c.findCampaignByName(context.Background(), "target")
	if err != nil {
		t.Fatalf("findCampaignByName: %v", err)
	}
	if id != "c2" {
		t.Errorf("findCampaignByName across pages = %q, want c2", id)
	}
	if calls != 2 {
		t.Errorf("expected 2 pages fetched, got %d", calls)
	}
}

// TestValidateDateStrict verifies validateDate rejects well-shaped but
// impossible calendar dates (e.g. 2026-99-99) that the shape regex accepts.
func TestValidateDateStrict(t *testing.T) {
	valid := []string{"2026-01-01", "2026-12-31", "2024-02-29"} // 2024 is a leap year
	for _, d := range valid {
		if err := validateDate("start", d); err != nil {
			t.Errorf("validateDate(%q) unexpected error: %v", d, err)
		}
	}
	invalid := []string{"2026-99-99", "2026-13-01", "2026-02-30", "2026-00-10", "bad", "2026-1-1"}
	for _, d := range invalid {
		if err := validateDate("start", d); err == nil {
			t.Errorf("validateDate(%q) expected error, got nil", d)
		}
	}
}

// TestCreateSendsQueryParams verifies X Ads create calls carry their params as
// URL query parameters (not a JSON body), use entity_status=PAUSED (not
// paused=true), and that the line-item create includes the required start_time
// and end_time fields.
func TestCreateSendsQueryParams(t *testing.T) {
	var campaignQuery, lineItemQuery url.Values
	var campaignBodyLen, lineItemBodyLen int64
	var campaignCT, lineItemCT string

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
			campaignQuery = r.URL.Query()
			campaignBodyLen = r.ContentLength
			campaignCT = r.Header.Get("Content-Type")
			_, _ = w.Write([]byte(`{"data":{"id":"cmp1"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "line_items"):
			lineItemQuery = r.URL.Query()
			lineItemBodyLen = r.ContentLength
			lineItemCT = r.Header.Get("Content-Type")
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
	)
	c.nonceFn = func() string { return "n" }
	c.timeFn = staticTime

	if _, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName: "KubeCon EU", Project: "CNCF", BudgetUsd: 500,
		StartDate: "2026-03-01", EndDate: "2026-03-10",
	}); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}

	// Campaign create: params on the query string, entity_status=PAUSED, no JSON body.
	if campaignQuery.Get("name") == "" {
		t.Errorf("campaign create missing name query param: %v", campaignQuery)
	}
	if campaignQuery.Get("funding_instrument_id") != "fi1" {
		t.Errorf("campaign create funding_instrument_id = %q, want fi1", campaignQuery.Get("funding_instrument_id"))
	}
	if campaignQuery.Get("entity_status") != "PAUSED" {
		t.Errorf("campaign create entity_status = %q, want PAUSED", campaignQuery.Get("entity_status"))
	}
	if campaignQuery.Has("paused") {
		t.Errorf("campaign create should not send deprecated paused param: %v", campaignQuery)
	}
	if campaignBodyLen > 0 {
		t.Errorf("campaign create should carry no body, got ContentLength=%d", campaignBodyLen)
	}
	if strings.Contains(campaignCT, "application/json") {
		t.Errorf("campaign create should not set JSON content-type, got %q", campaignCT)
	}

	// Line-item create: required start_time/end_time present, entity_status=PAUSED,
	// bid_strategy (not bid_type), params on the query string.
	if lineItemQuery.Get("start_time") != "2026-03-01T00:00:00Z" {
		t.Errorf("line item start_time = %q, want 2026-03-01T00:00:00Z", lineItemQuery.Get("start_time"))
	}
	if lineItemQuery.Get("end_time") != "2026-03-10T00:00:00Z" {
		t.Errorf("line item end_time = %q, want 2026-03-10T00:00:00Z", lineItemQuery.Get("end_time"))
	}
	if lineItemQuery.Get("entity_status") != "PAUSED" {
		t.Errorf("line item entity_status = %q, want PAUSED", lineItemQuery.Get("entity_status"))
	}
	if lineItemQuery.Get("bid_strategy") != "AUTO" {
		t.Errorf("line item bid_strategy = %q, want AUTO", lineItemQuery.Get("bid_strategy"))
	}
	if lineItemQuery.Has("bid_type") {
		t.Errorf("line item should not send deprecated bid_type param: %v", lineItemQuery)
	}
	if lineItemBodyLen > 0 {
		t.Errorf("line item create should carry no body, got ContentLength=%d", lineItemBodyLen)
	}
	if strings.Contains(lineItemCT, "application/json") {
		t.Errorf("line item create should not set JSON content-type, got %q", lineItemCT)
	}
}

// TestCreateCampaignLookupErrorAborts verifies that a transient 500 during the
// campaign name lookup aborts the flow with an error and does NOT proceed to a
// create POST — so a failed lookup is never treated as "not found" (which would
// create a duplicate).
func TestCreateCampaignLookupErrorAborts(t *testing.T) {
	var postCampaign int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/acc1"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":{"name":"LF Events"}}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "campaigns"):
			// Lookup fails hard.
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"errors":["boom"]}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "campaigns"):
			postCampaign++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":{"id":"should-not-happen"}}`))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[]}`))
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

	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName: "KubeCon EU", Project: "CNCF", BudgetUsd: 500,
		StartDate: "2026-03-01", EndDate: "2026-03-10",
	})
	if err == nil {
		t.Fatal("expected error when campaign lookup fails, got nil")
	}
	if postCampaign != 0 {
		t.Errorf("expected no campaign create POST after lookup failure, got %d", postCampaign)
	}
}
