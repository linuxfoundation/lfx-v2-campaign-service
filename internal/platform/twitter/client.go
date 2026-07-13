// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package twitter is a Go port of the X (Twitter) Ads platform client. It
// implements OAuth 1.0a (HMAC-SHA1) request signing and the
// campaign -> line_item -> promoted_tweet creation flow against the X Ads API.
//
// Credentials and account configuration are injected via NewClient; this
// package never reads environment variables or touches the database. In
// production the credentials come from a decrypted stored connection.
package twitter

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // OAuth 1.0a mandates HMAC-SHA1; not used for security hashing.
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Constants (mirror twitter.constants.ts + shared constants)
// ---------------------------------------------------------------------------

const (
	// DefaultBaseURL is the X Ads API origin. Mirrors TWITTER_ADS_BASE_URL.
	DefaultBaseURL = "https://ads-api.x.com"
	// DefaultAPIVersion mirrors TWITTER_ADS_API_VERSION.
	DefaultAPIVersion = "12"
	// AdsManagerURL mirrors TWITTER_ADS_MANAGER_URL.
	AdsManagerURL = "https://ads.x.com"

	// requestTimeout mirrors TWITTER_REQUEST_TIMEOUT_MS.
	requestTimeout = 30 * time.Second
	// writeDelay mirrors TWITTER_API_WRITE_DELAY_MS (1 write req/sec).
	writeDelay = 1 * time.Second
	// maxBudgetUsd caps the budget well below the int64 micro-unit overflow
	// threshold (int64 max / 1e6 ≈ 9.2e12) so the ×1e6 conversion in
	// toMicroCurrency can never wrap to a negative value. Mirrors the reddit
	// client's redditMaxBudgetUSD.
	maxBudgetUsd = 1_000_000_000.0
	// maxEventNameLen bounds the event name folded into campaign / line-item
	// names, guarding against unbounded input producing oversized API payloads.
	maxEventNameLen = 200
	// maxProjectLen bounds the project name folded into the campaign name. Like
	// EventName, Project is caller-supplied and otherwise unbounded, so it is
	// trimmed and length-capped before composition.
	maxProjectLen = 200
	// maxEntityNameLen is X's hard limit on a campaign / line-item entity name.
	// The composed name (event + project + fixed template) can exceed this even
	// when EventName and Project are individually within bounds, so the FINAL
	// composed names are validated against this rune limit before any create call.
	maxEntityNameLen = 255
	// retryMax mirrors TWITTER_API_RETRY_MAX.
	retryMax = 3
	// maxRetryWait caps how long a single 429 backoff will sleep. X rate-limit
	// windows can be far longer than a request is willing to wait; if the
	// server-declared reset exceeds this cap we abort with the rate-limit error
	// instead of sleeping pointlessly (and a hostile huge reset can't hang us).
	maxRetryWait = 90 * time.Second
	// maxResponseBody bounds how much of any response body is read into memory,
	// guarding against a hostile/oversized reply while comfortably exceeding any
	// normal X Ads response or error envelope.
	maxResponseBody = 1 << 20 // 1 MiB
)

// ---------------------------------------------------------------------------
// Injected configuration
// ---------------------------------------------------------------------------

// Credentials holds the OAuth 1.0a user-context credentials required for all
// X Ads API write operations. These are injected, never read from the
// environment.
type Credentials struct {
	ConsumerKey       string
	ConsumerSecret    string
	AccessToken       string
	AccessTokenSecret string
}

// AccountConfig identifies the ads account and its funding instrument.
type AccountConfig struct {
	AccountID           string
	FundingInstrumentID string
}

// Client is an X Ads API client. It is safe for sequential use; the X Ads API
// enforces a 1 write-request-per-second limit which this client honors.
type Client struct {
	creds   Credentials
	account AccountConfig

	baseURL    string
	apiVersion string
	httpClient *http.Client

	// nonceFn and timeFn are injectable for deterministic testing of the
	// OAuth signature. Production code uses the crypto/rand + wall-clock
	// defaults installed by NewClient.
	nonceFn func() string
	timeFn  func() time.Time

	// writeDelay paces sequential write requests within a single dispatch
	// (Twitter allows ~1 write/sec). Injectable so tests can set it to 0 rather
	// than incurring real per-request sleeps; defaults to the writeDelay const.
	writeDelay time.Duration
}

// Option customizes a Client at construction time.
type Option func(*Client)

// WithBaseURL overrides the API base URL (default DefaultBaseURL).
func WithBaseURL(u string) Option { return func(c *Client) { c.baseURL = u } }

// WithAPIVersion overrides the API version segment (default DefaultAPIVersion).
func WithAPIVersion(v string) Option { return func(c *Client) { c.apiVersion = v } }

// WithHTTPClient overrides the underlying *http.Client (default has a 30s
// timeout). A nil client is ignored so the option can't produce an unusable
// Client whose httpClient.Do would panic.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.httpClient = h
		}
	}
}

// WithWriteDelay overrides the inter-write pacing delay. A zero (or negative)
// value disables the pacing sleep entirely — useful in tests to avoid real
// per-request sleeps.
func WithWriteDelay(d time.Duration) Option { return func(c *Client) { c.writeDelay = d } }

// NewClient constructs a Client from injected credentials and account config.
func NewClient(creds Credentials, account AccountConfig, opts ...Option) *Client {
	// Normalize the account identifiers once, on the way in, so every method uses
	// the cleaned values. A stored connection can persist a padded id (" acc1 ");
	// left untrimmed it would validate non-empty yet corrupt the account path and
	// the funding_instrument_id param (a space-containing path/param guarantees an
	// API rejection). Trimming here keeps the trimmed value the one that is BOTH
	// validated non-empty AND sent in every request path/param.
	account.AccountID = strings.TrimSpace(account.AccountID)
	account.FundingInstrumentID = strings.TrimSpace(account.FundingInstrumentID)
	c := &Client{
		creds:      creds,
		account:    account,
		baseURL:    DefaultBaseURL,
		apiVersion: DefaultAPIVersion,
		httpClient: &http.Client{Timeout: requestTimeout},
		nonceFn:    defaultNonce,
		timeFn:     time.Now,
		writeDelay: writeDelay,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// ---------------------------------------------------------------------------
// OAuth 1.0a — HMAC-SHA1 signing
// ---------------------------------------------------------------------------

// percentEncode implements RFC 3986 percent-encoding as required by OAuth 1.0a.
// It mirrors the TS percentEncode (encodeURIComponent + escaping !'()*).
func percentEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// generateOAuthSignature computes the HMAC-SHA1 base64 signature over the
// OAuth 1.0a signature base string. Mirrors generateOAuthSignature in the TS.
func generateOAuthSignature(method, u string, params map[string]string, consumerSecret, tokenSecret string) string {
	// OAuth 1.0a (RFC 5849 §3.4.1.3.2) normalizes parameters by their
	// PERCENT-ENCODED name, breaking ties on the percent-encoded value — not by
	// the raw key. Sorting raw keys is wrong: e.g. "c@" encodes to "c%40" and
	// must sort BEFORE "c2" because '%' (0x25) < '2' (0x32), yet raw '@' (0x40)
	// sorts AFTER '2'. Encode first, then sort by (name, value) as a TUPLE.
	//
	// Sorting the joined "name=value" string is ALSO wrong: it misorders when one
	// encoded name is a prefix of another. Names "a" and "a1" must order a < a1,
	// but "a1=<v>" sorts BEFORE "a=<v>" on the joined form because '1' (0x31) <
	// '=' (0x3D). Compare names first, then values as a tiebreak.
	type encodedPair struct{ name, value string }
	pairs := make([]encodedPair, 0, len(params))
	for k, v := range params {
		pairs = append(pairs, encodedPair{percentEncode(k), percentEncode(v)})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].name != pairs[j].name {
			return pairs[i].name < pairs[j].name
		}
		return pairs[i].value < pairs[j].value
	})
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, p.name+"="+p.value)
	}
	paramString := strings.Join(parts, "&")

	baseString := strings.ToUpper(method) + "&" + percentEncode(u) + "&" + percentEncode(paramString)
	signingKey := percentEncode(consumerSecret) + "&" + percentEncode(tokenSecret)

	mac := hmac.New(sha1.New, []byte(signingKey))
	mac.Write([]byte(baseString))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// buildOAuthHeader builds the "Authorization: OAuth ..." header for a request.
