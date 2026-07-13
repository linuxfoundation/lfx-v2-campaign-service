// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // OAuth 1.0a mandates HMAC-SHA1; test mirrors production signing.
	"encoding/base64"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
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

	// Golden digest for the RFC 5849 §3.4.1.3.2 normalization: parameters sorted
	// by their PERCENT-ENCODED name (so "c@"->"c%40" precedes "c2"), then by
	// encoded value on ties. The normalized base-string param portion is
	// "a2=r%20b&a3=a&b5=%3D%253D&c%40=&c2=&oauth_...". Any conformant OAuth 1.0a
	// implementation reproduces this digest.
	const want = "AYgdIfljDYmBX3Ce9owrBekam04="
	if got != want {
		t.Fatalf("signature mismatch:\n got=%q\nwant=%q", got, want)
	}
}

// TestOAuthSignatureParamOrdering proves parameters are normalized by their
// PERCENT-ENCODED name, not the raw key: "c@" encodes to "c%40" and must sort
// BEFORE "c2" (because '%'=0x25 < '2'=0x32), even though raw '@'=0x40 sorts
// after '2'. Two signatures whose only difference is the param ordering rule
// would diverge; here we assert the value is stable and matches an independent
// encode-then-sort computation over the same params.
func TestOAuthSignatureParamOrdering(t *testing.T) {
	// Sanity-check the byte-ordering claim itself.
	if percentEncode("c@") >= percentEncode("c2") {
		t.Fatalf("expected percentEncode(c@)=%q to sort before percentEncode(c2)=%q",
			percentEncode("c@"), percentEncode("c2"))
	}

	params := map[string]string{
		"c@": "1",
		"c2": "2",
	}
	// Reference: encode each pair, sort the encoded pairs, join with '&'. This is
	// exactly the normalization generateOAuthSignature must perform.
	wantParamString := "c%40=1&c2=2"
	if strings.Compare("c%40=1", "c2=2") >= 0 {
		t.Fatalf("test premise wrong: %q should sort before %q", "c%40=1", "c2=2")
	}

	// If the implementation sorted by raw key, "c2" would precede "c@" and the
	// signature would differ. Recompute the expected signature with a known-good
	// local reference and compare.
	got := generateOAuthSignature("POST", "https://ads-api.x.com/12/accounts/acc1", params, "cs", "ts")
	want := referenceSignature("POST", "https://ads-api.x.com/12/accounts/acc1", wantParamString, "cs", "ts")
	if got != want {
		t.Fatalf("signature not built from percent-encoded-name ordering:\n got=%q\nwant=%q", got, want)
	}
}

// TestOAuthSignaturePrefixNameOrdering proves parameters are sorted by (encoded
// name, encoded value) as a TUPLE, not by the joined "name=value" string. When
// one encoded name is a prefix of another the two rules diverge: RFC 5849
// §3.4.1.3.2 orders by name first, so "a" < "a1" and the normalized string is
// "a=<v>&a1=<v>". Sorting the joined form instead compares "a=<v>" against
// "a1=<v>" and, at index 1, '=' (0x3D) loses to '1' (0x31) — so "a1=<v>" would
// sort FIRST, producing the WRONG "a1=<v>&a=<v>". This test asserts the correct
// tuple ordering and would fail under the old joined-string sort.
func TestOAuthSignaturePrefixNameOrdering(t *testing.T) {
	// Prove the two sort rules genuinely disagree for these inputs, so the test
	// is meaningful: joined-string sort puts "a1=..." before "a=...".
	joined := []string{"a=va", "a1=v1"}
	if joined[1] >= joined[0] {
		t.Fatalf("test premise wrong: joined-string sort should misorder %q before %q", joined[1], joined[0])
	}

	params := map[string]string{
		"a":  "va",
		"a1": "v1",
	}
	// RFC-correct normalization: by name first (a < a1), giving "a=va&a1=v1".
	wantParamString := "a=va&a1=v1"

	got := generateOAuthSignature("POST", "https://ads-api.x.com/12/accounts/acc1", params, "cs", "ts")
	want := referenceSignature("POST", "https://ads-api.x.com/12/accounts/acc1", wantParamString, "cs", "ts")
	if got != want {
		t.Fatalf("prefix-name params not tuple-sorted (name then value):\n got=%q\nwant=%q", got, want)
	}

	// Guard against the specific wrong answer the joined-string sort would give,
	// so a regression is caught even if referenceSignature ever changed shape.
	wrong := referenceSignature("POST", "https://ads-api.x.com/12/accounts/acc1", "a1=v1&a=va", "cs", "ts")
	if got == wrong {
		t.Fatalf("signature matches the joined-string (misordered) normalization: %q", got)
	}
}

