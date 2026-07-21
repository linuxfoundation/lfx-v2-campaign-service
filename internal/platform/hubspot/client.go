// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package hubspot is a Go client for the HubSpot API. It drives the email
// channel's HubSpot surface: marketing-email clone/patch/content-set, CRM
// contact-list CRUD, and event-definition lookups. Credentials and account
// configuration are injected via NewClient; the package never reads environment
// variables or touches the database. In production the bearer token comes from a
// decrypted stored connection (`hubspot_connections.private_app_token`).
//
// Unlike the ad-platform clients (OAuth2 refresh flow for Google Ads, OAuth1 for
// X), HubSpot authenticates with a STATIC private-app bearer token — there is no
// token-exchange endpoint, so the request layer just attaches the injected token.
// Everything else (no-follow redirects, bounded reads, typed body-free errors,
// pre-send/ambiguous/definite classification, 429 retry gated on an explicit
// idempotent flag) mirrors the googleads/reddit/meta/twitter clients.
package hubspot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const (
	// DefaultBaseURL is the HubSpot API origin. All v3/v4 REST endpoints hang off
	// this host (e.g. /marketing/v3/emails, /crm/v3/lists/).
	DefaultBaseURL = "https://api.hubapi.com"

	// AppBaseURL is the HubSpot app origin used to build human-facing links to
	// created lists/emails (returned to callers for reference, never sent to the API).
	AppBaseURL = "https://app.hubspot.com"

	// requestTimeout bounds a single HTTP round-trip.
	requestTimeout = 30 * time.Second

	// retryMax is the number of times a rate-limited (429) IDEMPOTENT request is
	// retried with bounded backoff. A non-idempotent (mutating) request is never
	// retried — HubSpot list/email creates have no idempotency key, so a 429 whose
	// first attempt may already have committed would double-create on retry.
	retryMax = 3

	// retryBaseDelay is the base backoff for a 429 retry; the effective wait honors
	// Retry-After when the server sends it, clamped to maxRetryWait.
	retryBaseDelay = 1 * time.Second

	// maxRetryWait caps how long one 429 backoff will sleep. If a server-declared
	// Retry-After exceeds this, the call aborts with the 429 error rather than
	// sleeping pointlessly (and a hostile huge value can't wedge the client).
	maxRetryWait = 60 * time.Second

	// maxResponseBody bounds how much of any response body is read into memory,
	// guarding against a hostile/oversized reply while comfortably exceeding any
	// normal HubSpot response or error envelope.
	maxResponseBody = 10 << 20 // 10 MiB
)

// ---------------------------------------------------------------------------
// Injected configuration
// ---------------------------------------------------------------------------

// Credentials holds the HubSpot private-app bearer token used for all API calls.
// It is injected (from a decrypted stored connection), never read from the
// environment.
type Credentials struct {
	// PrivateAppToken is the HubSpot private-app access token sent as the bearer
	// credential on every request.
	PrivateAppToken string
}

// AccountConfig identifies the HubSpot portal the client operates against. It is
// used to build human-facing app links (never sent to the API) and, later, to
// scope brand-kit-driven content.
type AccountConfig struct {
	// PortalID is the HubSpot portal (hub) id. Optional; only used to build app
	// links for created assets.
	PortalID string
	// Label is a human-readable account label surfaced on results.
	Label string
}

// Client is a HubSpot API client. It is safe for concurrent use (it holds no
// per-request mutable state; the bearer token is static).
type Client struct {
	creds   Credentials
	account AccountConfig

	baseURL    string
	appBaseURL string
	httpClient *http.Client

	// retryBaseDelay is injectable so tests avoid real per-retry sleeps.
	retryBaseDelay time.Duration

	// now is injectable so tests can compute an HTTP-date Retry-After delay
	// deterministically. Defaults to time.Now.
	now func() time.Time
}

// Option customizes a Client at construction time.
type Option func(*Client)