// Query parameters present on rawURL are folded into the OAuth 1.0a signature
// base string (X Ads create calls carry their params on the query string).
// bodyParams is retained for callers that sign extra form params; callers here
// pass nil since no request carries a body.
func (c *Client) buildOAuthHeader(method, rawURL string, bodyParams map[string]string) (string, error) {
	oauthParams := map[string]string{
		"oauth_consumer_key":     c.creds.ConsumerKey,
		"oauth_nonce":            c.nonceFn(),
		"oauth_signature_method": "HMAC-SHA1",
		"oauth_timestamp":        strconv.FormatInt(c.timeFn().Unix(), 10),
		"oauth_token":            c.creds.AccessToken,
		"oauth_version":          "1.0",
	}

	// allParams = oauthParams + bodyParams + query params, used only for signing.
	allParams := make(map[string]string, len(oauthParams)+len(bodyParams))
	for k, v := range oauthParams {
		allParams[k] = v
	}
	for k, v := range bodyParams {
		allParams[k] = v
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse url %q: %w", rawURL, err)
	}
	for k, vs := range parsed.Query() {
		if len(vs) > 0 {
			allParams[k] = vs[0]
		}
	}

	// Base URL for signing excludes the query string (origin + path) and MUST be
	// normalized per RFC 5849 §3.4.1.2: the scheme and host are lowercased and a
	// port equal to the scheme's default (80 for http, 443 for https) is omitted.
	// WithBaseURL accepts any valid URL, so an input like "HTTPS://ADS-API.X.COM:443"
	// would otherwise be signed verbatim and X would reject the signature. Only the
	// base STRING is normalized here; the actual request still targets parsed as-is.
	signingURL := normalizeSigningURL(parsed)
	oauthParams["oauth_signature"] = generateOAuthSignature(method, signingURL, allParams, c.creds.ConsumerSecret, c.creds.AccessTokenSecret)

	keys := make([]string, 0, len(oauthParams))
	for k := range oauthParams {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, percentEncode(k)+"=\""+percentEncode(oauthParams[k])+"\"")
	}
	return "OAuth " + strings.Join(parts, ", "), nil
}

// normalizeSigningURL returns the RFC 5849 §3.4.1.2 normalized origin+path used
// in the OAuth 1.0a signature base string: scheme and host lowercased, and the
// port dropped when it is the scheme's default (http:80 / https:443). A
// non-default port is preserved. The query string is excluded (its params are
// signed separately). This is applied ONLY to the value fed into the base
// string; the request itself still goes to the un-normalized URL.
func normalizeSigningURL(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	// Re-bracket an IPv6 literal: Hostname() strips the brackets, but a URI
	// authority requires them (e.g. "[::1]"), otherwise the host and any ":port"
	// below would be ambiguous and the base-string authority wouldn't match the
	// request URI.
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	port := u.Port()
	// Omit the port when it matches the scheme default; keep it otherwise.
	if port != "" && (scheme != "http" || port != "80") && (scheme != "https" || port != "443") {
		host = host + ":" + port
	}
	// Use EscapedPath(), not the decoded u.Path: the request is sent with the
	// escaped path, so signing the decoded form (e.g. "/proxy/twitter" for a base
	// path "/proxy%2Ftwitter") would produce a signature the verifier — which
	// reconstructs the escaped request URI — rejects.
	return scheme + "://" + host + u.EscapedPath()
}

func defaultNonce() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand should not fail; fall back to a timestamp-derived value.
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

// ---------------------------------------------------------------------------
// HTTP helper
// ---------------------------------------------------------------------------

// apiResponse is the loose envelope returned by X Ads endpoints.
type apiResponse struct {
	Data json.RawMessage `json:"data"`
	// NextCursor is set on cursor-paginated list endpoints (campaigns,
	// line_items). Empty when there are no further pages.
	NextCursor string `json:"next_cursor"`
}

// maxListPages caps how many pages a name-lookup will page through, a safety
// bound against an unexpectedly huge account. The find-by-name callers request
// count=1000 (the X Ads v12 list max page size), so 25 pages cover up to
// 25 × 1000 = 25,000 records — comfortably beyond the ~8,000 active campaigns
// an X account can hold. Hitting the cap with a cursor still outstanding is
// treated as inconclusive (an error), never as "not found".
const maxListPages = 25

func (c *Client) accountURL() string {
	return fmt.Sprintf("%s/%s/accounts/%s", c.baseURL, c.apiVersion, c.account.AccountID)
}

// request performs an account-scoped X Ads API GET/list request with OAuth1
// signing and 429 exponential-backoff retry. Any parameters must be encoded
// into path as a query string. Mirrors twitterRequest in the TS for reads.
func (c *Client) request(ctx context.Context, method, path string) (*apiResponse, error) {
	return c.doRequest(ctx, method, path, nil)
}

// createRequest performs an X Ads API create (POST) call. Per the X Ads v12
// contract, create endpoints (campaigns, line_items, promoted_tweets) accept
// their parameters as URL query parameters, not a JSON body. The params are
// appended to the request URL and also folded into the OAuth signature base
// string (OAuth 1.0a signs query params), and the request is sent with no
// body. Callers own the 1-req/sec write delay.
func (c *Client) createRequest(ctx context.Context, path string, params map[string]string) (*apiResponse, error) {
	return c.doRequest(ctx, http.MethodPost, path, params)
}

