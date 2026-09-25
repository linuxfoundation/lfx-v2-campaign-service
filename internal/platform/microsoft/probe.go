// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"errors"
	"net/http"
)

// ErrTokenRequestRejected marks Microsoft's token endpoint REFUSING the refresh exchange on
// the merits — a 4xx, which in practice means the refresh token has been revoked or expired,
// or the application credentials it was issued against no longer match. It is a fact about the
// stored credential, decided by Microsoft, and permanent: retrying re-sends the same refusal.
//
// It is exported because the connection-test path has to tell it apart from every other way a
// token refresh can fail, and cannot do that from the error text — which deliberately carries
// the status and nothing else, because that request body holds the client secret and the
// refresh token. See ProbeCredentialRejected.
var ErrTokenRequestRejected = errors.New("microsoft-ads: the token endpoint refused the refresh request")

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
	if errors.Is(err, ErrTokenRequestRejected) {
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
