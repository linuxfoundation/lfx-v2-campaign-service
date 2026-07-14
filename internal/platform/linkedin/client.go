// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Client is a standalone LinkedIn Marketing API client. Construct it with
// NewClient. The client holds no mutable state and its methods are safe to call
// concurrently, provided the injected RuntimeConfig (its slices/maps) is not
// mutated by the caller after construction.
type Client struct {
	creds      Credentials
	cfg        RuntimeConfig
	httpClient *http.Client
	baseURL    string
	apiVersion string
	// now allows tests to control the clock. Defaults to time.Now.
	now func() time.Time
	// retryBaseDelay is the base for exponential 429 backoff. Defaults to the
	// retryBaseDelay const; tests may shrink it to keep runs fast.
	retryBaseDelay time.Duration
}

// Option customizes a Client.
type Option func(*Client)

// WithHTTPClient overrides the default *http.Client (30s timeout).
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.httpClient = h
		}
	}
}

// WithBaseURL overrides the API base URL. Primarily for tests (httptest.Server).
func WithBaseURL(u string) Option {
	return func(c *Client) {
		if u != "" {
			c.baseURL = strings.TrimRight(u, "/")
		}
	}
}

// WithClock overrides the time source. For tests.
func WithClock(now func() time.Time) Option {
	return func(c *Client) {
		if now != nil {
			c.now = now
		}
	}
}

// withRetryBaseDelay overrides the exponential-backoff base for 429 retries.
// Unexported: only tests use it, to keep retry runs fast.
func withRetryBaseDelay(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.retryBaseDelay = d
		}
	}
}

// NewClient builds a Client from injected credentials and runtime config.
// The package never reads env vars or files; everything comes through here.
func NewClient(creds Credentials, cfg RuntimeConfig, opts ...Option) *Client {
	c := &Client{
		creds:          creds,
		cfg:            cfg,
		httpClient:     &http.Client{Timeout: requestTimeout},
		baseURL:        baseURL,
		apiVersion:     apiVersion,
		now:            time.Now,
		retryBaseDelay: retryBaseDelay,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// ---------------------------------------------------------------------------
// Local request / result types (mirror the TS request/result)
// ---------------------------------------------------------------------------

// CreativeVariant is one ad variant. Mirrors LinkedInCreativeVariant.
type CreativeVariant struct {
	IntroText string
	Headline  string
	ImageURN  string // optional
}

// CampaignInput is the full campaign-creation request. Mirrors
// LinkedInCampaignCreateRequest (the fields CreateCampaign consumes).
type CampaignInput struct {
	EventName        string
	RegistrationURL  string
	HSToken          string // optional
	BudgetUSD        float64
	LifetimeBudget   bool
	StartDate        string // YYYY-MM-DD
	EndDate          string // YYYY-MM-DD
	GeoTargets       []GeoTarget
	TargetingProfile string
	Variants         []CreativeVariant
	Project          string // required; the canonical LFX project slug (name's Project segment)
	// AdAccountID optionally overrides the default account. Must be in the
	// runtime config's accounts list when set.
	AdAccountID string
}

// CampaignResult is the outcome of CreateCampaign. Mirrors
// LinkedInCampaignCreateResult.
type CampaignResult struct {
	Platform          string   `json:"platform"`
	CampaignGroupName string   `json:"campaignGroupName"`
	CampaignGroupID   string   `json:"campaignGroupId"`
	CampaignName      string   `json:"campaignName"`
	CampaignID        string   `json:"campaignId"`
	CreativeCount     int      `json:"creativeCount"`
	LinkedInURL       string   `json:"linkedInUrl"`
	Steps             []string `json:"steps"`
}

// ---------------------------------------------------------------------------
// HTTP layer
// ---------------------------------------------------------------------------

// linkedInResponse is the decoded JSON body plus the resource ID promoted from
// the x-restli-id header. Mirrors LinkedInResponse.
type linkedInResponse struct {
	ID     flexibleID `json:"id"`
	Name   string     `json:"name"`
	Status string     `json:"status"`
	// Elements is a POINTER slice so the "elements field absent or null" case is
	// distinguishable from the "elements present but empty" case. A malformed 2xx
	// search body like `{}` or `null` decodes with Elements == nil (field absent)
	// and CANNOT prove absence, whereas an intentional empty result `{"elements":[]}`
	// decodes with a non-nil, len-0 slice and IS a confirmed not-found. Collapsing
	// both to a plain nil slice let a `{}`/`null` body read as "no elements → not
	// found", permitting a DUPLICATE create. See doRequest's search-presence guard.
	Elements *[]responseElement `json:"elements"`
	Metadata linkedInMetadata   `json:"metadata"`
}

// linkedInMetadata carries the cursor-pagination block used by the LinkedIn
// search APIs at LinkedIn-Version 202602: the response advertises the next
// page via metadata.nextPageToken, which the client echoes back as the
// `pageToken` request param. An empty nextPageToken means the result set is
// exhausted.
type linkedInMetadata struct {
	NextPageToken string `json:"nextPageToken"`
}

// flexibleID decodes a LinkedIn resource identifier that the API returns as
// EITHER a JSON number (a long, e.g. campaign/campaign-group search results) or
// a JSON string (e.g. a URN like "urn:li:sponsoredCampaign:200"). Both forms are
// normalized to their string representation. Decoding the numeric form into a Go
// string previously failed json.Unmarshal outright, silently breaking search
// once a real numeric id appeared.
type flexibleID string

// UnmarshalJSON accepts a JSON number or a JSON string and yields the string
// form. A JSON null (or absent field) decodes to the empty string.
func (f *flexibleID) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		*f = ""
		return nil
	}
	// Quoted string form: unquote to strip the JSON escaping.
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		*f = flexibleID(s)
		return nil
	}
	// Numeric form (a long): keep the exact digits via json.Number so a large id
	// is never distorted by float64 rounding.
	var n json.Number
	if err := json.Unmarshal(trimmed, &n); err != nil {
		return fmt.Errorf("resource id is neither a JSON string nor number: %w", err)
	}
	*f = flexibleID(n.String())
	return nil
}

// String returns the normalized string form of the id.
func (f flexibleID) String() string { return string(f) }

// responseElement mirrors LinkedInResponseElement. LinkedIn returns an
// element's identifier under any of `id`, `$URN`, or `urn` depending on the
// endpoint, so each is decoded into its own field and the read sites fall back
// through ID → DURN → URN. The `id` field is a flexibleID because search
// results return it as a numeric long while other endpoints return a quoted URN.
type responseElement struct {
	Name   string     `json:"name"`
	Status string     `json:"status"`
	ID     flexibleID `json:"id"`
	URN    string     `json:"urn"`
	DURN   string     `json:"$URN"`
	// CampaignGroup is the parent campaign-group URN a campaign belongs to
	// (e.g. "urn:li:sponsoredCampaignGroup:123"). It is only populated for
	// campaign search results and is used to scope the find-existing-campaign
	// lookup to the resolved group, so a same-name campaign under a DIFFERENT
	// (e.g. archived/replaced) group is not treated as a match.
	CampaignGroup string `json:"campaignGroup"`
}

var pathValidRE = regexp.MustCompile(`^[a-zA-Z0-9/_:?=&.-]*$`)

// apiError is a non-2xx HTTP response from the LinkedIn API. It carries the
// status code, method, and path so an error message names exactly which call
// failed and how. Note: findByName does NOT special-case any status (not even
// 404) — every non-2xx search response is propagated, because a 404 does not
// prove a searched resource is absent (see findByName / findMatch).
type apiError struct {
	StatusCode int
	Method     string
	Path       string
	Body       string
}

func (e *apiError) Error() string {
	// Deliberately DO NOT include e.Body: the upstream response body is untrusted
	// and can reflect request material (e.g. a destination URL's secret query, or a
	// bearer token echoed in a proxy diagnostic). Body is retained on the struct for
	// internal classification but is never surfaced when the error is stringified
	// into Steps / returned to a caller / logged. Report only the method, path, and
	// status. Mirrors the Reddit client's apiError.Error().
	return fmt.Sprintf("LinkedIn API %s %s -> %d", e.Method, e.Path, e.StatusCode)
}

// transportError wraps a failure of the HTTP round-trip itself (httpClient.Do)
// that happened AFTER the request was plausibly sent (mid-flight timeout,
// unexpected EOF, connection reset), OR a failure to read/decode a 2xx response:
// the server may or may not have processed the request, so the outcome is
// AMBIGUOUS. This is distinct from a pre-send failure (request build, or a
// pre-connect dial error — see isPreSendDialError), where the request never
// reached LinkedIn and a mutation definitely did not happen. Callers use it to
// decide whether a failed create is "may exist" (ambiguous) vs "not created".
// Mirrors the Reddit/Meta clients.
type transportError struct {
	Method string
	Path   string
	Err    error
}

func (e *transportError) Error() string {
	return fmt.Sprintf("linkedin %s %s: %v", e.Method, e.Path, e.Err)
}
func (e *transportError) Unwrap() error { return e.Err }

// isPreSendDialError reports whether a httpClient.Do error clearly happened
// BEFORE any request bytes could have reached LinkedIn (DNS resolution failure,
// connection refused, or no route/network unreachable). Such a failure means the
// request was NOT sent, so it must NOT be treated as an ambiguous "may exist"
// transportError. A failure AFTER a connection is established (mid-flight
// timeout, unexpected EOF) is genuinely ambiguous and IS wrapped as
// transportError. Mirrors the Reddit/Meta clients.
func isPreSendDialError(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	return errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH)
}