// doRequest is the shared HTTP path with OAuth1 signing and 429
// exponential-backoff retry. queryParams, when non-nil, are appended to the
// request URL (create calls pass their params here); the request carries no
// body in either mode.
func (c *Client) doRequest(ctx context.Context, method, path string, queryParams map[string]string) (*apiResponse, error) {
	// An empty path targets the account root itself (accountURL) — used by
	// verifyAccount's GET — so don't append a bare "/" that would change the URL.
	reqURL := c.accountURL()
	if p := strings.TrimPrefix(path, "/"); p != "" {
		reqURL += "/" + p
	}

	if len(queryParams) > 0 {
		vals := url.Values{}
		for k, v := range queryParams {
			vals.Set(k, v)
		}
		sep := "?"
		if strings.Contains(reqURL, "?") {
			sep = "&"
		}
		reqURL += sep + vals.Encode()
	}

	for attempt := 0; attempt <= retryMax; attempt++ {
		authHeader, err := c.buildOAuthHeader(method, reqURL, nil)
		if err != nil {
			return nil, err
		}

		req, err := http.NewRequestWithContext(ctx, method, reqURL, http.NoBody)
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Authorization", authHeader)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("x ads api %s %s: %w", method, path, err)
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			// If this was the last attempt, don't sleep+retry: the loop would
			// exit and the 429 would otherwise fall through to the generic
			// non-2xx return below. Surface the intended exhausted-rate-limit
			// error instead.
			if attempt >= retryMax {
				_ = resp.Body.Close()
				return nil, fmt.Errorf("x ads api %s %s -> exhausted %d retries after 429s", method, path, retryMax)
			}
			waitDur := c.parseRetryAfter(resp)
			_ = resp.Body.Close()
			if waitDur > 0 {
				// The server declared a reset time (Retry-After delay or
				// X-Rate-Limit-Reset epoch). Honor it rather than clamping to a
				// small value and burning every retry while still limited. If the
				// wait exceeds our cap, sleeping would consume a retry without any
				// chance of the window clearing, so abort with the rate-limit error.
				if waitDur > maxRetryWait {
					return nil, fmt.Errorf("x ads api %s %s -> 429: rate-limit reset in %s exceeds max wait %s; aborting", method, path, waitDur.Round(time.Second), maxRetryWait)
				}
			} else {
				// No server-declared reset: fall back to computed exponential
				// backoff, clamped to maxRetryWait to match the header path above.
				// (Bounded in practice today since attempt <= retryMax, but clamp
				// defensively so the two 429 paths stay consistent.)
				waitDur = writeDelay * time.Duration(1<<uint(attempt))
				if waitDur > maxRetryWait {
					waitDur = maxRetryWait
				}
			}
			if err := sleepCtx(ctx, waitDur); err != nil {
				return nil, err
			}
			continue
		}

		respBody, readErr := readAll(resp)
		_ = resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			if readErr != nil {
				return nil, fmt.Errorf("x ads api %s %s -> %d: %s (body read error: %v)", method, path, resp.StatusCode, truncate(respBody, 400), readErr)
			}
			return nil, fmt.Errorf("x ads api %s %s -> %d: %s", method, path, resp.StatusCode, truncate(respBody, 400))
		}
		if readErr != nil {
			// A 2xx with a body we couldn't fully/ cleanly read: don't decode a
			// partial body into a misleading result — surface the I/O failure.
			return nil, fmt.Errorf("x ads api %s %s: %w", method, path, readErr)
		}

		var out apiResponse
		if len(respBody) > 0 {
			if err := json.Unmarshal(respBody, &out); err != nil {
				return nil, fmt.Errorf("decode response: %w", err)
			}
		}
		return &out, nil
	}

	return nil, fmt.Errorf("x ads api %s %s -> exhausted %d retries after 429s", method, path, retryMax)
}

// parseRetryAfter returns how long to wait before retrying a 429, or 0 if no
// usable header is present. Header precedence mirrors the official X Ads SDK:
// X-Account-Rate-Limit-Reset (account-scoped limits) is checked first, then
// X-Rate-Limit-Reset (endpoint-scoped), then Retry-After. Both *-Rate-Limit-Reset
// headers carry a Unix epoch timestamp (the X Ads API commonly returns only these
// on a 429), so they must be converted to a duration-until-reset rather than
// treated as a delay. Retry-After is either a delay in seconds or an HTTP-date.
// Never returns a negative duration.
func (c *Client) parseRetryAfter(resp *http.Response) time.Duration {
	// Account-scoped rate limits take precedence: an account-scoped 429 stays
	// limited across every endpoint until this reset, so honoring the shorter
	// endpoint header (or falling back to exponential backoff) would burn retries
	// while still limited.
	if d := c.resetHeaderDelay(resp.Header.Get("X-Account-Rate-Limit-Reset")); d > 0 {
		return d
	}
	if d := c.resetHeaderDelay(resp.Header.Get("X-Rate-Limit-Reset")); d > 0 {
		return d
	}
	if v := strings.TrimSpace(resp.Header.Get("Retry-After")); v != "" {
		// Delay-seconds form. ParseInt (not Atoi) into an int64 so a large numeric
		// value is captured rather than overflowing the platform int that Atoi
		// returns (which on a 32-bit int would surface as an error and silently
		// drop a real, if outsized, reset). Mirrors reddit's parseRetryAfter.
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			// Even a validly-parsed int64 seconds value can overflow when scaled to
			// nanoseconds: time.Duration(n)*time.Second wraps NEGATIVE for n beyond
			// ~9.2e9, which would slip past the caller's `> maxRetryWait` abort and
			// trigger an immediate retry before the declared reset. Guard the
			// conversion: any n STRICTLY ABOVE the max-wait ceiling (in seconds)
			// already exceeds the cap, so report a duration just over maxRetryWait
			// and let the caller's over-cap abort fire — never perform the wrapping
			// multiply. A value EXACTLY at the cap is allowed through and returned
			// as-is (via the multiply below), so it isn't spuriously aborted.
			if n > int64(maxRetryWait/time.Second) {
				return maxRetryWait + time.Second
			}
			return time.Duration(n) * time.Second
		}
		if t, err := http.ParseTime(v); err == nil {
			if d := t.Sub(c.timeFn()); d > 0 {
				return d
			}
		}
	}
	return 0
}

// resetHeaderDelay interprets a *-Rate-Limit-Reset header value (a Unix epoch
// timestamp) as a duration-until-reset relative to the injectable clock. Returns
// 0 for an empty/unparseable value or a reset that has already passed.
func (c *Client) resetHeaderDelay(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	epoch, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0
	}
	if d := time.Unix(epoch, 0).Sub(c.timeFn()); d > 0 {
		return d
	}
	return 0
}

