// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"errors"
	"net/http"
)

// Graph error codes this client has to recognise by number, because Meta answers them with an
// HTTP status that does not say what they mean.
//
// Meta reports an expired, revoked or malformed access token as HTTP 400 with code 190, and a
// permission failure as 200 or 10 — statuses that read as "your request was malformed" at the
// HTTP layer. Classifying those by status alone would call a revoked token a defect in this
// service and page whoever owns the code instead of telling the operator to reauthorize. The
// rate-limit codes are already enumerated separately (graphRateLimitCodes) for the same
// underlying reason and are consulted by ProbeInconclusive rather than duplicated here.
// Meta's error_subcodes (460 session invalidated, 463 expired, …) are deliberately NOT listed:
// APIError does not carry a subcode field, and every subcode that matters here arrives under
// code 190 anyway.
const (
	graphCodeInvalidToken      = 190
	graphCodePermissionDenied  = 200
	graphCodeApplicationDenied = 10
)

// ProbeCredentialRejected reports whether err is Meta evaluating this connection's stored
// credential and REFUSING it: HTTP 401/403, or one of the Graph codes above that Meta returns
// under an HTTP 400.
//
// It is one half of the two-predicate probe vocabulary every platform client in this repo
// exposes; internal/dispatch/probe.go holds the shared rationale and is the only caller.
func ProbeCredentialRejected(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	if ae.StatusCode == http.StatusUnauthorized || ae.StatusCode == http.StatusForbidden {
		return true
	}
	// The STATUS gates the code, never the reverse — the same rule the token-refusal
	// classifiers follow (docs/knowledge/code/internal-platform-googleads.md). HTTP 400 is the
	// only status Meta uses to deliver these three as a verdict on the credential, and this
	// predicate is evaluated BEFORE ProbeInconclusive (internal/dispatch/probe.go's probeClass,
	// whose arm order is load-bearing). So without this gate a 429 or a 5xx that happens to
	// carry code 190 — a shed or failed request that evaluated nothing — was reported as a
	// CONFIRMED credential rejection, telling an operator to reauthorize a credential Meta
	// never looked at. That is the same false verdict the inconclusive arm exists to prevent,
	// arriving through the code field instead of the status.
	if ae.StatusCode != http.StatusBadRequest {
		return false
	}
	switch ae.Code {
	case graphCodeInvalidToken, graphCodePermissionDenied, graphCodeApplicationDenied:
		return true
	}
	return false
}

// ProbeInconclusive reports whether err left the probe genuinely unresolved — nothing was
// learned about the connection either way: a pre-send connection failure, a mid-flight
// transport failure, HTTP 429, any 5xx, or one of the rate-limit codes Meta delivers as an
// HTTP 400.
//
// EnvelopeUnreadable is inconclusive too, and for the reason that field exists: a non-2xx
// whose Graph envelope could not be read has no Code, and Meta reports rate limiting through
// Code far more often than through a 429. Treating an unread envelope as a clean rejection
// would let a shed request be reported as a confirmed verdict on the connection.
func ProbeInconclusive(err error) bool {
	if isPreSendDialError(err) {
		return true
	}
	var te *transportError
	if errors.As(err, &te) {
		return true
	}
	var ae *APIError
	if errors.As(err, &ae) {
		if ae.EnvelopeUnreadable {
			return true
		}
		if graphRateLimitCodes[ae.Code] {
			return true
		}
		return ae.StatusCode == http.StatusTooManyRequests || ae.StatusCode >= 500
	}
	// Not from the round trip at all — a malformed response this client refused to trust, or
	// one of its completeness guards. It proves nothing about the credential.
	return true
}