// isMutatingMethod reports whether an HTTP method mutates server state (POST,
// PUT, PATCH, DELETE). A failure on a non-mutating method (GET/HEAD) created
// nothing, so it can never be an ambiguous create. Used by
// createOutcomeAmbiguous to scope ambiguity to the calls that actually POST.
func isMutatingMethod(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// createOutcomeAmbiguous reports whether a failed MUTATING request MAY have been
// applied by LinkedIn despite the error — i.e. a POST/PUT/PATCH/DELETE plausibly
// reached the server and its outcome is unknowable. It is the single source of
// truth shared by the create paths so they classify identically:
//   - transportError from a mutating method: the round-trip failed AFTER a
//     connection was established (a pre-connect dial error is NOT wrapped as
//     transportError, so it never reaches here), or a 2xx body could not be
//     read/decoded — so the mutation may have been received and committed;
//   - *apiError with a 5xx status from a mutating method: LinkedIn received it and
//     may have committed the mutation before erroring.
//
// The METHOD gate is essential: a GET search that times out, returns a 5xx, or
// yields an undecodable/oversized 2xx body ran NO POST — nothing was created — so
// it must NOT read as an ambiguous create ("a campaign may exist"). A GET failure
// surfaces to the find-or-create caller as a plain error, which correctly aborts
// the flow before any create rather than reporting a phantom resource. Only a
// mutating-method failure can be an ambiguous create.
//
// A definite 4xx (LinkedIn rejected it), any pre-send failure (request build, a
// pre-connect dial error), or ANY non-mutating-method failure means NOT applied →
// returns false so the caller reports a clean "failed" rather than "may exist".
// The transportError/apiError types both carry the request Method (set at every
// wrap site alongside Path), so the classification needs no extra plumbing.
func createOutcomeAmbiguous(err error) bool {
	var te *transportError
	if errors.As(err, &te) {
		return isMutatingMethod(te.Method)
	}
	var ae *apiError
	return errors.As(err, &ae) && ae.StatusCode >= 500 && isMutatingMethod(ae.Method)
}

// doRequest performs one API call. It honors ctx, sets the OAuth2 bearer and
// LinkedIn headers, applies the client timeout, and promotes x-restli-id into
// the returned ID. Mirrors linkedInRequest().
func (c *Client) doRequest(ctx context.Context, method, path string, body map[string]any, params map[string]string) (*linkedInResponse, error) {
	sanitized := strings.TrimPrefix(path, "/")
	if !pathValidRE.MatchString(sanitized) || strings.Contains(sanitized, "..") {
		return nil, fmt.Errorf("invalid LinkedIn API path: %q", sanitized)
	}

	u, err := url.Parse(c.baseURL + "/" + sanitized)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	if len(params) > 0 {
		q := u.Query()
		for k, v := range params {
			q.Set(k, v)
		}
		u.RawQuery = q.Encode()
	}

	var encoded []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		encoded = b
	}

	// A 429 is retried up to retryMax times with a bounded backoff (honoring
	// Retry-After when present), since CreateCampaign drives several sequential
	// Marketing API calls (campaign group, campaign, dark post, creative) that
	// can trip a per-account rate limit mid-flow.
	//
	// Only SAFE/idempotent methods (GET, HEAD) are retried on a 429. A non-
	// idempotent method (POST — every campaign-group/campaign/post/creative
	// create) is NOT retried: LinkedIn's create endpoints carry no idempotency
	// key, so a 429 whose first attempt may already have succeeded upstream would
	// be double-sent on retry, creating a DUPLICATE resource. For those methods
	// the 429 is returned as an error immediately so the caller does not
	// double-create.
	idempotent := method == http.MethodGet || method == http.MethodHead
	for attempt := 0; attempt <= retryMax; attempt++ {
		var reqBody *bytes.Reader
		if encoded != nil {
			reqBody = bytes.NewReader(encoded)
		} else {
			reqBody = bytes.NewReader(nil)
		}

		req, err := http.NewRequestWithContext(ctx, method, u.String(), reqBody)
		if err != nil {
			return nil, fmt.Errorf("new request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+c.creds.AccessToken)
		req.Header.Set("LinkedIn-Version", c.apiVersion)
		req.Header.Set("X-RestLi-Protocol-Version", "2.0.0")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			// A Do error that clearly happened BEFORE the request could be sent (DNS
			// failure, connection refused, no route) means NOT sent — return it plain
			// so callers treat a create as "not applied". A failure after a connection
			// was established (mid-flight timeout, EOF) is genuinely ambiguous: wrap it
			// as transportError so callers treat a create as "may exist". Mirrors the
			// Reddit/Meta clients.
			if isPreSendDialError(err) {
				return nil, fmt.Errorf("linkedin %s %s: %w", method, path, err)
			}
			return nil, &transportError{Method: method, Path: path, Err: err}
		}

		if resp.StatusCode == http.StatusTooManyRequests && attempt < retryMax && idempotent {
			wait := c.parseRetryAfter(resp)
			// Drain (bounded) before closing so net/http can reuse the connection
			// for the retry instead of opening a fresh one while already rate-limited.
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
			_ = resp.Body.Close()
			if wait <= 0 {
				wait = c.retryBaseDelay * time.Duration(1<<uint(attempt))
			}
			if wait > maxRetryWait {
				wait = maxRetryWait
			}
			if err := sleepCtx(ctx, wait); err != nil {
				return nil, err
			}
			continue
		}

		// Bound the response body read so an unexpectedly large response can't
		// exhaust memory. Read ONE byte past the cap so a body of exactly
		// maxResponseBytes can be distinguished from a larger one truncated at the
		// limit: io.LimitReader returns EOF (not an error) at the limit, so a plain
		// LimitReader(cap) would silently accept a truncated body (or valid JSON plus
		// excess data) as a complete response. Reject when the read EXCEEDS the cap.
		// Mirrors the Meta/Reddit clients' maxResponseBody+1 boundary.
		buf := new(bytes.Buffer)
		if _, err := buf.ReadFrom(io.LimitReader(resp.Body, maxResponseBytes+1)); err != nil {
			_ = resp.Body.Close()
			// A read failure on a 2xx is AMBIGUOUS: LinkedIn may have committed the
			// mutation but we couldn't read the result — wrap it so a create is treated
			// as "may exist". A read failure on a non-2xx isn't a committed mutation, so
			// return it plain. Mirrors the Reddit/Meta clients.
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil, &transportError{Method: method, Path: path, Err: fmt.Errorf("read response body: %w", err)}
			}
			return nil, fmt.Errorf("read response body: %w", err)
		}
		if int64(buf.Len()) > maxResponseBytes {
			_ = resp.Body.Close()
			// An oversized 2xx body is AMBIGUOUS just like a read/decode failure: the
			// mutation may already be committed but we can't read the confirmation
			// (id/elements). Wrap it as transportError so a mutating request is treated
			// as "may exist" (createOutcomeAmbiguous → UNCONFIRMED) rather than a clean
			// failure a blind retry would duplicate. The Method is carried so FIX 1's
			// method gate still classifies a GET overflow as NOT-a-create. A non-2xx
			// oversized body is not a committed mutation, so it stays a plain error.
			// Mirrors the Reddit/Meta clients wrapping a 2xx read failure as transportError.
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil, &transportError{Method: method, Path: path, Err: fmt.Errorf("response exceeds %d bytes", maxResponseBytes)}
			}
			return nil, fmt.Errorf("linkedin %s %s: response exceeds %d bytes", method, path, maxResponseBytes)
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			text := buf.String()
			if len(text) > 400 {
				text = text[:400]
			}
			_ = resp.Body.Close()
			return nil, &apiError{StatusCode: resp.StatusCode, Method: method, Path: path, Body: text}
		}

		out := &linkedInResponse{}
		// Decode EVERY non-empty 2xx body regardless of the exact Content-Type. Gating
		// the decode on Content-Type == "application/json" meant a 2xx search response
		// with a missing Content-Type, a `+json` media type (e.g.
		// application/vnd.linkedin.normalized+json), or any non-exact match was left
		// UNDECODED — yielding an empty linkedInResponse. findMatch then saw no
		// elements and no cursor and reported a false "not found", triggering a
		// DUPLICATE create POST of a paid resource. So decode the body whenever it is
		// non-empty; a genuinely-JSON body is parsed regardless of the advertised type.
		if buf.Len() > 0 {
			if err := json.Unmarshal(buf.Bytes(), out); err != nil {
				_ = resp.Body.Close()
				// A 2xx we can't decode is AMBIGUOUS: the server returned success but we
				// can't read the payload (id/elements). Wrap it so a create is treated as
				// "may exist" rather than a definite failure. Mirrors the Reddit/Meta
				// clients wrapping a 2xx decode failure as transportError.
				return nil, &transportError{Method: method, Path: path, Err: fmt.Errorf("decode response: %w", err)}
			}
		} else if method == http.MethodGet {
			// An EMPTY 2xx body on a GET is a search response with no usable payload. A
			// search MUST return a JSON envelope (elements + metadata); an empty body
			// carries neither, so it cannot prove absence. Treating it as an empty
			// result set would make findMatch report a false "not found" and let the
			// find-or-create caller create a DUPLICATE paid resource. Reject it as
			// ambiguous rather than decode it into a misleading empty result. (POST
			// creates are exempt: they legitimately return an empty body with the id in
			// the x-restli-id header, handled by the header fallback below.)
			_ = resp.Body.Close()
			return nil, &transportError{Method: method, Path: path, Err: fmt.Errorf("search response had an empty body (no elements/metadata) — cannot confirm absence")}
		}

		// A GET search MUST carry the `elements` field to prove a result set. A
		// non-empty but malformed 2xx body like `{}` or `null` decodes cleanly yet
		// leaves Elements == nil (field absent/null): that CANNOT confirm absence, so
		// treating it as an empty result set would make findMatch report a false "not
		// found" and let the find-or-create caller create a DUPLICATE paid resource.
		// An intentional empty result `{"elements":[]}` decodes to a non-nil, len-0
		// slice and IS a valid confirmed-absence, so it is allowed through. Reject only
		// the field-absent/null case, as a transportError — with FIX 1's method gate a
		// GET surfaces this as a plain error (correct: a GET that can't confirm absence
		// must fail, not create). POST/other mutations are exempt: a create legitimately
		// returns an id-only body (e.g. {"id":...}) with no elements.
		if method == http.MethodGet && out.Elements == nil {
			_ = resp.Body.Close()
			return nil, &transportError{Method: method, Path: path, Err: fmt.Errorf("search response missing elements field — cannot confirm absence")}
		}

		// Promote the resource ID header when the body carried no id. Mirrors the
		// x-restli-id fallback in linkedInRequest().
		if out.ID == "" {
			// http.Header.Get canonicalizes the key, so a single lookup covers any
			// casing the server used (x-restli-id / X-RestLi-Id → X-Restli-Id).
			if rid := resp.Header.Get("x-restli-id"); rid != "" {
				out.ID = flexibleID(rid)
			}
		}

		_ = resp.Body.Close()
		return out, nil
	}

	// Unreachable: the bounded loop always returns from within its body — success,
	// a non-2xx *apiError (including a final-attempt 429, which doesn't satisfy the
	// attempt<retryMax retry guard and so falls through to the non-2xx return), or
	// an error. A panic documents the invariant instead of a misleading
	// "exhausted retries" string that can't occur.
	panic("linkedin doRequest: unreachable post-loop return")
}

// parseRetryAfter returns how long to wait before retrying a 429, or 0 if no
// usable header is present. LinkedIn returns Retry-After either as a delay in
// seconds or as an HTTP-date; both forms are honored. Never returns a negative
// duration.
func (c *Client) parseRetryAfter(resp *http.Response) time.Duration {
	v := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		// A purely numeric Retry-After that overflows int64 (ErrRange) is still a
		// numeric value, not an HTTP-date. ParseInt returns the clamped bound
		// (MaxInt64 on positive overflow, MinInt64 on negative overflow) alongside
		// ErrRange, so use the sign to stay consistent with the finite handling
		// below: a positive-overflow is a "wait a very long time" delay → clamp to
		// the ceiling; a negative-overflow mirrors a finite-negative value → no wait.
		// Only a non-numeric value should fall through and be tried as an HTTP-date.
		if errors.Is(err, strconv.ErrRange) {
			if n == math.MaxInt64 {
				return maxRetryWait
			}
			return 0
		}
	} else if n > 0 {
		// Cap before converting to Duration: a huge Retry-After (e.g.
		// 10000000000) would overflow time.Duration(n)*time.Second into a
		// negative value, defeating the maxRetryWait cap. maxRetryWait is the
		// ceiling anyway, so clamp seconds first.
		if n > int64(maxRetryWait/time.Second) {
			return maxRetryWait
		}
		return time.Duration(n) * time.Second
	} else {
		// A well-formed non-positive numeric value (0 or negative): no wait.
		return 0
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(c.now()); d > 0 {
			return d
		}
	}
	return 0
}