// pace waits c.writeDelay between sequential write requests within a single
// dispatch, honoring context cancellation. A non-positive writeDelay disables
// the sleep (used by tests).
func (c *Client) pace(ctx context.Context) error {
	if c.writeDelay <= 0 {
		return nil
	}
	return sleepCtx(ctx, c.writeDelay)
}

// sleepCtx waits for d, honoring context cancellation.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// readAll reads up to maxResponseBody bytes (plus one, so truncation is
// detectable) from the response, surfacing both read and truncation errors
// rather than silently discarding them. io.ReadAll can return bytes together
// with an error, so a discarded error can hide a partial/corrupt body and turn
// a transport failure into a misleading JSON decode error downstream.
func readAll(resp *http.Response) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	if err != nil {
		return body, fmt.Errorf("read response body: %w", err)
	}
	if int64(len(body)) > maxResponseBody {
		return body[:maxResponseBody], fmt.Errorf("response body exceeds %d bytes", maxResponseBody)
	}
	return body, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n])
}

// ---------------------------------------------------------------------------
// Conversion + formatting helpers
// ---------------------------------------------------------------------------

// toMicroCurrency converts USD to micro-currency (x 1,000,000), rounded.
func toMicroCurrency(usd float64) int64 {
	return int64(math.Round(usd * 1_000_000))
}

// fromMicroCurrency converts micro-currency back to USD (/ 1,000,000).
func fromMicroCurrency(micro int64) float64 {
	return float64(micro) / 1_000_000
}

// toIso8601Utc formats a YYYY-MM-DD date string as an ISO8601 UTC timestamp
// at midnight. Mirrors toIso8601Utc in the TS.
func toIso8601Utc(dateStr string) string {
	return dateStr + "T00:00:00Z"
}

// ---------------------------------------------------------------------------
// Campaign lookup (idempotency)
// ---------------------------------------------------------------------------

type campaignElement struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// findCampaignByName returns the id of a campaign matching name, or "" if no
// such campaign exists. A non-nil error signals a transient/unexpected lookup
// failure (failed GET or undecodable response) — the caller must abort rather
// than treat it as "not found" and create a duplicate.
func (c *Client) findCampaignByName(ctx context.Context, name string) (string, error) {
	// q=<name> is X Ads' server-side name filter: it narrows the list to entities
	// whose name matches, so a lookup is O(matches) instead of scanning the whole
	// account (which could exceed the page cap on a large account and fail
	// name-based idempotency). q does substring/prefix matching, so findByName
	// still enforces the EXACT-name comparison locally. count=1000 (max page size,
	// independent of cursor) keeps any residual paging cheap.
	return c.findByName(ctx, "campaigns?with_deleted=false&count=1000&q="+url.QueryEscape(name), name)
}

// findLineItemByName returns the id of a line item matching name within a
// campaign, or "" if none exists. A non-nil error signals a lookup failure the
// caller must not swallow (see findCampaignByName).
func (c *Client) findLineItemByName(ctx context.Context, campaignID, name string) (string, error) {
	// The X Ads list endpoint filters line items with campaign_ids (plural);
	// campaign_id (singular) is the CREATE parameter. Using the singular key here
	// would leave the lookup unscoped and could reuse a same-named line item from
	// another campaign.
	// q=<name> is the server-side name filter (see findCampaignByName); it makes
	// the lookup O(matches), not O(account), while findByName still enforces the
	// exact-name match locally. count=1000 is the max page size.
	return c.findByName(ctx, "line_items?campaign_ids="+url.QueryEscape(campaignID)+"&with_deleted=false&count=1000&q="+url.QueryEscape(name), name)
}

// findByName pages through a cursor-paginated X Ads list endpoint (campaigns /
// line_items) looking for an element whose name matches exactly. It returns
// (id, nil) on a match, ("", nil) for a genuine not-found (the pages were read
// successfully but held no match), and ("", err) when a page GET or decode
// fails — so a transient error is never conflated with "not found" and the
// caller can abort instead of creating a duplicate. A name match whose element
// carries no usable id is likewise returned as ("", err), not ("", nil), so the
// caller does not follow with a create and duplicate an existing element. It
// follows next_cursor so a match beyond the first page is still found, bounded
// by maxListPages.
func (c *Client) findByName(ctx context.Context, path, name string) (string, error) {
	sep := "&"
	if !strings.Contains(path, "?") {
		sep = "?"
	}
	cursor := ""
	for page := 0; page < maxListPages; page++ {
		p := path
		if cursor != "" {
			p = path + sep + "cursor=" + url.QueryEscape(cursor)
		}
		resp, err := c.request(ctx, http.MethodGet, p)
		if err != nil {
			return "", fmt.Errorf("lookup %q: %w", name, err)
		}
		if resp == nil {
			return "", fmt.Errorf("lookup %q: empty response", name)
		}
		var items []campaignElement
		if err := json.Unmarshal(resp.Data, &items); err != nil {
			return "", fmt.Errorf("lookup %q: decode list: %w", name, err)
		}
		for _, it := range items {
			if it.Name == name {
				// A match with no usable id cannot be reused. Returning ("", nil)
				// here would read as "not found" and drive the caller into a create
				// POST, risking a duplicate of an element that already exists.
				// Surface it as a lookup error so the caller aborts instead.
				if it.ID == "" {
					return "", fmt.Errorf("lookup %q: matching element has no id; aborting to avoid creating a duplicate", name)
				}
				return it.ID, nil
			}
		}
		if resp.NextCursor == "" {
			return "", nil
		}
		cursor = resp.NextCursor
	}
	// Hit the page cap with a cursor still outstanding: we can't be sure the name
	// doesn't exist further on, so return an error rather than "not found" (which
	// would let the caller create a duplicate).
	return "", fmt.Errorf("lookup %q: exceeded %d pages with more results remaining; aborting to avoid creating a duplicate", name, maxListPages)
}

// ---------------------------------------------------------------------------
// Campaign name + UTM builders
// ---------------------------------------------------------------------------

func buildTwitterCampaignName(in CampaignInput) string {
	event := strings.ReplaceAll(in.EventName, "|", "-")
	project := boundProject(in.Project)
	project = strings.ReplaceAll(project, "|", "-")
	return fmt.Sprintf("Events | %s | Global | Awareness | Prospecting | Promoted Post | %s | MoFU", event, project)
}

// boundProject trims the caller-supplied project name and caps its rune length.
// The data pipeline parses the campaign name for attribution and joins on the
// caller-supplied canonical project slug; see docs/api-catalog.md. CreateCampaign
// rejects an empty Project up front, so this always receives a non-empty value —
// it does not substitute a default (which would misattribute the campaign).
// Project is otherwise unbounded, so bounding it here keeps the composed campaign
// name from ballooning.
func boundProject(project string) string {
	project = strings.TrimSpace(project)
	if r := []rune(project); len(r) > maxProjectLen {
		project = string(r[:maxProjectLen])
	}
	return project
}

