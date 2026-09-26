// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
)

// unreachableUpstream answers everything with 503 and records that it was reached at all.
//
// 503 is the point: it is the canonical INCONCLUSIVE failure, which answers with an advisory about
// a platform that could not be reached. A probe that enumerates before checking whether the
// connection names an account can therefore be pushed by an unrelated outage into sending the
// operator to wait that outage out, instead of naming the empty field on their own row.
func unreachableUpstream(t *testing.T, hit *atomic.Bool) *httptest.Server {
	t.Helper()
	// atomic.Bool, not a plain bool: httptest.Server runs each handler in its own goroutine and
	// the test goroutine reads this value in an assertion, so a plain flag is a data race that
	// make test (go test -race) fails on.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit.Store(true)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestProbeConnection_NoAccountIsDecidedBeforeTheCall covers the two enumerating probes whose
// empty-account verdict used to live in probeMembership, AFTER the upstream read.
//
// probeMembership reaches the same verdict, but only on the path where the enumeration
// SUCCEEDS. Deferring the check meant an unrelated 5xx classified inconclusive and answered
// "the platform could not be reached" for a connection that names no ad account at all — a
// connection that cannot run a campaign under any circumstances, whose one broken field goes
// unnamed while the operator waits out an outage that has nothing to do with it. Microsoft, Reddit and X already
// decided it first, through their own ErrAccountNotSelected arms.
func TestProbeConnection_NoAccountIsDecidedBeforeTheCall(t *testing.T) {
	t.Run("google ads", func(t *testing.T) {
		var hit atomic.Bool
		srv := unreachableUpstream(t, &hit)
		conn := activeGoogleAdsConn(goodGoogleAdsCreds)
		conn.AccountID = ""
		d := NewGoogleAdsDispatcher(fakeConnReader{conn: conn}, identityEncryptor{},
			googleads.WithBaseURL(srv.URL), googleads.WithTokenURL(srv.URL))

		err := d.ProbeConnection(context.Background(), "tlf", model.ProviderGoogleAds)
		assertNoAccountVerdict(t, err, hit.Load())
	})

	t.Run("meta", func(t *testing.T) {
		var hit atomic.Bool
		srv := unreachableUpstream(t, &hit)
		conn := activeMetaConn(goodMetaCreds)
		conn.AccountID = ""
		d := NewMetaDispatcher(fakeConnReader{conn: conn}, identityEncryptor{}, meta.WithBaseURL(srv.URL))

		err := d.ProbeConnection(context.Background(), "tlf", model.ProviderMetaAds)
		assertNoAccountVerdict(t, err, hit.Load())
	})
}

func assertNoAccountVerdict(t *testing.T, err error, upstreamHit bool) {
	t.Helper()
	if !errors.Is(err, domain.ErrConnectionProbeFailed) {
		t.Fatalf("ProbeConnection = %v, want a confirmed failure; an unconfigured account is a verdict, "+
			"and letting an upstream outage classify it inconclusive sends the operator to wait out that "+
			"outage instead of to the empty field on a connection that cannot dispatch", err)
	}
	if errors.Is(err, domain.ErrConnectionProbeInconclusive) {
		t.Error("the inconclusive sentinel reached a connection that names no account")
	}
	if upstreamHit {
		t.Error("the probe called upstream before checking whether the connection names an account; " +
			"the verdict is decidable without sending anything, and sending first is what lets an " +
			"outage overrule it")
	}
}

// TestRedditProbe_MalformedAccountIDDoesNotBlameTheCredential covers the one pre-send verdict
// that is reachable in practice.
//
// Reddit's client refuses an account id that cannot be concatenated into a request path, from
// its own guard, before anything is sent — so the credential was never evaluated. Classifying
// that as ProbeCredentialRejected told the operator their stored credential had been refused
// and sent them to re-authorise a connection whose credential is fine; leaving it to the
// inconclusive default would have been worse still, blaming an outage instead. The dispatcher decides
// it instead, and names the field that is actually broken.
func TestRedditProbe_MalformedAccountIDDoesNotBlameTheCredential(t *testing.T) {
	var hit atomic.Bool
	srv := unreachableUpstream(t, &hit)
	conn := activeRedditConn(goodRedditCreds)
	conn.AccountID = "not/an/id"
	d := NewRedditDispatcher(fakeConnReader{conn: conn}, identityEncryptor{},
		reddit.WithBaseURL(srv.URL), reddit.WithTokenURL(srv.URL))

	err := d.ProbeConnection(context.Background(), "tlf", model.ProviderRedditAds)
	if !errors.Is(err, domain.ErrConnectionProbeFailed) {
		t.Fatalf("ProbeConnection = %v, want a confirmed failure; an id no Reddit request can address "+
			"cannot dispatch a campaign, and the inconclusive default would blame an unreachable platform", err)
	}
	if strings.Contains(err.Error(), "credential") {
		t.Errorf("message %q blames the stored credential, which Reddit never saw; the remedy is the "+
			"account id on the connection row", err)
	}
}