// sleepCtx waits for d, returning early if ctx is cancelled.
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

// maxListPages bounds how many pages findByName will walk before giving up — a
// safety valve against an infinite loop if the API ever returned a non-empty
// nextPageToken forever. The cap exists ONLY for that runaway case; it must be
// large enough that no realistic account ever legitimately reaches it, because
// name-based idempotency depends on actually walking the whole collection to
// confirm a same-name resource is absent.
//
// A mature ad account whose matching collection exceeds the cap — while the API
// still hands back a cursor — would hit the cap on EVERY lookup that isn't
// answered by the server-side name filter, and the inconclusive-cap error below
// would then make it unable to create anything. The cap is therefore set to 1000
// pages, which at the maximum pageSize of 1000 (see findMatch) covers ~1,000,000
// entries — comfortably beyond any realistic campaign-group/campaign collection
// while still bounding the loop. Reaching this cap with a next-page token still
// present is reported as an error (see findMatch), NOT a silent no-match — a
// false no-match would let the caller create a DUPLICATE.
const maxListPages = 1000

// findByName searches a nested resource path for a live element matching name.
// It returns the trailing numeric ID, or "" ONLY when a SUCCESSFUL (2xx) search
// exhausts the result set with no match — i.e. the resource is provably absent.
// Statuses in skipStatuses are ignored. Mirrors findByName() (paginated search
// across all statuses).
//
// At LinkedIn-Version 202602 the search APIs use CURSOR pagination, not offset
// pagination: each response carries metadata.nextPageToken, and the client
// echoes that token back as the `pageToken` request param to fetch the next
// page. Pagination stops when nextPageToken comes back empty. An offset-based
// walk (start/count) or a "full page" heuristic is the wrong model for this API
// version and can miss results or loop, so it is not used.
//
// Unlike the TypeScript original, NO search failure is swallowed — not even a
// 404. A 404 does not prove the searched resource is absent: it can equally mean
// the finder/collection/account PATH is wrong, so treating it as a clean
// no-match would let the find-or-create caller proceed to a create POST despite a
// real error. Only an empty 2xx result set means "absent, safe to create". Every
// error (404, other HTTP status, network, decode) is returned so the caller
// aborts instead of creating a duplicate. Pagination is followed to exhaustion
// (up to maxListPages); reaching the cap with a next-page token still present
// also returns an error rather than a false no-match.
func (c *Client) findByName(ctx context.Context, nestedPath, name string) (string, error) {
	return c.findMatch(ctx, nestedPath, name, func(el responseElement) bool {
		return el.Name == name
	})
}

// findCampaignByNameInGroup searches the campaign collection for a live campaign
// whose name matches AND whose parent campaignGroup URN resolves to groupID. The
// group constraint is essential: the campaign search is account-wide, so a
// same-name campaign under a DIFFERENT (e.g. archived/replaced) group would
// otherwise be returned as an idempotent match and the new campaign would never
// be created under the correct group. Elements missing a campaignGroup are not
// matched, since without it the parent cannot be confirmed.
func (c *Client) findCampaignByNameInGroup(ctx context.Context, campaignsPath, name, groupID string) (string, error) {
	return c.findMatch(ctx, campaignsPath, name, func(el responseElement) bool {
		return el.Name == name && el.CampaignGroup != "" && trailingID(el.CampaignGroup) == groupID
	})
}

// findMatch runs the cursor-paginated search-by-name walk shared by findByName
// and findCampaignByNameInGroup, returning the trailing numeric ID of the first
// element for which match reports true (and whose status is not in
// skipStatuses), or "" when no such element exists. Error handling and the
// max-pages guard match the findByName contract documented above.
//
// The search request carries a SERVER-SIDE name filter
// (search=(name:(values:List(<name>)))), so the API returns only same-name
// elements rather than the whole account. This narrows a lookup from O(account)
// to O(same-name matches): a miss now costs ~one page instead of walking every
// campaign/campaign-group in the account. The match callback is still applied
// client-side because the server filters on name alone — findCampaignByNameInGroup
// additionally scopes to the parent campaign group, and both callers still enforce
// the exact-name and non-skipStatuses checks. pageSize is set to the API maximum
// (1000) so any account that legitimately has many same-name resources is covered
// in as few round-trips as possible. The cursor/repeated-token/page-cap guards
// below remain the correctness backstop.
func (c *Client) findMatch(ctx context.Context, nestedPath, name string, match func(responseElement) bool) (string, error) {
	// The LinkedIn Marketing API caps search pageSize at 1000; request the max so
	// the (rare) case of many same-name matches resolves in the fewest pages.
	const pageSize = 1000
	pageToken := ""
	// Guard against a server that returns the same non-empty cursor repeatedly:
	// without this, a stuck token would replay the same GET up to maxListPages
	// times (each with its own retries), burning quota and stalling the request.
	seenTokens := make(map[string]struct{})
	for page := 0; page < maxListPages; page++ {
		params := map[string]string{
			"q": "search",
			// Server-side name filter: the adCampaigns/adCampaignGroups `search`
			// finder supports search.name.values, so only elements whose name equals
			// the lookup name are returned. This keeps the lookup O(matches), not
			// O(account). The value is Rest.li-encoded so names containing reserved
			// characters (parens, commas, colons) can't break out of the List(...)
			// literal; url.Values.Encode() then applies the outer percent-encoding.
			"search": "(name:(values:List(" + restliEncode(name) + ")))",
			// Cursor pagination at LinkedIn-Version 202602 uses `pageSize` (paired
			// with `pageToken`), NOT the legacy offset param `count`. Sending
			// `count` here was ignored by the cursor contract, so the page size the
			// caller asked for silently did not take effect. No offset param
			// (`start`/`count`) is sent — the cursor token alone advances pages.
			"pageSize": strconv.Itoa(pageSize),
		}
		if pageToken != "" {
			params["pageToken"] = pageToken
		}
		resp, err := c.doRequest(ctx, http.MethodGet, nestedPath, nil, params)
		if err != nil {
			// ANY search error propagates — including a 404. A 404 on the search
			// call does NOT prove the named resource is absent: it can equally mean
			// the finder/collection/account PATH is wrong (e.g. a mistyped or
			// unauthorized adAccounts/<id>/... path), and treating that as a clean
			// "not found" would let the subsequent create POST run despite a real
			// error, silently creating a resource under the wrong assumptions.
			// Only a SUCCESSFUL (2xx) search whose result set is empty proves the
			// resource is absent; that case is handled below via an exhausted cursor.
			// So every error here (404, 401/429/5xx, network, decode) is surfaced so
			// the find-or-create caller aborts rather than proceeding to create.
			return "", fmt.Errorf("search %q by name: %w", nestedPath, err)
		}
		// resp.Elements is guaranteed non-nil here for a GET: doRequest rejects a
		// search whose elements field is absent/null (see the search-presence guard).
		// A non-nil, len-0 slice (`{"elements":[]}`) is a confirmed-empty page.
		var elements []responseElement
		if resp.Elements != nil {
			elements = *resp.Elements
		}
		for _, el := range elements {
			if !match(el) {
				continue
			}
			if _, skip := skipStatuses[el.Status]; skip {
				continue
			}
			raw := el.ID.String()
			if raw == "" {
				raw = el.DURN
			}
			if raw == "" {
				raw = el.URN
			}
			if raw == "" {
				// An element that SATISFIES the match (same name, right group, not a
				// skipped status) but carries no usable id under id/$URN/urn PROVES a
				// same-name resource already exists — the search found it. We just
				// can't extract its id to return. Reporting a false "" no-match here
				// would let the find-or-create caller proceed to a create POST and
				// produce a DUPLICATE, defeating fail-closed idempotency. So this is an
				// ERROR, not a not-found (mirrors the Twitter client's findByName fix).
				return "", fmt.Errorf("search %q by name: matched element %q has no usable id (id/$URN/urn all empty) — aborting to avoid creating a duplicate", nestedPath, el.Name)
			}
			// Validate the EXTRACTED id, not just the raw field. A URN like
			// "urn:li:sponsoredCampaignGroup:" is non-empty above, yet trailingID
			// returns "" (empty trailing segment). Returning that empty id would look
			// like ABSENCE to the find-or-create caller and trigger a DUPLICATE create
			// POST. The element MATCHED, so a same-name resource exists; we just can't
			// extract its id. Report an ERROR to abort, mirroring the id-less case above.
			id := trailingID(raw)
			if id == "" {
				return "", fmt.Errorf("search %q by name: matched element %q has an id %q with an empty trailing segment — aborting to avoid creating a duplicate", nestedPath, el.Name, raw)
			}
			return id, nil
		}
		// Cursor pagination: an empty nextPageToken marks the end of the result
		// set. Otherwise carry the token into the next request.
		if resp.Metadata.NextPageToken == "" {
			return "", nil
		}
		if _, seen := seenTokens[resp.Metadata.NextPageToken]; seen {
			// The server handed back a cursor we've already followed: pagination is
			// looping. Abort with the inconclusive-search error rather than replaying
			// the same page — reporting a false no-match would let the caller create a
			// duplicate.
			return "", fmt.Errorf("search %q by name: pagination returned a repeated page token — aborting to avoid creating a duplicate", nestedPath)
		}
		seenTokens[resp.Metadata.NextPageToken] = struct{}{}
		pageToken = resp.Metadata.NextPageToken
	}
	// Cap reached with a next-page token still present: refuse to report a false
	// no-match, which would let the caller create a duplicate resource.
	return "", fmt.Errorf("search %q by name: exceeded %d pages without exhausting results — aborting to avoid creating a duplicate", nestedPath, maxListPages)
}

// restliReplacer percent-encodes the characters that are structurally
// significant inside a Rest.li query value — the delimiters of the
// List(...)/(key:value) grammar. Leaving them raw would let a resource name
// containing, say, a comma or paren break out of the List(...) literal and
// corrupt the filter (or, at worst, inject additional criteria). This is the
// Rest.li "reduced encoding" applied to values embedded in a query string; the
// surrounding url.Values.Encode() then percent-encodes everything else (spaces,
// pipes, etc.) for transport.
var restliReplacer = strings.NewReplacer(
	"%", "%25", // must be first so the escapes below aren't double-encoded
	"(", "%28",
	")", "%29",
	",", "%2C",
	":", "%3A",
	"'", "%27",
)

// restliEncode returns name safe for embedding inside a Rest.li List(...) value.
func restliEncode(name string) string {
	return restliReplacer.Replace(name)
}

// trailingID returns the segment after the last colon of a URN, or the input
// unchanged when it contains no colon. Mirrors `id.split(':').pop()`.
func trailingID(raw string) string {
	if i := strings.LastIndex(raw, ":"); i >= 0 {
		return raw[i+1:]
	}
	return raw
}

