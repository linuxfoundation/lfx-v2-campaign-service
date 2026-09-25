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
// The per-platform ProbeConnection methods pass their own package's pair to probeClass below,
// which is the only place the two are ever combined and the only place either is evaluated.
// They are evaluated IN ORDER, and the order is load-bearing: ProbeInconclusive
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

// whichAccount renders the account as a sentence OBJECT rather than as a trailing clause, for
// the one verdict that names the account mid-sentence ("does not reach account X"). where()'s
// " for account X" reads correctly after "rejected the stored credential" and ungrammatically
// after "does not reach", so the two renderings are kept apart rather than one being bent to
// serve both.
func (s probeSubject) whichAccount() string {
	id := strings.TrimSpace(s.accountID)
	if id == "" {
		return "the configured account"
	}
	return "account " + id
}

// confirmedProbeVerdictError attaches domain.ErrConnectionProbeFailed to a message without
// changing the text that message renders.
//
// This is the same device linkedin.go's confirmedOrgVerdictError exists for, and for the same
// reason: fmt.Errorf("%w: ...", domain.ErrConnectionProbeFailed, ...) prepends the sentinel's
// own sentence ("the connection failed verification against the platform"), which the service
// arm then renders a SECOND time alongside its own prefix, producing
//
//	connection found, but reddit ads verification failed: the connection failed verification
//	against the platform: reddit ads rejected the stored credential for account t2_x
//
// for the one class whose text reaches the operator verbatim. The tag exists to be MATCHED,
// not read, so Error() forwards and Is() answers for the sentinel.
type confirmedProbeVerdictError struct{ err error }

func (e *confirmedProbeVerdictError) Error() string { return e.err.Error() }
func (e *confirmedProbeVerdictError) Unwrap() error { return e.err }
func (e *confirmedProbeVerdictError) Is(target error) bool {
	return target == domain.ErrConnectionProbeFailed
}

// confirmedProbeVerdict builds a confirmed-failure verdict from service-authored text.
func confirmedProbeVerdict(format string, args ...any) error {
	return &confirmedProbeVerdictError{err: fmt.Errorf(format, args...)}
}

// preSendProbeVerdictError is a confirmed verdict that ALSO answers for
// domain.ErrConnectionProbeNotAttempted.
//
// It renders the same sentence and carries the same ErrConnectionProbeFailed status as any
// other confirmed verdict — the operator's answer does not change because the verdict was
// cheap to reach. The extra sentinel is read by exactly one caller,
// Orchestrator.ProbeConnection, which must not book an upstream-call sample for a platform it
// never called; see that sentinel's doc for why.
//
// It embeds nothing and wraps the confirmed verdict rather than replacing it, so the two
// sentinels cannot drift apart: an error that is not a confirmed failure can never be built
// through here.
type preSendProbeVerdictError struct{ err *confirmedProbeVerdictError }

func (e *preSendProbeVerdictError) Error() string { return e.err.Error() }
func (e *preSendProbeVerdictError) Unwrap() error { return e.err }
func (e *preSendProbeVerdictError) Is(target error) bool {
	return target == domain.ErrConnectionProbeNotAttempted
}

// preSendProbeVerdict builds a confirmed verdict that was reached before anything was sent.
func preSendProbeVerdict(format string, args ...any) error {
	return &preSendProbeVerdictError{err: &confirmedProbeVerdictError{err: fmt.Errorf(format, args...)}}
}