// noFollow is the CheckRedirect policy for every client this package uses: it
// returns http.ErrUseLastResponse so the client does NOT follow redirects and
// hands the 3xx response back to the request layer, where a non-2xx status is
// surfaced as an error. HubSpot returns JSON directly and never legitimately
// 3xx-redirects these calls; not following keeps outcome classification sound — a
// redirect can't carry an already-sent mutating POST to a different target.
// Mirrors the reddit/googleads clients.
func noFollow(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}

// WithBaseURL overrides the API base URL (default DefaultBaseURL). Trailing
// slashes are trimmed so path building never produces a double slash. Primarily
// for tests (httptest.Server).
func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") }
}

// WithAppBaseURL overrides the app base URL used for human-facing links.
func WithAppBaseURL(u string) Option {
	return func(c *Client) { c.appBaseURL = strings.TrimRight(u, "/") }
}

// WithHTTPClient overrides the underlying *http.Client (default has a 30s
// timeout). A nil client is ignored so the option can't produce an unusable
// Client whose httpClient.Do would panic. Redirect following is force-disabled on
// whatever client ends up in use (see NewClient).
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.httpClient = h
		}
	}
}

// withRetryBaseDelay overrides the 429 backoff base (tests set it to ~0).
func withRetryBaseDelay(d time.Duration) Option {
	return func(c *Client) { c.retryBaseDelay = d }
}

// withClock overrides the clock so tests can compute an HTTP-date Retry-After delay
// deterministically.
func withClock(now func() time.Time) Option {
	return func(c *Client) { c.now = now }
}

// NewClient constructs a Client from injected credentials and account config.
// Redirect following is force-disabled on the client actually used — including one
// supplied via WithHTTPClient — by building a FRESH *http.Client carrying only the
// caller's reusable exported fields (Transport, Jar, Timeout) plus noFollow, so the
// caller's own client is never mutated. Mirrors the meta/googleads clients.
func NewClient(creds Credentials, account AccountConfig, opts ...Option) *Client {
	c := &Client{
		creds:          creds,
		account:        account,
		baseURL:        DefaultBaseURL,
		appBaseURL:     AppBaseURL,
		httpClient:     &http.Client{Timeout: requestTimeout, CheckRedirect: noFollow},
		retryBaseDelay: retryBaseDelay,
		now:            time.Now,
	}
	for _, opt := range opts {
		opt(c)
	}
	// Enforce no-follow on whatever client is now in use without mutating a
	// caller-supplied client: rebuild it from its exported reusable fields.
	c.httpClient = &http.Client{
		Transport:     c.httpClient.Transport,
		Jar:           c.httpClient.Jar,
		Timeout:       c.httpClient.Timeout,
		CheckRedirect: noFollow,
	}
	return c
}

// ---------------------------------------------------------------------------
// Typed errors (mirror the sibling clients: bodies/secrets never surfaced)
// ---------------------------------------------------------------------------

// apiError is a non-2xx HubSpot response. Error() renders only method/path/status
// — the response body is NOT echoed (a HubSpot error envelope can quote request
// material), matching the reddit/googleads discipline. Body is retained solely for
// internal classification (e.g. matching a specific HubSpot error category) and is
// never surfaced by Error().
type apiError struct {
	StatusCode int
	Method     string
	Path       string
	// Body is a bounded snapshot retained for classification only; never rendered.
	Body string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("hubspot %s %s -> %d", e.Method, e.Path, e.StatusCode)
}

// transportError wraps a round-trip failure that happened AFTER the request was
// plausibly sent (mid-flight timeout, EOF, reset), OR a failure to read a 2xx
// response: the server may or may not have processed the request, so the outcome
// is AMBIGUOUS. A pre-send failure (request build, pre-connect dial — see
// isPreSendDialError) is NOT wrapped as transportError. Error() renders a URL-free
// cause so a request URL (which can carry query material) never leaks; Unwrap()
// retains the cause for errors.Is/As. Mirrors the twitter client's safeTransportCause.
type transportError struct {
	Method string
	Path   string
	Err    error
}

func (e *transportError) Error() string {
	return fmt.Sprintf("hubspot %s %s: %s", e.Method, e.Path, safeCause(e.Err))
}

