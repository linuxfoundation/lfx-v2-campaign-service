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

// errConfiguredCustomerRejected marks Microsoft refusing an AccountsInfo/Query that carried the
// CONFIGURED customer id: the customer does not exist, or these credentials cannot reach it.
//
// It is unexported because no caller needs to name it — ProbeConfiguredCustomerRejected is the
// only reader, exactly as errTokenEndpointUnavailable is ProbeInconclusive's. It is attached in
// one place, markConfiguredCustomerRejection, which carries the reasoning for both the status
// gate and the provenance gate.
var errConfiguredCustomerRejected = errors.New("microsoft-ads: the configured customer id was refused")

// ProbeConfiguredCustomerRejected reports whether err is Microsoft accepting this connection's
// credential and then refusing the CUSTOMER it was asked to enumerate under.
//
// It is this package's departure from the three-predicate vocabulary, and it is the same shape
// of departure reddit.ProbeAccountUnreachable and twitter.ProbeAccountUnreachable are: a
// confirmed failure that neither standard predicate can state correctly. It is named for the
// customer rather than the account because that is the field it indicts. Those two answer for a
// 404 on the configured ACCOUNT, which this probe cannot produce — it enumerates and checks
// membership, so an account it cannot reach is simply absent from a list that was returned, and
// probeMembership already answers that. Microsoft's extra identity is the one ABOVE the account:
// customer_id scopes the enumeration itself, so a refusal of it is not the account being missing
// but the question being unaskable as configured.
//
// The two remedies it keeps apart are the two fields on the same connection row.
// ProbeCredentialRejected would send the operator to re-authorise a credential Microsoft had
// just honoured; probeMembership's accountNotReachable would send them to repoint an account id
// that may be perfectly correct. Only this one names customer_id.
//
// apiError is unexported, so this classification cannot be made by the dispatcher; like every
// other member of the vocabulary it has to be answered by the package that owns the type.
// internal/dispatch/probe.go holds the shared rationale and is the only caller.
func ProbeConfiguredCustomerRejected(err error) bool {
	return errors.Is(err, errConfiguredCustomerRejected)
}

// ConfiguredCustomerRejectionCodes returns the machine-readable Microsoft error codes carried by
// a rejection ProbeConfiguredCustomerRejected just claimed, or nil.
//
// It exists because that predicate gates on the STATUS and the id's provenance and reads no code
// at all — deliberately, since the Customer Management codes for a missing or unreachable
// customer are pinned by nothing in this repo and by no test against the live API, and a guessed
// literal that never matched would restore the paging 500 while looking handled. The cost of not
// reading them is that a 400 arriving for some OTHER reason — a moved contract, an
// operation-level validation this build has not met — is answered as "your customer_id is
// unreachable", and nothing anywhere records that it might not have been.
//
// This is what closes that: the dispatcher logs these codes whenever it renders the verdict, so
// the first real occurrence in any environment leaves behind exactly the evidence an allowlist
// would need. Until someone has that evidence, the honest classification is the one the status
// and the provenance actually support.
//
// It returns a COPY. ErrorCodes is bounded at parse time (maxRetainedErrorCodes,
// maxErrorCodeLen) and holds no upstream body text — apiError drops the raw body after
// extraction precisely so this material is safe to carry — but handing out the slice itself
// would let a caller alias the error's own state.
func ConfiguredCustomerRejectionCodes(err error) []string {
	if !errors.Is(err, errConfiguredCustomerRejected) {
		return nil
	}
	var ae *apiError
	if !errors.As(err, &ae) || len(ae.ErrorCodes) == 0 {
		return nil
	}
	codes := make([]string, len(ae.ErrorCodes))
	copy(codes, ae.ErrorCodes)
	return codes
}

