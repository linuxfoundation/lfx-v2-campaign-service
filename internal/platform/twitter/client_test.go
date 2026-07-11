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
	got := buildTwitterCampaignName(CampaignInput{EventName: "KubeCon | EU", Project: ""})
	want := "Events | KubeCon - EU | Global | Awareness | Prospecting | Promoted Post | tlf | MoFU"
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
	cases := []CampaignInput{
		{EventName: "E", BudgetUsd: 0, StartDate: "2026-01-01", EndDate: "2026-01-02"},
		{EventName: "E", BudgetUsd: 100, StartDate: "bad", EndDate: "2026-01-02"},
		{EventName: "E", BudgetUsd: 100, StartDate: "2026-01-01", EndDate: "bad"},
		{EventName: "E", BudgetUsd: 100, StartDate: "2026-01-02", EndDate: "2026-01-01"},
		// Well-shaped but impossible calendar dates must be rejected before any
		// mutating call (the regex alone would let these through).
		{EventName: "E", BudgetUsd: 100, StartDate: "2026-99-99", EndDate: "2026-12-31"},
		{EventName: "E", BudgetUsd: 100, StartDate: "2026-01-01", EndDate: "2026-99-99"},
		{EventName: "E", BudgetUsd: 100, StartDate: "2026-02-30", EndDate: "2026-12-31"},
		// Empty / whitespace-only event name.
		{EventName: "", BudgetUsd: 100, StartDate: "2026-01-01", EndDate: "2026-01-02"},
		{EventName: "   ", BudgetUsd: 100, StartDate: "2026-01-01", EndDate: "2026-01-02"},
		// Over-length event name.
		{EventName: strings.Repeat("x", maxEventNameLen+1), BudgetUsd: 100, StartDate: "2026-01-01", EndDate: "2026-01-02"},
		// Budget above the int64 micro-unit overflow cap.
		{EventName: "E", BudgetUsd: maxBudgetUsd + 1, StartDate: "2026-01-01", EndDate: "2026-01-02"},
		// Positive-but-rounds-to-zero micro-units (< half a micro-unit).
		{EventName: "E", BudgetUsd: 1e-9, StartDate: "2026-01-01", EndDate: "2026-01-02"},
		// NaN / Inf budgets.
		{EventName: "E", BudgetUsd: math.NaN(), StartDate: "2026-01-01", EndDate: "2026-01-02"},
		{EventName: "E", BudgetUsd: math.Inf(1), StartDate: "2026-01-01", EndDate: "2026-01-02"},
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
	base := CampaignInput{EventName: "E", StartDate: "2026-01-01", EndDate: "2026-01-02"}

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
// call. A 200-char event (the per-field max) with the default project composes
// to ~286 chars — within the per-field bounds but over the entity-name limit.
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

	// Sanity-check the premise: 200-char event + default project overflows 255.
	name := buildTwitterCampaignName(CampaignInput{EventName: strings.Repeat("x", maxEventNameLen)})
	if got := len([]rune(name)); got <= maxEntityNameLen {
		t.Fatalf("test premise wrong: composed name is %d runes, expected > %d", got, maxEntityNameLen)
	}

	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName: strings.Repeat("x", maxEventNameLen), // valid per-field, no default project
		BudgetUsd: 500,
		StartDate: "2026-03-01",
		EndDate:   "2026-03-10",
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

// TestCreateCampaignEventNameRuneLimit verifies the EventName length guard
// counts runes, not UTF-8 bytes. A multi-byte name that is under the 200-rune
// limit but well over 200 bytes must pass the length guard (byte-counting would
// wrongly reject it), while a name over 200 runes must be rejected.
func TestCreateCampaignEventNameRuneLimit(t *testing.T) {
	// 150 CJK runes = 450 bytes: under 200 runes but far over 200 bytes. The
	// composed campaign name (with the default project) stays under 255 runes,
	// so the whole create flow succeeds.
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
		WithWriteDelay(0),
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

// TestPromotedTweetDuplicateTreatedIdempotent verifies that a recognizable
// duplicate-association error is treated as success (the association already
// exists): no warning, and a step records the idempotent reuse.
func TestPromotedTweetDuplicateTreatedIdempotent(t *testing.T) {
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
	})
	if err != nil {
		t.Fatalf("CreateCampaign should not be fatal on duplicate promoted-tweet: %v", err)
	}
	if res.PromotedTweetWarning != "" {
		t.Errorf("expected no PromotedTweetWarning for idempotent duplicate, got %q", res.PromotedTweetWarning)
	}
	var found bool
	for _, s := range res.Steps {
		if strings.Contains(s, "already associated") || strings.Contains(s, "idempotent") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected an idempotent-reuse step for duplicate promoted-tweet, steps: %v", res.Steps)
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
	}); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if got := atomic.LoadInt32(&promotedHit); got != 0 {
		t.Errorf("promoted_tweets hit %d times for a whitespace-only TweetID, want 0", got)
	}
}