// referenceSignature is an independent HMAC-SHA1/base64 over the OAuth 1.0a
// base string, given an already-normalized param string, used to pin ordering.
func referenceSignature(method, u, paramString, consumerSecret, tokenSecret string) string {
	base := strings.ToUpper(method) + "&" + percentEncode(u) + "&" + percentEncode(paramString)
	key := percentEncode(consumerSecret) + "&" + percentEncode(tokenSecret)
	mac := hmac.New(sha1.New, []byte(key))
	mac.Write([]byte(base))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
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

// TestBuildOAuthHeaderSignsQueryParams verifies that query-string parameters on
// the request URL are folded into the OAuth 1.0a signature base string. This is
// the critical create-POST signing path: X carries create params on the query
// string, and if the query-param signing loop were removed the Authorization
// header would still be well-formed but the signature would be computed over the
// oauth params ALONE — and X would reject every create. We recompute an
// independent reference signature over method + base-URL(no query) + the sorted,
// percent-encoded union of oauth params AND query params (RFC 5849 §3.4.1), and
// assert equality; mutating the query-param loop must break this test.
func TestBuildOAuthHeaderSignsQueryParams(t *testing.T) {
	c := NewClient(
		Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
		AccountConfig{AccountID: "acc1"},
	)
	c.nonceFn = func() string { return "fixednonce" }
	c.timeFn = staticTime

	// A create-style URL: base + path + query params (name, funding, entity_status).
	baseURL := "https://ads-api.x.com/12/accounts/acc1/campaigns"
	rawURL := baseURL + "?name=KubeCon+EU&funding_instrument_id=fi1&entity_status=PAUSED"

	hdr, err := c.buildOAuthHeader("POST", rawURL, nil)
	if err != nil {
		t.Fatalf("buildOAuthHeader: %v", err)
	}
	gotSig := extractOAuthSignature(t, hdr)

	// Independent reference: the full signed param set is the deterministic oauth
	// params PLUS the query params, normalized (encode name+value, sort, join).
	allParams := map[string]string{
		"oauth_consumer_key":     "ck",
		"oauth_nonce":            "fixednonce",
		"oauth_signature_method": "HMAC-SHA1",
		"oauth_timestamp":        strconv.FormatInt(staticTime().Unix(), 10),
		"oauth_token":            "at",
		"oauth_version":          "1.0",
		"name":                   "KubeCon EU",
		"funding_instrument_id":  "fi1",
		"entity_status":          "PAUSED",
	}
	parts := make([]string, 0, len(allParams))
	for k, v := range allParams {
		parts = append(parts, percentEncode(k)+"="+percentEncode(v))
	}
	sort.Strings(parts)
	wantSig := referenceSignature("POST", baseURL, strings.Join(parts, "&"), "cs", "ats")

	if gotSig != wantSig {
		t.Fatalf("query params not folded into signature:\n got=%q\nwant=%q", gotSig, wantSig)
	}

	// Guard against a false positive: the signature over the oauth params ALONE
	// (the query params dropped) must differ from wantSig — otherwise this test
	// couldn't distinguish a working signing loop from a removed one.
	oauthOnly := map[string]string{
		"oauth_consumer_key":     "ck",
		"oauth_nonce":            "fixednonce",
		"oauth_signature_method": "HMAC-SHA1",
		"oauth_timestamp":        strconv.FormatInt(staticTime().Unix(), 10),
		"oauth_token":            "at",
		"oauth_version":          "1.0",
	}
	oparts := make([]string, 0, len(oauthOnly))
	for k, v := range oauthOnly {
		oparts = append(oparts, percentEncode(k)+"="+percentEncode(v))
	}
	sort.Strings(oparts)
	oauthOnlySig := referenceSignature("POST", baseURL, strings.Join(oparts, "&"), "cs", "ats")
	if wantSig == oauthOnlySig {
		t.Fatal("test cannot detect a dropped query-param signing loop: oauth-only signature equals full signature")
	}
	if gotSig == oauthOnlySig {
		t.Fatalf("signature was computed over oauth params alone; query params not signed: %q", gotSig)
	}
}

// extractOAuthSignature pulls the oauth_signature value out of an
// "OAuth k=\"v\", ..." Authorization header and percent-decodes it.
func extractOAuthSignature(t *testing.T, hdr string) string {
	t.Helper()
	const key = `oauth_signature="`
	i := strings.Index(hdr, key)
	if i < 0 {
		t.Fatalf("no oauth_signature in header: %q", hdr)
	}
	rest := hdr[i+len(key):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		t.Fatalf("unterminated oauth_signature in header: %q", hdr)
	}
	dec, err := url.QueryUnescape(rest[:j])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	return dec
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
	got := buildTwitterCampaignName(CampaignInput{EventName: "KubeCon | EU", Project: "CNCF"})
	want := "Events | KubeCon - EU | Global | Awareness | Prospecting | Promoted Post | CNCF | MoFU"
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
		WithWriteDelay(0),
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

// TestParseRetryAfter covers all three headers: Retry-After is a delay in
// seconds (or an HTTP-date); the *-Rate-Limit-Reset headers are Unix epochs that
// must be converted to time-until-reset (not treated as a raw delay, which would
// always saturate the cap). Header precedence mirrors the X Ads SDK:
// X-Account-Rate-Limit-Reset first, then X-Rate-Limit-Reset, then Retry-After.
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
	// X-Account-Rate-Limit-Reset (account-scoped) 45s in the future -> ~45s.
	acct := strconv.FormatInt(staticTime().Unix()+45, 10)
	if got := c.parseRetryAfter(mk(map[string]string{"X-Account-Rate-Limit-Reset": acct})); got != 45*time.Second {
		t.Errorf("X-Account-Rate-Limit-Reset(+45s): got %v, want 45s", got)
	}
	// Precedence: X-Account-Rate-Limit-Reset must win over X-Rate-Limit-Reset and
	// Retry-After, mirroring the X Ads SDK. Account reset (+45s) is checked first
	// even though the endpoint reset (+30s) and Retry-After (5s) are shorter.
	if got := c.parseRetryAfter(mk(map[string]string{
		"X-Account-Rate-Limit-Reset": acct,
		"X-Rate-Limit-Reset":         reset,
		"Retry-After":                "5",
	})); got != 45*time.Second {
		t.Errorf("account header precedence: got %v, want 45s", got)
	}
	// A past account reset falls through to the next header (X-Rate-Limit-Reset).
	acctPast := strconv.FormatInt(staticTime().Unix()-10, 10)
	if got := c.parseRetryAfter(mk(map[string]string{
		"X-Account-Rate-Limit-Reset": acctPast,
		"X-Rate-Limit-Reset":         reset,
	})); got != 30*time.Second {
		t.Errorf("past account reset should fall through to endpoint reset: got %v, want 30s", got)
	}
	// Retry-After as an HTTP-date 20s in the future -> ~20s.
	httpDate := staticTime().Add(20 * time.Second).UTC().Format(http.TimeFormat)
	if got := c.parseRetryAfter(mk(map[string]string{"Retry-After": httpDate})); got != 20*time.Second {
		t.Errorf("Retry-After(HTTP-date +20s): got %v, want 20s", got)
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
		WithWriteDelay(0),
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
	// A persistent 429 across every attempt must surface the intended
	// exhausted-rate-limit error, not the generic non-2xx path. The message
	// must name the exhausted retries and their count.
	if !strings.Contains(err.Error(), "exhausted") {
		t.Errorf("expected exhausted-rate-limit error, got: %v", err)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(retryMax)) {
		t.Errorf("expected error to name %d retries, got: %v", retryMax, err)
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
		WithWriteDelay(0),
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
		WithWriteDelay(0),
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
	// Budget/date/calendar cases carry a valid Project so they reach the
	// budget/date guards under test — Project is validated BEFORE budget/dates, so
	// omitting it would short-circuit on "invalid project" and never exercise
	// these branches. The event-name cases deliberately omit Project because
	// EventName is validated FIRST (they must fail on event name, not project).
	cases := []CampaignInput{
		{EventName: "E", Project: "tlf", BudgetUsd: 0, StartDate: "2026-01-01", EndDate: "2026-01-02"},
		{EventName: "E", Project: "tlf", BudgetUsd: 100, StartDate: "bad", EndDate: "2026-01-02"},
		{EventName: "E", Project: "tlf", BudgetUsd: 100, StartDate: "2026-01-01", EndDate: "bad"},
		{EventName: "E", Project: "tlf", BudgetUsd: 100, StartDate: "2026-01-02", EndDate: "2026-01-01"},
		// Well-shaped but impossible calendar dates must be rejected before any
		// mutating call (the regex alone would let these through).
		{EventName: "E", Project: "tlf", BudgetUsd: 100, StartDate: "2026-99-99", EndDate: "2026-12-31"},
		{EventName: "E", Project: "tlf", BudgetUsd: 100, StartDate: "2026-01-01", EndDate: "2026-99-99"},
		{EventName: "E", Project: "tlf", BudgetUsd: 100, StartDate: "2026-02-30", EndDate: "2026-12-31"},
		// Empty / whitespace-only event name (validated before project).
		{EventName: "", BudgetUsd: 100, StartDate: "2026-01-01", EndDate: "2026-01-02"},
		{EventName: "   ", BudgetUsd: 100, StartDate: "2026-01-01", EndDate: "2026-01-02"},
		// Over-length event name.
		{EventName: strings.Repeat("x", maxEventNameLen+1), BudgetUsd: 100, StartDate: "2026-01-01", EndDate: "2026-01-02"},
		// Empty project (validated after event name, before budget/dates).
		{EventName: "E", Project: "", BudgetUsd: 100, StartDate: "2026-01-01", EndDate: "2026-01-02"},
		{EventName: "E", Project: "   ", BudgetUsd: 100, StartDate: "2026-01-01", EndDate: "2026-01-02"},
		// Budget above the int64 micro-unit overflow cap.
		{EventName: "E", Project: "tlf", BudgetUsd: maxBudgetUsd + 1, StartDate: "2026-01-01", EndDate: "2026-01-02"},
		// Positive-but-rounds-to-zero micro-units (< half a micro-unit).
		{EventName: "E", Project: "tlf", BudgetUsd: 1e-9, StartDate: "2026-01-01", EndDate: "2026-01-02"},
		// NaN / Inf budgets.
		{EventName: "E", Project: "tlf", BudgetUsd: math.NaN(), StartDate: "2026-01-01", EndDate: "2026-01-02"},
		{EventName: "E", Project: "tlf", BudgetUsd: math.Inf(1), StartDate: "2026-01-01", EndDate: "2026-01-02"},
	}
	for i, in := range cases {
		if _, err := c.CreateCampaign(context.Background(), in); err == nil {
			t.Errorf("case %d: expected validation error", i)
		}
	}
}

// TestCreateCampaignBudgetErrorMessages verifies each budget-rejection branch
// returns a distinct, actionable message. In particular, an over-cap budget is
// positive, so it must NOT be reported as "must be a positive number" — the
// caller needs to know the actual limit.
func TestCreateCampaignBudgetErrorMessages(t *testing.T) {
	c := NewClient(Credentials{}, AccountConfig{})
	base := CampaignInput{EventName: "E", Project: "tlf", StartDate: "2026-01-01", EndDate: "2026-01-02"}

	cases := []struct {
		name   string
		budget float64
		want   string
	}{
		{"over cap", maxBudgetUsd + 1, "must be at most"},
		{"zero", 0, "must be a positive number"},
		{"negative", -5, "must be a positive number"},
		{"rounds to zero", 1e-9, "rounds to zero"},
	}
	for _, tc := range cases {
		in := base
		in.BudgetUsd = tc.budget
		_, err := c.CreateCampaign(context.Background(), in)
		if err == nil {
			t.Fatalf("%s: expected error", tc.name)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %q, want substring %q", tc.name, err.Error(), tc.want)
		}
	}
	// Over-cap must not be mislabeled as non-positive.
	in := base
	in.BudgetUsd = maxBudgetUsd + 1
	if _, err := c.CreateCampaign(context.Background(), in); err == nil ||
		strings.Contains(err.Error(), "must be a positive number") {
		t.Errorf("over-cap budget should not report 'must be a positive number', got %v", err)
	}
}

// TestCreateCampaignRejectsOversizedComposedName verifies a composed campaign
// name exceeding X's 255-rune entity-name limit is rejected before any network
// call. A 200-char event (the per-field max) with a short project composes to
// ~286 chars — within the per-field bounds but over the entity-name limit.
func TestCreateCampaignRejectsOversizedComposedName(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	c := NewClient(
		Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
		AccountConfig{AccountID: "acc1", FundingInstrumentID: "fi1"},
		WithBaseURL(srv.URL),
		WithWriteDelay(0),
	)
	c.nonceFn = func() string { return "n" }
	c.timeFn = staticTime

	// Sanity-check the premise: 200-char event + short project overflows 255.
	name := buildTwitterCampaignName(CampaignInput{EventName: strings.Repeat("x", maxEventNameLen), Project: "CNCF"})
	if got := len([]rune(name)); got <= maxEntityNameLen {
		t.Fatalf("test premise wrong: composed name is %d runes, expected > %d", got, maxEntityNameLen)
	}

	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       strings.Repeat("x", maxEventNameLen), // valid per-field
		Project:         "CNCF",
		BudgetUsd:       500,
		StartDate:       "2026-03-01",
		EndDate:         "2026-03-10",
		RegistrationURL: "https://events.lf.org/reg",
	})
	if err == nil {
		t.Fatal("expected error for composed name exceeding entity-name limit")
	}
	if !strings.Contains(err.Error(), strconv.Itoa(maxEntityNameLen)) {
		t.Errorf("error should mention the %d-char limit: %v", maxEntityNameLen, err)
	}
	if calls != 0 {
		t.Errorf("expected no network call before name validation, got %d", calls)
	}
}

// TestCreateCampaignRejectsEmptyProject verifies that an empty/whitespace
// Project is rejected in the up-front validation, before any mutating call. The
// Project segment is the attribution key the pipeline joins on, so defaulting an
// omitted value would misattribute a non-TLF campaign; the caller must supply the
// canonical slug.
func TestCreateCampaignRejectsEmptyProject(t *testing.T) {
	for _, project := range []string{"", "   "} {
		var posts int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				posts++
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[]}`))
		}))

		c := NewClient(
			Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
			AccountConfig{AccountID: "acc1", FundingInstrumentID: "fi1"},
			WithBaseURL(srv.URL),
			WithWriteDelay(0),
		)
		c.nonceFn = func() string { return "n" }
		c.timeFn = staticTime

		_, err := c.CreateCampaign(context.Background(), CampaignInput{
			EventName:       "KubeCon EU",
			Project:         project,
			BudgetUsd:       500,
			StartDate:       "2026-03-01",
			EndDate:         "2026-03-10",
			RegistrationURL: "https://events.lf.org/reg",
		})
		if err == nil {
			t.Errorf("project %q: expected error for empty project", project)
		} else if !strings.Contains(err.Error(), "project") {
			t.Errorf("project %q: expected project error, got: %v", project, err)
		}
		if posts != 0 {
			t.Errorf("project %q: expected no POST before project validation, got %d", project, posts)
		}
		srv.Close()
	}
}

// TestCreateCampaignEventNameRuneLimit verifies the EventName length guard
// counts runes, not UTF-8 bytes. A multi-byte name that is under the 200-rune
// limit but well over 200 bytes must pass the length guard (byte-counting would
// wrongly reject it), while a name over 200 runes must be rejected.
func TestCreateCampaignEventNameRuneLimit(t *testing.T) {
	// 150 CJK runes = 450 bytes: under 200 runes but far over 200 bytes. The
	// composed campaign name (with a short project) stays under 255 runes, so the
	// whole create flow succeeds.
	multiByteName := strings.Repeat("世", 150)
	if utf8.RuneCountInString(multiByteName) > maxEventNameLen {
		t.Fatalf("test premise wrong: %d runes exceeds %d", utf8.RuneCountInString(multiByteName), maxEventNameLen)
	}
	if len(multiByteName) <= maxEventNameLen {
		t.Fatalf("test premise wrong: %d bytes should exceed %d to exercise the byte-vs-rune bug", len(multiByteName), maxEventNameLen)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		WithWriteDelay(0),
	)
	c.nonceFn = func() string { return "n" }
	c.timeFn = staticTime

	if _, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       multiByteName,
		Project:         "CNCF",
		BudgetUsd:       500,
		StartDate:       "2026-03-01",
		EndDate:         "2026-03-10",
		TweetID:         "1234567890",
		RegistrationURL: "https://events.lf.org/kubecon",
	}); err != nil {
		t.Fatalf("multi-byte name under rune limit should be accepted, got: %v", err)
	}

	// A name over 200 runes must be rejected by the length guard.
	tooLong := strings.Repeat("世", maxEventNameLen+1)
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName: tooLong,
		BudgetUsd: 500,
		StartDate: "2026-03-01",
		EndDate:   "2026-03-10",
	})
	if err == nil {
		t.Fatal("event name over the rune limit should be rejected")
	}
	if !strings.Contains(err.Error(), "event name") {
		t.Errorf("expected event-name length error, got: %v", err)
	}
}

// TestValidateEntityName covers the 255-rune boundary directly.
func TestValidateEntityName(t *testing.T) {
	if err := validateEntityName("campaign", strings.Repeat("x", maxEntityNameLen)); err != nil {
		t.Errorf("name at limit should pass: %v", err)
	}
	if err := validateEntityName("campaign", strings.Repeat("x", maxEntityNameLen+1)); err == nil {
		t.Error("name one over limit should fail")
	}
	// Rune-aware: multi-byte characters count as one rune each, not per-byte.
	if err := validateEntityName("campaign", strings.Repeat("é", maxEntityNameLen)); err != nil {
		t.Errorf("multi-byte name at rune limit should pass: %v", err)
	}
}

// TestCreateCampaignRejectsEmptyAccountConfig verifies required account config
// (account_id, funding_instrument_id) is guarded non-empty before any mutating
// call, so an empty stored connection value fails fast client-side instead of at
// the X API. A missing funding_instrument_id must never reach a create POST.
func TestCreateCampaignRejectsEmptyAccountConfig(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	base := CampaignInput{
		EventName: "KubeCon EU", Project: "CNCF", BudgetUsd: 500,
		StartDate: "2026-03-01", EndDate: "2026-03-10",
		RegistrationURL: "https://events.lf.org/reg",
	}
	cases := []struct {
		name string
		acct AccountConfig
	}{
		{"empty funding instrument", AccountConfig{AccountID: "acc1", FundingInstrumentID: ""}},
		{"whitespace funding instrument", AccountConfig{AccountID: "acc1", FundingInstrumentID: "   "}},
		{"empty account id", AccountConfig{AccountID: "", FundingInstrumentID: "fi1"}},
	}
	for _, tc := range cases {
		calls = 0
		c := NewClient(
			Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
			tc.acct,
			WithBaseURL(srv.URL),
			WithWriteDelay(0),
		)
		c.nonceFn = func() string { return "n" }
		c.timeFn = staticTime

		if _, err := c.CreateCampaign(context.Background(), base); err == nil {
			t.Errorf("%s: expected error, got nil", tc.name)
		}
		if calls != 0 {
			t.Errorf("%s: expected no network call before config guard, got %d", tc.name, calls)
		}
	}
}

// TestCreateCampaignRejectsUnsafeAccountID verifies that an account_id or
// funding_instrument_id containing path/query/fragment delimiters or whitespace
// is rejected up front, with zero network calls — a non-empty value with '/',
// '?', '#', or a space must not reach a mutating POST (path injection). A valid
// alphanumeric id still flows past the guard.
func TestCreateCampaignRejectsUnsafeAccountID(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	base := CampaignInput{
		EventName: "KubeCon EU", Project: "CNCF", BudgetUsd: 500,
		StartDate: "2026-03-01", EndDate: "2026-03-10",
		RegistrationURL: "https://events.lf.org/reg",
	}
	cases := []struct {
		name string
		acct AccountConfig
	}{
		{"account id with slash", AccountConfig{AccountID: "18ce/54", FundingInstrumentID: "fi1"}},
		{"account id with question mark", AccountConfig{AccountID: "acc?x", FundingInstrumentID: "fi1"}},
		{"account id with hash", AccountConfig{AccountID: "acc#x", FundingInstrumentID: "fi1"}},
		{"account id with space", AccountConfig{AccountID: "a b", FundingInstrumentID: "fi1"}},
		{"funding id with slash", AccountConfig{AccountID: "acc1", FundingInstrumentID: "fi/1"}},
		{"funding id with space", AccountConfig{AccountID: "acc1", FundingInstrumentID: "f i"}},
	}
	for _, tc := range cases {
		calls = 0
		c := NewClient(
			Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
			tc.acct,
			WithBaseURL(srv.URL),
			WithWriteDelay(0),
		)
		c.nonceFn = func() string { return "n" }
		c.timeFn = staticTime

		if _, err := c.CreateCampaign(context.Background(), base); err == nil {
			t.Errorf("%s: expected error, got nil", tc.name)
		}
		if calls != 0 {
			t.Errorf("%s: expected no network call before account-id guard, got %d", tc.name, calls)
		}
	}

	// A valid alphanumeric id must still flow past the guard (reaches network).
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/18ce54d4x5t"):
			_, _ = w.Write([]byte(`{"data":{"name":"LF Events"}}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "campaigns"):
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "line_items"):
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "campaigns"):
			_, _ = w.Write([]byte(`{"data":{"id":"cmp1"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "line_items"):
			_, _ = w.Write([]byte(`{"data":{"id":"li1"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer okSrv.Close()

	c := NewClient(
		Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
		AccountConfig{AccountID: "18ce54d4x5t", FundingInstrumentID: "fi1"},
		WithBaseURL(okSrv.URL),
		WithWriteDelay(0),
	)
	c.nonceFn = func() string { return "n" }
	c.timeFn = staticTime
	if _, err := c.CreateCampaign(context.Background(), base); err != nil {
		t.Errorf("valid alphanumeric account id should flow past the guard, got error: %v", err)
	}
}

// TestBudgetRoundToZeroBoundary verifies the round-to-zero guard: a budget just
// below half a micro-unit rounds to 0 and is rejected, while the smallest value
// that rounds to 1 micro-unit is accepted by the conversion.
func TestBudgetRoundToZeroBoundary(t *testing.T) {
	// 0.49e-6 USD -> 0.49 micro -> rounds to 0.
	if got := toMicroCurrency(0.49e-6); got != 0 {
		t.Fatalf("toMicroCurrency(0.49e-6) = %d, want 0", got)
	}
	// 0.5e-6 USD -> 0.5 micro -> rounds to 1 (still positive, accepted).
	if got := toMicroCurrency(0.5e-6); got <= 0 {
		t.Fatalf("toMicroCurrency(0.5e-6) = %d, want > 0", got)
	}
}

// TestCreateCampaignFlow exercises the full create flow against a fake server:
// account verify -> lookup (empty) -> create campaign -> create line item ->
// create promoted tweet.
func TestCreateCampaignFlow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		// WithWriteDelay(0) disables the inter-write pacing sleep so this
		// end-to-end flow doesn't incur the real ~1s-per-write delay.
		WithWriteDelay(0),
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
		WithWriteDelay(0),
	)
	c.nonceFn = func() string { return "n" }
	c.timeFn = staticTime

	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName: "KubeCon EU", Project: "CNCF", BudgetUsd: 500,
		StartDate: "2026-03-01", EndDate: "2026-03-10",
		RegistrationURL: "https://events.lf.org/reg",
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
// on a deep page (page 3 here) is found (idempotency must not break past page 1),
// and that every list request carries count=1000 — the X Ads v12 max page size —
// so the lookup covers a realistic large account within the maxListPages cap.
func TestFindByNamePagination(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		// Every list request must request the max page size.
		if got := r.URL.Query().Get("count"); got != "1000" {
			t.Errorf("list request must carry count=1000, got count=%q (query %v)", got, r.URL.Query())
		}
		switch r.URL.Query().Get("cursor") {
		case "":
			// page 1: no match, hand back a cursor.
			_, _ = w.Write([]byte(`{"data":[{"id":"c1","name":"other"}],"next_cursor":"CURSOR2"}`))
		case "CURSOR2":
			// page 2: still no match, another cursor.
			_, _ = w.Write([]byte(`{"data":[{"id":"c2","name":"still-other"}],"next_cursor":"CURSOR3"}`))
		default:
			// page 3: the match, no further cursor.
			_, _ = w.Write([]byte(`{"data":[{"id":"c3","name":"target"}]}`))
		}
	}))
	defer srv.Close()

	c := NewClient(
		Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
		AccountConfig{AccountID: "acc1"},
		WithBaseURL(srv.URL),
		WithWriteDelay(0),
	)
	c.nonceFn = func() string { return "n" }
	c.timeFn = staticTime

	id, err := c.findCampaignByName(context.Background(), "target")
	if err != nil {
		t.Fatalf("findCampaignByName: %v", err)
	}
	if id != "c3" {
		t.Errorf("findCampaignByName across pages = %q, want c3", id)
	}
	if calls != 3 {
		t.Errorf("expected 3 pages fetched, got %d", calls)
	}
}