// ProbeCredentialRejected reports whether err is Microsoft evaluating this connection's stored
// credential and REFUSING it: a token refresh Microsoft itself turned down, or an Ads API call
// answered 401/403.
//
// A 400 is deliberately NOT here, and neither is it inconclusive. One kind of 400 says
// something this predicate cannot — the credential was accepted and the configured CUSTOMER was
// refused — and it has its own predicate, ProbeConfiguredCustomerRejected, which the dispatcher
// asks BEFORE probeClass. Claiming it here would render "microsoft ads rejected the stored
// credential" and send an operator to re-authorise a credential Microsoft had just honoured,
// which is the same misreading that predicate's siblings on Reddit and X exist to prevent.
//
// It is one half of the two-predicate probe vocabulary every platform client in this repo
// exposes; internal/dispatch/probe.go holds the shared rationale for the split and is the only
// caller. "Neither predicate" is the third outcome and is deliberately not a predicate of its
// own — it means Microsoft refused the request THIS SERVICE built, which is our defect rather
// than a verdict on the operator's connection. Every 4xx that is not 401/403, not a 408 or 429,
// and not a 400 the configured customer id provoked still lands there, including a 400 raised
// about a customer this client discovered from User/Query itself.
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
// Unlike its siblings this predicate does NOT consult isPreSendDialError, and the reason is
// specific to ONE of this client's two legs. The REST pre-send arm renders the cause through
// safeCause into a plain string rather than wrapping it with %w — deliberately, so a custom
// RoundTripper's error text can never reach a persisted campaign step — so the dial classifier
// cannot see through that error and calling it for that leg would assert a match that can never
// happen. Such an error falls to the default below, which is inconclusive anyway, so the
// classification is unchanged; only the claim would have been false. The explicit
// errRequestNotSent marker carries that leg instead, which is why the marker had to exist here
// and nowhere else.
//
// The TOKEN leg is the opposite and must not be read into the paragraph above:
// tokenTransportError renders only safeCause but its Unwrap DOES preserve the cause, so the dial
// classifier sees through it perfectly well. This predicate needs nothing extra for that — the
// tokenTransportError arm below already answers true — but ProbeNotSent does, and the two legs
// are why it consults both markers rather than one.
func ProbeInconclusive(err error) bool {
	// ErrTokenRequestRejected is the one error this package recognises that is NEITHER
	// predicate, and it has to say so EXPLICITLY, because the fall-through at the bottom of
	// this function answers true for anything it does not recognise. Left to that default, a
	// token request only this service composes would be answered INCONCLUSIVE: the operator
	// gets an advisory naming a platform that could not be reached, on a connection no amount
	// of waiting repairs, and nobody is told this service composed a request the token endpoint
	// refused. The ok field does not move — an inconclusive probe answers false too — so the
	// whole cost lands on the status axis and on who gets sent to fix what.
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
		// 408 sits with 429 and 5xx rather than with the refusals, for the reason this
		// package's own token path already gives: a 408 means the endpoint — or an
		// intermediary in front of it — gave up waiting for the request, so nothing evaluated
		// the credential and the same call can succeed on a retry. Both of those are what a
		// refusal promises the opposite of.
		//
		// Only the TOKEN leg carried that reading. An account-read 408 arrives as an apiError
		// and matched neither predicate, so it fell through probeClass's default arm and became
		// a typed 500 that pages us — for a timeout, which the probe contract calls inconclusive.
		return ae.StatusCode == http.StatusTooManyRequests ||
			ae.StatusCode == http.StatusRequestTimeout ||
			ae.StatusCode >= 500
	}
	// Not from the round trip at all — a malformed response this client refused to trust, or
	// one of the completeness guards ListAdAccounts documents ("an incomplete answer is an
	// ERROR, never a short list"). It proves nothing about the credential.
	return true
}

// ProbeNotSent reports whether err PROVES the FAILING request never left this process, so the outcome
// belongs to no platform at all.
//
// It is the third member of the probe vocabulary, and unlike the other two it is not about the
// operator's answer: an inconclusive probe reads the same either way. It exists for the metrics
// arm alone — see internal/dispatch/probe.go and domain.ErrConnectionProbeNotAttempted — so an
// unresolvable host or a refused connection does not land on this platform's upstream-call
// series as a near-zero-latency error sample.
//
// It defaults FALSE for anything it does not recognise, the opposite of ProbeInconclusive's
// default and for the same reason that one defaults true: the safe direction here is to record
// a sample for a call that may have happened, not to silently drop a real platform failure.
//
// It reads BOTH markers because this client reaches the network on two legs and they fail
// differently. errRequestNotSent covers the REST leg, whose pre-send arm flattens the cause to a
// string the dial classifier cannot see through. isPreSendDialError covers the TOKEN leg: a
// fresh probe refreshes before it reads, and a DNS failure or refused connection there comes
// back as a tokenTransportError whose Unwrap preserves the dial error but which carries no
// errRequestNotSent — that marker is attached by the REST path alone. Reading only the marker
// therefore charged Microsoft an upstream-call sample for a probe that never left this process,
// which is precisely what this predicate exists to prevent. isPreSendDialError matches only
// DNS failures and dial-op ECONNREFUSED/EHOSTUNREACH/ENETUNREACH, so a TLS handshake failure —
// a real conversation with a real host — stays off it and remains the platform's sample.
//
// "The failing request", not "no bytes at all": on a two-leg probe a token refresh may have
// SUCCEEDED before the account read failed to dial, and this still answers true. See
// domain.ErrConnectionProbeNotAttempted, which explains why that is the cheap direction — the
// suppressed sample is an ERROR sample, so recording it would charge this deployment's own DNS
// fault to the provider.
//
// It also reads errTokenContextAlreadyDone. That marker is attached at exactly one place — the
// entry check of token acquisition, where the caller's context was ALREADY done before this
// client built, dialled or sent anything — so it proves the same thing a dial refusal proves,
// by a different route. A BARE context error stays unclaimed: cancellation that lands after
// transmission is indistinguishable from cancellation before it, and only the entry check knows
// which side of the wire it is on.
func ProbeNotSent(err error) bool {
	return errors.Is(err, errRequestNotSent) || isPreSendDialError(err) ||
		errors.Is(err, errTokenContextAlreadyDone)
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
	// The STATUS gates the body, never the other way round. A 3xx, 404 or 405 is not a status
	// any token endpoint answers a well-formed refresh with, so nothing arriving alongside one
	// is a verdict on the credential — and an OAuth-shaped body CAN arrive with one, from a
	// proxy, a gateway error page, or whatever now answers at the moved address. Reading the
	// body first let a 404 carrying {"error":"invalid_grant"} report a credential the platform
	// never evaluated as refused, which is the exact misclassification this function exists to
	// remove.
	switch statusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
		// The three statuses a token endpoint genuinely uses to refuse. Only here is the body
		// worth reading, and only for its allowlisted code.
	default:
		return ErrTokenRequestRejected
	}
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
	return ErrCredentialRejected
}
