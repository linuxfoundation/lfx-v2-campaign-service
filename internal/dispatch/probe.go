// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"fmt"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// The connection-probe classification, shared by every platform's ProbeConnection.
//
// A connection test asks two questions of the stored credential — does it authenticate, and
// does it reach the account this connection is configured for — by making one read-only call
// per platform. What the six platforms do NOT share is how they report a failure: Google and
// Reddit answer a dead refresh token from a token endpoint, Meta reports a revoked token as an
// HTTP 400 carrying code 190, Microsoft renders its pre-send failures into a URL-free string,
// HubSpot has no refresh exchange at all. Each platform client therefore owns the reading of
// its own errors and exposes the result as the same two predicates:
//
//	ProbeCredentialRejected(err) bool  // the platform evaluated the credential and refused it
//	ProbeInconclusive(err) bool        // nothing was learned about the connection either way
//
// This file is the only caller of either, and probeClass below is the only place the two are
// combined. They are evaluated IN ORDER, and the order is load-bearing: ProbeInconclusive
// defaults to true for an error it does not recognise, so a class both predicates would claim
// must be settled by the rejection arm first. "Neither predicate" is the third outcome and is
// deliberately not a predicate of its own — a 4xx that is not 401/403/429 means the platform
// refused the request this service built, which is our defect.
//
// The three outcomes are converted HERE rather than in internal/service, for the reason
// linkedin.go's VerifyAccountOrg already converts its own: this is the one layer that knows
// both the service's error contract and the platform clients' types, so internal/service
// classifies a probe without importing internal/platform/*. The conversion is also the safety
// boundary. The platform error chain is DROPPED, never wrapped: meta.APIError.Message falls
// back to the raw response body, several clients render a request URL, and the connection-test
// arm echoes a confirmed verdict's text straight to the operator. Every message this file
// produces is authored from its own vocabulary plus the operator's own configured account id,
// so an error carrying these sentinels is safe to log and safe to show.

// probeSubject names what a probe was run against, and authors every sentence about it.
type probeSubject struct {
	platform model.Provider
	// accountID is the account the connection is configured for, as stored. It is operator-
	// supplied configuration already visible on the connection row, which is why it is the one
	// value allowed into an echoed message — it tells an operator WHICH account failed without
	// quoting anything the platform sent back.
	accountID string
}

// where renders the account clause, or nothing when the connection names no account. A probe
// that got this far with an empty id is reporting some other verdict about it, so the clause
// simply drops out rather than printing an empty pair of quotes.
func (s probeSubject) where() string {
	id := strings.TrimSpace(s.accountID)
	if id == "" {
		return ""
	}
	return fmt.Sprintf(" for account %s", id)
}

// probeClass maps a platform probe error onto exactly one of the three service-level outcomes,
// using that platform's own two predicates. err must be non-nil.
func (s probeSubject) probeClass(err error, credentialRejected, inconclusive func(error) bool) error {
	switch {
	case credentialRejected(err):
		// The ONLY arm whose text reaches the operator verbatim (domain.ErrConnectionProbeFailed
		// is an echo allowlist), and it is authored entirely here for that reason.
		return fmt.Errorf("%w: %s rejected the stored credential%s",
			domain.ErrConnectionProbeFailed, s.platform, s.where())
	case inconclusive(err):
		// No detail beyond the platform name: there is nothing useful to say that is also safe
		// to say, and this arm maps to OK: true with an advisory, where a half-explanation
		// reads as a diagnosis.
		return fmt.Errorf("%w: the %s check could not be completed", domain.ErrConnectionProbeInconclusive, s.platform)
	default:
		// ErrServiceDefect selects the status; ErrConnectionProbeRequestRejected travels
		// alongside it as the reason token, the arrangement both sentinels' docs require and
		// the one linkedin.go's ErrAccountDiscoveryRejected arm already follows. Without it an
		// operator log reads reason=unclassified for a defect this service owns.
		return fmt.Errorf("%w: %w: %s refused the connection-probe request this service built",
			domain.ErrServiceDefect, domain.ErrConnectionProbeRequestRejected, s.platform)
	}
}

// noAccountConfigured is the verdict for a connection that names no account to reach.
//
// It is decided before anything is sent, and it is a VERDICT rather than a failure to check:
// campaign creation on this connection cannot succeed, which is the question the test asks.
// Answering "inconclusive" — OK: true — for a connection that is provably unusable is the
// exact defect this whole path removes.
func (s probeSubject) noAccountConfigured() error {
	return fmt.Errorf("%w: the %s connection names no ad account to verify",
		domain.ErrConnectionProbeFailed, s.platform)
}

// accountNotReachable is the verdict for a completed enumeration that did not contain the
// configured account.
//
// "Completed" is what makes it a verdict. Every client behind these probes fails a partial
// enumeration rather than returning a short list — microsoft.ListAdAccounts states the rule
// outright ("an incomplete answer is an ERROR, never a short list") — so absence from a list
// that was returned at all means the credential genuinely does not reach the account, not that
// the walk was cut short.
func (s probeSubject) accountNotReachable() error {
	return fmt.Errorf("%w: the %s credential authenticates but does not reach%s",
		domain.ErrConnectionProbeFailed, s.platform, s.where())
}

// probeMembership checks the configured account against a completed enumeration, normalising
// both sides the way the platform stores them.
//
// normalize is the platform's own id-normalising function where its stored form and its wire
// form differ (Meta's act_ prefix); platforms whose ids round-trip verbatim pass nil.
func (s probeSubject) probeMembership(accessible []string, normalize func(string) string) error {
	want := strings.TrimSpace(s.accountID)
	if want == "" {
		return s.noAccountConfigured()
	}
	if normalize != nil {
		want = normalize(want)
	}
	for _, got := range accessible {
		got = strings.TrimSpace(got)
		if normalize != nil {
			got = normalize(got)
		}
		if got != "" && got == want {
			return nil
		}
	}
	return s.accountNotReachable()
}
