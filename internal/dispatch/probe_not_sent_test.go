// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"errors"
	"net"
	"syscall"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
)

// probeVocabulary is one platform package's three probe predicates, gathered so the table below
// can run the REAL ones rather than stubs. A stub returning the collapsed sentinel would pass
// whatever probeClass does with it; only the platform's own predicate proves the fact survives
// the conversion.
type probeVocabulary struct {
	platform           model.Provider
	credentialRejected func(error) bool
	inconclusive       func(error) bool
	notSent            func(error) bool
}

// realProbeVocabularies covers every platform whose pre-send failures can be CONSTRUCTED from
// outside its package, which is every one except Microsoft. That client renders its pre-send
// cause through safeCause into a plain string and carries the fact on an unexported marker
// instead, so its equivalent coverage lives in internal/platform/microsoft, driven through the
// real client — see TestProbeNotSent_RealDialFailureThroughTheClient there.
//
// LinkedIn is absent because it does not use probeClass at all: dispatch/linkedin.go converts
// its own probe errors, for the reasons that file gives.
var realProbeVocabularies = []probeVocabulary{
	{model.ProviderGoogleAds, googleads.ProbeCredentialRejected, googleads.ProbeInconclusive, googleads.ProbeNotSent},
	{model.ProviderMetaAds, meta.ProbeCredentialRejected, meta.ProbeInconclusive, meta.ProbeNotSent},
	{model.ProviderRedditAds, reddit.ProbeCredentialRejected, reddit.ProbeInconclusive, reddit.ProbeNotSent},
	{model.ProviderTwitterAds, twitter.ProbeCredentialRejected, twitter.ProbeInconclusive, twitter.ProbeNotSent},
	{model.ProviderHubSpot, hubspot.ProbeCredentialRejected, hubspot.ProbeInconclusive, hubspot.ProbeNotSent},
}

// dnsFailure is the error an http.Client returns for a host that does not resolve — the single
// most common way a probe fails without reaching anything, and the one that hits EVERY
// connection on a platform at once when a cluster's egress or resolver breaks.
func dnsFailure() error {
	return &net.OpError{
		Op:  "dial",
		Net: "tcp",
		Err: &net.DNSError{Err: "no such host", Name: "ads.example.invalid", IsNotFound: true},
	}
}

// connectionRefused is the other proven pre-send shape: the name resolved, and nothing was
// listening. Constructed the way net returns it so errors.Is reaches the syscall errno, because
// that is exactly what each platform's classifier matches on.
func connectionRefused() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
}

// TestProbeClass_PreSendFailuresKeepTheirProvenance is the metrics gate's other half, pinned at
// the layer that is the LAST one able to tell.
//
// probeClass drops the platform error chain on purpose (see probe.go's header), so an
// inconclusive outcome leaving this function is indistinguishable from any other inconclusive
// outcome. If the "nothing was sent" fact is not captured here it does not exist downstream, and
// Orchestrator.ProbeConnection books an upstream-call sample against a provider that was never
// contacted — turning a DNS or egress fault in this deployment into that provider's error rate,
// on every connection it has, simultaneously.
func TestProbeClass_PreSendFailuresKeepTheirProvenance(t *testing.T) {
	for _, v := range realProbeVocabularies {
		t.Run(string(v.platform), func(t *testing.T) {
			s := probeSubject{platform: v.platform, accountID: "acct-1"}

			for _, tc := range []struct {
				name string
				err  error
			}{
				{"host does not resolve", dnsFailure()},
				{"connection refused", connectionRefused()},
			} {
				t.Run(tc.name, func(t *testing.T) {
					got := s.probeClass(tc.err, v.credentialRejected, v.inconclusive, v.notSent)
					if !errors.Is(got, domain.ErrConnectionProbeInconclusive) {
						t.Fatalf("probeClass = %v; a dial failure proves nothing about the credential and must stay inconclusive", got)
					}
					if !errors.Is(got, domain.ErrConnectionProbeNotAttempted) {
						t.Errorf("probeClass = %v; the not-sent marker is missing, so the metrics arm will record an upstream call for a platform nothing reached", got)
					}
					// The operator's answer must not move because of the marker: the check
					// still could not be completed, and it is still not a failed connection.
					if errors.Is(got, domain.ErrConnectionProbeFailed) {
						t.Error("a probe that never left the process was hardened into a confirmed failed connection")
					}
				})
			}

			t.Run("an inconclusive outcome that may have been sent is still recorded", func(t *testing.T) {
				// Unrecognised by every platform's classifier, so ProbeInconclusive's default
				// claims it and ProbeNotSent's default does not. That pairing is the whole
				// safety argument: unproven is never reported as proven, and an unclassified
				// failure never silently leaves the upstream series.
				got := s.probeClass(errors.New("boom"), v.credentialRejected, v.inconclusive, v.notSent)
				if !errors.Is(got, domain.ErrConnectionProbeInconclusive) {
					t.Fatalf("probeClass = %v, want inconclusive", got)
				}
				if errors.Is(got, domain.ErrConnectionProbeNotAttempted) {
					t.Error("an error no classifier recognised was claimed as never sent; a real platform failure would vanish from the upstream series")
				}
			})
		})
	}
}

// TestProbeNotSentIsNeverAVerdict pins the one thing the third predicate must NOT do.
//
// It is provenance, not classification. A platform that answered and refused the credential is a
// confirmed failure whatever else is true of it, and a pre-send predicate answering true for
// such an error — a future classifier widening too far — must not quietly turn that verdict into
// "could not be verified" and hand the operator a retry instead of a repair.
func TestProbeNotSentIsNeverAVerdict(t *testing.T) {
	s := probeSubject{platform: model.ProviderGoogleAds, accountID: "8666746580"}

	got := s.probeClass(dnsFailure(), alwaysTrue, alwaysTrue, alwaysTrue)
	if !errors.Is(got, domain.ErrConnectionProbeFailed) {
		t.Fatalf("probeClass = %v; the rejection arm is evaluated first and notSent must not reach it", got)
	}
	if errors.Is(got, domain.ErrConnectionProbeInconclusive) {
		t.Error("a confirmed rejection was demoted to inconclusive")
	}
}