// validateEntityName enforces X's 255-rune entity-name limit on a FINAL composed
// campaign / line-item name. Even with EventName and Project individually bounded,
// the composed name (event + project + fixed template) can exceed 255, so it is
// checked here before any create call. kind is "campaign" or "line item".
func validateEntityName(kind, name string) error {
	if n := len([]rune(name)); n > maxEntityNameLen {
		return fmt.Errorf("invalid %s name: composed name is %d characters, exceeds X's %d-character limit", kind, n, maxEntityNameLen)
	}
	return nil
}

var spaceRe = regexp.MustCompile(`\s+`)

// validateRegistrationURL ensures a user-supplied registration URL is an
// absolute http/https URL with a real host, before any mutating call. In the
// manual-tweet workflow (TweetID omitted) this URL is the only ad destination
// (it feeds the UTM/destination via buildTwitterUtmURL), and url.Parse alone is
// far too permissive: url.Parse("") succeeds (yielding a query-only
// "?utm_source=..." string), relative URLs are accepted, and "https://:443/x"
// parses with an empty Hostname(). Mirrors validateRegistrationURL in the
// reddit/linkedin clients: TrimSpace, require IsAbs()+Hostname()!="", scheme
// http/https.
func validateRegistrationURL(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fmt.Errorf("registration URL is required")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Errorf("registration URL %q is not a valid URL: %w", raw, err)
	}
	// Require a real host: url.Parse accepts "https://:443/path" (Host=":443")
	// where Hostname() is empty, so check Hostname() not just Host.
	if !u.IsAbs() || u.Hostname() == "" {
		return fmt.Errorf("registration URL %q must be absolute (include scheme and host)", raw)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return nil
	default:
		return fmt.Errorf("registration URL %q must use an http or https scheme, got %q", raw, u.Scheme)
	}
}

func buildTwitterUtmURL(in CampaignInput) string {
	slug := in.EventSlug
	if slug == "" {
		slug = spaceRe.ReplaceAllString(strings.ToLower(in.EventName), "-")
	}
	campaign := in.HSToken
	if campaign == "" {
		campaign = slug
	}

	raw := strings.TrimSpace(in.RegistrationURL)
	u, err := url.Parse(raw)
	if err != nil {
		// Unparseable URL: return it unchanged rather than corrupting it.
		return raw
	}
	// Merge UTM params into the URL's existing query and re-render, so the query
	// lands before any fragment (a naive string append would put it inside the
	// fragment, e.g. https://x/reg#a?utm_...).
	q := u.Query()
	q.Set("utm_source", "twitter")
	q.Set("utm_medium", "paid-social")
	q.Set("utm_campaign", campaign)
	q.Set("utm_term", spaceRe.ReplaceAllString(strings.ToLower(in.EventName), "-"))
	q.Set("utm_content", "promoted-tweet")
	u.RawQuery = q.Encode()
	return u.String()
}

// ---------------------------------------------------------------------------
// Public API — campaign creation
// ---------------------------------------------------------------------------

// CampaignInput carries the fields required to create an X Ads campaign.
// Mirrors the TS TwitterCampaignCreateRequest.
type CampaignInput struct {
	EventName       string
	EventSlug       string
	Project         string
	BudgetUsd       float64
	StartDate       string // YYYY-MM-DD
	EndDate         string // YYYY-MM-DD
	TweetID         string
	RegistrationURL string
	HSToken         string
}

// CampaignResult is the outcome of a campaign creation attempt, including a
// step-by-step log. Mirrors the TS TwitterCampaignCreateResult.
type CampaignResult struct {
	Platform        string
	CampaignName    string
	CampaignID      string
	LineItemName    string
	LineItemID      string
	PromotedTweetID string
	// PromotedTweetWarning is non-empty when the promoted-tweet association could
	// not be confirmed (POST failed, or returned a malformed/empty response). The
	// campaign and line item may still have been created, so the overall call is
	// not fatal, but consumers MUST NOT treat a result with this set as an
	// unqualified success — the promoted tweet may need to be added manually.
	PromotedTweetWarning string
	TwitterURL           string
	Steps                []string
}

var dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// accountIDRe restricts an X Ads account_id / funding_instrument_id to a safe
// charset. These ids interpolate directly into the account-scoped request path
// (accountURL) and into query params, so a value containing a path/query/
// fragment delimiter ('/', '?', '#') or whitespace/control chars could redirect
// a campaign/list POST to a DIFFERENT account-scoped path (path injection) or
// corrupt the funding param. Real X Ads ids are alphanumeric handles (e.g.
// "18ce54d4x5t"), so restrict to letters and digits — the tightest charset that
// still accepts every real id — and validate up front, before any mutating call.
var accountIDRe = regexp.MustCompile(`^[A-Za-z0-9]+$`)

// tweetIDRe matches an X Tweet id: a positive decimal snowflake of 1–19 digits
// with no leading zero. A malformed value ("not-a-tweet", "0", or an
// arbitrarily long decimal that can't be a real snowflake) would otherwise reach
// the promoted_tweets POST and be rejected AFTER the campaign and line item are
// already created, leaving a partial/orphaned campaign — so the format is
// validated up front, before any mutating call. (Snowflakes are positive int64s,
// so at most 19 digits; "0" and leading-zero forms are not valid ids.)
var tweetIDRe = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)

// validateDate enforces both the YYYY-MM-DD shape and that the value is a real
// calendar date. The regex alone accepts impossible dates like "2026-99-99",
// which would be forwarded as a bogus ISO8601 timestamp to the X Ads API; a
// strict time.Parse (which rejects out-of-range months/days) closes that gap
// before any mutating call. label is "start" or "end" for the error message.
func validateDate(label, date string) error {
	if !dateRe.MatchString(date) {
		return fmt.Errorf("invalid %s date format: %s — expected YYYY-MM-DD", label, date)
	}
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return fmt.Errorf("invalid %s date: %s is not a real calendar date", label, date)
	}
	return nil
}