// ---------------------------------------------------------------------------
// Timestamp helpers (milliseconds since epoch)
// ---------------------------------------------------------------------------

var dateRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// toMs converts a YYYY-MM-DD date to epoch milliseconds. Mirrors toMs():
//   - eod=false: start-of-day UTC; if in the past, returns now+startTimeBuffer.
//   - eod=true: end-of-day UTC (23:59:59.999); errors if in the past.
func (c *Client) toMs(dateStr string, eod bool) (int64, error) {
	if !dateRE.MatchString(dateStr) {
		return 0, fmt.Errorf("invalid date format: %s — expected YYYY-MM-DD", dateStr)
	}
	t, err := time.ParseInLocation("2006-01-02", dateStr, time.UTC)
	if err != nil {
		return 0, fmt.Errorf("invalid date: %s", dateStr)
	}
	nowMs := c.now().UTC().UnixMilli()
	if eod {
		end := time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, int(999*time.Millisecond), time.UTC)
		endMs := end.UnixMilli()
		if endMs <= nowMs {
			return 0, fmt.Errorf("end date %s is in the past", dateStr)
		}
		return endMs, nil
	}
	startMs := t.UnixMilli()
	if startMs <= nowMs {
		// Nudge a today/past start forward by startTimeBuffer so it isn't already in
		// the past by the time LinkedIn receives the (possibly retried) POST. The
		// buffer must exceed doRequest's worst-case lookup+retry budget — see
		// startTimeBuffer's godoc — or a long-running create flow could still POST a
		// past start and be rejected, orphaning the campaign group.
		return nowMs + startTimeBuffer.Milliseconds(), nil
	}
	return startMs, nil
}

// validateSchedule enforces the start/end date contract ONCE, up front in
// CreateCampaign, before any idempotency lookup or mutating POST, and RETURNS the
// computed epoch-millisecond start/end. It mirrors what toMs (plus the
// endMs<=startMs guard) enforces: both dates must parse as YYYY-MM-DD, the end
// date must not be in the past, and the end must be strictly after the start.
//
// This up-front computation is necessary because toMs is otherwise only reached
// AFTER the create helpers' find-existing idempotency lookups: if a same-name
// group AND campaign already exist, malformed/past/reversed dates would bypass
// toMs entirely and the flow would still proceed to create dark posts and
// creatives on a broken schedule.
//
// The returned startMs is used for the campaign GROUP create (which runs right
// after this preflight, so its start is still fresh). The CAMPAIGN's start is NOT
// taken from this value: createSponsoredCampaign recomputes it (now+buffer) just
// before its POST, because the campaign create runs after the group lookup+create
// and the campaign lookup, by which time a once-computed today/past start could
// have slipped into the past (see startTimeBuffer and createSponsoredCampaign).
// endMs is a fixed calendar instant that does not drift, so it is threaded through
// to the campaign unchanged.
func (c *Client) validateSchedule(startDate, endDate string) (startMs, endMs int64, err error) {
	startMs, err = c.toMs(startDate, false)
	if err != nil {
		return 0, 0, err
	}
	endMs, err = c.toMs(endDate, true)
	if err != nil {
		return 0, 0, err
	}
	if endMs <= startMs {
		return 0, 0, fmt.Errorf("end date (%s) must be after start date (%s)", endDate, startDate)
	}
	return startMs, endMs, nil
}

// ---------------------------------------------------------------------------
// Hierarchy creation
// ---------------------------------------------------------------------------

// findOrCreateCampaignGroup returns an existing ACTIVE-eligible group's ID or
// creates a new ACTIVE campaign group. Mirrors findOrCreateCampaignGroup():
// campaign groups are always created with status ACTIVE.
//
// Unexported by design: accountID is trusted to have already passed the
// cross-tenant fail-closed check (resolveAccountID) in CreateCampaign. Exposing
// it would let a caller create resources under an arbitrary, unvalidated account
// id, bypassing that check. All hierarchy helpers are internal for this reason.
//
// NOT ATOMIC: the find-then-create is best-effort, not an atomic upsert (LinkedIn
// exposes no upsert primitive). This is a GET-then-POST: two concurrent
// CreateCampaign calls for the same name can both observe "not found" and both
// POST, creating duplicate campaign groups. The client does NOT attempt to close
// this window and provides NO single-flight guarantee: this find-or-create is
// best-effort and re-POSTs on a repeat call. An orchestrator per-(brief, platform)
// single-flight claim is PLANNED but NOT provided here (tracked separately as
// LFXV2-2665); until it exists, callers MUST NOT rely on any dedup guarantee and
// must serialize concurrent calls for the same name on their own.
func (c *Client) findOrCreateCampaignGroup(ctx context.Context, accountID, name string, startMs, endMs int64) (string, error) {
	groupsPath := fmt.Sprintf("adAccounts/%s/adCampaignGroups", accountID)

	existing, err := c.findByName(ctx, groupsPath, name)
	if err != nil {
		return "", err
	}
	if existing != "" {
		return existing, nil
	}

	// startMs/endMs are the schedule timestamps computed ONCE by validateSchedule
	// in CreateCampaign; they are the single source of truth so the campaign group
	// and the campaign share identical, preflight-validated values (toMs is not
	// re-called here — that would drift for a today/past start).
	body := map[string]any{
		"account": accountURN(accountID),
		"name":    name,
		"status":  "ACTIVE",
		"runSchedule": map[string]any{
			"start": startMs,
			"end":   endMs,
		},
	}

	resp, err := c.doRequest(ctx, http.MethodPost, groupsPath, body, nil)
	if err != nil {
		return "", err
	}
	if resp.ID == "" {
		// A 2xx with no id is a malformed SUCCESS: LinkedIn may have created the group
		// but we can't read its id. Wrap as transportError so the caller classifies it
		// as "may exist" (createOutcomeAmbiguous → UNCONFIRMED) rather than a definite
		// failure that a retry would treat as safe-to-recreate. Mirrors the Meta client.
		return "", &transportError{Method: http.MethodPost, Path: groupsPath, Err: fmt.Errorf("campaign group creation returned no ID")}
	}
	// Validate the EXTRACTED id, not just the raw field: a create response like
	// "urn:li:sponsoredCampaignGroup:" passes the non-empty check above yet
	// trailingID returns "" (empty trailing segment). Proceeding would build an
	// invalid group URN ("urn:li:sponsoredCampaignGroup:") for the campaign and
	// lose the identifier of the group just created. The create response is
	// malformed; abort rather than continue. Wrap as transportError (2xx malformed
	// success → "may exist") for the same reason as the no-id case above.
	groupID := trailingID(resp.ID.String())
	if groupID == "" {
		return "", &transportError{Method: http.MethodPost, Path: groupsPath, Err: fmt.Errorf("campaign group creation returned an ID %q with an empty trailing segment", resp.ID.String())}
	}
	return groupID, nil
}

// createSponsoredCampaign returns an existing campaign's ID (idempotent by
// name) or creates a new PAUSED sponsored-updates campaign. Budget is sent as a
// decimal string (not micros); timestamps are milliseconds. Mirrors
// createCampaign().
//
// Unexported by design (see findOrCreateCampaignGroup): accountID is trusted to
// have passed resolveAccountID in CreateCampaign.
//
// NOT ATOMIC: like findOrCreateCampaignGroup, the find-then-create here is
// best-effort, not an atomic upsert. The findCampaignByNameInGroup lookup and the
// subsequent create POST are separate calls, so two concurrent CreateCampaign
// runs for the same (name, group) can both miss and both create a duplicate
// campaign. LinkedIn offers no upsert primitive to close this window client-side,
// and this client provides NO single-flight guarantee: the find-or-create is
// best-effort and re-POSTs on a repeat call. An orchestrator per-(brief, platform)
// single-flight claim is PLANNED but NOT provided here (tracked separately as
// LFXV2-2665); until it exists, callers MUST NOT rely on any dedup guarantee and
// must serialize concurrent calls for the same name on their own.
func (c *Client) createSponsoredCampaign(ctx context.Context, accountID, groupID, name, startDate string, endMs int64, budgetUSD float64, geoURNs []string, targetingProfile string, lifetimeBudget bool) (string, error) {
	campaignsPath := fmt.Sprintf("adAccounts/%s/adCampaigns", accountID)

	// Scope the idempotency lookup to the resolved campaign group: the campaign
	// search is account-wide by name, so a same-name campaign under a DIFFERENT
	// (e.g. archived/replaced) group must NOT be treated as a match — otherwise a
	// new campaign is never created under the correct group.
	existing, err := c.findCampaignByNameInGroup(ctx, campaignsPath, name, groupID)
	if err != nil {
		return "", err
	}
	if existing != "" {
		return existing, nil
	}

	// Re-derive the campaign's start JUST BEFORE this mutating POST, rather than
	// reusing the value computed by validateSchedule at the top of CreateCampaign.
	// For a today/past start, toMs returns now+startTimeBuffer; that value was
	// computed BEFORE the (worst-case ~5min) campaign-group lookup + group create +
	// (worst-case ~5min) campaign lookup ran, so by the time this POST fires the
	// preflight start could already be in the PAST — LinkedIn would reject the POST
	// and orphan the just-created ACTIVE group. Recomputing here guarantees the
	// start is `now + startTimeBuffer` at the moment of the mutation, independent of
	// how long the preceding lookups took. The endMs was validated up front (end is
	// a fixed calendar instant that does not drift) and threaded through unchanged.
	startMs, err := c.toMs(startDate, false)
	if err != nil {
		return "", err
	}
	// Guard the recomputed start against the (fixed) end: a start nudged forward by
	// the buffer must still precede the end, or LinkedIn would reject the schedule
	// only after the group already exists. This can only trip for an end date very
	// close to now, but fail closed rather than POST an invalid schedule.
	if endMs <= startMs {
		return "", fmt.Errorf("campaign start (now+%s buffer) is not before the end date — the run window is too short", startTimeBuffer)
	}

	targeting, err := c.buildTargetingCriteria(targetingProfile, geoURNs)
	if err != nil {
		return "", err
	}

	// Budget as a decimal string, e.g. "100.00" — not micros. Mirrors toFixed(2).
	amount := strconv.FormatFloat(budgetUSD, 'f', 2, 64)
	budgetField := "dailyBudget"
	if lifetimeBudget {
		budgetField = "totalBudget"
	}

	body := map[string]any{
		"account":                accountURN(accountID),
		"campaignGroup":          "urn:li:sponsoredCampaignGroup:" + groupID,
		"name":                   name,
		"status":                 "PAUSED",
		"type":                   "SPONSORED_UPDATES",
		"objectiveType":          "WEBSITE_CONVERSION",
		"costType":               "CPM",
		"locale":                 map[string]any{"country": "US", "language": "en"},
		"offsiteDeliveryEnabled": true,
		"politicalIntent":        "NOT_POLITICAL",
		budgetField:              map[string]any{"amount": amount, "currencyCode": "USD"},
		"runSchedule":            map[string]any{"start": startMs, "end": endMs},
	}
	// Merge the targetingCriteria block.
	for k, v := range targeting {
		body[k] = v
	}

	resp, err := c.doRequest(ctx, http.MethodPost, campaignsPath, body, nil)
	if err != nil {
		return "", err
	}
	if resp.ID == "" {
		// A 2xx with no id is a malformed SUCCESS: the campaign may exist but its id is
		// unreadable. Wrap as transportError so the caller classifies it as "may exist"
		// (UNCONFIRMED) rather than a definite failure. Mirrors the Meta client.
		return "", &transportError{Method: http.MethodPost, Path: campaignsPath, Err: fmt.Errorf("campaign creation returned no ID")}
	}
	// Validate the EXTRACTED id, not just the raw field: a create response like
	// "urn:li:sponsoredCampaign:" passes the non-empty check above yet trailingID
	// returns "" (empty trailing segment). Returning "" here would let CreateCampaign
	// proceed to build the dark post + creative against "urn:li:sponsoredCampaign:",
	// leaving an orphaned post. The create response is malformed; abort before any
	// downstream resource is created. Wrap as transportError (2xx malformed success →
	// "may exist") for the same reason as the no-id case above.
	campaignID := trailingID(resp.ID.String())
	if campaignID == "" {
		return "", &transportError{Method: http.MethodPost, Path: campaignsPath, Err: fmt.Errorf("campaign creation returned an ID %q with an empty trailing segment", resp.ID.String())}
	}
	return campaignID, nil
}

