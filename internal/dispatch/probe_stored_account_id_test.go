// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
)

// assertStoredIDNotUsable is the shared assertion for the two probes that now hold the STORED
// account id to the rule dispatch will apply to it.
//
// All three parts matter. The verdict must be a confirmed failure, or the endpoint reports a
// connection that provably cannot dispatch as healthy. It must carry the not-attempted marker,
// because the platform never evaluated the credential and an operator sent to re-authorise a
// perfectly good credential is worse than no answer. And nothing may have been sent: a test that
// checked only the error would still pass if the request were made and its answer discarded,
// which is exactly the state that lets an unrelated outage classify inconclusive and answer with a
// platform that could not be reached.
func assertStoredIDNotUsable(t *testing.T, err error, upstreamHit bool) {
	t.Helper()
	if !errors.Is(err, domain.ErrConnectionProbeFailed) {
		t.Fatalf("ProbeConnection = %v, want a confirmed failure; a stored id no campaign create "+
			"will accept cannot be reported as a healthy connection", err)
	}
	if !errors.Is(err, domain.ErrConnectionProbeNotAttempted) {
		t.Error("the verdict is missing the not-attempted marker; it is decided from the stored row " +
			"alone, before the platform sees the credential")
	}
	if errors.Is(err, domain.ErrConnectionProbeInconclusive) {
		t.Error("the inconclusive sentinel reached a locally-decided verdict")
	}
	if upstreamHit {
		t.Error("the probe called upstream before checking the stored account id; the verdict is " +
			"decidable without sending anything, and sending first is what lets an outage overrule it")
	}
}

// metaEnumerates serves one ad account under the act_-prefixed node id Meta actually returns,
// and records that it was reached.
func metaEnumerates(t *testing.T, nodeID string, hit *atomic.Bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit.Store(true)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"`+nodeID+`","name":"LF Events"}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestMetaProbe_BareNumericStoredIDIsRefusedBeforeTheCall pins the gap the act_-stripping
// comparison left open.
//
// probeMembership normalises both sides with trimMetaAccountPrefix, so a legacy row storing the
// bare "123" compared equal to the enumerated "act_123" and the endpoint answered OK: true.
// meta.Client.CreateCampaign then rejects that same stored value on its own accountIDRE, because
// MetaDispatcher.Dispatch hands the stored id to meta.AccountConfig UNTOUCHED — nothing
// canonicalises it on the way to the platform. So the connection tested clean and failed on
// first use, which is the one outcome this endpoint exists to prevent.
//
// The upstream here deliberately DOES serve act_123: a probe that still trimmed would find the
// account and pass, so this fails loudly if the guard is removed and the normalisation relied on
// again.
func TestMetaProbe_BareNumericStoredIDIsRefusedBeforeTheCall(t *testing.T) {
	var hit atomic.Bool
	srv := metaEnumerates(t, "act_123", &hit)
	conn := activeMetaConn(goodMetaCreds)
	conn.AccountID = "123"
	d := NewMetaDispatcher(fakeConnReader{conn: conn}, identityEncryptor{}, meta.WithBaseURL(srv.URL))

	err := d.ProbeConnection(context.Background(), "tlf", model.ProviderMetaAds)
	assertStoredIDNotUsable(t, err, hit.Load())
	// accountIDNotUsable's sentence, not a reachability one: the operator's repair is the stored
	// field, and a "not reachable" verdict would send them to Meta's account picker instead.
	if !strings.Contains(err.Error(), "not a valid") {
		t.Errorf("verdict %q does not name the stored account id as the thing to repair", err)
	}
}

// TestMetaProbe_CanonicalStoredIDStillPasses is the other half: the guard must refuse only what
// dispatch would refuse. act_777 is what activeMetaConn stores and what Meta returns, so this
// connection is healthy and has to keep testing that way.
func TestMetaProbe_CanonicalStoredIDStillPasses(t *testing.T) {
	var hit atomic.Bool
	srv := metaEnumerates(t, "act_777", &hit)
	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL))

	if err := d.ProbeConnection(context.Background(), "tlf", model.ProviderMetaAds); err != nil {
		t.Fatalf("ProbeConnection = %v, want nil; act_777 is stored, enumerated and dispatchable", err)
	}
	if !hit.Load() {
		t.Error("the probe never enumerated; a healthy verdict has to rest on an upstream answer")
	}
}

// TestMicrosoftProbe_UnusableStoredAccountIDIsRefusedBeforeTheCall covers the same shape on
// Microsoft, where the guard had a second way to go missing.
//
// ProbeConnection validated customer_id but not account_id, and it builds its discovery client
// with CustomerID ONLY — so Client.validateAccountIDs, which applies microsoft.ValidateAccountID
// on the dispatch path, never ran here. validateMicrosoftConnection proves the id present, not
// that it names an account, and account_id is operator-settable through the connection config
// API, so "0" and a 19-digit value above MaxInt64 are both storable. Both cost an upstream
// enumeration they cannot benefit from, and a transient 503 on that enumeration classifies
// inconclusive — reporting an unreachable platform for a connection every campaign request
// deterministically rejects.
func TestMicrosoftProbe_UnusableStoredAccountIDIsRefusedBeforeTheCall(t *testing.T) {
	// The upstream is the canonical inconclusive failure, so a probe that enumerated first would
	// blame that outage rather than merely reaching the right verdict by a longer route.
	for _, accountID := range []string{"0", "007", "-1", "abc", "1.5", "9999999999999999999"} {
		t.Run(accountID, func(t *testing.T) {
			var hit atomic.Bool
			srv := unreachableUpstream(t, &hit)
			conn := activeMicrosoftConn(goodMicrosoftCreds)
			conn.AccountID = accountID
			d := NewMicrosoftDispatcher(fakeConnReader{conn: conn}, identityEncryptor{},
				microsoft.WithBaseURL(srv.URL), microsoft.WithCustomerBaseURL(srv.URL),
				microsoft.WithTokenURL(srv.URL+"/token"))

			err := d.ProbeConnection(context.Background(), "cncf", model.ProviderMicrosoftAds)
			assertStoredIDNotUsable(t, err, hit.Load())
		})
	}
}