func (e *transportError) Unwrap() error { return e.Err }

// safeCause renders a URL-free description of a round-trip error. A *url.Error's
// %v embeds the request URL, so peel every *url.Error layer down to the underlying
// cause (timeout/EOF/reset), which carries no URL.
func safeCause(err error) string {
	if err == nil {
		return "transport failure"
	}
	for {
		var ue *url.Error
		if !errors.As(err, &ue) {
			break
		}
		if ue.Err == nil {
			return "transport failure"
		}
		err = ue.Err
	}
	return err.Error()
}

// isPreSendDialError reports whether a httpClient.Do error clearly happened BEFORE
// the request could be sent — a DNS resolution failure or a connect-time dial
// failure (connection refused / no route / network unreachable). Only these prove
// the request never reached HubSpot, so a mutation definitely did not happen. Every
// other Do error (mid-flight timeout, EOF, reset) is ambiguous. Mirrors the
// reddit/meta/googleads clients.
func isPreSendDialError(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		if errors.Is(err, syscall.ECONNREFUSED) ||
			errors.Is(err, syscall.EHOSTUNREACH) ||
			errors.Is(err, syscall.ENETUNREACH) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Request layer
// ---------------------------------------------------------------------------

// doRequest performs one HubSpot REST call against {baseURL}/{path}, attaching the
// static bearer token. body is JSON-encoded when non-nil. It returns the raw 2xx
// body bytes; non-2xx and transport failures are classified per the ambiguity
// contract (apiError / transportError / plain pre-send error).
//
// idempotent gates 429 retry behavior: a rate-limited idempotent call (a GET read)
// is retried up to retryMax times with a bounded backoff honoring Retry-After. A
// NON-idempotent call (a create/clone) is NOT retried — HubSpot creates have no
// idempotency key, so a 429 whose first attempt may already have committed would
// double-create on retry; for those the 429 is returned as an apiError immediately.
func (c *Client) doRequest(ctx context.Context, method, path string, body any, idempotent bool) ([]byte, error) {
	if c.creds.PrivateAppToken == "" {
		return nil, fmt.Errorf("hubspot: missing private-app token")
	}

	var encoded []byte
	if body != nil {
		b, mErr := json.Marshal(body)
		if mErr != nil {
			return nil, fmt.Errorf("marshal request body: %w", mErr)
		}
		encoded = b
	}

	u := c.baseURL + "/" + strings.TrimPrefix(path, "/")

	for attempt := 0; attempt <= retryMax; attempt++ {
		var reqBody io.Reader
		if encoded != nil {
			reqBody = bytes.NewReader(encoded)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, reqBody)
		if err != nil {
			// Request build failure is definitively pre-send (nothing was sent).
			return nil, fmt.Errorf("hubspot build request %s %s: %w", method, path, err)
		}
		req.Header.Set("Authorization", "Bearer "+c.creds.PrivateAppToken)
		req.Header.Set("Accept", "application/json")
		if encoded != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			if isPreSendDialError(err) {
				// Definitely not sent — a clean pre-send failure (plain error, not
				// ambiguous). Rendered URL-free via safeCause.
				return nil, fmt.Errorf("hubspot %s %s: %s", method, path, safeCause(err))
			}
			return nil, &transportError{Method: method, Path: path, Err: err}
		}

		// 429: retry only an idempotent call; a mutating 429 may have committed.
		if resp.StatusCode == http.StatusTooManyRequests {
			if !idempotent || attempt >= retryMax {
				snap := c.readErrorSnapshot(resp)
				_ = resp.Body.Close()
				return nil, &apiError{StatusCode: resp.StatusCode, Method: method, Path: path, Body: snap}
			}
			wait := c.retryAfter(resp, attempt)
			_ = resp.Body.Close()
			if wait > maxRetryWait {
				return nil, &apiError{StatusCode: http.StatusTooManyRequests, Method: method, Path: path}
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
			continue
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			snap := c.readErrorSnapshot(resp)
			_ = resp.Body.Close()
			return nil, &apiError{StatusCode: resp.StatusCode, Method: method, Path: path, Body: snap}
		}

		raw, rErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
		_ = resp.Body.Close()
		if rErr != nil {
			// A 2xx whose body could not be fully read is AMBIGUOUS (the mutation may
			// have been applied server-side even though we couldn't read the reply).
			return nil, &transportError{Method: method, Path: path, Err: rErr}
		}
		if int64(len(raw)) > maxResponseBody {
			return nil, &transportError{Method: method, Path: path, Err: fmt.Errorf("response body exceeds %d bytes", maxResponseBody)}
		}
		return raw, nil
	}

	// Unreachable: the loop returns on the last attempt.
	return nil, &apiError{StatusCode: http.StatusTooManyRequests, Method: method, Path: path}
}

// readErrorSnapshot reads a bounded prefix of a non-2xx body for internal
// classification only (never surfaced by apiError.Error()).
func (c *Client) readErrorSnapshot(resp *http.Response) string {
	const maxErrSnapshot = 512
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrSnapshot))
	return string(b)
}