// createDarkPost creates an unpublished-to-feed sponsored post
// (feedDistribution NONE) and returns its share URN. Mirrors createDarkPost().
//
// The post uses an article content block. Per the TS, callToAction is NOT sent
// for article ads. The dark-post nature comes from distribution.feedDistribution
// = "NONE".
//
// Unexported by design (see findOrCreateCampaignGroup): accountID is trusted to
// have passed resolveAccountID in CreateCampaign.
func (c *Client) createDarkPost(ctx context.Context, accountID, introText, headline, destURL, imageURN string) (string, error) {
	author, err := c.orgURN(accountID)
	if err != nil {
		return "", err
	}

	// Normalize (trim + strip dashes) so the text sent matches what up-front
	// validation checked; bare stripDashes would leave surrounding whitespace.
	intro := normalizeCreativeText(introText)
	// LinkedIn single-image ad intro/primary (commentary) text is capped at 600
	// characters; the TS source truncates intro_text too. Truncate rune-safely so
	// a multi-byte rune is never split into invalid UTF-8.
	if len([]rune(intro)) > 600 {
		intro = truncateRunes(intro, 600)
	}
	head := normalizeCreativeText(headline)
	if len([]rune(head)) > 200 {
		head = truncateRunes(head, 200)
	}

	article := map[string]any{
		"source":      destURL,
		"title":       head,
		"description": "",
	}
	if imageURN != "" {
		article["thumbnail"] = imageURN
	}

	body := map[string]any{
		"author":     author,
		"commentary": intro,
		"visibility": "PUBLIC",
		"distribution": map[string]any{
			"feedDistribution":               "NONE",
			"targetEntities":                 []any{},
			"thirdPartyDistributionChannels": []any{},
		},
		"content":        map[string]any{"article": article},
		"lifecycleState": "PUBLISHED",
		"adContext":      map[string]any{"dscAdAccount": accountURN(accountID)},
	}

	resp, err := c.doRequest(ctx, http.MethodPost, "posts", body, nil)
	if err != nil {
		return "", err
	}
	if resp.ID == "" {
		// A 2xx with no id is a malformed SUCCESS: the dark post may exist but its id
		// is unreadable. A dark post has no reconciliation lookup, so wrap as
		// transportError → the caller reports UNCONFIRMED (may exist) rather than a
		// definite failure a retry would duplicate. Mirrors the Meta client.
		return "", &transportError{Method: http.MethodPost, Path: "posts", Err: fmt.Errorf("dark post creation returned no ID")}
	}
	// Validate the extracted trailing segment, not just the raw field: a malformed
	// create response like "urn:li:share:" passes the non-empty check yet has no
	// id, and would be used verbatim as the creative's share reference. Abort on a
	// malformed response rather than continue (mirrors the group/campaign creates).
	// Wrap as transportError (2xx malformed success → "may exist") as above.
	if trailingID(resp.ID.String()) == "" {
		return "", &transportError{Method: http.MethodPost, Path: "posts", Err: fmt.Errorf("dark post creation returned an ID %q with an empty trailing segment", resp.ID.String())}
	}
	return resp.ID.String(), nil
}

// createCreative creates a DRAFT creative referencing a share URN and returns
// its ID. Mirrors createCreative().
//
// Unexported by design (see findOrCreateCampaignGroup): accountID is trusted to
// have passed resolveAccountID in CreateCampaign.
func (c *Client) createCreative(ctx context.Context, accountID, campaignID, shareURN, adName string) (string, error) {
	body := map[string]any{
		"campaign":       "urn:li:sponsoredCampaign:" + campaignID,
		"intendedStatus": "DRAFT",
		"content":        map[string]any{"reference": shareURN},
	}
	if adName != "" {
		if len([]rune(adName)) > 255 {
			adName = truncateRunes(adName, 255)
		}
		body["name"] = adName
	}

	resp, err := c.doRequest(ctx, http.MethodPost, fmt.Sprintf("adAccounts/%s/creatives", accountID), body, nil)
	if err != nil {
		return "", err
	}
	creativesPath := fmt.Sprintf("adAccounts/%s/creatives", accountID)
	if resp.ID == "" {
		// A 2xx with no id is a malformed SUCCESS: the creative may exist but its id is
		// unreadable. A creative has no reconciliation lookup, so wrap as
		// transportError → the caller reports UNCONFIRMED (may exist) rather than a
		// definite failure a retry would duplicate. Mirrors the Meta client.
		return "", &transportError{Method: http.MethodPost, Path: creativesPath, Err: fmt.Errorf("creative creation returned no ID")}
	}
	// Reject a malformed URN with an empty trailing segment (e.g.
	// "urn:li:sponsoredCreative:") that passes the non-empty check but carries no
	// id, mirroring the group/campaign creates. Wrap as transportError (2xx malformed
	// success → "may exist") as above.
	if trailingID(resp.ID.String()) == "" {
		return "", &transportError{Method: http.MethodPost, Path: creativesPath, Err: fmt.Errorf("creative creation returned an ID %q with an empty trailing segment", resp.ID.String())}
	}
	return resp.ID.String(), nil
}

// ---------------------------------------------------------------------------
// UTM helper
// ---------------------------------------------------------------------------

