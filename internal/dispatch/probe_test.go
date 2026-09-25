// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"errors"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// TestProbeClass_EvaluationOrderIsLoadBearing is the single most important test on this path.
//
// Every platform's ProbeInconclusive returns TRUE for an error it does not recognise — it has
// to, because an unrecognised error proves nothing about the credential and the alternative is
// reporting connections broken on guesses. The consequence is that a credential rejection
// satisfies BOTH predicates on most platforms, and only the evaluation order decides which
// verdict the operator sees.
//
// Get the order wrong and a revoked refresh token classifies as inconclusive, which maps to
// OK: true — restoring, exactly, the bug this whole change removes.
func TestProbeClass_EvaluationOrderIsLoadBearing(t *testing.T) {
	s := probeSubject{platform: model.ProviderGoogleAds, accountID: "8666746580"}
	boom := errors.New("token refresh refused")

	// Both predicates say yes, which is the real shape of a revoked token on every platform
	// whose ProbeInconclusive defaults to true.
	got := s.probeClass(boom, alwaysTrue, alwaysTrue)
	if !errors.Is(got, domain.ErrConnectionProbeFailed) {
		t.Fatalf("probeClass = %v; with both predicates true the rejection must win, or a revoked credential is reported as a healthy connection", got)
	}
	if errors.Is(got, domain.ErrConnectionProbeInconclusive) {
		t.Error("the inconclusive sentinel is attached to a confirmed rejection")
	}
}

func alwaysTrue(error) bool  { return true }
func alwaysFalse(error) bool { return false }

func TestProbeClass(t *testing.T) {
	s := probeSubject{platform: model.ProviderMetaAds, accountID: "act_193556282970417"}
	boom := errors.New("upstream boom")

	t.Run("rejected", func(t *testing.T) {
		err := s.probeClass(boom, alwaysTrue, alwaysFalse)
		if !errors.Is(err, domain.ErrConnectionProbeFailed) {
			t.Fatalf("err = %v, want ErrConnectionProbeFailed", err)
		}
		// The message must name the platform and the account, because this is the ONE probe
		// error the service layer echoes and "verification failed" alone leaves an operator
		// nothing to repair.
		if !strings.Contains(err.Error(), string(model.ProviderMetaAds)) {
			t.Errorf("message %q does not name the platform", err)
		}
		if !strings.Contains(err.Error(), "act_193556282970417") {
			t.Errorf("message %q does not name the configured account", err)
		}
	})

	t.Run("inconclusive", func(t *testing.T) {
		err := s.probeClass(boom, alwaysFalse, alwaysTrue)
		if !errors.Is(err, domain.ErrConnectionProbeInconclusive) {
			t.Fatalf("err = %v, want ErrConnectionProbeInconclusive", err)
		}
		if errors.Is(err, domain.ErrConnectionProbeFailed) {
			t.Error("the failed sentinel is attached to an inconclusive outcome; it is the service layer's echo allowlist and maps to OK: false")
		}
	})

	t.Run("neither predicate is a service defect, not a verdict", func(t *testing.T) {
		err := s.probeClass(boom, alwaysFalse, alwaysFalse)
		if !errors.Is(err, domain.ErrServiceDefect) {
			t.Fatalf("err = %v, want ErrServiceDefect so the service returns a typed 500 rather than telling the operator to audit fields the request never consulted", err)
		}
		if !errors.Is(err, domain.ErrConnectionProbeRequestRejected) {
			t.Error("the reason token is missing; ErrServiceDefect selects the status, and the response carries no detail, so the token is all an operator reading the log gets")
		}
		if errors.Is(err, domain.ErrConnectionProbeInconclusive) {
			t.Error("classified as inconclusive, which maps to OK: true — a whole platform's connection tests would silently stop testing anything the day an endpoint moves")
		}
	})

	t.Run("the platform error chain is dropped, not wrapped", func(t *testing.T) {
		const canary = "DO-NOT-LEAK https://graph.facebook.com/v21.0/me/adaccounts?access_token=SECRET"
		for _, err := range []error{
			s.probeClass(errors.New(canary), alwaysTrue, alwaysFalse),
			s.probeClass(errors.New(canary), alwaysFalse, alwaysTrue),
			s.probeClass(errors.New(canary), alwaysFalse, alwaysFalse),
		} {
			if strings.Contains(err.Error(), "DO-NOT-LEAK") {
				t.Errorf("classified error %q carries the platform chain; the rejection arm is echoed verbatim to the caller, and several of these clients render request URLs and raw bodies", err)
			}
		}
	})
}

