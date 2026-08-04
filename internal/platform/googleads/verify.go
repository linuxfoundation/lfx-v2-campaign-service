// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"errors"
)

// verifyQuery is the READ-ONLY probe used by VerifyCredential.
//
// It selects the customer resource for the account named in the client's AccountConfig. Two
// properties matter and neither is incidental:
//
//  1. It is ACCOUNT-SCOPED. gaqlSearch issues it against customers/{CustomerID}/googleAds:search,
//     so success proves the credential can read THE CONFIGURED ACCOUNT. A tenant-scoped probe
//     (e.g. listing accessible customers) would succeed for a credential pointed at the WRONG
//     ad account — exactly the misconfiguration this endpoint exists to catch.
//  2. It cannot mutate. GAQL search is read-only, so a verification call can never alter a
//     paid resource, no matter how it fails.
//
// LIMIT 1 keeps the response tiny; the rows are discarded — only the call's success or failure
// is the signal.
const verifyQuery = "SELECT customer.id FROM customer LIMIT 1"

// CredentialRejected reports whether err is a DEFINITIVE rejection of the credential by
// Google — as opposed to an ambiguous or transient failure.
//
// The distinction drives an operator action, so it must not be guessed:
//   - a 401/403 means the credential (or its access to this customer id) was refused →
//     re-authenticate. Google returns 403 for a developer-token or customer-access problem
//     and 401 for a bad/expired access token.
//   - a 400 on this query means the request itself was rejected. The only caller-supplied
//     input is the customer id, so in practice this is a customer id Google will not accept.
//     It is a configuration defect the operator must fix, and it will fail identically forever
//     — so it is a definite rejection, not a transient one.
//   - a 404 likewise means the named customer does not exist.
//
// The rule is therefore "any 4xx except 429": Google refused the request, and the only inputs
// are the credential and the customer id.
//
// Everything else — 5xx, 429, transport failures, anything ambiguous — is deliberately NOT a
// rejection. Reporting a provider outage as "your credential is invalid" would send an
// operator to re-authenticate a perfectly good credential, which is strictly worse than
// reporting that we do not know.
//
// Exported so the dispatch layer can classify across the package boundary, mirroring
// IsOutcomeUnconfirmed.
func CredentialRejected(err error) bool {
	if err == nil {
		return false
	}
	// An ambiguous outcome is never a rejection, even if it also carries a status code.
	// Checked FIRST so the 5xx/429/transport cases can't fall through to the status test.
	if IsOutcomeUnconfirmed(err) {
		return false
	}
	var ae *apiError
	if !errors.As(err, &ae) {
		// A non-API error (bad customer-id format caught locally, context cancellation,
		// a marshal failure) tells us nothing about the credential.
		return false
	}
	// A DEFINITIVE client-side refusal: Google received the request and rejected it, and the
	// only inputs are the credential and the customer id.
	//
	// No 429 exclusion and no upper bound are written here, DELIBERATELY. The ambiguity guard
	// above already returns false for every 5xx AND for 429 (createOutcomeAmbiguous treats
	// `StatusCode >= 500 || StatusCode == 429` as ambiguous), so any such test here would be
	// UNREACHABLE — dead code that reads like a live policy decision and that no test could
	// distinguish from its absence. The guard is the single place that owns "ambiguous"; this
	// line owns only "definitely refused". Pinned by
	// TestCredentialRejected_AmbiguityGuardBeatsTheStatusCode.
	return ae.StatusCode >= 400
}

// VerifyCredential performs a READ-ONLY, ACCOUNT-SCOPED probe of this client's credential.
//
// It returns nil when Google accepted the credential for the configured customer id. On
// failure the error is returned unwrapped for the caller to classify with CredentialRejected
// (definite rejection) — every other error must be treated as "unknown", never as invalid.
//
// This method mutates nothing: its only upstream call is a GAQL search.
//
// NOTE on account-id validation: there is deliberately NO validateAccountIDs call here.
// doRequest already validates the customer/login-customer ids as its FIRST action, before the
// OAuth token exchange and before any request is built, so a malformed id never reaches the
// network and no credential material leaves the process. Repeating the check here would be
// unreachable defensive code that no test could distinguish from its absence. That local error
// is a plain error (not an apiError), so CredentialRejected reports false for it and the
// dispatcher surfaces it as unverifiable-with-reason rather than blaming Google.
func (c *Client) VerifyCredential(ctx context.Context) error {
	// Discard the rows: only reachability + acceptance are the signal. gaqlSearch marks the
	// call idempotent, so ordinary Google throttling is retried rather than surfacing as a
	// spurious verification failure.
	if _, err := c.gaqlSearch(ctx, verifyQuery); err != nil {
		return err
	}
	return nil
}
