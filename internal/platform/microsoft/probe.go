// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"encoding/json"
	"errors"
	"net/http"
)

// ErrCredentialRejected marks Microsoft EVALUATING this connection's stored credential and
// REFUSING it — a revoked or expired refresh token, or application credentials that no longer
// match. It is a fact about the stored credential, decided by Microsoft, and permanent: retrying
// re-sends the same refusal. The remedy is the operator's, and it is the one class this probe
// reports as a confirmed failed test.
//
// It is exported because the connection-test path has to tell it apart from every other way a
// token refresh can fail, and cannot do that from the error text — which deliberately carries
// the status and nothing else, because that request body holds the client secret and the
// refresh token. See ProbeCredentialRejected.
var ErrCredentialRejected = errors.New("microsoft-ads: the token endpoint refused the stored credential")

// ErrTokenRequestRejected marks the token endpoint refusing the SHAPE of the request this
// service built, rather than the credential it carried: RFC 6749 §5.2's invalid_request,
// unsupported_grant_type and invalid_scope, plus a status no OAuth token endpoint answers a
// well-formed refresh with at all (a 3xx, since redirects are not followed; a 404 or 405, which
// mean the endpoint moved).
//
// It matches NEITHER probe predicate on purpose, so probeClass routes it to
// domain.ErrServiceDefect — a typed 500 that pages us — which is the remedy that fits: nobody
// re-authorising anything can repair a request only this service composes.
//
// The name and meaning are now the ones domain.ErrTokenRequestRejected and
// linkedin.ErrTokenRequestRejected already carried ("this is a service defect, file a bug").
// This package used the SAME NAME for the opposite meaning until LFXV2-2665: every non-429
// sub-500 status became a credential verdict, so a moved endpoint or a malformed request told
// the operator Microsoft had rejected their credential and sent them to replace a working one.
var ErrTokenRequestRejected = errors.New("microsoft-ads: the token endpoint refused the request this service built")

// errTokenEndpointUnavailable is its opposite and is unexported because no caller needs to
// name it: a 5xx from the token endpoint means Microsoft could not answer, so nothing was
// learned about the credential. ProbeInconclusive is the only reader.
var errTokenEndpointUnavailable = errors.New("microsoft-ads: the token endpoint is unavailable")

// ProbeCredentialRejected reports whether err is Microsoft evaluating this connection's stored
// credential and REFUSING it: a token refresh Microsoft itself turned down, or an Ads API call
// answered 401/403.
//
// It is one half of the two-predicate probe vocabulary every platform client in this repo
// exposes; internal/dispatch/probe.go holds the shared rationale for the split and is the only
// caller. "Neither predicate" is the third outcome and is deliberately not a predicate of its
// own — it means Microsoft refused the request THIS SERVICE built (any other 4xx), which is
// our defect rather than a verdict on the operator's connection.
func ProbeCredentialRejected(err error) bool {
	if errors.Is(err, ErrCredentialRejected) {
		return true
	}
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.StatusCode == http.StatusUnauthorized || ae.StatusCode == http.StatusForbidden
	}
	return false
}

// ProbeInconclusive reports whether err left the probe genuinely unresolved — nothing was
// learned about the connection either way: the token endpoint being unavailable, a mid-flight
// transport failure, a token-exchange round-trip failure, HTTP 429, or any 5xx.
//
// Unlike its siblings this predicate does NOT consult isPreSendDialError. This client's
// pre-send arm renders the cause through safeCause into a plain string rather than wrapping it
// with %w — deliberately, so a custom RoundTripper's error text can never reach a persisted
// campaign step — so the dial classifier cannot see through the returned error and calling it
// here would assert a match that can never happen. Such an error falls to the default below,
// which is inconclusive anyway, so the classification is unchanged; only the claim would have
// been false.
func ProbeInconclusive(err error) bool {
	// ErrTokenRequestRejected is the one error this package recognises that is NEITHER
	// predicate, and it has to say so EXPLICITLY, because the fall-through at the bottom of
	// this function answers true for anything it does not recognise. Left to that default, a
	// token request only this service composes would report the connection as OK with an
	// advisory — unproven reported as healthy, the exact shape LFXV2-2665 exists to remove.
	// Answering false lets probeClass fall to its default arm and raise domain.ErrServiceDefect,
	// which pages us. dispatch/linkedin.go routes linkedin.ErrTokenRequestRejected the same way.
	if errors.Is(err, ErrTokenRequestRejected) {
		return false
	}
	if errors.Is(err, errTokenEndpointUnavailable) {
		return true
	}
	var tte *tokenTransportError
	if errors.As(err, &tte) {
		return true
	}
	var te *transportError
	if errors.As(err, &te) {
		return true
	}
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.StatusCode == http.StatusTooManyRequests || ae.StatusCode >= 500
	}
	// Not from the round trip at all — a malformed response this client refused to trust, or
	// one of the completeness guards ListAdAccounts documents ("an incomplete answer is an
	// ERROR, never a short list"). It proves nothing about the credential.
	return true
}

// RFC 6749 §5.2 defines exactly six token-endpoint `error` codes, and they split by REMEDY —
// the only thing this classification needs from them. The set is CLOSED, so enumerating it once
// means any body carrying one of the six is classified and only something outside the RFC
// reaches the fallback. linkedin/token.go carries the full reasoning; this is the same split,
// duplicated rather than imported because each platform package owns its own error vocabulary
// and the two sentinel sets are not interchangeable.
const (
	oauthErrorInvalidClient      = "invalid_client"
	oauthErrorInvalidGrant       = "invalid_grant"
	oauthErrorInvalidRequest     = "invalid_request"
	oauthErrorUnauthorizedClient = "unauthorized_client"
	oauthErrorUnsupportedGrant   = "unsupported_grant_type"
	oauthErrorInvalidScope       = "invalid_scope"
)

// classifyTokenRefusal picks the sentinel for a non-2xx token-endpoint response that is neither
// a 5xx nor a 429 (both of which are decided before this is called, and mean nothing was
// learned).
//
// body is the bounded response already read by the caller. Only the allowlisted `error` CODE is
// ever read out of it, and the code is compared against — never rendered — because that request
// carried the client secret and the refresh token and an OAuth or proxy diagnostic body may
// reflect them.
//
// The fallback is deliberately CONSERVATIVE. An unrecognised body on a 400/401/403 stays a
// credential verdict, which is what this package has always reported and what such a status
// means in practice; promoting it to a service defect would turn the ordinary revoked-token
// case — the failure this whole endpoint exists to catch — into a 500 that pages us instead of
// an answer for the operator. Only a status no token endpoint answers a well-formed refresh
// with (3xx, 404, 405, anything else) is reclassified on status alone.
func classifyTokenRefusal(statusCode int, body []byte) error {
	var envelope struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil {
		switch envelope.Error {
		case oauthErrorInvalidGrant, oauthErrorInvalidClient, oauthErrorUnauthorizedClient:
			return ErrCredentialRejected
		case oauthErrorInvalidRequest, oauthErrorUnsupportedGrant, oauthErrorInvalidScope:
			return ErrTokenRequestRejected
		}
	}
	switch statusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
		return ErrCredentialRejected
	default:
		return ErrTokenRequestRejected
	}
}