func TestProbeMembership(t *testing.T) {
	t.Run("configured account present", func(t *testing.T) {
		s := probeSubject{platform: model.ProviderGoogleAds, accountID: "8666746580"}
		if err := s.probeMembership([]string{"1234567890", "8666746580"}, nil); err != nil {
			t.Fatalf("probeMembership = %v, want nil", err)
		}
	})

	t.Run("configured account absent is a confirmed failure", func(t *testing.T) {
		s := probeSubject{platform: model.ProviderGoogleAds, accountID: "8666746580"}
		err := s.probeMembership([]string{"1234567890"}, nil)
		if !errors.Is(err, domain.ErrConnectionProbeFailed) {
			t.Fatalf("err = %v, want ErrConnectionProbeFailed", err)
		}
		if !strings.Contains(err.Error(), "8666746580") {
			t.Errorf("message %q does not name the account that was not reached", err)
		}
	})

	t.Run("no account configured", func(t *testing.T) {
		s := probeSubject{platform: model.ProviderGoogleAds}
		err := s.probeMembership([]string{"1234567890"}, nil)
		if !errors.Is(err, domain.ErrConnectionProbeFailed) {
			t.Fatalf("err = %v, want ErrConnectionProbeFailed; a connection naming no account has nothing to verify and cannot run a campaign", err)
		}
		if !strings.Contains(err.Error(), "names no ad account") {
			t.Errorf("message %q does not distinguish an unconfigured account from an unreachable one; the remedies differ", err)
		}
	})

	t.Run("normalize is applied to BOTH sides", func(t *testing.T) {
		// Meta's enumeration answers with `act_`-prefixed ids and the connection may store
		// either spelling. Normalizing only one side would report a working connection broken
		// over a prefix.
		s := probeSubject{platform: model.ProviderMetaAds, accountID: "193556282970417"}
		if err := s.probeMembership([]string{"act_193556282970417"}, trimMetaAccountPrefix); err != nil {
			t.Fatalf("probeMembership = %v with the stored id unprefixed, want nil", err)
		}
		s.accountID = "act_193556282970417"
		if err := s.probeMembership([]string{"193556282970417"}, trimMetaAccountPrefix); err != nil {
			t.Fatalf("probeMembership = %v with the enumerated id unprefixed, want nil", err)
		}
	})

	t.Run("an empty enumerated id never matches", func(t *testing.T) {
		// A blank entry in the platform's answer must not satisfy a blank-after-normalization
		// configured id, or a malformed response would read as a passing test.
		s := probeSubject{platform: model.ProviderMetaAds, accountID: "act_"}
		if err := s.probeMembership([]string{"", "  "}, trimMetaAccountPrefix); err == nil {
			t.Fatal("probeMembership = nil for a configured id that normalizes to empty against blank entries")
		}
	})

	t.Run("surrounding whitespace does not break a match", func(t *testing.T) {
		s := probeSubject{platform: model.ProviderMicrosoftAds, accountID: " 1234 "}
		if err := s.probeMembership([]string{"1234"}, nil); err != nil {
			t.Fatalf("probeMembership = %v, want nil", err)
		}
	})
}

func TestProbeSubjectWhere(t *testing.T) {
	if got := (probeSubject{}).where(); got != "" {
		t.Errorf("where() = %q for an unconfigured account, want empty so the message does not read \"for account \"", got)
	}
	if got := (probeSubject{accountID: "  x  "}).where(); got != " for account x" {
		t.Errorf("where() = %q, want the id trimmed", got)
	}
}
