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
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrCredentialsExpired marks a LinkedIn connection whose access token is no
// longer usable AND cannot be refreshed — either the app holds no refresh token
// (LinkedIn issues them only to approved Marketing Developer Platform partners)
// or the refresh token itself is expired/revoked. It is the actionable,
// non-500 surface the dispatcher maps to a "reconnect this connection" outcome;
// without it the only evidence was a 401 buried in a server log.
var ErrCredentialsExpired = errors.New("linkedin credentials expired")

// credentialsExpiredError names the connection a human must reconnect. It
// deliberately carries NO token material — only the operator-set connection
// label, a short cause, and the request coordinates the outcome classifier needs.
//
// Method/StatusCode exist because a 401 answering an in-flight request states TWO
// things, not one. "Reconnect this connection" is the actionable half; on a
// MUTATING request it is not the whole story. LinkedIn "reserves the right to
// revoke Refresh Tokens or Access Tokens at any time", including AFTER it accepted
// and committed a create POST but before the response was written — so a 401 on a
// POST leaves the create outcome genuinely UNKNOWABLE, exactly like the mutating
// 5xx/429/3xx cases createOutcomeAmbiguous already recognises. Discarding the
// method and status at the 401 wrap site erased that second fact, so the create
// cascade reported a clean "credentials expired, reconnect" for a campaign group
// that may already exist upstream and be billing — an orphan nothing was told to
// look for.
//
// Both fields are ZERO on the PRE-SEND arms (accessTokenValue's fail-closed checks
// and the token-exchange rejection): nothing was sent, so nothing can have landed,
// and a zero Method is not a mutating method — createOutcomeAmbiguous therefore
// reports false for them without a special case. Only the arms that observed a
// real response to a real request populate them.
type credentialsExpiredError struct {
	Connection string
	Reason     string
	// Method is the HTTP method of the request LinkedIn answered with the 401, or
	// "" when the failure happened before any request was sent.
	Method string
	// StatusCode is the status LinkedIn answered with (401 on the response arms), or
	// 0 when the failure happened before any request was sent.
	StatusCode int
}

func (e *credentialsExpiredError) Error() string {
	// Method/StatusCode are deliberately NOT rendered: they exist for classification,
	// and the operator-facing message stays the single actionable sentence. The
	// Reason already names the 401 on the arms that carry one.
	return fmt.Sprintf("linkedin: %s must be reconnected: %s (re-authorize the LinkedIn connection to continue)",
		e.Connection, e.Reason)
}

func (e *credentialsExpiredError) Unwrap() error { return ErrCredentialsExpired }

// tokenRefreshError wraps a transport failure of the OAuth2 token exchange. Error()
// renders only text this package owns; Unwrap preserves the (already-redacted) cause so
// callers can still errors.Is it (e.g. context.Canceled). Mirrors the microsoft client's
// tokenTransportError discipline.
type tokenRefreshError struct{ err error }

func (e *tokenRefreshError) Error() string { return "linkedin token refresh: " + e.err.Error() }

func (e *tokenRefreshError) Unwrap() error { return e.err }

// tokenRefresh holds the shared result of one in-flight token refresh. done is
// closed when the refresh completes; token/err carry the outcome.
type tokenRefresh struct {
	done  chan struct{}
	token string
	err   error
}

// tokenResponse is LinkedIn's documented token-endpoint response. Field names and
// shape are taken from LinkedIn's published sample response, NOT inferred:
// access_token, expires_in, refresh_token, refresh_token_expires_in, scope.
// https://learn.microsoft.com/en-us/linkedin/shared/authentication/programmatic-refresh-tokens
type tokenResponse struct {
	AccessToken           string `json:"access_token"`
	ExpiresIn             int    `json:"expires_in"`               // seconds
	RefreshToken          string `json:"refresh_token"`            // LinkedIn returns the refresh token again
	RefreshTokenExpiresIn int    `json:"refresh_token_expires_in"` // seconds; TTL does NOT reset on use
}