// BuildUTMURL appends LinkedIn UTM params to baseURL for a given variant.
// Mirrors buildLinkedInUtmUrl().
//
// The URL is parsed so UTM params merge into the query and the fragment stays at
// the end: naive string concatenation on "https://x.org/reg#tickets" would yield
// "https://x.org/reg#tickets?utm_..." (query inside the fragment, which browsers
// drop). Any existing query params are preserved.
//
// Mirroring the TS source, a single trailing slash is stripped from the path
// before the UTM query is appended (so ".../reg/" becomes ".../reg?utm_...").
// The strip operates on the escaped path so an encoded "%2F" is not corrupted,
// and it only removes a literal, unencoded trailing "/". Query and fragment are
// preserved.
func BuildUTMURL(baseURL, hsToken, campaignName string, variantIndex int) string {
	term := strings.ReplaceAll(campaignName, " | ", "_")
	term = strings.Join(strings.Fields(term), "-")
	term = strings.ToLower(term)

	u, err := url.Parse(baseURL)
	if err != nil {
		// Fall back to concatenation if the URL is unparseable; better a slightly
		// malformed URL than dropping the UTM params entirely.
		trimmed := strings.TrimRight(baseURL, "/")
		sep := "?"
		if strings.Contains(baseURL, "?") {
			sep = "&"
		}
		return trimmed + sep + utmValues(hsToken, term, variantIndex).Encode()
	}

	// Strip one literal trailing slash from the path, mirroring the TS source.
	// Operate on EscapedPath so an encoded "%2F" (which is NOT a real path
	// separator) is preserved rather than being treated as a strippable slash.
	if esc := u.EscapedPath(); strings.HasSuffix(esc, "/") {
		trimmed := esc[:len(esc)-1]
		// Re-decode into Path/RawPath so the URL re-marshals consistently.
		if decoded, derr := url.PathUnescape(trimmed); derr == nil {
			u.Path = decoded
			u.RawPath = trimmed
		}
	}

	q := u.Query()
	for k, vals := range utmValues(hsToken, term, variantIndex) {
		for _, val := range vals {
			q.Set(k, val)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// utmValues builds the LinkedIn UTM query parameters.
func utmValues(hsToken, term string, variantIndex int) url.Values {
	v := url.Values{}
	v.Set("utm_source", "linkedin")
	v.Set("utm_medium", "paid-social")
	if hsToken != "" {
		v.Set("utm_campaign", hsToken)
	}
	v.Set("utm_term", term)
	v.Set("utm_content", fmt.Sprintf("variant-%d", variantIndex))
	return v
}

// truncateRunes returns at most n runes of s, never splitting a multi-byte rune
// (byte-slicing would corrupt non-ASCII text into invalid UTF-8 that json.Marshal
// replaces with U+FFFD). Mirrors the TS substring behavior.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// normalizeCreativeText applies the same normalization createDarkPost/createCreative
// perform on a variant's user-supplied text before it is sent to LinkedIn: trim
// surrounding whitespace, then collapse em/en dashes to commas (stripDashes).
// CreateCampaign uses it to validate up front that a mandatory field does not
// normalize to empty, so a variant that LinkedIn would reject (e.g. a
// whitespace-only or dash-only headline) is caught before the first mutating POST
// rather than after the campaign group and campaign already exist.
func normalizeCreativeText(text string) string {
	return strings.TrimSpace(stripDashes(strings.TrimSpace(text)))
}

// stripDashes normalizes em/en dashes to commas. Mirrors the TS stripDashes.
func stripDashes(text string) string {
	// " — "/" – " (with surrounding spaces) -> ", "
	text = strings.ReplaceAll(text, " — ", ", ")
	text = strings.ReplaceAll(text, " – ", ", ")
	// bare em/en dashes -> ", "
	text = strings.ReplaceAll(text, "—", ", ")
	text = strings.ReplaceAll(text, "–", ", ")
	// trim a leading or trailing ", "
	text = strings.TrimPrefix(text, ", ")
	text = strings.TrimSuffix(text, ", ")
	return text
}

// ---------------------------------------------------------------------------
// Orchestration
// ---------------------------------------------------------------------------

// validatePrerequisites probes runtime-config-dependent lookups before any
// side-effecting call, so a config gap can't leave orphan LinkedIn artifacts.
// Mirrors validateLinkedInPrerequisites().
//
// Beyond confirming the targeting profile EXISTS, it validates that the resolved
// profile actually yields usable targeting — i.e. that the FINAL assembled
// targeting criteria would be non-empty. Mirroring the TS source, an empty
// skills/groups config is acceptable AS LONG AS the resulting include criteria
// is non-empty: buildTargetingCriteria always adds the hardcoded jobFunctions
// facets to the include block, so a profile with empty skills AND groups still
// contributes real targeting (the jobFunctions) and is accepted. The rejection
// keys off the ASSEMBLED include facets being empty, not merely off skills/groups
// being empty. Because jobFunctions is always present, a config-only-blank
// skills/groups profile is accepted, exactly as the TS does.
//
// The "custom" profile aliases "cloud-native": both are normalized to the same
// lookup and evaluated identically. validatePrerequisites REQUIRES the resolved
// profile to EXIST (it errors when absent, exactly like any other profile). The
// non-empty-criteria check keys on the normalized profile (after custom ->
// cloud-native aliasing) plus the always-present jobFunctions, so custom and
// cloud-native are provably equivalent here: both are accepted when jobFunctions
// keep the criteria non-empty, and both are rejected only if the assembled
// criteria would be truly empty. (The lower-level buildTargetingCriteria
// additionally tolerates the aliased profile being absent, but that branch is
// unreachable via this public flow, which fails closed here first.)
func (c *Client) validatePrerequisites(accountID, profile string) error {
	if _, err := c.orgURN(accountID); err != nil {
		return err
	}
	lookup := profile
	if profile == "custom" {
		lookup = "cloud-native"
	}
	for i := range c.cfg.TargetingProfiles {
		p := &c.cfg.TargetingProfiles[i]
		if p.ID != lookup {
			continue
		}
		// Profile found: require the FINAL assembled include targeting criteria to be
		// non-empty. Count only NON-BLANK skill/group entries — a config-supplied
		// slice can contain blank strings (e.g. []string{""} or {"  "}) that are not
		// usable facets and are dropped by buildTargetingCriteria before the wire —
		// then add the always-present hardcoded jobFunctions facets that
		// buildTargetingCriteria injects into the SAME include `or` block. Mirroring
		// the TS source, empty skills AND groups is acceptable so long as the
		// assembled criteria is non-empty: because jobFunctions is always non-empty,
		// the include criteria is never empty and such a profile is ACCEPTED. Only a
		// truly-empty assembled criteria (no skills, no groups, AND no jobFunctions)
		// is rejected.
		//
		// This check operates on the NORMALIZED profile (the resolved lookup after
		// custom->cloud-native aliasing) plus the shared jobFunctions, so it does NOT
		// special-case the ORIGINAL name: custom and cloud-native resolve to the same
		// lookup and the same TargetingProfileConfig (p) and are evaluated
		// identically. Both are accepted when jobFunctions keep the criteria
		// non-empty; both are rejected only if the assembled criteria would be truly
		// empty. (Absence of the aliased profile is still enforced in the not-found
		// branch below for both names.)
		assembledIncludeFacets := len(nonBlankFacets(p.Skills)) + len(nonBlankFacets(p.Groups)) + len(nonBlankFacets(jobFunctions))
		if assembledIncludeFacets == 0 {
			return fmt.Errorf("LinkedIn targeting profile %q would yield empty targeting criteria (no skills, groups, or job functions) — refusing to create a campaign with no targeting", lookup)
		}
		// Validate facet URN shapes up front (skills, groups, employer exclusions),
		// so a malformed value fails here rather than after the campaign group is
		// created inside buildTargetingCriteria.
		if _, err := validFacets("skills", p.Skills); err != nil {
			return err
		}
		if _, err := validFacets("groups", p.Groups); err != nil {
			return err
		}
		if _, err := validFacets("employer-exclusions", c.cfg.EmployerExclusions); err != nil {
			return err
		}
		return nil
	}
	// Not found. Matching the TS validateLinkedInPrerequisites contract, the
	// aliased "cloud-native" profile must exist even for "custom" here — only the
	// lower-level builder tolerates its absence. So do NOT special-case custom:
	// require the (aliased) profile to be present in the runtime config.
	return fmt.Errorf("LinkedIn targeting profile %q not found in runtime config — refusing to start campaign creation", lookup)
}

// validateRegistrationURL rejects a registration URL before any permanent
// resource is created. LinkedIn's ad API only surfaces a bad landing-page URL
// AFTER the campaign group and campaign already exist, orphaning them; catching
// it up front keeps CreateCampaign side-effect-free on invalid input. The URL
// must parse, be absolute, and use an http/https scheme.
func validateRegistrationURL(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fmt.Errorf("registration URL is required")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		// Do NOT echo the raw/trimmed URL (nor wrap the *url.Error, which embeds the
		// full raw URL in its own message): the caller URL can carry secrets in its
		// userinfo or query string, and this error is logged/persisted. Report a
		// generic message. Mirrors the Reddit client (validateRegistrationURL), which
		// never surfaces the raw URL.
		return fmt.Errorf("registration URL is not a valid URL")
	}
	// Require a real host: url.Parse accepts "https://:443/path" (Host=":443")
	// where Hostname() is empty, so check Hostname() not just Host.
	if !u.IsAbs() || u.Hostname() == "" {
		return fmt.Errorf("registration URL must be absolute (include scheme and host)")
	}
	// url.Parse does NOT validate percent-encoding in the query. A URL like
	// ".../reg?ticket=%zz" parses fine here, but u.Query() (used by BuildUTMURL)
	// silently DROPS the malformed pair, so the paid ad would be created with a
	// different destination than the caller supplied. Reject a malformed query up
	// front, before any mutating call. Mirrors the Reddit client.
	if _, qerr := url.ParseQuery(u.RawQuery); qerr != nil {
		return fmt.Errorf("registration URL has a malformed query string")
	}
	// Reject embedded userinfo (user[:password]@host): an ad destination never
	// needs URL credentials, and BuildUTMURL would otherwise forward the password
	// to LinkedIn and echo it downstream, leaking it. Mirrors the Reddit client.
	if u.User != nil {
		return fmt.Errorf("registration URL must not contain embedded credentials (userinfo)")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return nil
	default:
		// Do NOT echo the raw URL or the scheme value (a bespoke scheme could itself
		// reflect caller material); report a generic message.
		return fmt.Errorf("registration URL must use an http or https scheme")
	}
}

// CreateCampaign runs the full campaign-creation flow: verify prerequisites,
// find/create the ACTIVE campaign group, create the PAUSED campaign, then for
// each variant create a dark post and a DRAFT creative. Mirrors
// executeLinkedInCampaignCreation().
//
// Intentional divergences from the TypeScript source. All of these make the
// contract stricter than the TS original in exactly one direction — they FAIL
// FAST, before the first mutating POST, rather than after the campaign group
// and/or campaign (permanent, paid resources) already exist. Failing before any
// permanent resource is created is the safer contract, so these are deliberate:
//   - Geo resolution is a pure, cache-only function: ResolveGeoTargets performs
//     NO network fallback and REPORTS names it could not resolve to the caller
//     (see ResolveGeoTargets) instead of dropping them silently; any unresolved
//     geo the caller still forwards (an empty-URN GeoTarget) is surfaced here as
//     a Step rather than silently narrowing the audience.
//   - An input whose geo targets ALL resolve to nothing (empty URN set) is
//     rejected up front. The TS source would proceed and create a campaign with
//     empty geo targeting; here that is refused before the first create POST so
//     no orphaned, un-targeted campaign is ever created.
//   - Up-front validation of budget (finite, > 0, and non-zero at 2dp),
//     registration URL, schedule (parseable/non-past/end-after-start), event
//     name, project, per-variant mandatory content, and a non-empty variant set
//     — each rejected before any POST rather than surfaced by LinkedIn only
//     after permanent resources exist.
//   - Transient/unexpected search failures during find-by-name idempotency
//     lookups are NOT swallowed (see findByName), so a duplicate is never
//     created off a hidden failure.
//
// Resumability limitation: creative creation is NOT idempotent, and neither is
// the per-variant dark-post/creative loop as a whole. Unlike the campaign group
// and campaign — which are found idempotently by name (see
// findOrCreateCampaignGroup / createSponsoredCampaign) — dark posts and creatives
// have NO name-based lookup and LinkedIn exposes no upsert primitive, so every
// CreateCampaign re-call RE-CREATES all dark posts and creatives, duplicating
// them. This is the same inherent client-level non-atomicity documented on
// findOrCreateCampaignGroup and createSponsoredCampaign. The client does NOT
// attempt per-creative idempotency. A PLANNED orchestrator per-(brief, platform)
// single-flight claim (LFXV2-2665) is intended to be the authoritative dedup so
// the creative loop isn't re-executed against an already-populated campaign, but
// that claim is NOT implemented/enforced yet — this client provides no dedup
// guarantee of its own. Until it lands, a caller must serialize CreateCampaign
// per (brief, platform) itself to avoid duplicate dark posts/creatives.
//
// If a later variant fails after earlier ones succeeded, the group and campaign
// are found (idempotent by name) on a retry, but each already-created dark post is
// recreated because dark posts have no name-based lookup — a blind retry would
// duplicate the surviving creatives. To keep the caller from retrying blindly,
// a mid-variant failure returns an error that states how many variants
// succeeded versus failed AND still returns a *CampaignResult carrying the
// group/campaign IDs and the steps completed so far (including the created
// creatives). Callers should inspect the partial result rather than re-invoking
// CreateCampaign unchanged. A full idempotent-resume implementation is out of
// scope for this fix.
func (c *Client) CreateCampaign(ctx context.Context, in CampaignInput) (*CampaignResult, error) {
	// Fail fast on a missing or malformed token rather than sending an invalid
	// "Authorization: Bearer <...>" and getting a less actionable API error after
	// network round-trips. A bearer token cannot contain surrounding whitespace,
	// so a padded value (e.g. " token ") is a configuration error, not something
	// to silently trim — reject it explicitly.
	if c.creds.AccessToken == "" || strings.TrimSpace(c.creds.AccessToken) == "" {
		return nil, fmt.Errorf("linkedin: access token is required")
	}
	if c.creds.AccessToken != strings.TrimSpace(c.creds.AccessToken) {
		return nil, fmt.Errorf("linkedin: access token must not have leading or trailing whitespace")
	}

	accountID, err := c.resolveAccountID(in.AdAccountID)
	if err != nil {
		return nil, err
	}

	if err := c.validatePrerequisites(accountID, in.TargetingProfile); err != nil {
		return nil, err
	}

	// EventName is semantically required: it is the sole distinguishing token in
	// both the campaign-group name ("Events | <EventName> | <Project>") and the
	// campaign name, so an empty or whitespace-only value collapses every campaign
	// to the same idempotency key (e.g. "Events |  | TLF"). Reject it up front,
	// before any POST that would create a permanent, mislabeled resource.
	//
	// Trim ONCE here and use the trimmed value everywhere downstream (group name,
	// campaign name, ad name / idempotency keys): validating a trimmed value but
	// then building resources from the original untrimmed field let a value like
	// "  KubeCon  " pass validation yet produce resources with leading/trailing
	// whitespace and an inconsistent idempotency key.
	eventName := strings.TrimSpace(in.EventName)
	if eventName == "" {
		return nil, fmt.Errorf("event name is required and must not be empty or whitespace-only")
	}
	// The campaign-group/campaign names are pipe-delimited ("Events | <name> | ..."),
	// so a caller value containing "|" would inject extra fields and corrupt the
	// naming schema / project attribution and name-based idempotency. Replace "|"
	// with "-" before composing names, matching the other platform clients.
	eventName = strings.ReplaceAll(eventName, "|", "-")

	// Trim the registration URL ONCE up front and use the trimmed value both for
	// validation and everywhere downstream (BuildUTMURL). Validating a trimmed
	// value but then building the UTM URL from the original untrimmed field let a
	// value with surrounding whitespace pass validation yet produce a malformed
	// UTM URL (embedded spaces).
	reg := strings.TrimSpace(in.RegistrationURL)

	// Validate the registration URL BEFORE any POST so an empty/relative/malformed
	// URL is rejected up front rather than after the campaign group and campaign
	// (permanent resources) already exist.
	if err := validateRegistrationURL(reg); err != nil {
		return nil, err
	}

	// Validate the budget BEFORE any POST. BudgetUSD is formatted straight into
	// the campaign body, so a non-positive, NaN, or Inf value would otherwise be
	// rejected by LinkedIn only AFTER the campaign group (a permanent resource)
	// already exists, orphaning it.
	if math.IsNaN(in.BudgetUSD) || math.IsInf(in.BudgetUSD, 0) {
		return nil, fmt.Errorf("budget must be a finite number, got %v", in.BudgetUSD)
	}
	if in.BudgetUSD <= 0 {
		return nil, fmt.Errorf("budget must be greater than zero, got %v", in.BudgetUSD)
	}
	// Round the budget to the SAME 2-decimal precision createSponsoredCampaign
	// serializes to the wire (strconv.FormatFloat(_, 'f', 2, 64)) and validate
	// against THAT value, so the amount checked here is exactly the amount sent.
	// Validating the raw float instead diverged from the wire value: e.g. 9.999
	// is sent as "10.00" (meets the $10 minimum) yet the raw-float check rejected
	// it, and 99.999 hit the same problem for the lifetime minimum.
	roundedBudgetStr := strconv.FormatFloat(in.BudgetUSD, 'f', 2, 64)
	roundedBudget, parseErr := strconv.ParseFloat(roundedBudgetStr, 64)
	if parseErr != nil {
		// Should be unreachable for a finite float already validated above, but
		// fail closed rather than proceed with an unverified budget.
		return nil, fmt.Errorf("budget %v could not be normalized to a 2-decimal amount: %w", in.BudgetUSD, parseErr)
	}
	// A sub-cent budget (e.g. 0.001) passes the > 0 / NaN / Inf checks yet rounds
	// to "0.00" at the wire precision — a zero budget LinkedIn would reject only
	// AFTER the campaign group (a permanent resource) already exists, orphaning
	// it. Reject any budget that rounds to zero, up front, before any POST.
	if roundedBudgetStr == "0.00" {
		return nil, fmt.Errorf("budget %v is below the minimum billable amount (0.01) and would round to zero at the API boundary", in.BudgetUSD)
	}
	// Enforce LinkedIn's per-campaign budget minimums BEFORE any POST. LinkedIn
	// rejects a dailyBudget under $10 and a totalBudget (lifetime) under $100, but
	// only AFTER the campaign group (a permanent resource) already exists,
	// orphaning it. LifetimeBudget selects the totalBudget field downstream (see
	// createSponsoredCampaign), so the minimum tracks that choice. These minimums
	// are USD-specific — the client only ever sends currencyCode "USD". The check
	// uses roundedBudget so the value checked matches the value sent.
	if in.LifetimeBudget {
		if roundedBudget < minLifetimeBudgetUSD {
			return nil, fmt.Errorf("lifetime budget %v is below LinkedIn's minimum of $%.0f for a total (lifetime) budget", in.BudgetUSD, minLifetimeBudgetUSD)
		}
	} else if roundedBudget < minDailyBudgetUSD {
		return nil, fmt.Errorf("daily budget %v is below LinkedIn's minimum of $%.0f for a daily budget", in.BudgetUSD, minDailyBudgetUSD)
	}

	// Refuse to create a campaign with no creatives: LinkedIn campaign-group and
	// campaign creation are permanent side effects, so an empty variant set would
	// leave an orphaned, un-adorned campaign upstream.
	if len(in.Variants) == 0 {
		return nil, fmt.Errorf("at least one creative variant is required")
	}

	// Validate every variant's mandatory CONTENT up front, before any POST.
	// Checking only the NUMBER of variants (len > 0) let a variant whose Headline
	// or IntroText is empty / whitespace-only / dash-only slip through: those
	// fields are rejected by LinkedIn only AFTER the campaign group and campaign
	// (permanent resources) already exist, orphaning them. Normalize each field
	// exactly the way createDarkPost/createCreative will (trim + stripDashes) so a
	// value that normalizes to empty — e.g. a lone em dash "—" that stripDashes
	// collapses away — is caught here rather than upstream. Both the article
	// headline (title) and the primary/commentary text (introText) are required
	// for the article dark post, so both are validated.
	for i, variant := range in.Variants {
		if normalizeCreativeText(variant.Headline) == "" {
			return nil, fmt.Errorf("variant-%d headline is required and must not be empty, whitespace-only, or dash-only after normalization", i+1)
		}
		if normalizeCreativeText(variant.IntroText) == "" {
			return nil, fmt.Errorf("variant-%d intro text is required and must not be empty, whitespace-only, or dash-only after normalization", i+1)
		}
		// ImageURN is optional public/AI-derived input. When non-empty it is sent
		// verbatim as the article thumbnail by createDarkPost, so validate its
		// digital-asset URN shape up front: unlike geo URNs, an unchecked malformed
		// value would otherwise reach LinkedIn only AFTER the campaign group and
		// campaign (permanent resources) already exist. An empty ImageURN stays
		// allowed.
		if variant.ImageURN != "" && !imageURNRE.MatchString(variant.ImageURN) {
			return nil, fmt.Errorf("variant-%d image URN %q is malformed: expected urn:li:image:<id> or urn:li:digitalmediaAsset:<id>", i+1, variant.ImageURN)
		}
	}

	// Project is the campaign-name Project segment the data pipeline joins on for
	// foundation attribution. Require the caller's canonical LFX slug rather than
	// silently defaulting (a hardcoded default mis-attributes every non-TLF
	// campaign), matching the api-catalog contract and the twitter/reddit clients.
	// Trim once (a whitespace-only value is treated as empty and rejected), and
	// sanitize "|" -> "-" so it can't inject extra pipe-delimited name fields.
	project := strings.ReplaceAll(strings.TrimSpace(in.Project), "|", "-")
	if project == "" {
		return nil, fmt.Errorf("project is required: supply the canonical LFX project slug for the campaign name's Project segment")
	}

	steps := []string{}

	// Build the geo URN set and refuse to create anything with no usable geo
	// targeting BEFORE the first create POST. ResolveGeoTargets (the pure, cache-only
	// resolver the caller runs to build in.GeoTargets) reports names it could not
	// resolve to the caller rather than dropping them silently; an unresolved name
	// that the caller still forwards arrives here as a GeoTarget with an EMPTY URN.
	// Rather than silently narrow the audience, SURFACE each dropped (empty-URN) geo
	// as a Step (mirroring how the Meta client surfaces dropped geos), so the
	// narrowing is visible in the result. A caller passing only unresolved geos
	// arrives with an empty URN set; creating the group/campaign anyway (both
	// permanent side effects) would leave an orphaned campaign with empty geo
	// targeting, so that is refused below.
	geoURNs := make([]string, 0, len(in.GeoTargets))
	droppedGeos := make([]string, 0, len(in.GeoTargets))
	for _, g := range in.GeoTargets {
		if g.URN == "" {
			// Prefer the human-readable Label for the surfaced Step; fall back to a
			// placeholder when even the label is blank so the count is still visible.
			label := strings.TrimSpace(g.Label)
			if label == "" {
				label = "(unnamed)"
			}
			droppedGeos = append(droppedGeos, label)
			continue
		}
		// A non-empty URN isn't necessarily valid: GeoTarget is public input, so a
		// caller can pass a malformed URN. Reject it up front rather than sending an
		// invalid location to LinkedIn only after the campaign group is created.
		if !geoURNRE.MatchString(g.URN) {
			return nil, fmt.Errorf("invalid geo target URN %q: expected urn:li:geo:<id>", g.URN)
		}
		geoURNs = append(geoURNs, g.URN)
	}
	if len(geoURNs) == 0 {
		return nil, fmt.Errorf("no usable geo targets: all supplied geos resolved to nothing — refusing to create a campaign with empty geo targeting")
	}
	if len(droppedGeos) > 0 {
		steps = append(steps, fmt.Sprintf("Geo targets not resolved (omitted from targeting — not in the resolver map): %s", strings.Join(droppedGeos, ", ")))
	}

	// Validate the schedule ONCE up front, before the first idempotency lookup or
	// POST, and capture the computed epoch-millisecond start/end. These values are
	// the single source of truth threaded into the create helpers so the campaign
	// group and campaign share identical timestamps: toMs is non-deterministic for
	// a today/past start (it returns a moving now+startTimeBuffer), so re-computing
	// per helper could otherwise send DIFFERENT start millis than this preflight
	// validated.
	// Enforcing here also guarantees the date contract regardless of idempotency
	// state (the helpers only reached toMs AFTER their find-existing lookups).
	startMs, endMs, err := c.validateSchedule(in.StartDate, in.EndDate)
	if err != nil {
		return nil, err
	}

	groupName := fmt.Sprintf("Events | %s | %s", eventName, project)
	campaignName := fmt.Sprintf("Events | %s | LinkedIn | Conversions | Prospecting | Static | %s | MoFU", eventName, project)
	// LinkedIn limits campaign-group and campaign names to 255 characters. Validate
	// both generated names before the first create so an over-long event name/project
	// fails fast instead of after the group is created.
	if n := len([]rune(groupName)); n > maxNameLen {
		return nil, fmt.Errorf("campaign group name is %d characters, exceeds the %d-character limit; shorten the event name or project", n, maxNameLen)
	}
	if n := len([]rune(campaignName)); n > maxNameLen {
		return nil, fmt.Errorf("campaign name is %d characters, exceeds the %d-character limit; shorten the event name or project", n, maxNameLen)
	}

	groupID, err := c.findOrCreateCampaignGroup(ctx, accountID, groupName, startMs, endMs)
	if err != nil {
		// An AMBIGUOUS failure (transport/timeout after send, a 5xx, or an
		// undecodable 2xx) can occur AFTER LinkedIn committed the group create: the
		// possibly-created ACTIVE group carries the deterministic groupName, so report
		// an UNCONFIRMED outcome and a partial result carrying the name rather than a
		// definite failure that could let a retry duplicate it. On retry the group is
		// found idempotently by name (reconcile-by-name), so this is recoverable. A
		// clearly non-ambiguous error (a 4xx rejection, or a search error before the
		// create POST) means nothing was created — keep the plain (nil, err). Mirrors
		// the Meta client's createOutcomeAmbiguous handling.
		//
		// A CALLER cancellation (ctx cancelled / deadline exceeded) is a deliberate
		// abort, NOT an ambiguous server outcome: doRequest wraps context.Canceled as
		// a transportError, so without this guard createOutcomeAmbiguous would report a
		// misleading "UNCONFIRMED / verify before recreating" step. Abort cleanly with
		// the cancellation error instead. Mirrors the Reddit client's ctx.Err() guard.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("linkedin campaign creation aborted (campaign group creation): %w", ctxErr)
		}
		if createOutcomeAmbiguous(err) {
			steps = append(steps, fmt.Sprintf("Campaign group creation outcome is UNCONFIRMED (timeout or server error); an ACTIVE group %q may exist — verify by name in Campaign Manager before recreating", groupName))
			return c.buildResult(accountID, groupName, "", campaignName, "", 0, steps),
				fmt.Errorf("campaign group creation UNCONFIRMED (an ACTIVE group %q may exist — on retry it is found idempotently by name): %w", groupName, err)
		}
		return nil, err
	}
	steps = append(steps, fmt.Sprintf("Campaign group: %s (ID: %s)", groupName, groupID))

	campaignID, err := c.createSponsoredCampaign(ctx, accountID, groupID, campaignName, in.StartDate, endMs, in.BudgetUSD, geoURNs, in.TargetingProfile, in.LifetimeBudget)
	if err != nil {
		// findOrCreateCampaignGroup may have just created a PERMANENT campaign group
		// (groupID) whose creation is a real side effect. Returning nil,err here
		// would discard that known id, leaving the caller unable to see or reconcile
		// the created group. Mirror the partial-variant-failure path: return a
		// non-nil partial *CampaignResult carrying the created CampaignGroupID and
		// the steps so far (campaignID is still empty), alongside the error, so no
		// created permanent resource is silently orphaned. On the next retry the
		// group is found idempotently by name, so surfacing it is safe.
		//
		// An AMBIGUOUS failure (transport/timeout after send, a 5xx, or an
		// undecodable 2xx) can occur AFTER LinkedIn committed the campaign create: the
		// possibly-created PAUSED campaign carries the deterministic campaignName and
		// is found idempotently by name-in-group on retry, so report an UNCONFIRMED
		// outcome rather than a definite failure that could let a retry duplicate it.
		// A definite (4xx / pre-send / search) error means the campaign was not
		// created. Mirrors the Meta client's createOutcomeAmbiguous handling.
		//
		// A CALLER cancellation (ctx cancelled / deadline exceeded) is a deliberate
		// abort, NOT an ambiguous server outcome: doRequest wraps context.Canceled as
		// a transportError, so without this guard createOutcomeAmbiguous would report a
		// misleading "UNCONFIRMED / verify before recreating" step. Abort cleanly with
		// the cancellation error instead. Mirrors the Reddit client's ctx.Err() guard.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("linkedin campaign creation aborted (campaign creation): %w", ctxErr)
		}
		if createOutcomeAmbiguous(err) {
			steps = append(steps, fmt.Sprintf("Campaign creation outcome is UNCONFIRMED (timeout or server error); a PAUSED campaign %q may exist — verify by name before recreating", campaignName))
			return c.buildResult(accountID, groupName, groupID, campaignName, "", 0, steps),
				fmt.Errorf("campaign creation UNCONFIRMED for campaign group %q (%q) (a PAUSED campaign %q may exist and is found idempotently by name on retry): %w — inspect the returned partial result", groupID, groupName, campaignName, err)
		}
		return c.buildResult(accountID, groupName, groupID, campaignName, "", 0, steps),
			fmt.Errorf("campaign creation failed for campaign group %q (%q) (the group was created or already existed): %w — on retry the group is found idempotently by name; inspect the returned partial result", groupID, groupName, err)
	}
	// Neutral wording: createSponsoredCampaign is find-or-create, so campaignID may
	// be a NEWLY-created campaign (which is PAUSED) OR an existing same-name campaign
	// found idempotently in this group, which can be in any non-terminal status
	// (including ACTIVE). Reporting "created ... (PAUSED)" would be false on the
	// found path, so the step states only that the campaign is ensured to exist.
	steps = append(steps, fmt.Sprintf("Campaign ensured: %s (ID: %s)", campaignName, campaignID))

	// NOT idempotent: dark posts and creatives have no name-based lookup and
	// LinkedIn has no upsert, so re-running this loop duplicates every dark post
	// and creative. A single-flight guard one layer up (the PLANNED orchestrator
	// per-(brief, platform) claim, LFXV2-2665) is intended to prevent re-runs, but
	// it is NOT implemented yet — this loop provides no dedup itself; see the
	// CreateCampaign godoc.
	creativeCount := 0
	for i, variant := range in.Variants {
		destURL := BuildUTMURL(reg, in.HSToken, campaignName, i+1)
		shareURN, err := c.createDarkPost(ctx, accountID, variant.IntroText, variant.Headline, destURL, variant.ImageURN)
		if err != nil {
			// Dark posts have NO name-based reconciliation lookup and LinkedIn exposes
			// no upsert, so an AMBIGUOUS failure (transport/timeout after send, a 5xx,
			// or an undecodable 2xx) is especially dangerous: the post MAY already have
			// been created, and a blind retry would orphan a duplicate that can't be
			// found and cleaned up by name. Report an UNCONFIRMED outcome (verify before
			// recreating) rather than a definite failure. A definite (4xx / pre-send)
			// error means nothing was created. Mirrors the Meta client's ambiguous ad/
			// creative handling.
			//
			// A CALLER cancellation (ctx cancelled / deadline exceeded) is a deliberate
			// abort, NOT an ambiguous server outcome: doRequest wraps context.Canceled as
			// a transportError, so without this guard createOutcomeAmbiguous would report a
			// misleading "UNCONFIRMED / verify before recreating" step. Abort cleanly with
			// the cancellation error instead. Mirrors the Reddit client's ctx.Err() guard.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, fmt.Errorf("linkedin campaign creation aborted (dark post creation): %w", ctxErr)
			}
			if createOutcomeAmbiguous(err) {
				steps = append(steps, fmt.Sprintf("Dark post variant-%d outcome UNCONFIRMED (timeout or server error); it may have been created — verify before recreating (a dark post has no idempotency lookup)", i+1))
				return c.buildResult(accountID, groupName, groupID, campaignName, campaignID, creativeCount, steps),
					fmt.Errorf("variant-%d dark post UNCONFIRMED after %d of %d variant(s) created: %w — group %q and campaign %q already exist; the dark post MAY have been created and has no idempotency lookup, so do NOT blindly retry (would duplicate it); inspect the returned partial result", i+1, creativeCount, len(in.Variants), err, groupID, campaignID)
			}
			return c.buildResult(accountID, groupName, groupID, campaignName, campaignID, creativeCount, steps),
				fmt.Errorf("variant-%d dark post failed after %d of %d variant(s) created: %w — group %q and campaign %q already exist; do NOT blindly retry (would duplicate the %d created creative(s)); inspect the returned partial result", i+1, creativeCount, len(in.Variants), err, groupID, campaignID, creativeCount)
		}
		steps = append(steps, fmt.Sprintf("Dark post variant-%d: %s", i+1, shareURN))

		adName := fmt.Sprintf("%s | variant-%d", eventName, i+1)
		creativeID, err := c.createCreative(ctx, accountID, campaignID, shareURN, adName)
		if err != nil {
			// The creative failed but this variant's dark post (shareURN) was
			// already created. Report it explicitly: a blind retry would duplicate
			// not just the previously-completed creatives but ALSO this orphaned
			// dark post, which has no name-based idempotency lookup. Surfacing the
			// shareURN keeps recovery state clear even for the first variant, where
			// creativeCount is still 0 yet a dark post already exists upstream.
			//
			// Creatives ALSO have no name-based reconciliation lookup, so an AMBIGUOUS
			// failure (transport/timeout after send, a 5xx, or an undecodable 2xx) means
			// the creative MAY already exist: report an UNCONFIRMED outcome rather than a
			// definite failure, since a blind retry would orphan a duplicate. A definite
			// (4xx / pre-send) error means the creative was not created. Mirrors the Meta
			// client's ambiguous ad/creative handling.
			//
			// A CALLER cancellation (ctx cancelled / deadline exceeded) is a deliberate
			// abort, NOT an ambiguous server outcome: doRequest wraps context.Canceled as
			// a transportError, so without this guard createOutcomeAmbiguous would report a
			// misleading "UNCONFIRMED / verify before recreating" step. Abort cleanly with
			// the cancellation error instead. Mirrors the Reddit client's ctx.Err() guard.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, fmt.Errorf("linkedin campaign creation aborted (creative creation): %w", ctxErr)
			}
			if createOutcomeAmbiguous(err) {
				steps = append(steps, fmt.Sprintf("Creative variant-%d outcome UNCONFIRMED (timeout or server error); it may have been created — verify before recreating (orphaned dark post: %s)", i+1, shareURN))
				return c.buildResult(accountID, groupName, groupID, campaignName, campaignID, creativeCount, steps),
					fmt.Errorf("variant-%d creative UNCONFIRMED after %d of %d variant(s) created: %w — group %q and campaign %q already exist AND this variant's dark post %q was already created; the creative MAY have been created and has no idempotency lookup, so do NOT blindly retry (would duplicate the creative and the dark post %q); inspect the returned partial result", i+1, creativeCount, len(in.Variants), err, groupID, campaignID, shareURN, shareURN)
			}
			return c.buildResult(accountID, groupName, groupID, campaignName, campaignID, creativeCount, steps),
				fmt.Errorf("variant-%d creative failed after %d of %d variant(s) created: %w — group %q and campaign %q already exist AND this variant's dark post %q was already created; do NOT blindly retry (would duplicate the %d created creative(s) and the dark post %q, which has no idempotency lookup); inspect the returned partial result", i+1, creativeCount, len(in.Variants), err, groupID, campaignID, shareURN, creativeCount, shareURN)
		}
		steps = append(steps, fmt.Sprintf("Creative (DRAFT): %s", creativeID))
		creativeCount++
	}

	return c.buildResult(accountID, groupName, groupID, campaignName, campaignID, creativeCount, steps), nil
}

// campaignManagerURL returns the Campaign Manager deep link. When no campaign was
// created (empty id, e.g. a group-created-but-campaign-failed partial result), it
// links to the account's campaigns list rather than a dangling /campaigns/ URL.
func campaignManagerURL(accountID, campaignID string) string {
	base := fmt.Sprintf("https://www.linkedin.com/campaignmanager/accounts/%s/campaigns", accountID)
	if campaignID == "" {
		return base
	}
	return base + "/" + campaignID
}

// buildResult assembles a CampaignResult from the created hierarchy pieces.
func (c *Client) buildResult(accountID, groupName, groupID, campaignName, campaignID string, creativeCount int, steps []string) *CampaignResult {
	return &CampaignResult{
		Platform:          "linkedin-ads",
		CampaignGroupName: groupName,
		CampaignGroupID:   groupID,
		CampaignName:      campaignName,
		CampaignID:        campaignID,
		CreativeCount:     creativeCount,
		LinkedInURL:       campaignManagerURL(accountID, campaignID),
		Steps:             steps,
	}
}
