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

// ErrApplicationCredentialsInvalid marks a token exchange LinkedIn refused with
// `invalid_client`: the stored client_id/client_secret pair is wrong, unknown to
// LinkedIn, or the app was deleted. It is a PERMANENT, operator-actionable fault.
//
// It is deliberately NOT ErrCredentialsExpired. That sentinel resolves to "the member
// must re-authorize", and no member re-authorization repairs an APPLICATION credential —
// the stored refresh token was never the problem. Sending that remedy is worse than
// sending none, because it is actionable and wrong.
//
// It needs its own sentinel rather than only a message, for the same reason every other
// reason token in this repo does: the arms that classify it match STRUCTURALLY. A bare
// fmt.Errorf here unwrapped to nothing, so it picked up neither this reason nor
// domain.ErrConnectionNotUsable and fell through to the generic retryable arm — a 503
// telling a caller to retry a condition that cannot clear until someone edits the
// connection. A typo in the LF system client_id would disable LinkedIn for every project
// falling back to it, on that same opaque retry surface.
var ErrApplicationCredentialsInvalid = errors.New("linkedin application credentials rejected")

// applicationCredentialsError names the connection whose APPLICATION credentials
// LinkedIn rejected. Like credentialsExpiredError it carries no token material — only the
// operator-set connection label, which is metadata and never secret.
//
// It carries no Method/StatusCode: this arm is reachable only from the token exchange,
// which happens BEFORE the request it would authorize, so nothing was ever sent and no
// outcome can be ambiguous. createOutcomeAmbiguous therefore reports false for it with no
// special case, exactly as it does for the pre-send expiry arms.
type applicationCredentialsError struct {
	Connection string
	// StatusCode is the status LinkedIn answered the TOKEN EXCHANGE with (400 or 401).
	// It is recorded for classification and deliberately not rendered.
	StatusCode int
}

func (e *applicationCredentialsError) Error() string {
	return fmt.Sprintf("linkedin: %s was refused by LinkedIn: the stored application credentials "+
		"(client_id/client_secret) are wrong or unknown to LinkedIn — correct the connection's "+
		"application credentials; re-authorizing the member will not help", e.Connection)
}