// accessTokenValue returns a usable bearer token, refreshing via LinkedIn's OAuth2
// token endpoint when the current one is absent or within tokenExpiryBuffer of a
// KNOWN expiry.
//
// Three shapes, in order:
//
//  1. No refresh material (the common non-MDP case) — return the injected access
//     token unchanged, preserving today's bearer-only behaviour exactly. If that
//     token's expiry is known and already past, fail closed with
//     ErrCredentialsExpired rather than sending a request guaranteed to 401.
//  2. Cached/injected token still valid past the buffer — return it, no network.
//  3. Otherwise refresh, coalescing concurrent callers single-flight so N callers
//     produce ONE exchange.
//
// The lock is never held across the network call; every waiter selects on its own
// ctx so a cancelled caller returns promptly without tearing down the shared
// refresh. Mirrors the google-ads and microsoft clients deliberately.
func (c *Client) accessTokenValue(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	c.tokenMu.Lock()
	// Fast path: a cached refreshed token that is still valid past the buffer.
	if c.accessToken != "" && c.now().Add(tokenExpiryBuffer).Before(c.tokenExpiry) {
		token := c.accessToken
		c.tokenMu.Unlock()
		return token, nil
	}

	if !c.creds.CanRefresh() {
		c.tokenMu.Unlock()
		// Bearer-only connection. Fail closed ONLY on a known-past expiry: an unknown
		// (zero) expiry must keep working exactly as it does today — absence of an
		// expiry is not evidence of expiry.
		if !c.creds.AccessTokenExpiresAt.IsZero() && !c.now().Before(c.creds.AccessTokenExpiresAt) {
			return "", &credentialsExpiredError{
				Connection: c.creds.ConnectionLabel(),
				Reason:     "the access token expired and no refresh token is stored",
			}
		}
		return c.creds.AccessToken, nil
	}

	// Refresh is possible. Reuse the injected access token while it is known-good.
	if c.accessToken == "" && c.creds.AccessToken != "" &&
		!c.creds.AccessTokenExpiresAt.IsZero() &&
		c.now().Add(tokenExpiryBuffer).Before(c.creds.AccessTokenExpiresAt) {
		token := c.creds.AccessToken
		c.tokenMu.Unlock()
		return token, nil
	}

	// A refresh token that is itself expired can never mint anything: fail closed
	// with the actionable error instead of spending a round-trip to learn it.
	if !c.refreshExpiry.IsZero() && !c.now().Before(c.refreshExpiry) {
		c.tokenMu.Unlock()
		return "", &credentialsExpiredError{
			Connection: c.creds.ConnectionLabel(),
			Reason:     "the refresh token expired, so a new access token cannot be minted",
		}
	}

	inflight := c.inflight
	if inflight == nil {
		// Become the leader: publish the shared result and run the fetch on a context
		// detached from this caller's CANCELLATION (one caller's cancel must not tear
		// down a refresh others depend on) while preserving its request-scoped VALUES.
		inflight = &tokenRefresh{done: make(chan struct{})}
		c.inflight = inflight
		refreshValuesCtx := context.WithoutCancel(ctx)
		go func() {
			fetchCtx, cancel := context.WithTimeout(refreshValuesCtx, requestTimeout)
			token, err := c.fetchToken(fetchCtx)
			cancel()

			c.tokenMu.Lock()
			inflight.token = token
			inflight.err = err
			c.inflight = nil
			close(inflight.done)
			c.tokenMu.Unlock()
		}()
	}
	c.tokenMu.Unlock()

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-inflight.done:
		return inflight.token, inflight.err
	}
}