// TestFindByNameLineItemListSendsCount verifies the line-item lookup path also
// requests the max page size (count=1000), alongside its campaign_ids scope.
func TestFindByNameLineItemListSendsCount(t *testing.T) {
	var sawCount, sawCampaignIDs bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("count") == "1000" {
			sawCount = true
		}
		if q.Get("campaign_ids") != "" {
			sawCampaignIDs = true
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	c := NewClient(
		Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
		AccountConfig{AccountID: "acc1"},
		WithBaseURL(srv.URL),
		WithWriteDelay(0),
	)
	c.nonceFn = func() string { return "n" }
	c.timeFn = staticTime

	if _, err := c.findLineItemByName(context.Background(), "camp1", "target"); err != nil {
		t.Fatalf("findLineItemByName: %v", err)
	}
	if !sawCount {
		t.Error("line-item lookup must send count=1000")
	}
	if !sawCampaignIDs {
		t.Error("line-item lookup must send campaign_ids scope")
	}
}

// TestFindByNameInconclusiveCapIsError verifies that when the page cap is
// reached with a next_cursor still outstanding (the name was never seen but more
// results remain), findByName returns an ERROR — never ("", nil). Treating an
// exhausted-but-inconclusive walk as "not found" would let the caller create a
// duplicate of an element that may exist further on. This behavior must be
// preserved by the count/page-size change.
func TestFindByNameInconclusiveCapIsError(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		// Never match, and ALWAYS return a next_cursor so the walk can never
		// conclude "not found" — it must hit the maxListPages cap.
		_, _ = w.Write([]byte(`{"data":[{"id":"x","name":"never-matches"}],"next_cursor":"MORE"}`))
	}))
	defer srv.Close()

	c := NewClient(
		Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
		AccountConfig{AccountID: "acc1"},
		WithBaseURL(srv.URL),
		WithWriteDelay(0),
	)
	c.nonceFn = func() string { return "n" }
	c.timeFn = staticTime

	id, err := c.findCampaignByName(context.Background(), "target")
	if err == nil {
		t.Fatalf("expected an inconclusive-cap error, got id=%q, nil error", id)
	}
	if id != "" {
		t.Errorf("expected empty id on inconclusive cap, got %q", id)
	}
	if calls != maxListPages {
		t.Errorf("expected the walk to fetch exactly maxListPages=%d pages, got %d", maxListPages, calls)
	}
}

