// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"errors"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// TestPreSendVerdictsAreMarkedNotAttempted pins which probe verdicts say "nothing was sent".
//
// Orchestrator.ProbeConnection reads domain.ErrConnectionProbeNotAttempted to decide whether to
// record an upstream call, so the marker is the difference between a platform's latency
// histogram measuring network work and it measuring how fast this service can reject a
// misconfigured row. Both halves of the table matter: a missing marker books a call that never
// happened, and a marker on a verdict reached AFTER a call deletes a real one.
func TestPreSendVerdictsAreMarkedNotAttempted(t *testing.T) {
	s := probeSubject{platform: model.ProviderMicrosoftAds, accountID: "1234567"}

	for _, tc := range []struct {
		name string
		err  error
		want bool
		why  string
	}{
		{
			name: "no account configured",
			err:  s.noAccountConfigured(),
			want: true,
			why:  "every caller decides it from the stored row, before a client is even asked for a request",
		},
		{
			name: "account id not usable",
			err:  s.accountIDNotUsable(),
			want: true,
			why:  "the id is refused by the platform's own validator before anything is built from it",
		},
		{
			name: "customer id not usable",
			err:  s.customerIDNotUsable(),
			want: true,
			why:  "the customer id scopes the request that was never built",
		},
		{
			name: "account not reachable",
			err:  s.accountNotReachable(),
			want: false,
			why:  "an enumeration completed to reach this verdict, so a call was made and belongs in the series",
		},
		{
			name: "account is a manager account",
			err:  s.accountIsManagerAccount(),
			want: false,
			why:  "the credential reached the account, which takes a call",
		},
		{
			name: "account not enabled",
			err:  s.accountNotEnabled(),
			want: false,
			why:  "same call, different status on what came back",
		},
		{
			name: "credential rejected",
			err:  s.probeClass(errors.New("boom"), func(error) bool { return true }, func(error) bool { return false }),
			want: false,
			why:  "the platform evaluated the credential, which it can only do over a request",
		},
		{
			name: "inconclusive",
			err:  s.probeClass(errors.New("boom"), func(error) bool { return false }, func(error) bool { return true }),
			want: false,
			why:  "a call was attempted and failed on the way; that attempt is exactly what the error series counts",
		},
		{
			name: "service defect",
			err:  s.probeClass(errors.New("boom"), func(error) bool { return false }, func(error) bool { return false }),
			want: false,
			why:  "the platform answered and refused the request this service sent it",
		},
		{
			name: "membership miss on an empty id after a completed enumeration",
			err:  probeSubject{platform: model.ProviderMetaAds}.probeMembership([]string{"act_1"}, nil),
			want: false,
			why: "probeMembership renders the no-account sentence only after the enumeration returned, " +
				"so unlike noAccountConfigured it must stay in the upstream series",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := errors.Is(tc.err, domain.ErrConnectionProbeNotAttempted); got != tc.want {
				t.Errorf("errors.Is(%v, ErrConnectionProbeNotAttempted) = %v, want %v: %s", tc.err, got, tc.want, tc.why)
			}
		})
	}
}

// TestPreSendVerdictsAreStillConfirmedFailures is the regression half of the marker.
//
// The marker rides ALONGSIDE domain.ErrConnectionProbeFailed and changes nothing an operator
// sees: these verdicts still map to OK: false and their text is still echoed verbatim. A
// wrapper that answered only for the new sentinel would turn three confirmed failures into
// unclassified errors — a typed 500 on the endpoint whose whole point is answering "no" without
// one.
func TestPreSendVerdictsAreStillConfirmedFailures(t *testing.T) {
	s := probeSubject{platform: model.ProviderMicrosoftAds, accountID: "1234567"}

	for _, tc := range []struct {
		name     string
		err      error
		wantText string
	}{
		{
			name:     "no account configured",
			err:      s.noAccountConfigured(),
			wantText: "the microsoft-ads connection names no ad account to verify",
		},
		{
			name:     "account id not usable",
			err:      s.accountIDNotUsable(),
			wantText: "the microsoft-ads connection's configured ad account id is not a valid microsoft-ads account id",
		},
		{
			name:     "customer id not usable",
			err:      s.customerIDNotUsable(),
			wantText: "the microsoft-ads connection's configured customer id is not a valid microsoft-ads customer id",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !errors.Is(tc.err, domain.ErrConnectionProbeFailed) {
				t.Errorf("%v does not match ErrConnectionProbeFailed; the service arm would answer a typed 500 "+
					"instead of the confirmed OK: false verdict this text describes", tc.err)
			}
			if errors.Is(tc.err, domain.ErrConnectionProbeInconclusive) {
				t.Errorf("%v matches ErrConnectionProbeInconclusive, which maps to OK: true — the false positive "+
					"this whole path removes", tc.err)
			}
			if got := tc.err.Error(); got != tc.wantText {
				t.Errorf("Error() = %q, want %q: the marker must not change the sentence the operator reads", got, tc.wantText)
			}
		})
	}
}

// TestProbeMembershipEmptyIDMatchesTheNoAccountSentence pins that the two renderings of the
// no-account verdict stay one sentence. They differ only in the marker, and an operator must
// not be able to tell which code path produced their answer.
func TestProbeMembershipEmptyIDMatchesTheNoAccountSentence(t *testing.T) {
	s := probeSubject{platform: model.ProviderMetaAds}
	afterCall := s.probeMembership([]string{"act_1"}, nil)
	beforeCall := s.noAccountConfigured()
	if afterCall.Error() != beforeCall.Error() {
		t.Errorf("probeMembership's empty-id verdict = %q, noAccountConfigured = %q; one verdict, two sentences",
			afterCall.Error(), beforeCall.Error())
	}
	if !errors.Is(afterCall, domain.ErrConnectionProbeFailed) {
		t.Errorf("%v does not match ErrConnectionProbeFailed", afterCall)
	}
}
