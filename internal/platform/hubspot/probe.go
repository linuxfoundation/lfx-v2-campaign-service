// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"errors"
	"net/http"
)

// ProbeCredentialRejected reports whether err is HubSpot evaluating this connection's stored
// credential and REFUSING it: HTTP 401 or 403.
//
// There is no token-refresh arm here, and its absence is a property of the credential rather
// than an omission. HubSpot private apps authenticate with a long-lived token and this client
// runs no OAuth exchange, so the whole class of "the refresh was refused" — the arm that
// carries the headline failure on Google, Microsoft and Reddit — cannot occur. A revoked or
// rotated private-app token surfaces as a 401 on the probe call itself.
//
// 403 is a rejection rather than a service defect even though it can mean a missing scope. A
// private app's scopes are chosen when the token is issued, so a scope the probe needs and the
// token lacks is a fact about the stored credential that the operator fixes by reissuing it —
// exactly what a failed connection test should say.
//
// It is one half of the two-predicate probe vocabulary every platform client in this repo
// exposes; internal/dispatch/probe.go holds the shared rationale and is the only caller.
func ProbeCredentialRejected(err error) bool {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.StatusCode == http.StatusUnauthorized || ae.StatusCode == http.StatusForbidden
	}
	return false
}

// ProbeInconclusive reports whether err left the probe genuinely unresolved — nothing was
// learned about the connection either way: a pre-send failure, a mid-flight transport failure,
// HTTP 429, or any 5xx.
//
// preSendError is this client's own definite-never-sent type (see IsNeverSent). "Definite"
// there is a claim about whether a mutation was applied, not about the credential: a request
// that never left the process taught us nothing about the token, so it is inconclusive here.
func ProbeInconclusive(err error) bool {
	var pse *preSendError
	if errors.As(err, &pse) {
		return true
	}
	if isPreSendDialError(err) {
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
	// Not from the round trip at all — AuthenticatedPortalID's own guards land here: a body
	// that is not a token-info response, or one carrying no usable hubId. Both are a
	// successful call that established nothing, which is the definition of inconclusive.
	return true
}

// ProbeNotSent reports whether err PROVES nothing left this process — this client's own
// preSendError (a DNS or dial failure, a request that would not build, or a context already
// cancelled when the call began), or a bare dial failure — so the probe's outcome belongs to no
// platform at all.
//
// It is the third member of the probe vocabulary and the only one that is not about the
// operator's answer: an inconclusive probe reads identically either way. It exists for the
// metrics arm alone — see internal/dispatch/probe.go and domain.ErrConnectionProbeNotAttempted
// — so an unresolvable host does not land on HubSpot's upstream-call series as a near-zero-
// latency error sample and invent a provider outage out of a local network fault.
//
// It is IsNeverSent asked for the one purpose that predicate was already written to answer;
// the wrapper exists so every platform package presents the same probe vocabulary under the
// same name, rather than internal/dispatch carrying a per-platform exception.
//
// It defaults FALSE for anything it does not recognise, the OPPOSITE of ProbeInconclusive's
// default and for the same reason that one defaults true: each defaults to the answer that is
// wrong in the cheap direction. Here that is recording a sample for a call that may have
// happened, rather than silently dropping a real platform failure out of the series.
func ProbeNotSent(err error) bool {
	return IsNeverSent(err) || isPreSendDialError(err)
}