// fetchToken performs the refresh_token -> access_token exchange and caches the
// result under tokenMu. It is only ever invoked from the leader's detached refresh
// goroutine, so at most one call is in flight at a time.
func (c *Client) fetchToken(ctx context.Context) (string, error) {
	// Read the refresh token under tokenMu: a previous exchange may have rotated it
	// (see the adoption below), and c.creds is otherwise read by callers on other
	// goroutines. ClientID/ClientSecret are immutable after construction.
	c.tokenMu.Lock()
	refreshToken := c.refreshToken
	c.tokenMu.Unlock()

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", c.creds.ClientID)
	form.Set("client_secret", c.creds.ClientSecret)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build linkedin token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// The token endpoint runs no mutation, so a failure here is unambiguous and must
		// NOT be wrapped as transportError ("may exist").
		//
		// Do NOT %w the raw error: this request's BODY carried client_secret and
		// refresh_token, and WithHTTPClient accepts an arbitrary RoundTripper whose error
		// text is caller-controlled and can echo that body. http.Client.Do then wraps it
		// in a *url.Error, so stripping one layer would still leave the credential-bearing
		// error reachable by anything that renders the chain — and this error can be
		// persisted into a campaign's Steps, making a leak durable. redactHTTPDoError
		// rebuilds the cause from its CLASSIFICATION alone, so no untrusted text survives
		// while errors.Is/As still work. tokenRefreshError keeps Unwrap for classification
		// without rendering the cause's text.
		return "", &tokenRefreshError{err: redactHTTPDoError(err)}
	}
	defer func() { _ = resp.Body.Close() }()

	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(io.LimitReader(resp.Body, maxResponseBytes+1)); err != nil {
		// Redacted for the same reason the Do error above is: WithHTTPClient accepts an
		// arbitrary RoundTripper, so the *response* it returns is caller-controlled too and
		// its Body.Read error text can echo the request body that carried client_secret and
		// the refresh token. Wrapping the raw cause with %w bypassed the redaction guarantee
		// the transport arm establishes, and this error can be persisted into a campaign's
		// Steps, so the leak would be durable. redactBodyReadError rebuilds the cause from
		// its classification, preserving context.Canceled/DeadlineExceeded for errors.Is
		// while rendering no untrusted text.
		return "", fmt.Errorf("read linkedin token response: %w", redactBodyReadError(err))
	}
	if int64(buf.Len()) > maxResponseBytes {
		return "", fmt.Errorf("linkedin token response exceeds %d bytes", maxResponseBytes)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// LinkedIn returns 400 invalid_request for a refresh token that is expired,
		// revoked OR invalid — its docs give the SAME status and error for all three,
		// and the resolution for all three is identical: re-authorize the member. So
		// classify on status, not on a parsed reason, and surface the actionable error.
		//
		// The body is NEVER echoed: this request carried client_secret and the refresh
		// token, and an OAuth/proxy diagnostic body is untrusted and may reflect that
		// credential material. This error can be persisted into a campaign's Steps, so
		// a leak would be durable. Report status only.
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized {
			return "", &credentialsExpiredError{
				Connection: c.creds.ConnectionLabel(),
				Reason: fmt.Sprintf("LinkedIn rejected the refresh token (HTTP %d: expired, revoked or invalid)",
					resp.StatusCode),
			}
		}
		return "", fmt.Errorf("linkedin token refresh -> %d", resp.StatusCode)
	}

	var tok tokenResponse
	if err := json.Unmarshal(buf.Bytes(), &tok); err != nil {
		// Report SHAPE only, never the cause. This is the one arm of fetchToken that still
		// wrapped an untrusted cause, and it decodes the most sensitive body in the package:
		// a 2xx token response carrying access_token and the rotated refresh_token. A
		// json.UnmarshalTypeError reproduces an out-of-range NUMBER LITERAL verbatim and
		// unbounded (a 200-digit literal renders in full), and SyntaxError echoes the
		// offending character — upstream text that reaches a campaign's persisted Steps.
		//
		// A full credential is NOT expressible here — a token is not all-digits, and any
		// non-numeric byte fails as a syntax error before the literal is rendered — so this
		// is defence in depth, not a demonstrated leak. It matches what the sibling decode
		// at metrics.go already does for the same reason, and it means every arm of this
		// function now rebuilds its error from classification rather than forwarding text.
		return "", fmt.Errorf("decode linkedin token response: malformed JSON (%d bytes)", buf.Len())
	}
	if tok.AccessToken == "" {
		// Fail closed: never fall back to the stale token on a malformed success.
		return "", errors.New("linkedin token refresh returned an empty access_token")
	}

	// expires_in may be absent or non-positive; default so a missing value neither
	// pins a stale token forever nor caches an already-expired entry.
	ttl := time.Duration(tok.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = defaultTokenTTL
	}

	c.tokenMu.Lock()
	c.accessToken = tok.AccessToken
	c.tokenExpiry = c.now().Add(ttl)
	// LinkedIn returns the refresh token again on every exchange. Adopt it in-memory
	// so a rotated value is used for the NEXT refresh within this client's lifetime.
	//
	// The rotated values live in c.refreshToken / c.refreshExpiry, NOT back in c.creds:
	// c.creds is injected at construction and read WITHOUT the lock by other paths
	// (validateCredentialShape, the CanRefresh fast path), so writing to it here would
	// be a data race. These two fields are the only mutable refresh state, and every
	// access to them is under tokenMu.
	//
	// Deliberately NOT persisted: this package never touches the database (credentials
	// are injected), and LinkedIn does not reset the refresh TTL on use, so the stored
	// value stays valid until the member re-authorizes.
	if rt := strings.TrimSpace(tok.RefreshToken); rt != "" {
		c.refreshToken = rt
	}
	if tok.RefreshTokenExpiresIn > 0 {
		c.refreshExpiry = c.now().Add(time.Duration(tok.RefreshTokenExpiresIn) * time.Second)
	}
	refreshDeadline := c.refreshExpiry
	c.tokenMu.Unlock()

	c.warnIfRefreshTokenNearExpiry(ctx, refreshDeadline)
	return tok.AccessToken, nil
}