// CreateCampaign runs the campaign -> line_item -> promoted_tweet creation
// flow, reusing existing entities by name for idempotency. It mirrors
// executeTwitterCampaignCreation in the TS. The campaign and line item are
// created PAUSED (entity_status=PAUSED); the promoted-tweet association is
// created ACTIVE by the API (the endpoint does not accept entity_status), but
// the paused line item gates delivery so nothing serves until it is enabled.
func (c *Client) CreateCampaign(ctx context.Context, in CampaignInput) (*CampaignResult, error) {
	steps := []string{}

	// Validate EventName before any mutating call: an empty/whitespace value
	// would produce identical generic campaign & line-item names for every such
	// request, letting the find-by-name lookup silently reuse an unrelated
	// campaign. Trim and normalize it up front so every downstream builder sees
	// the cleaned value.
	in.EventName = strings.TrimSpace(in.EventName)
	if in.EventName == "" {
		return nil, fmt.Errorf("invalid event name: must not be empty")
	}
	if utf8.RuneCountInString(in.EventName) > maxEventNameLen {
		return nil, fmt.Errorf("invalid event name: exceeds %d characters", maxEventNameLen)
	}

	// Validate Project before any mutating call. The Project segment of the
	// composed campaign name is the attribution key the data pipeline joins on, so
	// it must be the authenticated caller-supplied canonical slug. Defaulting an
	// omitted Project to a hardcoded slug (e.g. "tlf") would misattribute a
	// non-TLF campaign, so reject an empty/whitespace value outright. Trim and
	// store the cleaned value so every downstream builder sees the same input.
	in.Project = strings.TrimSpace(in.Project)
	if in.Project == "" {
		return nil, fmt.Errorf("invalid project: must not be empty")
	}

	// Validate budget. Reject NaN/Inf/non-positive, reject values above the
	// int64 micro-unit overflow cap, and reject anything that rounds to zero (or
	// negative) micro-units — such a value passes a naive >0 check but would send
	// a zero/negative daily_budget_amount_local_micro.
	if math.IsNaN(in.BudgetUsd) || math.IsInf(in.BudgetUsd, 0) || in.BudgetUsd <= 0 {
		return nil, fmt.Errorf("invalid budget: must be a positive number")
	}
	if in.BudgetUsd > maxBudgetUsd {
		return nil, fmt.Errorf("invalid budget: must be at most %v", maxBudgetUsd)
	}
	if toMicroCurrency(in.BudgetUsd) <= 0 {
		return nil, fmt.Errorf("invalid budget: %g rounds to zero micro-units", in.BudgetUsd)
	}
	if err := validateDate("start", in.StartDate); err != nil {
		return nil, err
	}
	if err := validateDate("end", in.EndDate); err != nil {
		return nil, err
	}
	if in.EndDate <= in.StartDate {
		return nil, fmt.Errorf("end date %s must be after start date %s", in.EndDate, in.StartDate)
	}

	// Validate the registration URL up front, before any mutating call. In the
	// manual-tweet workflow (TweetID omitted) it is the only ad destination and is
	// fed into buildTwitterUtmURL, which would otherwise accept an empty/relative/
	// non-http value and emit a corrupt destination. Require a valid absolute
	// http/https URL with a real host always, mirroring the reddit/linkedin clients.
	if err := validateRegistrationURL(in.RegistrationURL); err != nil {
		return nil, err
	}

	// Validate the tweet id FORMAT up front, before any mutating call. A blank
	// TweetID is optional (it skips the promoted-tweet step below), so only a
	// supplied value is checked. X Tweet ids are decimal snowflake ids; a
	// non-numeric value ("not-a-tweet") would otherwise reach the promoted_tweets
	// POST and be rejected only AFTER the campaign and line item are created,
	// leaving a partial campaign. Trim and store the cleaned value so the same
	// value is validated here and sent in Step 4.
	in.TweetID = strings.TrimSpace(in.TweetID)
	if in.TweetID != "" {
		if !tweetIDRe.MatchString(in.TweetID) {
			return nil, fmt.Errorf("invalid tweet id: %q must be a numeric X Tweet id", in.TweetID)
		}
		// The 1–19 digit shape still admits values above the max positive int64
		// snowflake (e.g. 9999999999999999999); parse to reject those before any
		// mutating call rather than letting X reject tweet_ids after the campaign
		// and line item exist.
		if _, perr := strconv.ParseInt(in.TweetID, 10, 64); perr != nil {
			return nil, fmt.Errorf("invalid tweet id: %q is out of range for an X Tweet id", in.TweetID)
		}
	}

	// Validate required account config before any mutating call. account_id and
	// funding_instrument_id are both required by the X Ads campaign-create
	// contract, but a stored connection may persist them as empty (or
	// whitespace-only) strings (the connection contract permits
	// funding_instrument_id to be omitted). NewClient already trimmed both, so a
	// padded " acc1 " is now stored — and thus validated AND sent — as "acc1";
	// checking the stored (trimmed) value here means a whitespace-only input is
	// rejected outright rather than corrupting the account path / funding param.
	if c.account.AccountID == "" {
		return nil, fmt.Errorf("invalid account config: account_id must not be empty")
	}
	if !accountIDRe.MatchString(c.account.AccountID) {
		// A non-empty check is not enough: account_id is interpolated into the
		// account-scoped request path (accountURL), so a value with '/', '?', '#',
		// or whitespace/control chars could redirect this POST to a different
		// account path. Reject anything outside the safe alphanumeric charset.
		return nil, fmt.Errorf("invalid account config: account_id %q must contain only letters and digits", c.account.AccountID)
	}
	if c.account.FundingInstrumentID == "" {
		return nil, fmt.Errorf("invalid account config: funding_instrument_id must not be empty")
	}
	if !accountIDRe.MatchString(c.account.FundingInstrumentID) {
		return nil, fmt.Errorf("invalid account config: funding_instrument_id %q must contain only letters and digits", c.account.FundingInstrumentID)
	}

	// Compose and validate the entity names before ANY network call: even with
	// EventName and Project individually bounded, the composed campaign / line-item
	// names can exceed X's 255-rune entity-name limit, so reject an oversized name
	// up front rather than after a wasted account-verify / lookup round trip.
	campaignName := buildTwitterCampaignName(in)
	if err := validateEntityName("campaign", campaignName); err != nil {
		return nil, err
	}
	lineItemName := fmt.Sprintf("Events | %s | Promoted Tweets | AUTO", strings.ReplaceAll(in.EventName, "|", "-"))
	if err := validateEntityName("line item", lineItemName); err != nil {
		return nil, err
	}

	// Step 1: verify account (non-fatal).
	c.verifyAccount(ctx, &steps)

	// Step 2: create campaign (PAUSED), reusing by name.
	campaignID, err := c.findCampaignByName(ctx, campaignName)
	if err != nil {
		return nil, err
	}
	// Track whether the campaign was created by THIS call or reused from a prior
	// one, so downstream partial-failure messages don't claim "created" for a
	// resource this call merely found.
	campaignReused := campaignID != ""
	if campaignID != "" {
		// Find-or-create is idempotent by name, but a reused campaign may have been
		// created with a DIFFERENT budget/config than THIS request carries (e.g. a
		// re-dispatch with a corrected BudgetUsd). We deliberately do NOT update the
		// campaign here — that is a separate PUT endpoint and an authoritative
		// reconcile is the orchestrator's job (LFXV2-2665). Surface the divergence as
		// a warning step (mirroring the promoted-tweet warning pattern) so an operator
		// can see the existing config was NOT changed to match this request.
		steps = append(steps, fmt.Sprintf("Reusing existing campaign: %s", campaignID))
		steps = append(steps, fmt.Sprintf("Warning: reused existing campaign %s by name; its budget/config were NOT updated to match this request ($%.2f/day) — verify/reconcile in X Ads Manager", campaignID, in.BudgetUsd))
	} else {
		// X Ads v12 create endpoints take parameters as URL query params (not a
		// JSON body), and use entity_status=PAUSED (not paused=true). Note: the
		// campaign endpoint does NOT accept start_time/end_time in v12 — flight
		// dates belong on the line item (sent below); including them here gets the
		// campaign create rejected.
		campaignParams := map[string]string{
			"name":                            campaignName,
			"funding_instrument_id":           c.account.FundingInstrumentID,
			"daily_budget_amount_local_micro": strconv.FormatInt(toMicroCurrency(in.BudgetUsd), 10),
			"entity_status":                   "PAUSED",
		}
		// These inter-request sleeps pace THIS dispatch's own sequential writes
		// (campaign -> line item -> promoted tweet) to stay under X's per-second
		// write rate. They do NOT enforce X's account-wide write limit across
		// concurrent or replicated dispatches: this service dispatches jobs async
		// (possibly across replicas), and separately-constructed clients in
		// different goroutines/processes can wake and POST at the same instant.
		// Correct account-wide limiting needs shared cross-replica coordination
		// (a distributed limiter or the orchestrator serializing per account),
		// which is out of scope for this stateless per-request client and is
		// tracked by LFXV2-2665 (durable dispatch). If the account limit is hit
		// anyway, the 429 exponential-backoff retry in doRequest is the backstop.
		if err := c.pace(ctx); err != nil {
			return nil, err
		}
		resp, err := c.createRequest(ctx, "campaigns", campaignParams)
		if err != nil {
			return nil, err
		}
		campaignID = extractID(resp)
		if campaignID == "" {
			return nil, fmt.Errorf("x campaign creation succeeded but returned no campaign ID")
		}
		steps = append(steps, fmt.Sprintf("Campaign created: %s (PAUSED, $%.2f/day)", campaignID, in.BudgetUsd))
	}

	// partialResult builds a *CampaignResult carrying the already-created (PAUSED)
	// campaign — and, once known, the line item — plus the steps completed so far.
	// It is returned ALONGSIDE the error at every downstream failure point after the
	// campaign POST already succeeded, so an orphaned paid resource is identifiable
	// for cleanup/reconcile and a caller retry can reconcile it instead of blindly
	// creating a duplicate. This only makes the orphan IDENTIFIABLE — it does not
	// resume creation. True retry-safe idempotency (not re-creating the campaign /
	// line item on retry) needs provider idempotency keys / the orchestrator claim,
	// tracked in LFXV2-2665. Mirrors the meta/reddit clients' partial-result helper.
	// lineItemID is captured by reference so the returned result includes it once
	// Step 3 has created it.
	var lineItemID string
	var lineItemReused bool
	partialResult := func() *CampaignResult {
		return &CampaignResult{
			Platform:     "twitter-ads",
			CampaignName: campaignName,
			CampaignID:   campaignID,
			LineItemName: lineItemName,
			LineItemID:   lineItemID,
			TwitterURL:   AdsManagerURL,
			Steps:        steps,
		}
	}
	// campaignStatus / lineItemStatus describe, in partial-failure error messages,
	// whether the resource that already exists was CREATED by this call or REUSED
	// (found by name). Wording a reused resource as "created" is misleading during
	// cleanup/reconcile, so the message reflects the actual provenance.
	campaignStatus := func() string {
		if campaignReused {
			return fmt.Sprintf("campaign %s reused, PRE-EXISTING", campaignID)
		}
		return fmt.Sprintf("campaign %s created, PAUSED", campaignID)
	}
	lineItemStatus := func() string {
		if lineItemReused {
			return fmt.Sprintf("line item %s reused, PRE-EXISTING", lineItemID)
		}
		return fmt.Sprintf("line item %s created, PAUSED", lineItemID)
	}

	// Step 3: create line item (ad group), reusing by name.
	lineItemID, err = c.findLineItemByName(ctx, campaignID, lineItemName)
	if err != nil {
		return partialResult(), fmt.Errorf("x line item lookup failed (%s): %w", campaignStatus(), err)
	}
	if lineItemID != "" {
		lineItemReused = true
		// A same-name line item is reused without re-checking its entity_status or
		// flight dates. If it was previously ENABLED, the promoted-tweet POST below
		// attaches an ACTIVE association to a line item that could be serving — the
		// PAUSED/flight gating this request expects is NOT re-applied. We do NOT PATCH
		// the line item to PAUSED here (separate endpoint; authoritative reconcile is
		// the orchestrator's job, LFXV2-2665). Surface a warning step so an operator
		// knows delivery may not be gated as expected.
		steps = append(steps, fmt.Sprintf("Reusing existing line item: %s", lineItemID))
		steps = append(steps, fmt.Sprintf("Warning: reused existing line item %s by name; its entity_status/flight dates were NOT reset to the requested PAUSED/%s–%s — it may already be ENABLED and serving; verify in X Ads Manager", lineItemID, in.StartDate, in.EndDate))
	} else {
		// X Ads v12 line_items: params go on the query string; start_time and
		// end_time are REQUIRED; bid_strategy=AUTO selects automatic bidding
		// (the field is bid_strategy in v12, not bid_type); entity_status
		// replaces the removed paused flag.
		lineItemParams := map[string]string{
			"campaign_id":   campaignID,
			"name":          lineItemName,
			"product_type":  "PROMOTED_TWEETS",
			"placements":    "ALL_ON_TWITTER",
			"objective":     "WEBSITE_CLICKS",
			"bid_strategy":  "AUTO",
			"start_time":    toIso8601Utc(in.StartDate),
			"end_time":      toIso8601Utc(in.EndDate),
			"entity_status": "PAUSED",
		}
		if err := c.pace(ctx); err != nil {
			return partialResult(), fmt.Errorf("x line item creation aborted (%s): %w", campaignStatus(), err)
		}
		resp, err := c.createRequest(ctx, "line_items", lineItemParams)
		if err != nil {
			return partialResult(), fmt.Errorf("x line item creation failed (%s): %w", campaignStatus(), err)
		}
		lineItemID = extractID(resp)
		if lineItemID == "" {
			return partialResult(), fmt.Errorf("x line item creation succeeded but returned no line item ID (%s)", campaignStatus())
		}
		steps = append(steps, fmt.Sprintf("Line item created: %s (PAUSED, ALL_ON_TWITTER, AUTO bid)", lineItemID))
	}

	// Step 4: create promoted tweet if a tweet ID was provided. in.TweetID was
	// already trimmed AND format-validated (numeric) in the up-front validation
	// block, so a whitespace-only value ("   ") is treated as absent, a padded
	// value (" 123 ") is sent as "123", and a non-numeric value never reaches
	// here — it fails before the campaign + line item are created.
	tweetID := in.TweetID
	var promotedTweetID string
	var promotedTweetWarning string
	if tweetID != "" {
		if err := c.pace(ctx); err != nil {
			// The campaign AND line item are already created (both PAUSED). Returning
			// a nil result would discard both IDs, preventing cleanup/reconciliation
			// and letting a caller retry create a duplicate. Return a partial result
			// carrying both IDs (and the steps so far) alongside the wrapped error.
			return partialResult(), fmt.Errorf("x promoted tweet creation aborted (%s / %s): %w", campaignStatus(), lineItemStatus(), err)
		}
		// The promoted_tweets endpoint does not accept entity_status; the API
		// creates the association ACTIVE. Delivery is still gated by the PAUSED
		// line item above, so we intentionally send only the association params.
		// This POST is always re-issued on a repeated CreateCampaign (unlike the
		// find-or-create campaign/line-item steps), so a lost first response can
		// make the retry hit a duplicate — handled below.
		resp, err := c.createRequest(ctx, "promoted_tweets", map[string]string{
			"line_item_id": lineItemID,
			"tweet_ids":    tweetID,
		})
		switch {
		case err != nil && isDuplicatePromotedTweetErr(err):
			// X reports the tweet is already promoted (DUPLICATE_PROMOTABLE_ENTITY).
			// This is NOT proof the tweet is attached to THIS line item — X returns
			// the same code when the tweet is promoted by a DIFFERENT line item — so
			// we do NOT treat it as idempotent success. Surface a warning (step +
			// result field) so the association is verified manually. Non-fatal: the
			// campaign and line item still return.
			promotedTweetWarning = fmt.Sprintf("promoted-tweet association for tweet %s may already exist (X returned DUPLICATE_PROMOTABLE_ENTITY), possibly on a different line item — verify manually in X Ads Manager", tweetID)
			steps = append(steps, fmt.Sprintf("Promoted tweet reported as duplicate for line item %s (tweet: %s) — the association may already exist (possibly on a different line item); verify manually in X Ads Manager", lineItemID, tweetID))
		case err != nil:
			// A real POST failure. Do NOT report unqualified success: record a
			// warning both in the step log and on the result so the caller can see
			// the promoted tweet may not have been created/associated.
			promotedTweetWarning = fmt.Sprintf("promoted-tweet POST failed for tweet %s: %s", tweetID, err.Error())
			steps = append(steps, fmt.Sprintf("Promoted tweet creation failed: %s — add manually in X Ads Manager", err.Error()))
		default:
			promotedTweetID = extractPromotedTweetID(resp)
			if promotedTweetID != "" {
				steps = append(steps, fmt.Sprintf("Promoted tweet created: %s (tweet: %s; created ACTIVE by the API but held from serving by the PAUSED line item)", promotedTweetID, tweetID))
			} else {
				// A 2xx response missing data.id is a malformed success: don't
				// silently treat it as done. Surface a warning (step + result field)
				// so the gap is visible without making the whole flow fatal.
				promotedTweetWarning = fmt.Sprintf("promoted-tweet POST returned no ID (malformed response) for tweet %s", tweetID)
				steps = append(steps, fmt.Sprintf("Promoted tweet creation returned no promoted-tweet ID (malformed response, tweet: %s) — add it manually in X Ads Manager", tweetID))
			}
		}
	} else {
		utmURL := buildTwitterUtmURL(in)
		steps = append(steps, "No tweet ID provided — post a tweet manually, then add it as a promoted tweet in X Ads Manager")
		steps = append(steps, fmt.Sprintf("Destination URL with UTM: %s", utmURL))
	}

	return &CampaignResult{
		Platform:             "twitter-ads",
		CampaignName:         campaignName,
		CampaignID:           campaignID,
		LineItemName:         lineItemName,
		LineItemID:           lineItemID,
		PromotedTweetID:      promotedTweetID,
		PromotedTweetWarning: promotedTweetWarning,
		TwitterURL:           AdsManagerURL,
		Steps:                steps,
	}, nil
}

