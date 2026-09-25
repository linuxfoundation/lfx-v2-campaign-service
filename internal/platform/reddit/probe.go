// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// ErrTokenRequestRejected marks Reddit's token endpoint REFUSING the refresh exchange on the
// merits — a 4xx, which in practice means the refresh token has been revoked or expired, or
// the application credentials it was issued against no longer match. It is a fact about the
// stored credential, decided by Reddit, and permanent.
//
// It is exported because the connection-test path has to tell it apart from every other way a
// token refresh can fail, and cannot do that from the error text — which deliberately carries
// the status and nothing else, because that request body holds the client secret and the
// refresh token. See ProbeCredentialRejected.
var ErrTokenRequestRejected = errors.New("reddit: the token endpoint refused the refresh request")

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
// A 404 on the account read is here too, and is the one place this predicate departs from its
// siblings. Every other client's probe enumerates and then checks membership, so a 404 could
// only mean the endpoint moved — our defect. This probe names the configured account IN the
// path, so a 404 is Reddit answering the question that was asked: this credential does not
// reach that account. Reporting it as a service defect would page us for a connection the
// operator needs to repoint.
//
// It is one half of the two-predicate probe vocabulary every platform client in this repo
// exposes; internal/dispatch/probe.go holds the shared rationale and is the only caller.
func ProbeCredentialRejected(err error) bool {
	if errors.Is(err, ErrTokenRequestRejected) || errors.Is(err, ErrInvalidAccountID) {
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
// learned about the connection either way: a pre-send connection failure, a mid-flight
// transport failure, the token endpoint being unavailable, HTTP 429, or any 5xx.
func ProbeInconclusive(err error) bool {
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
		return ae.StatusCode == http.StatusTooManyRequests || ae.StatusCode >= 500
	}
	// Not from the round trip at all — a malformed response this client refused to trust, or
	// one of its completeness guards. It proves nothing about the credential.
	return true
}