// warnIfRefreshTokenNearExpiry logs when the REFRESH token is inside its final
// window. LinkedIn does not extend a refresh token's TTL when it is used, so this
// deadline is a hard stop that only a human re-authorization clears — surfacing it
// early is the difference between a scheduled reconnect and an outage. Logs only
// the connection label and a whole-day count: never a token.
func (c *Client) warnIfRefreshTokenNearExpiry(ctx context.Context, deadline time.Time) {
	if deadline.IsZero() {
		return
	}
	remaining := deadline.Sub(c.now())
	if remaining > refreshTokenExpiryWarning {
		return
	}
	slog.WarnContext(ctx, "linkedin refresh token is nearing expiry; re-authorize the connection to avoid an outage",
		"connection", c.creds.ConnectionLabel(),
		"days_remaining", int(remaining.Hours()/24),
	)
}

// isTokenExpiryResponse reports whether a LinkedIn 401 indicates the access token
// is expired/revoked/invalid rather than some other authorization problem.
//
// Deliberately NOT keyed solely on serviceErrorCode 65602: LinkedIn's error-handling
// guide documents "expired access token", "the token has been revoked" and "invalid
// access token" as distinct 401 conditions and publishes NO subcode for the latter
// two, so a 65602-only test would miss revocation entirely. The subcode is used as a
// positive signal when present; otherwise a 401 on an authenticated call is itself
// the signal.
func isTokenExpiryResponse(statusCode int, body string) bool {
	if statusCode != http.StatusUnauthorized {
		return false
	}
	var parsed struct {
		ServiceErrorCode int `json:"serviceErrorCode"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err == nil &&
		parsed.ServiceErrorCode == serviceErrorCodeExpiredAccessToken {
		return true
	}
	// A 401 from an authenticated LinkedIn call means the bearer token was not
	// accepted; every documented cause is resolved by re-authorizing.
	return true
}

// invalidateAccessToken clears the cached access token so the next caller
// re-exchanges instead of replaying one LinkedIn has already rejected. Used when a
// 401 proves the token is dead despite any expiry timestamp saying otherwise — a
// revocation carries no advance notice.
//
// It clears only the CACHE, never the refresh material: a revoked access token does
// not imply a revoked refresh token, and discarding the latter would turn a
// recoverable state into one needing a human re-authorization.
//
// "The next caller" means a SUBSEQUENT OPERATION, not the failing one. Both 401 arms
// (doRequest and doAdAnalyticsAttempt) return immediately after calling this, and
// dispatch constructs a fresh Client per operation — so the caller that hit the 401
// still surfaces ErrCredentialsExpired, and the self-heal happens on the next
// dispatch, whose empty cache forces an exchange.
//
// A refresh-and-replay INSIDE the failing operation is deliberately not done, even
// when CanRefresh() is true. doRequest's retry rule (see the `idempotent` predicate)
// already forbids re-sending a plain create POST, because those endpoints carry no
// idempotency key and a rejected attempt may still have committed upstream; a 401
// says no more about whether the write landed than the 429 that rule was written
// for. Retrying only the idempotent methods would leave the create cascade — the
// path this ticket was opened for — unchanged, so it would buy the least where it
// is wanted most. One failed operation, then a clean one, is the cheaper trade.
func (c *Client) invalidateAccessToken() {
	c.tokenMu.Lock()
	c.accessToken = ""
	c.tokenExpiry = time.Time{}
	c.tokenMu.Unlock()
}