// verifyAccount performs a best-effort account lookup, appending a step. It goes
// through doRequest (an empty path targets the account root) so it gets the SAME
// OAuth1 signing and 429 rate-limit retry/backoff as every other call — unlike
// the earlier version, which fired httpClient.Do directly and thus skipped the
// shared retry path. All failures remain non-fatal (mirrors the TS Step 1
// try/catch): doRequest surfaces a non-2xx status as an error, which is recorded
// here as a warning step and NOT propagated, so verification never aborts
// CreateCampaign.
func (c *Client) verifyAccount(ctx context.Context, steps *[]string) {
	resp, err := c.request(ctx, http.MethodGet, "")
	if err != nil {
		*steps = append(*steps, fmt.Sprintf("Account verification warning: %s", err.Error()))
		return
	}
	name := c.account.AccountID
	if resp != nil && len(resp.Data) > 0 {
		var obj struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(resp.Data, &obj); err == nil && obj.Name != "" {
			name = obj.Name
		}
	}
	*steps = append(*steps, fmt.Sprintf("Account verified: %s", name))
}

// extractID reads data.id from a response envelope.
func extractID(resp *apiResponse) string {
	if resp == nil || len(resp.Data) == 0 {
		return ""
	}
	var obj struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(resp.Data, &obj); err == nil {
		return obj.ID
	}
	return ""
}