// retryAfter computes the 429 backoff: honor a server-declared Retry-After when
// present, else exponential backoff off retryBaseDelay. An over-cap server value is
// returned as maxRetryWait+1s to signal "over cap" so the caller aborts.
//
// Retry-After has TWO valid forms (RFC 7231): a delay in seconds ("120") OR an
// HTTP-date ("Wed, 21 Oct 2026 07:28:00 GMT"). HubSpot can send either; parsing only
// the seconds form silently dropped an HTTP-date and fell back to exponential
// backoff, ignoring the server's stated reset time.
func (c *Client) retryAfter(resp *http.Response, attempt int) time.Duration {
	if d, ok := c.parseRetryAfter(resp.Header.Get("Retry-After")); ok {
		if d > maxRetryWait {
			return maxRetryWait + time.Second // signal "over cap" to the caller
		}
		return d
	}
	d := c.retryBaseDelay * time.Duration(1<<uint(attempt))
	if d > maxRetryWait {
		d = maxRetryWait
	}
	return d
}

// parseRetryAfter parses a Retry-After header value in either RFC 7231 form —
// delay-seconds or an HTTP-date — into a positive wait. Returns ok=false when the
// header is absent/blank/unparseable or the resulting delay is not positive (a
// past/now HTTP-date), so the caller falls back to exponential backoff.
func (c *Client) parseRetryAfter(ra string) (time.Duration, bool) {
	ra = strings.TrimSpace(ra)
	if ra == "" {
		return 0, false
	}
	if secs, err := parsePositiveInt(ra); err == nil && secs > 0 {
		// Clamp before the *time.Second multiply: a huge value would otherwise
		// overflow time.Duration and could wrap to a non-positive result, bypassing
		// the over-cap abort. retryAfter treats anything > maxRetryWait as "over cap",
		// so any value past the cap collapses to the same abort signal — no overflow.
		if secs > overCapSeconds {
			secs = overCapSeconds
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(ra); err == nil {
		if d := t.Sub(c.now()); d > 0 {
			return d, true
		}
	}
	return 0, false
}

// overCapSeconds is a ceiling for a parsed Retry-After (seconds): safely far above
// maxRetryWait (60s) yet small enough that overCapSeconds*time.Second stays within
// int64 (MaxInt64/1e9 ≈ 9.2e9 seconds; 1<<31 ≈ 2.1e9 is well under that). Any value
// at this ceiling already trips the over-cap abort, so saturating here is lossless
// for the decision.
const overCapSeconds = 1 << 31

// parsePositiveInt parses a non-negative integer string (Retry-After seconds). It
// caps accumulation at overCapSeconds so a very long digit string can't overflow int
// and wrap negative — the caller treats anything over the cap as "over cap" anyway.
func parsePositiveInt(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("non-numeric Retry-After")
		}
		n = n*10 + int(r-'0')
		if n > overCapSeconds {
			n = overCapSeconds // saturate; further digits can't reduce it
		}
	}
	return n, nil
}
