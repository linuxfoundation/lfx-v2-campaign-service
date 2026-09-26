// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// ErrAccountNotConfigured marks a client asked to verify an account when its AccountConfig
// names none. It is decided before anything is sent: there is no account to reach, so the
// connection cannot dispatch, and saying so is a verdict rather than a failure to check.
var ErrAccountNotConfigured = errors.New("twitter: no ad account is configured on this connection")

// ErrInvalidAccountID marks a stored account id that cannot form a valid X Ads request at all.
// Like ErrAccountNotConfigured it is decided before anything is sent, from this client's own
// configuration, so X never evaluated the credential.
var ErrInvalidAccountID = errors.New("twitter: the configured x ads account id is not usable")

// VerifyAccount reads the ad account this client is configured for, and reports whether the
// stored credential can reach it. It creates nothing and changes nothing.
//
// It is the same GET the unexported verifyAccount makes for CreateCampaign's Step 1 (an empty
// path targets the account root), lifted into an exported method so the connection-test path
// can make it on its own. The two differ in exactly one way, deliberately: there a failure is
// a non-fatal warning step, because a campaign create should not be refused over a
// verification call, whereas here the call IS the test and its failure is the answer.
//
// The response is DISCARDED. X's account resource carries a display name the create path uses
// for its step text, but a connection test asks only whether the account is reachable, and
// rendering an upstream-supplied name into a test result would put untrusted response text on
// a path whose message an operator reads as this service's own.
func (c *Client) VerifyAccount(ctx context.Context) error {
	accountID := strings.TrimSpace(c.account.AccountID)
	if accountID == "" {
		return ErrAccountNotConfigured
	}
	// Validated before the path is built, for the reason accountIDRe exists: the id is
	// interpolated into the account-scoped path (accountURL), so a value carrying '/', '?' or
	// '#' would address a DIFFERENT account subresource — and a 2xx from whatever that turns out
	// to be would report this connection as healthy on the strength of a request it never made.
	// CreateCampaign applies the charset guard at its own top; the length bound is the one
	// accounts.go applies to every id it hands back from enumeration, so a stored value is held
	// to the same rule as a discovered one.
	if !accountIDRe.MatchString(accountID) || len(accountID) > maxAccountIDLen {
		return fmt.Errorf("%w: %q", ErrInvalidAccountID, accountID)
	}
	if _, err := c.request(ctx, http.MethodGet, ""); err != nil {
		return err
	}
	return nil
}

// ProbeCredentialRejected reports whether err is X evaluating this connection's stored
// credential and REFUSING it: HTTP 401 or 403.
//
// A 404 is deliberately NOT here, for the same reason it is not on the Reddit probe: it says
// the credential was accepted and the ACCOUNT was not found, which is a different sentence
// from "X refused your credential" and points the operator at a different field. It has its
// own predicate, ProbeAccountUnreachable, and its own verdict — see
// probeSubject.accountNotReachable.
//
// ErrAccountNotConfigured and ErrInvalidAccountID are deliberately NOT here. VerifyAccount
// raises both before anything is sent, from this client's own configuration, so X never
// evaluated the credential; reporting either as a rejected credential would tell an operator to
// re-authorise a connection whose credentials were never in question. They are still verdicts —
// see probeSubject.noAccountConfigured and probeSubject.accountIDNotUsable — but ones the
// dispatcher authors, next to its ErrAccountNotSelected arm.
//
// It is one half of the two-predicate probe vocabulary every platform client in this repo
// exposes; internal/dispatch/probe.go holds the shared rationale and is the only caller.
func ProbeCredentialRejected(err error) bool {
	var ae *apiError
	if errors.As(err, &ae) {
		switch ae.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return true
		}
	}
	return false
}

// ProbeAccountUnreachable reports whether err is X accepting these OAuth1 credentials and then
// failing to find the account they were asked about: HTTP 404 on the account read.
//
// It exists on this client and on Reddit's, and on no other, because only these two probes name
// the configured account IN the request path — VerifyAccount's URL is the account resource
// itself. Every other client enumerates and checks membership, so a 404 there could only mean
// the endpoint moved — our defect, and correctly inconclusive. Here the 404 IS the answer to
// the question asked, and distinguishing it from a rejection is what keeps the verdict from
// telling an operator to re-authorise credentials X just honoured.
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
// learned about the connection either way: a pre-send failure, a mid-flight transport
// failure, HTTP 429, or any 5xx.
//
// preSendError is checked as well as isPreSendDialError because this client wraps a request
// that never left the process (OAuth1 signing, request build) in that type rather than
// letting the dial classifier see it.
func ProbeInconclusive(err error) bool {
	if isPreSendDialError(err) {
		return true
	}
	var pse *preSendError
	if errors.As(err, &pse) {
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
	// Not from the round trip at all — a malformed response this client refused to trust.
	// It proves nothing about the credential.
	return true
}

// ProbeNotSent reports whether err PROVES nothing left this process — a DNS failure, a
// connect-time dial refusal, or this client's own preSendError — so the probe's outcome belongs
// to no platform at all.
//
// It is the third member of the probe vocabulary and the only one that is not about the
// operator's answer: an inconclusive probe reads identically either way. It exists for the
// metrics arm alone — see internal/dispatch/probe.go and domain.ErrConnectionProbeNotAttempted
// — so an unresolvable host does not land on X's upstream-call series as a near-zero-latency
// error sample and invent a provider outage out of a local network fault.
//
// preSendError is matched as well as isPreSendDialError for the same reason ProbeInconclusive
// matches both: this client wraps a dial failure in preSendError to strip the URL, and the
// wrapper keeps the cause reachable, so either route can present the same fact.
//
// It defaults FALSE for anything it does not recognise, the OPPOSITE of ProbeInconclusive's
// default and for the same reason that one defaults true: each defaults to the answer that is
// wrong in the cheap direction. Here that is recording a sample for a call that may have
// happened, rather than silently dropping a real platform failure out of the series.
func ProbeNotSent(err error) bool {
	if isPreSendDialError(err) {
		return true
	}
	var pse *preSendError
	return errors.As(err, &pse)
}