func (e *applicationCredentialsError) Unwrap() error { return ErrApplicationCredentialsInvalid }

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
	//
	// UNREACHABLE IN PRODUCTION TODAY — kept deliberately, and NOT a cache that exists.
	// Nothing persists an access-token expiry: design/connection.go's
	// linkedin-ads-credentials type declares access_token, refresh_token, client_id and
	// client_secret and NO expiry attribute, so the encrypted blob never carries one.
	// internal/dispatch/linkedin.go's linkedinCreds does declare AccessTokenExpiresAt and
	// copies it into Credentials, but the JSON it decodes can never contain the key, so
	// the value is always the zero time and this guard's !IsZero() test always fails.
	// The bootstrap installer (internal/bootstrap/sysacct.go) writes no expiry either.
	//
	// The consequence is real and worth stating: EVERY refresh-capable client performs a
	// token exchange on its first request, because this is the only branch that could
	// have reused an injected token. internal/dispatch/linkedin.go builds a fresh client
	// per operation, so a brief-level fan-out is one OAuth exchange per campaign.
	//
	// Retained rather than deleted because it is the correct behaviour the moment an
	// expiry IS persisted, and deleting it would read as a decision that reuse is wrong.
	// Persisting the expiry (and the rotated refresh token) is a schema and behaviour
	// change; it is deliberately NOT made here.
	//
	// One coupling for whoever makes this branch live: invalidateAccessToken clears only
	// the CACHE (c.accessToken/c.tokenExpiry) and not c.creds, which is sound ONLY while
	// this branch is dead. Once an injected expiry is real, a 401 would invalidate the
	// cache and this branch would immediately re-serve the SAME rejected token from
	// c.creds. Making the expiry live therefore requires invalidateAccessToken to
	// suppress the injected token too.
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
	// goroutines. ClientID/ClientSecret are immutable after construction, so they are read
	// without the lock below.
	c.tokenMu.Lock()
	refreshToken := c.refreshToken
	c.tokenMu.Unlock()

	// Trimmed to match what CanRefresh() gated on. CanRefresh() tests the TRIMMED value,
	// so a padded " id " passes every validator; sending it RAW then fails at LinkedIn as
	// invalid_client on every exchange, forever, with no signal that padding is the cause.
	// c.refreshToken is already trimmed at construction (NewClient), so these two were the
	// only raw reads left on the send path.
	//
	// The AUTHORITATIVE fix is at the write boundaries, which now REFUSE padding rather
	// than storing it (validateLinkedInRefreshCredentials in internal/service/connection.go
	// and validateConditionalGroups in internal/bootstrap/sysacct.go) — a stored padded
	// value would otherwise stay wrong for every future reader. This is defence in depth
	// for rows written BEFORE those validators existed, which no reconnect has rewritten.
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", strings.TrimSpace(c.creds.ClientID))
	form.Set("client_secret", strings.TrimSpace(c.creds.ClientSecret))

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
		// Status alone does not identify the fault. An OAuth2 token endpoint answers 400
		// (and LinkedIn sometimes 401) for BOTH a dead refresh token and a wrong client
		// credential, and the two have OPPOSITE remedies. Classifying every 400/401 as
		// "expired" told an operator whose client_id held a typo to "re-authorize the
		// connection" — a member re-authorization that cannot possibly help, because the
		// stored refresh token was never the problem. The RFC 6749 §5.2 `error` code is
		// the only thing that separates them, so it is parsed.
		//
		// Only the CLASSIFICATION is carried, never the upstream text. This request's body
		// held client_secret and the refresh token, an OAuth/proxy diagnostic body is
		// untrusted and may reflect that material, and this error can be persisted into a
		// campaign's Steps, so a leak would be durable. `error_description` is deliberately
		// ignored; the parsed `error` code is matched against a fixed local allow-list and
		// never rendered, so no upstream byte reaches the message on any arm.
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized {
			if _, appFault := oauthAppFaultCodes[oauthErrorCode(buf.Bytes())]; appFault {
				// Operator/implementation fault: the client_id/client_secret pair is wrong,
				// unknown to LinkedIn or the app was deleted (invalid_client); the app is
				// not authorized for this grant type (unauthorized_client); or the request
				// itself is malformed, uses an unsupported grant, or asks for an invalid
				// scope. Deliberately NOT an ErrCredentialsExpired — that resolves to
				// "re-authorize", and no member re-authorization repairs any of these.
				// Whoever configured the connection, or this client, must correct it.
				//
				// It is returned as a TYPED error, not a bare fmt.Errorf. Every arm that
				// acts on this classification matches structurally (errors.Is), so an
				// error unwrapping to nothing is invisible to all of them: it would carry
				// neither this reason nor domain.ErrConnectionNotUsable and would fall
				// through to the generic retryable arm — the opaque 503 this split exists
				// to retire. Only the classification travels; no upstream byte is rendered.
				return "", &applicationCredentialsError{
					Connection: c.creds.ConnectionLabel(),
					StatusCode: resp.StatusCode,
				}
			}
			// `invalid_grant` — the ONE §5.2 code that describes a dead grant — plus the
			// deliberate fallback for a body this client cannot read.
			//
			// LinkedIn documents expired, revoked and invalid refresh tokens under that same
			// code, and all three are repaired the same way: the member re-authorizes.
			//
			// The FALLBACK is the deliberate part. An absent, non-JSON, or unrecognised code
			// lands here rather than on the app-fault arm, and that asymmetry is chosen: this
			// arm's remedy (re-authorize) is recoverable if wrong — the member repeats an
			// authorization and the real fault resurfaces unchanged — whereas telling an
			// operator their app credentials are broken when the token merely expired sends
			// them auditing a correct configuration. On no evidence, prefer the remedy whose
			// failure is self-correcting. Note this is the OPPOSITE default from before the
			// split, when every unclassified 400/401 landed here by construction rather than
			// by choice.
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

// RFC 6749 §5.2 defines exactly six token-endpoint `error` codes, and they split cleanly by
// REMEDY — which is the only thing this client needs from them. All six are handled, rather
// than special-casing one at a time, because the codes are a CLOSED set: enumerating them once
// means a body carrying any of them is classified, and only a body carrying something outside
// the RFC (or nothing readable) reaches the fallback.
//
// Exactly one of the six describes a dead GRANT — the case a member re-authorization repairs:
//
//	invalid_grant: "The provided authorization grant ... or refresh token is invalid, expired,
//	revoked, does not match the redirection URI ..., or was issued to another client."
//
// The other five describe the CLIENT or the REQUEST, and no member re-authorization repairs
// any of them:
//
//	invalid_client        — client authentication failed: unknown client_id, wrong
//	                        client_secret, or a deleted app.
//	invalid_request       — the request is malformed, missing a required parameter, or
//	                        repeats one. A defect in what THIS CLIENT sent.
//	unauthorized_client   — the client is not authorized to use this grant type; on LinkedIn
//	                        this is the app lacking refresh-token grant (a Marketing Developer
//	                        Platform approval), not a token that aged out.
//	unsupported_grant_type— the server does not support this grant type at all.
//	invalid_scope         — the requested scope is invalid, unknown or malformed.
//
// Sending "re-authorize the connection" for any of those five is worse than sending nothing:
// it is actionable, and it provably cannot work — the member repeats an authorization whose
// result was never the problem, and the failure recurs unchanged. They are application- and
// implementation-level faults, so they take ErrApplicationCredentialsInvalid, whose remedy is
// "an operator must correct the connection/app". That sentinel is slightly wider than its name
// (it also covers a malformed request or an unsupported grant), which is deliberate: the
// remedy is identical for all five, and it is the remedy — not the taxonomy — that the caller
// acts on. Splitting them further would add sentinels no call site could treat differently.
const (
	oauthErrorInvalidClient      = "invalid_client"
	oauthErrorInvalidGrant       = "invalid_grant"
	oauthErrorInvalidRequest     = "invalid_request"
	oauthErrorUnauthorizedClient = "unauthorized_client"
	oauthErrorUnsupportedGrant   = "unsupported_grant_type"
	oauthErrorInvalidScope       = "invalid_scope"
)

// oauthAppFaultCodes are the RFC 6749 §5.2 codes that describe the CLIENT or the REQUEST
// rather than a dead grant. Every one of them is permanent until an operator changes the
// connection or this client's request, and none is repaired by a member re-authorization.
var oauthAppFaultCodes = map[string]struct{}{
	oauthErrorInvalidClient:      {},
	oauthErrorInvalidRequest:     {},
	oauthErrorUnauthorizedClient: {},
	oauthErrorUnsupportedGrant:   {},
	oauthErrorInvalidScope:       {},
}

// oauthErrorCode extracts the RFC 6749 §5.2 `error` code from a token-endpoint error
// body, returning "" when the body is absent, not JSON, or carries no string `error`.
//
// The returned value is only ever COMPARED against a local constant, never rendered:
// the body of a token-endpoint response is untrusted and this request carried
// client_secret and the refresh token. Returning the raw code keeps the parse honest at
// one site while every caller is a fixed equality test, so no upstream text can reach a
// message or a persisted campaign Step. `error_description` is deliberately not read —
// it is free-form upstream prose and exactly the thing that must not be echoed.
func oauthErrorCode(body []byte) string {
	var parsed struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ""
	}
	return parsed.Error
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

// expiredCredentialsError is the SINGLE construction site for a response-arm 401. It
// returns nil when the status is not a token expiry, so a caller can use it as a guard
// ahead of its generic apiError return.
//
// It exists because the 401 arms are reached from THREE different exit paths in
// doRequest — a readable non-2xx body, a body-read failure, and an over-cap body — and
// each must produce the SAME error. Before this helper only the readable-body path
// classified 401s, so a 401 whose body could not be read (a mid-flight connection reset
// after the status line, or a body over maxResponseBytes) fell through to a bare
// *apiError. That cost two distinct things:
//
//   - the cached access token was NOT invalidated, so a token LinkedIn had already
//     rejected survived in cache and was replayed by the next caller; and
//   - createOutcomeAmbiguous saw an *apiError whose status list covers only 3xx/429/5xx,
//     so a mutating POST answered 401 classified as a DEFINITE failure. The create
//     cascade then returned a nil result, the dispatcher released the claim, and a retry
//     could duplicate a campaign group LinkedIn may already have committed and be billing
//     — the precise harm the 401 ambiguity fix was written to prevent.
//
// The body is OPTIONAL: pass "" from the arms that never obtained one. An absent body
// costs nothing, because isTokenExpiryResponse uses the serviceErrorCode only as a
// positive signal and already treats an unparseable body as an expiry — the 401 status
// alone is the operative signal. Method is carried so createOutcomeAmbiguous's method
// gate still holds: a GET 401 and every pre-send expiry (Method == "") stay DEFINITE.
func (c *Client) expiredCredentialsError(statusCode int, body, method string) *credentialsExpiredError {
	if !isTokenExpiryResponse(statusCode, body) {
		return nil
	}
	// Invalidate the cached token so a later call re-exchanges rather than replaying the
	// rejected one. Only the CACHE is cleared — a revoked access token does not imply a
	// revoked refresh token.
	c.invalidateAccessToken()
	return &credentialsExpiredError{
		Connection: c.creds.ConnectionLabel(),
		Reason:     "LinkedIn rejected the access token (HTTP 401: expired, revoked or invalid)",
		Method:     method,
		StatusCode: statusCode,
	}
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