// isDuplicatePromotedTweetErr reports whether err from a promoted_tweets POST is
// X's DUPLICATE_PROMOTABLE_ENTITY rejection. Because doRequest surfaces non-2xx
// bodies as the error string, we match X's recognizable error code. A match does
// NOT prove this tweet is attached to THIS line item: X returns this code when
// the tweet is already promoted by a DIFFERENT line item, so it cannot be treated
// as idempotent success — callers surface it as a warning to verify manually
// rather than as an unqualified success. NOTE: true cross-call idempotency
// (idempotency keys sent to X) is tracked in LFXV2-2665.
func isDuplicatePromotedTweetErr(err error) bool {
	if err == nil {
		return false
	}
	// Match only X's specific DUPLICATE_PROMOTABLE_ENTITY error code. Broad
	// substring matches on "already promoted"/"already associated" widened the net
	// to messages that don't actually prove a duplicate, so drop them: the code is
	// the recognizable, unambiguous signal.
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "duplicate_promotable_entity")
}

// extractPromotedTweetID reads the promoted tweet id, which the X Ads API
// returns as an array (data[0].id) or occasionally a single object.
func extractPromotedTweetID(resp *apiResponse) string {
	if resp == nil || len(resp.Data) == 0 {
		return ""
	}
	var arr []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(resp.Data, &arr); err == nil {
		if len(arr) > 0 {
			return arr[0].ID
		}
		return ""
	}
	return extractID(resp)
}
