// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// ErrAccountNotConfigured marks a client asked to verify an account when its AccountConfig
// names none. It is decided before anything is sent: there is no account to reach, so the
// connection cannot dispatch, and saying so is a verdict rather than a failure to check.
var ErrAccountNotConfigured = errors.New("twitter: no ad account is configured on this connection")

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
	if strings.TrimSpace(c.account.AccountID) == "" {
		return ErrAccountNotConfigured
	}
	if _, err := c.request(ctx, http.MethodGet, ""); err != nil {
		return err
	}
	return nil
}

// ProbeCredentialRejected reports whether err is X evaluating this connection's stored
// credential and REFUSING it: HTTP 401 or 403.
//
// A 404 is here too, for the same reason it is on the Reddit probe: this call names the
// configured account IN its path, so a 404 is X answering the question asked — these OAuth1
// credentials do not reach that account — rather than evidence that an endpoint moved.
//
// It is one half of the two-predicate probe vocabulary every platform client in this repo
// exposes; internal/dispatch/probe.go holds the shared rationale and is the only caller.
func ProbeCredentialRejected(err error) bool {
	if errors.Is(err, ErrAccountNotConfigured) {
		return true
	}
	var ae *apiError
	if errors.As(err, &ae) {
		switch ae.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
			return true
		}
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