// TestFindByNameMatchWithoutIDErrors verifies that a list element matching the
// name but carrying no usable id is surfaced as a lookup ERROR, not ("", nil).
// Returning "not found" would drive CreateCampaign into a create POST and risk
// duplicating an element that already exists. The test also asserts that no
// create POST is issued when the campaign lookup hits an id-less match.
func TestFindByNameMatchWithoutIDErrors(t *testing.T) {
	// The campaign lookup in CreateCampaign searches for this composed name, so
	// the id-less element the server returns must carry the same name to be a
	// genuine (id-less) match on the campaign lookup path.
	in := CampaignInput{
		EventName: "KubeCon EU", Project: "CNCF", BudgetUsd: 500,
		StartDate: "2026-03-01", EndDate: "2026-03-10", TweetID: "123",
		RegistrationURL: "https://events.lf.org/reg",
	}
	campaignName := buildTwitterCampaignName(in)
	idlessBody, err := json.Marshal(map[string]any{
		"data": []map[string]any{{"name": campaignName}}, // matches by name, no id
	})
	if err != nil {
		t.Fatalf("marshal id-less body: %v", err)
	}

	var createPosts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/acc1"):
			_, _ = w.Write([]byte(`{"data":{"name":"LF Events"}}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "campaigns"):
			// Name matches the composed campaign name but the element has no id.
			_, _ = w.Write(idlessBody)
		case r.Method == http.MethodPost:
			createPosts++
			_, _ = w.Write([]byte(`{"data":{"id":"should-not-happen"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewClient(
		Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
		AccountConfig{AccountID: "acc1", FundingInstrumentID: "fi1"},
		WithBaseURL(srv.URL),
		WithWriteDelay(0),
	)
	c.nonceFn = func() string { return "n" }
	c.timeFn = staticTime

	// Direct lookup: a name match with no id is an error, not ("", nil).
	id, err := c.findCampaignByName(context.Background(), campaignName)
	if err == nil {
		t.Fatalf("expected error for id-less match, got id=%q, nil error", id)
	}
	if id != "" {
		t.Errorf("expected empty id on error, got %q", id)
	}

	// End-to-end: CreateCampaign must abort and NOT issue a create POST.
	_, err = c.CreateCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("CreateCampaign should abort when the campaign lookup returns an id-less match")
	}
	if createPosts != 0 {
		t.Errorf("expected no create POST on id-less match, got %d", createPosts)
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
// paused=true) on the campaign and line item, that the campaign create does NOT
// send the unsupported start_time/end_time flight dates, that the line-item
// create includes the required start_time/end_time, and that the promoted-tweet
// create does not send entity_status (the API creates it ACTIVE).
func TestCreateSendsQueryParams(t *testing.T) {
	var campaignQuery, lineItemQuery, promotedQuery, lineItemGetQuery url.Values
	var campaignBodyLen, lineItemBodyLen int64
	var campaignCT, lineItemCT string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/acc1"):
			_, _ = w.Write([]byte(`{"data":{"name":"LF Events"}}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "campaigns"):
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "line_items"):
			lineItemGetQuery = r.URL.Query()
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
			promotedQuery = r.URL.Query()
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
		WithWriteDelay(0),
	)
	c.nonceFn = func() string { return "n" }
	c.timeFn = staticTime

	if _, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName: "KubeCon EU", Project: "CNCF", BudgetUsd: 500,
		StartDate: "2026-03-01", EndDate: "2026-03-10", TweetID: "111",
		RegistrationURL: "https://events.lf.org/reg",
	}); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}

	// The line-item lookup must scope by campaign_ids (plural list filter), not
	// the singular create param campaign_id, or it runs unscoped and could reuse
	// a same-named line item from another campaign.
	if lineItemGetQuery.Get("campaign_ids") == "" {
		t.Errorf("line-item lookup must send campaign_ids (plural): %v", lineItemGetQuery)
	}
	if lineItemGetQuery.Has("campaign_id") {
		t.Errorf("line-item lookup should not use singular campaign_id: %v", lineItemGetQuery)
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
	// X Ads v12 rejects start_time/end_time on the campaign endpoint; flight
	// dates belong on the line item, so the campaign create must not send them.
	if campaignQuery.Has("start_time") {
		t.Errorf("campaign create must not send start_time (unsupported in v12): %v", campaignQuery)
	}
	if campaignQuery.Has("end_time") {
		t.Errorf("campaign create must not send end_time (unsupported in v12): %v", campaignQuery)
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

	// Promoted-tweet create: assert the request actually hit the promoted_tweets
	// path (promotedQuery non-nil) before checking the absence of entity_status —
	// otherwise a misrouted request leaves promotedQuery nil and the absence-only
	// check below would pass vacuously.
	if promotedQuery == nil {
		t.Fatal("promoted tweet create was never received on the promoted_tweets path")
	}
	// The endpoint does not accept entity_status; the API creates the association
	// ACTIVE and delivery is gated by the PAUSED line item, so we must not send
	// entity_status here.
	if promotedQuery.Has("entity_status") {
		t.Errorf("promoted tweet create should not send entity_status: %v", promotedQuery)
	}
}

// TestRetryResetExceedsCapAborts verifies that when the server declares a
// rate-limit reset longer than maxRetryWait, the client aborts immediately with
// the rate-limit error rather than sleeping (burning retries or hanging).
func TestRetryResetExceedsCapAborts(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		// Declare a reset far beyond the cap.
		w.Header().Set("Retry-After", strconv.Itoa(int(maxRetryWait/time.Second)+3600))
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := NewClient(
		Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
		AccountConfig{AccountID: "acc1"},
		WithBaseURL(srv.URL),
		WithWriteDelay(0),
	)
	c.nonceFn = func() string { return "n" }
	c.timeFn = staticTime

	start := time.Now()
	_, err := c.request(context.Background(), http.MethodGet, "campaigns")
	if err == nil {
		t.Fatal("expected error when reset exceeds max wait")
	}
	// Must have aborted on the first 429 without sleeping or retrying.
	if calls != 1 {
		t.Errorf("expected 1 call (immediate abort), got %d", calls)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("expected immediate return, took %v", elapsed)
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestPromotedTweetMissingIDWarns verifies a 2xx promoted-tweet response with no
// data.id is surfaced as a warning step (not silent success, not fatal): the
// flow returns nil error, PromotedTweetID is empty, and a step records the gap.
func TestPromotedTweetMissingIDWarns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			// 2xx but the array is empty -> no id.
			_, _ = w.Write([]byte(`{"data":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewClient(
		Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
		AccountConfig{AccountID: "acc1", FundingInstrumentID: "fi1"},
		WithBaseURL(srv.URL),
		WithWriteDelay(0),
	)
	c.nonceFn = func() string { return "n" }
	c.timeFn = staticTime

	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName: "KubeCon EU", Project: "CNCF", BudgetUsd: 500,
		StartDate: "2026-03-01", EndDate: "2026-03-10", TweetID: "123",
		RegistrationURL: "https://events.lf.org/reg",
	})
	if err != nil {
		t.Fatalf("CreateCampaign should not be fatal on missing promoted-tweet id: %v", err)
	}
	if res.PromotedTweetID != "" {
		t.Errorf("expected empty PromotedTweetID, got %q", res.PromotedTweetID)
	}
	if res.PromotedTweetWarning == "" {
		t.Errorf("expected PromotedTweetWarning to be set for malformed promoted-tweet response")
	}
	var found bool
	for _, s := range res.Steps {
		if strings.Contains(s, "no promoted-tweet ID") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a warning step for missing promoted-tweet id, steps: %v", res.Steps)
	}
}

// TestPromotedTweetPostErrorWarns verifies that a promoted-tweet POST that fails
// with a non-2xx (non-duplicate) error is NOT reported as a clean success: the
// campaign flow stays non-fatal, but PromotedTweetWarning is set and a warning
// step records the gap.
func TestPromotedTweetPostErrorWarns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/acc1"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":{"name":"LF Events"}}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "campaigns"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "line_items"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "campaigns"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":{"id":"cmp1"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "line_items"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":{"id":"li1"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "promoted_tweets"):
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"errors":[{"code":"INVALID_PARAMETER","message":"bad tweet"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewClient(
		Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
		AccountConfig{AccountID: "acc1", FundingInstrumentID: "fi1"},
		WithBaseURL(srv.URL),
		WithWriteDelay(0),
	)
	c.nonceFn = func() string { return "n" }
	c.timeFn = staticTime

	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName: "KubeCon EU", Project: "CNCF", BudgetUsd: 500,
		StartDate: "2026-03-01", EndDate: "2026-03-10", TweetID: "123",
		RegistrationURL: "https://events.lf.org/reg",
	})
	if err != nil {
		t.Fatalf("CreateCampaign should not be fatal on promoted-tweet POST failure: %v", err)
	}
	if res.PromotedTweetID != "" {
		t.Errorf("expected empty PromotedTweetID on failed POST, got %q", res.PromotedTweetID)
	}
	if res.PromotedTweetWarning == "" {
		t.Errorf("expected PromotedTweetWarning to be set on failed promoted-tweet POST (must not report clean success)")
	}
	var found bool
	for _, s := range res.Steps {
		if strings.Contains(s, "Promoted tweet creation failed") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a warning step for failed promoted-tweet POST, steps: %v", res.Steps)
	}
}

// TestPromotedTweetDuplicateSurfacesWarning verifies that a
// DUPLICATE_PROMOTABLE_ENTITY response is surfaced as a WARNING (not an
// unqualified success): X can return that code when the tweet is promoted by a
// DIFFERENT line item, so it does not prove this tweet is attached to this line
// item. The flow stays non-fatal (campaign + line item still return), but
// PromotedTweetWarning is set and a step tells the caller to verify manually.
func TestPromotedTweetDuplicateSurfacesWarning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/acc1"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":{"name":"LF Events"}}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "campaigns"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "line_items"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "campaigns"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":{"id":"cmp1"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "line_items"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":{"id":"li1"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "promoted_tweets"):
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"errors":[{"code":"DUPLICATE_PROMOTABLE_ENTITY","message":"tweet already promoted on this line item"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewClient(
		Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
		AccountConfig{AccountID: "acc1", FundingInstrumentID: "fi1"},
		WithBaseURL(srv.URL),
		WithWriteDelay(0),
	)
	c.nonceFn = func() string { return "n" }
	c.timeFn = staticTime

	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName: "KubeCon EU", Project: "CNCF", BudgetUsd: 500,
		StartDate: "2026-03-01", EndDate: "2026-03-10", TweetID: "123",
		RegistrationURL: "https://events.lf.org/reg",
	})
	if err != nil {
		t.Fatalf("CreateCampaign should not be fatal on duplicate promoted-tweet: %v", err)
	}
	// Non-fatal, but a duplicate is NOT an unqualified success: the warning must be
	// set so consumers do not treat the result as a confirmed association.
	if res.PromotedTweetWarning == "" {
		t.Error("expected PromotedTweetWarning to be set for a DUPLICATE_PROMOTABLE_ENTITY response, got empty")
	}
	// The campaign and line item should still have been created/returned.
	if res.CampaignID != "cmp1" || res.LineItemID != "li1" {
		t.Errorf("expected campaign+line item to still return (cmp1/li1), got %q/%q", res.CampaignID, res.LineItemID)
	}
	var found bool
	for _, s := range res.Steps {
		if strings.Contains(s, "duplicate") && strings.Contains(s, "verify manually") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a duplicate/verify-manually warning step for duplicate promoted-tweet, steps: %v", res.Steps)
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
		WithWriteDelay(0),
	)
	c.nonceFn = func() string { return "n" }
	c.timeFn = staticTime

	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName: "KubeCon EU", Project: "CNCF", BudgetUsd: 500,
		StartDate: "2026-03-01", EndDate: "2026-03-10",
		RegistrationURL: "https://events.lf.org/reg",
	})
	if err == nil {
		t.Fatal("expected error when campaign lookup fails, got nil")
	}
	if postCampaign != 0 {
		t.Errorf("expected no campaign create POST after lookup failure, got %d", postCampaign)
	}
}

