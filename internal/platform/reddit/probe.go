// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// ErrCredentialRejected marks Reddit EVALUATING this connection's stored credential and
// REFUSING it — a revoked or expired refresh token, or application credentials that no longer
// match. It is a fact about the stored credential, decided by Reddit, and permanent: retrying
// re-sends the same refusal. The remedy is the operator's, and it is the one class this probe
// reports as a confirmed failed test.
//
// It is exported because the connection-test path has to tell it apart from every other way a
// token refresh can fail, and cannot do that from the error text — which deliberately carries
// the status and nothing else, because that request body holds the client secret and the
// refresh token. See ProbeCredentialRejected.
var ErrCredentialRejected = errors.New("reddit: the token endpoint refused the stored credential")

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
// the operator Reddit had rejected their credential and sent them to replace a working one.
var ErrTokenRequestRejected = errors.New("reddit: the token endpoint refused the request this service built")

// errTokenEndpointUnavailable is its opposite and is unexported because no caller needs to
// name it: a 5xx from the token endpoint means Reddit could not answer, so nothing was learned
// about the credential. ProbeInconclusive is the only reader.
var errTokenEndpointUnavailable = errors.New("reddit: the token endpoint is unavailable")

// VerifyAccount reads the ad account this client is configured for, and reports whether the
// stored credential can reach it. It creates nothing and changes nothing.
//
// It is the READ CreateCampaign already makes as its Step 1 account verification
// (GET /ad_accounts/{id}), lifted into an exported method so the connection-test path can make
// it on its own. The two uses differ in exactly one way, and the difference is deliberate:
// there a failure is a non-fatal warning step, because a campaign create should not be refused
// over a verification call, whereas here the call IS the test and its failure is the answer.
//
// Reddit is the one platform in this repo where the probe addresses the configured account
// directly rather than enumerating and checking membership. That makes it the strongest form
// of the check available — it proves reachability of the account this connection will actually
// dispatch to, not merely that some account is reachable — and it is why Reddit's connection
// test needs no account enumeration to exist first.
func (c *Client) VerifyAccount(ctx context.Context) error {
	// Validated before the path is built, for the reason accountIDRe exists: the id is
	// concatenated into the request path, so a value carrying a slash would inject extra
	// segments. CreateCampaign applies the identical guard at its own top.
	accountID := strings.TrimSpace(c.account.AccountID)
	if accountID == "" {
		return fmt.Errorf("%w: reddit ad account id is required", ErrInvalidAccountID)
	}
	if !accountIDRe.MatchString(accountID) {
		return fmt.Errorf("%w: invalid reddit account id %q", ErrInvalidAccountID, accountID)
	}
	if _, err := c.request(ctx, http.MethodGet, "/ad_accounts/"+accountID, nil); err != nil {
		return err
	}
	return nil
}

// ProbeCredentialRejected reports whether err is Reddit evaluating this connection's stored
// credential and REFUSING it: a token refresh Reddit itself turned down, or an Ads API call
// answered 401/403.
//
// A 404 is deliberately NOT here, even though it is a confirmed failure. It says something
// this predicate cannot: the credential was accepted and the ACCOUNT was not found. Answering
// it as a rejection renders "reddit ads rejected the stored credential", which sends an
// operator to re-authorise a connection whose credential Reddit just honoured. It has its own
// predicate, ProbeAccountUnreachable, and its own verdict — see probeSubject.accountNotReachable.
//
// ErrInvalidAccountID is deliberately NOT here. VerifyAccount raises it before anything is
// sent, from this client's own guard on the configured id — Reddit never saw the credential, so
// answering "the platform rejected your credential" would send an operator to re-authorise a
// connection whose credential is fine and whose account id is the only broken part. The
// dispatcher settles that case itself, next to its ErrAccountNotSelected arm.
//
// It is one half of the two-predicate probe vocabulary every platform client in this repo
// exposes; internal/dispatch/probe.go holds the shared rationale and is the only caller.
func ProbeCredentialRejected(err error) bool {
	if errors.Is(err, ErrCredentialRejected) {
		return true
	}
	var ae *apiError
	if errors.As(err, &ae) {
		switch ae.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return true
		}
	}
	return false
}

// ProbeAccountUnreachable reports whether err is Reddit accepting this connection's credential
// and then failing to find the account it was asked about: HTTP 404 on the account read.
//
// It exists on this client and on X's, and on no other, because only these two probes name the
// configured account IN the request path. Every other client enumerates and checks membership,
// so a 404 there could only mean the endpoint moved — our defect, and correctly inconclusive.
// Here the 404 IS the answer to the question asked, and it is a different answer from the one
// ProbeCredentialRejected gives: the token worked, the account id is the broken half. That
// distinction is the whole point of splitting it out — the two produce the same OK: false, but
// only one of them tells the operator to re-authorise, and it would be the wrong one.
//
// apiError is unexported, so this classification cannot be made by the dispatcher; like both
// halves of the standard vocabulary it has to be answered by the package that owns the type.
// internal/dispatch/probe.go holds the shared rationale and is the only caller.
func ProbeAccountUnreachable(err error) bool {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.StatusCode == http.StatusNotFound
	}
	return false
}

// ProbeInconclusive reports whether err left the probe genuinely unresolved — nothing was
// learned about the connection either way: a pre-send connection failure, a mid-flight
// transport failure, the token endpoint being unavailable, HTTP 429, or any 5xx.
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
	if isPreSendDialError(err) {
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
	// one of its completeness guards. It proves nothing about the credential.
	return true
}

// ProbeNotSent reports whether err PROVES the FAILING request never left this process — a DNS failure, or a
// connect-time dial refusal — so the probe's outcome belongs to no platform at all.
//
// It is the third member of the probe vocabulary and the only one that is not about the
// operator's answer: an inconclusive probe reads identically either way. It exists for the
// metrics arm alone — see internal/dispatch/probe.go and domain.ErrConnectionProbeNotAttempted
// — so an unresolvable host does not land on Reddit's upstream-call series as a near-zero-
// latency error sample and invent a provider outage out of a local network fault.
//
// A cancelled or expired context is deliberately NOT claimed here, for the reason
// isPreSendDialError itself gives: the bytes may already have been written when the deadline
// fired, so nothing about it proves the platform was not reached.
//
// It defaults FALSE for anything it does not recognise, the OPPOSITE of ProbeInconclusive's
// default and for the same reason that one defaults true: each defaults to the answer that is
// wrong in the cheap direction. Here that is recording a sample for a call that may have
// happened, rather than silently dropping a real platform failure out of the series.
//
// "The failing request", not "no bytes at all": on a two-leg probe a token refresh may have
// SUCCEEDED before the account read failed to dial, and this still answers true. See
// domain.ErrConnectionProbeNotAttempted, which explains why that is the cheap direction — the
// suppressed sample is an ERROR sample, so recording it would charge this deployment's own DNS
// fault to the provider.
func ProbeNotSent(err error) bool {
	return isPreSendDialError(err)
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
