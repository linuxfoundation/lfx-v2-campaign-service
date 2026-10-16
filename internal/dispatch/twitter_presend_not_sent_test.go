// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
)

// A connection test whose request the X client cannot sign never reached X: the verdict stays
// inconclusive and carries the not-attempted marker, so the metrics arm books no upstream-call
// sample against X for a fault in this deployment's configuration.
//
// This is the dispatch-level reach of the client's pre-send marker. The keyword write levers
// cannot be driven into it from here: each reads the line item and its criteria through the same
// client first, so a URL that cannot be signed fails the read before any write — their NOT_SENT
// mapping is pinned in the twitter package, at the predicate the dispatcher's arm reads.
func TestTwitter_ProbeConnection_UnsignableRequestIsNotAttempted(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	t.Cleanup(srv.Close)
	d := NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{},
		twitter.WithBaseURL(srv.URL+"/%zz"), twitter.WithWriteDelay(0))

	err := d.ProbeConnection(context.Background(), "proj", model.ProviderTwitterAds)
	if !errors.Is(err, domain.ErrConnectionProbeInconclusive) {
		t.Fatalf("ProbeConnection = %v, want inconclusive", err)
	}
	if !errors.Is(err, domain.ErrConnectionProbeNotAttempted) {
		t.Errorf("ProbeConnection = %v; the not-attempted marker is missing, so X is charged an error sample for a request it never received", err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("server saw %d requests, want 0", n)
	}
}