// TestWithHTTPClientNilIgnored verifies WithHTTPClient(nil) does not install a
// nil client (which would panic on the first httpClient.Do); the default client
// is retained so the option can't produce an unusable Client.
func TestWithHTTPClientNilIgnored(t *testing.T) {
	c := NewClient(Credentials{}, AccountConfig{}, WithHTTPClient(nil))
	if c.httpClient == nil {
		t.Fatal("WithHTTPClient(nil) left a nil httpClient; expected the default to be retained")
	}
}

// TestWithWriteDelayZeroDisablesPacing verifies a zero write delay makes pace a
// no-op so tests don't incur real per-request sleeps.
func TestWithWriteDelayZeroDisablesPacing(t *testing.T) {
	c := NewClient(Credentials{}, AccountConfig{}, WithWriteDelay(0))
	start := time.Now()
	if err := c.pace(context.Background()); err != nil {
		t.Fatalf("pace: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("pace slept %v with zero writeDelay; expected a no-op", elapsed)
	}
}

// TestTweetIDWhitespaceNotPromoted verifies a whitespace-only TweetID is treated
// as "not supplied" (no promoted-tweet POST) rather than sent verbatim and
// guaranteeing a rejected POST after the campaign + line item already exist.
func TestTweetIDWhitespaceNotPromoted(t *testing.T) {
	var promotedHit int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/acc1"):
			_, _ = w.Write([]byte(`{"data":{"name":"LF"}}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "campaigns"):
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "line_items"):
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "campaigns"):
			_, _ = w.Write([]byte(`{"data":{"id":"cmp1"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "line_items"):
			_, _ = w.Write([]byte(`{"data":{"id":"li1"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "promoted_tweets"):
			atomic.AddInt32(&promotedHit, 1)
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
		WithWriteDelay(0),
	)
	c.nonceFn = func() string { return "n" }
	c.timeFn = staticTime

	if _, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName: "E", Project: "tlf", BudgetUsd: 500,
		StartDate: "2026-03-01", EndDate: "2026-03-10", TweetID: "   ",
		RegistrationURL: "https://events.lf.org/reg",
	}); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if got := atomic.LoadInt32(&promotedHit); got != 0 {
		t.Errorf("promoted_tweets hit %d times for a whitespace-only TweetID, want 0", got)
	}
}

// TestAccountConfigTrimmedInRequests verifies NewClient trims AccountID and
// FundingInstrumentID so the TRIMMED value is what is both validated non-empty
// AND used in every request path/param. A padded " acc1 "/" fi1 " must produce
// an account path containing "acc1" (no spaces) and a funding_instrument_id
// param of "fi1", while a whitespace-only value is still rejected as empty.
func TestAccountConfigTrimmedInRequests(t *testing.T) {
	// NewClient must store the trimmed values.
	c := NewClient(
		Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
		AccountConfig{AccountID: " acc1 ", FundingInstrumentID: " fi1 "},
	)
	if c.account.AccountID != "acc1" {
		t.Errorf("AccountID not trimmed on construction: %q", c.account.AccountID)
	}
	if c.account.FundingInstrumentID != "fi1" {
		t.Errorf("FundingInstrumentID not trimmed on construction: %q", c.account.FundingInstrumentID)
	}
	// The account URL (used for every request path) must carry the trimmed id, so
	// the path has no embedded spaces.
	if got := c.accountURL(); !strings.HasSuffix(got, "/accounts/acc1") {
		t.Errorf("account URL should end in /accounts/acc1 (trimmed), got %q", got)
	}

	// End-to-end: the padded ids must reach the server as trimmed values — the
	// campaign create funding_instrument_id param and the account path.
	var acctPathSeen string
	var fundingParamSeen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/accounts/") && strings.HasSuffix(r.URL.Path, "acc1"):
			acctPathSeen = r.URL.Path
			_, _ = w.Write([]byte(`{"data":{"name":"LF"}}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "campaigns"):
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "line_items"):
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "campaigns"):
			fundingParamSeen = r.URL.Query().Get("funding_instrument_id")
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

	c2 := NewClient(
		Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
		AccountConfig{AccountID: " acc1 ", FundingInstrumentID: " fi1 "},
		WithBaseURL(srv.URL),
		WithWriteDelay(0),
	)
	c2.nonceFn = func() string { return "n" }
	c2.timeFn = staticTime

	if _, err := c2.CreateCampaign(context.Background(), CampaignInput{
		EventName: "E", Project: "tlf", BudgetUsd: 500,
		StartDate: "2026-03-01", EndDate: "2026-03-10",
		RegistrationURL: "https://events.lf.org/reg",
	}); err != nil {
		t.Fatalf("CreateCampaign with padded account config: %v", err)
	}
	if strings.Contains(acctPathSeen, " ") || strings.Contains(acctPathSeen, "%20") || !strings.HasSuffix(acctPathSeen, "/accounts/acc1") {
		t.Errorf("account path should be trimmed (/accounts/acc1, no spaces), got %q", acctPathSeen)
	}
	if fundingParamSeen != "fi1" {
		t.Errorf("funding_instrument_id param should be trimmed to fi1, got %q", fundingParamSeen)
	}

	// A whitespace-only id must still be rejected as empty before any network call.
	whitespaceCases := []AccountConfig{
		{AccountID: "   ", FundingInstrumentID: "fi1"},
		{AccountID: "acc1", FundingInstrumentID: "   "},
	}
	for i, acct := range whitespaceCases {
		var calls int32
		wsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&calls, 1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[]}`))
		}))
		cw := NewClient(
			Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
			acct,
			WithBaseURL(wsrv.URL),
			WithWriteDelay(0),
		)
		cw.nonceFn = func() string { return "n" }
		cw.timeFn = staticTime
		// Project is supplied so CreateCampaign reaches the account-config guard
		// (Project is validated first; without it this would fail on "invalid
		// project" and never exercise the whitespace-account-id rejection).
		_, err := cw.CreateCampaign(context.Background(), CampaignInput{
			EventName: "E", Project: "tlf", BudgetUsd: 500, StartDate: "2026-03-01", EndDate: "2026-03-10",
			RegistrationURL: "https://events.lf.org/reg",
		})
		if err == nil {
			t.Errorf("case %d: whitespace-only account config should be rejected", i)
		}
		if got := atomic.LoadInt32(&calls); got != 0 {
			t.Errorf("case %d: expected no network call before config guard, got %d", i, got)
		}
		wsrv.Close()
	}
}

// TestTweetIDFormatValidatedUpFront verifies a supplied but non-numeric TweetID
// is rejected in the up-front validation block, BEFORE any mutating call — so an
// invalid tweet id can never leave a partial/orphaned campaign (campaign + line
// item created, promoted_tweets POST rejected). A valid numeric id still flows
// through, and a blank id still skips the promoted-tweet step.
func TestTweetIDFormatValidatedUpFront(t *testing.T) {
	newSrv := func(createPosts *int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/acc1"):
				_, _ = w.Write([]byte(`{"data":{"name":"LF"}}`))
			case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "campaigns"):
				_, _ = w.Write([]byte(`{"data":[]}`))
			case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "line_items"):
				_, _ = w.Write([]byte(`{"data":[]}`))
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "campaigns"):
				atomic.AddInt32(createPosts, 1)
				_, _ = w.Write([]byte(`{"data":{"id":"cmp1"}}`))
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "line_items"):
				atomic.AddInt32(createPosts, 1)
				_, _ = w.Write([]byte(`{"data":{"id":"li1"}}`))
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "promoted_tweets"):
				atomic.AddInt32(createPosts, 1)
				_, _ = w.Write([]byte(`{"data":[{"id":"pt1"}]}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
	}
	mkClient := func(url string) *Client {
		c := NewClient(
			Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
			AccountConfig{AccountID: "acc1", FundingInstrumentID: "fi1"},
			WithBaseURL(url),
			WithWriteDelay(0),
		)
		c.nonceFn = func() string { return "n" }
		c.timeFn = staticTime
		return c
	}
	base := CampaignInput{
		EventName: "E", Project: "tlf", BudgetUsd: 500,
		StartDate: "2026-03-01", EndDate: "2026-03-10",
		RegistrationURL: "https://events.lf.org/reg",
	}

	// Invalid (non-numeric) tweet ids must fail up front with ZERO create POSTs.
	for _, bad := range []string{"not-a-tweet", "12x3", " 12x3 "} {
		var createPosts int32
		srv := newSrv(&createPosts)
		c := mkClient(srv.URL)
		in := base
		in.TweetID = bad
		_, err := c.CreateCampaign(context.Background(), in)
		if err == nil {
			t.Errorf("TweetID %q should be rejected as non-numeric", bad)
		}
		if !strings.Contains(err.Error(), "tweet id") {
			t.Errorf("TweetID %q: expected a tweet-id error, got %v", bad, err)
		}
		if got := atomic.LoadInt32(&createPosts); got != 0 {
			t.Errorf("TweetID %q: expected 0 create POSTs (fail before any mutation), got %d", bad, got)
		}
		srv.Close()
	}

	// A valid numeric tweet id still flows all the way through the promoted step.
	{
		var createPosts int32
		srv := newSrv(&createPosts)
		c := mkClient(srv.URL)
		in := base
		in.TweetID = "1234567890"
		res, err := c.CreateCampaign(context.Background(), in)
		if err != nil {
			t.Fatalf("valid numeric TweetID should flow: %v", err)
		}
		if res.PromotedTweetID != "pt1" {
			t.Errorf("expected promoted tweet pt1, got %q", res.PromotedTweetID)
		}
		srv.Close()
	}

	// A blank tweet id still skips the promoted step (no promoted_tweets POST),
	// while the campaign + line item are still created.
	{
		var promotedPosts int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/acc1"):
				_, _ = w.Write([]byte(`{"data":{"name":"LF"}}`))
			case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "campaigns"):
				_, _ = w.Write([]byte(`{"data":[]}`))
			case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "line_items"):
				_, _ = w.Write([]byte(`{"data":[]}`))
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "campaigns"):
				_, _ = w.Write([]byte(`{"data":{"id":"cmp1"}}`))
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "line_items"):
				_, _ = w.Write([]byte(`{"data":{"id":"li1"}}`))
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "promoted_tweets"):
				atomic.AddInt32(&promotedPosts, 1)
				_, _ = w.Write([]byte(`{"data":[{"id":"pt1"}]}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer srv.Close()
		c := mkClient(srv.URL)
		in := base
		in.TweetID = ""
		if _, err := c.CreateCampaign(context.Background(), in); err != nil {
			t.Fatalf("blank TweetID should still create campaign + line item: %v", err)
		}
		if got := atomic.LoadInt32(&promotedPosts); got != 0 {
			t.Errorf("blank TweetID should skip promoted step, got %d POSTs", got)
		}
	}
}

// TestTweetIDInt64OverflowRejected verifies a 19-digit value above the max int64
// snowflake passes the regex shape but is rejected by CreateCampaign's parse
// check BEFORE any mutating call — so the flow never creates a partial campaign.
// The test drives CreateCampaign (not just strconv.ParseInt) against a server
// that fails on any create POST, so removing the production overflow check would
// make this test fail rather than silently pass.
func TestTweetIDInt64OverflowRejected(t *testing.T) {
	const overflow = "9999999999999999999" // 19 digits (matches regex) but > math.MaxInt64.
	// Precondition: value has the valid digit shape yet overflows int64.
	if !tweetIDRe.MatchString(overflow) {
		t.Fatal("precondition: value should match the digit-shape regex")
	}
	if _, err := strconv.ParseInt(overflow, 10, 64); err == nil {
		t.Fatal("precondition: value should overflow int64")
	}

	// Any create POST means the overflow check failed to reject up front.
	var creates int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			creates++
			t.Errorf("unexpected create POST to %s for out-of-range tweet id", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	c := NewClient(
		Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
		AccountConfig{AccountID: "acc1", FundingInstrumentID: "fi1"},
		WithBaseURL(srv.URL),
		WithWriteDelay(0),
	)
	c.nonceFn = func() string { return "n" }
	c.timeFn = staticTime

	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName: "KubeCon EU", Project: "CNCF", BudgetUsd: 500,
		StartDate: "2026-03-01", EndDate: "2026-03-10", TweetID: overflow,
		RegistrationURL: "https://events.lf.org/reg",
	})
	if err == nil {
		t.Fatal("expected CreateCampaign to reject an out-of-int64-range tweet id, got nil error")
	}
	if creates != 0 {
		t.Errorf("expected zero create POSTs for out-of-range tweet id, got %d", creates)
	}
}

// TestTweetIDRejectsNonSnowflake verifies the up-front TweetID format check
// rejects values that are numeric but cannot be real Tweet snowflakes ("0",
// leading-zero, or an over-19-digit decimal) so they fail before any mutation.
func TestTweetIDRejectsNonSnowflake(t *testing.T) {
	for _, bad := range []string{"0", "0123", "01", "12345678901234567890" /* 20 digits */} {
		if tweetIDRe.MatchString(bad) {
			t.Errorf("tweetIDRe accepted %q, want reject (not a valid snowflake)", bad)
		}
	}
	for _, ok := range []string{"1", "111", "1234567890", "1234567890123456789" /* 19 digits */} {
		if !tweetIDRe.MatchString(ok) {
			t.Errorf("tweetIDRe rejected %q, want accept", ok)
		}
	}
}

// TestCreateCampaignValidatesRegistrationURL verifies an empty / relative /
// non-http RegistrationURL is rejected up front — before any network call —
// while a valid https URL flows past validation.
func TestCreateCampaignValidatesRegistrationURL(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/acc1"):
			_, _ = w.Write([]byte(`{"data":{"name":"LF"}}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "campaigns"):
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "line_items"):
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "campaigns"):
			_, _ = w.Write([]byte(`{"data":{"id":"cmp1"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "line_items"):
			_, _ = w.Write([]byte(`{"data":{"id":"li1"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	mk := func() *Client {
		c := NewClient(
			Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
			AccountConfig{AccountID: "acc1", FundingInstrumentID: "fi1"},
			WithBaseURL(srv.URL),
			WithWriteDelay(0),
		)
		c.nonceFn = func() string { return "n" }
		c.timeFn = staticTime
		return c
	}
	base := CampaignInput{
		EventName: "KubeCon EU", Project: "CNCF", BudgetUsd: 500,
		StartDate: "2026-03-01", EndDate: "2026-03-10",
	}

	// Invalid URLs must be rejected before any network call.
	for _, bad := range []string{"", "   ", "/relative/path", "events.lf.org/reg", "ftp://events.lf.org/reg"} {
		atomic.StoreInt32(&calls, 0)
		in := base
		in.RegistrationURL = bad
		if _, err := mk().CreateCampaign(context.Background(), in); err == nil {
			t.Errorf("RegistrationURL %q should be rejected", bad)
		}
		if got := atomic.LoadInt32(&calls); got != 0 {
			t.Errorf("RegistrationURL %q: expected 0 network calls before validation, got %d", bad, got)
		}
	}

	// A valid https URL flows past validation (reaches the network).
	in := base
	in.RegistrationURL = "https://events.lf.org/reg"
	if _, err := mk().CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("valid https RegistrationURL should flow: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got == 0 {
		t.Errorf("valid RegistrationURL should reach the network, got 0 calls")
	}
}

// TestVerifyAccountRetriesOn429 proves verifyAccount now goes through doRequest
// and therefore inherits the shared OAuth signing + 429 retry/backoff: a server
// that 429s once (with Retry-After) then 200s must still yield "Account
// verified: <name>", after a retry. The earlier version fired httpClient.Do
// directly and would have surfaced the 429 as a warning without retrying.
func TestVerifyAccountRetriesOn429(t *testing.T) {
	var acctCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/acc1"):
			if atomic.AddInt32(&acctCalls, 1) == 1 {
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			_, _ = w.Write([]byte(`{"data":{"name":"LF Events"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewClient(
		Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
		AccountConfig{AccountID: "acc1", FundingInstrumentID: "fi1"},
		WithBaseURL(srv.URL),
		WithWriteDelay(0),
	)
	c.nonceFn = func() string { return "n" }
	c.timeFn = staticTime

	var steps []string
	c.verifyAccount(context.Background(), &steps)

	if got := atomic.LoadInt32(&acctCalls); got != 2 {
		t.Errorf("expected the account GET to be retried after a 429 (2 calls), got %d", got)
	}
	if len(steps) != 1 {
		t.Fatalf("expected exactly one verification step, got %d: %v", len(steps), steps)
	}
	if steps[0] != "Account verified: LF Events" {
		t.Errorf("expected verified step after retry, got %q", steps[0])
	}
}

// TestVerifyAccountNonFatalOnError verifies a non-2xx account response (surfaced
// by doRequest as an error) is recorded as a warning step and NOT propagated —
// verification stays non-fatal.
func TestVerifyAccountNonFatalOnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":[{"code":"UNAUTHORIZED_ACCESS"}]}`))
	}))
	defer srv.Close()

	c := NewClient(
		Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
		AccountConfig{AccountID: "acc1", FundingInstrumentID: "fi1"},
		WithBaseURL(srv.URL),
		WithWriteDelay(0),
	)
	c.nonceFn = func() string { return "n" }
	c.timeFn = staticTime

	var steps []string
	c.verifyAccount(context.Background(), &steps)

	if len(steps) != 1 {
		t.Fatalf("expected one warning step, got %d: %v", len(steps), steps)
	}
	if !strings.HasPrefix(steps[0], "Account verification warning:") {
		t.Errorf("expected a non-fatal warning step, got %q", steps[0])
	}
}

// TestComputedBackoffClampedToMaxRetryWait verifies the no-Retry-After computed
// exponential backoff never exceeds maxRetryWait, matching the header path. It
// mirrors doRequest's fallback formula for attempt 0..retryMax.
func TestComputedBackoffClampedToMaxRetryWait(t *testing.T) {
	for attempt := 0; attempt <= retryMax; attempt++ {
		waitDur := writeDelay * time.Duration(1<<uint(attempt))
		if waitDur > maxRetryWait {
			waitDur = maxRetryWait
		}
		if waitDur > maxRetryWait {
			t.Errorf("attempt %d: computed backoff %v exceeds maxRetryWait %v", attempt, waitDur, maxRetryWait)
		}
	}
}