// probeClass maps a platform probe error onto exactly one of the three service-level outcomes,
// using that platform's own two predicates. err must be non-nil.
func (s probeSubject) probeClass(err error, credentialRejected, inconclusive func(error) bool) error {
	// The arm ORDER is load-bearing, not stylistic: every platform's ProbeInconclusive returns
	// true for an error it does not recognise, so an inconclusive-first switch would make the
	// rejection arm unreachable for any error both predicates claim. Do not reorder.
	switch {
	case credentialRejected(err):
		// The ONLY arm whose text reaches the operator verbatim (domain.ErrConnectionProbeFailed
		// is an echo allowlist), and it is authored entirely here for that reason.
		return confirmedProbeVerdict("%s rejected the stored credential%s", s.platform, s.where())
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
//
// It carries domain.ErrConnectionProbeNotAttempted because every caller of THIS method decides
// it before building a request. probeMembership reaches the same sentence after an enumeration
// that did happen, and renders it through noAccountConfiguredText without the marker — a call
// that was made must stay in the upstream series whatever verdict it leads to.
func (s probeSubject) noAccountConfigured() error {
	return preSendProbeVerdict(noAccountConfiguredText, s.platform)
}

// noAccountConfiguredText is the single wording behind both renderings above, so the two can
// never drift into two different sentences for one verdict.
const noAccountConfiguredText = "the %s connection names no ad account to verify"

// accountIDNotUsable is the verdict for a connection whose configured account id cannot form a
// valid request for its platform at all.
//
// Like noAccountConfigured it is decided before anything is sent, and it is a verdict for the
// same reason: campaign creation on this connection cannot succeed. It is kept apart from the
// credential-rejection arm because no credential was evaluated — telling an operator their
// credential was refused would send them to re-authorise a connection whose only broken part is
// a value they can see on the row and fix themselves.
func (s probeSubject) accountIDNotUsable() error {
	return preSendProbeVerdict("the %s connection's configured ad account id is not a valid %s account id",
		s.platform, s.platform)
}

// customerIDNotUsable is accountIDNotUsable's sibling for the OTHER operator-settable identity
// on a connection: Microsoft Advertising's customer_id, which scopes the whole enumeration
// rather than naming the account.
//
// Kept separate from accountIDNotUsable because the operator has to know WHICH field to fix,
// and the two live on the same row. Like its sibling it is decided before anything is sent and
// no credential is evaluated, so it must not surface as a credential rejection — nor as
// inconclusive, which is what an unrecognised error from the enumeration used to produce:
// OK: true for a connection that can never dispatch.
func (s probeSubject) customerIDNotUsable() error {
	return preSendProbeVerdict("the %s connection's configured customer id is not a valid %s customer id",
		s.platform, s.platform)
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
	return confirmedProbeVerdict("the %s credential authenticates but does not reach %s",
		s.platform, s.whichAccount())
}

// accountIsManagerAccount and accountNotEnabled are the two verdicts for an account the
// credential DOES reach but which cannot hold a campaign.
//
// They exist because accountNotReachable was answering for both, and it is false for both:
// the credential reaches the account, and telling an operator it does not sends them to
// repoint an account id that is correct. The remedy differs too — a manager account means the
// connection names the wrong LEVEL of the hierarchy, a disabled one means the account itself
// needs reinstating in the Google Ads UI — and neither is "check your credential".
//
// Both sentences are authored here, like every other verdict in this file. The platform's own
// status string is compared against in the client and never travels this far: it is upstream
// text, and the confirmed-verdict arm is echoed to the operator verbatim.
func (s probeSubject) accountIsManagerAccount() error {
	return confirmedProbeVerdict("the %s credential reaches %s, but that is a manager account and cannot hold campaigns",
		s.platform, s.whichAccount())
}

func (s probeSubject) accountNotEnabled() error {
	return confirmedProbeVerdict("the %s credential reaches %s, but that account is not enabled",
		s.platform, s.whichAccount())
}

// requiredConfigMissing is the verdict for a connection field that is neither an id nor a
// credential, but without which the platform rejects EVERY campaign create.
//
// It is the one verdict that is not about authentication or reachability, and it exists
// because those two can both pass on a connection whose every campaign the platform will
// still reject — which is precisely the state a connection test is supposed to catch.
// Reddit's conversion_pixel_id is the only such field today (reddit.Client refuses the create
// before any upstream call, so the rejection is certain rather than predicted).
//
// "EVERY create" is scoped to the campaigns this service builds. A caller may be able to carry
// the value per campaign in the brief — Reddit's pixel can be — and such a campaign dispatches
// fine. The verdict stands anyway: the service's own create path never sets those overrides, so
// no campaign the product builds gets past the platform, and a connection that only works for a
// hand-written brief is not a healthy connection.
//
// It is raised AFTER the reachability check rather than before it, and so is deliberately NOT
// marked not-attempted: a credential that does not authenticate makes the pixel irrelevant,
// and answering the pixel first would send an operator to fill in a field on a connection
// whose real problem is the credential. Running reachability first also means an upstream call
// genuinely happened by the time this verdict is reached, so it belongs in the upstream series
// like any other post-call verdict.
//
// remedy names where the operator finds the value; it is service-authored text like every
// other sentence in this file, never anything the platform said.
func (s probeSubject) requiredConfigMissing(field, remedy string) error {
	return confirmedProbeVerdict("the %s credential reaches %s, but this connection sets no %s and %s rejects every campaign without one (%s)",
		s.platform, s.whichAccount(), field, s.platform, remedy)
}

// probeMembership checks the configured account against a completed enumeration, normalising
// both sides the way the platform stores them.
//
// normalize is the platform's own id-normalising function where its stored form and its wire
// form differ (Meta's act_ prefix); platforms whose ids round-trip verbatim pass nil.
func (s probeSubject) probeMembership(accessible []string, normalize func(string) string) error {
	want := strings.TrimSpace(s.accountID)
	if want == "" {
		// The same verdict noAccountConfigured renders, deliberately WITHOUT the not-attempted
		// marker: reaching this line means the enumeration above already completed, so an
		// upstream call was made and belongs in the upstream series. Every caller today checks
		// for an empty id before calling out, so this arm is defensive — and it has to stay
		// correct anyway, because the day one stops checking is the day the marker would
		// silently delete a real call from the metrics.
		return confirmedProbeVerdict(noAccountConfiguredText, s.platform)
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
