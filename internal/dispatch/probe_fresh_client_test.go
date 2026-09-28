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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
)

// TestRedditProbe_DoesNotAnswerFromACachedAccessToken pins that the Reddit probe presents the
// STORED refresh token rather than whatever access token an earlier dispatch left behind.
//
// Two caches compose to produce the defect, and neither is wrong on its own. reddit.Client holds
// its access token until the expiry buffer, so refreshToken's fast path returns it without a
// token-endpoint round trip; d.clients.buildOnce holds the CLIENT for the life of the connection
// row version, so that access token outlives the call that minted it. A probe served from that
// cache therefore never presents the refresh token at all, and a refresh token revoked an hour
// ago answers OK: true until the access token ages out — the exact production failure this
// endpoint was built to catch, reproduced by the endpoint that exists to catch it.
//
// The test seeds the cache the way production does (a dispatch-shaped call that mints a token),
// then revokes the refresh token upstream and probes. Before the fix the probe returned nil.
func TestRedditProbe_DoesNotAnswerFromACachedAccessToken(t *testing.T) {
	// atomic because the handler runs on its own goroutine and the test goroutine flips revoked
	// between the seeding call and the probe.
	var revoked atomic.Bool
	var tokenCalls atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "access_token") {
			tokenCalls.Add(1)
			if revoked.Load() {
				// What Reddit returns for a refresh token that has been revoked.
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"bearer"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"t2_acct"}`)
	}))
	defer srv.Close()

	d := NewRedditDispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(srv.URL), reddit.WithTokenURL(srv.URL+"/api/v1/access_token"))

	// Seed both caches exactly as a dispatch would: resolve through the CACHED path, then make a
	// call that mints and stores an access token.
	seeded, _, err := d.resolveRedditClientWithCreds(context.Background(), "tlf", model.ProviderRedditAds, d.creds.resolveOwned)
	if err != nil {
		t.Fatalf("seeding the client cache failed: %v", err)
	}
	if verr := seeded.VerifyAccount(context.Background()); verr != nil {
		t.Fatalf("seeding call failed: %v", verr)
	}
	if tokenCalls.Load() == 0 {
		t.Fatal("the seeding call minted no token, so the cache this test is about was never populated")
	}

	// The refresh token is revoked at the platform. The cached ACCESS token is still valid.
	revoked.Store(true)
	before := tokenCalls.Load()

	err = d.ProbeConnection(context.Background(), "tlf", model.ProviderRedditAds)
	if err == nil {
		t.Fatal("ProbeConnection reported a healthy connection while the stored refresh token was " +
			"revoked; it answered from a cached access token and never presented the credential " +
			"the operator is asking about")
	}
	if !errors.Is(err, domain.ErrConnectionProbeFailed) {
		t.Errorf("ProbeConnection = %v, want a confirmed failure: Reddit evaluated this refresh "+
			"token and refused it on the merits", err)
	}
	if tokenCalls.Load() == before {
		t.Error("the probe made no token-endpoint request, so it cannot have tested the stored " +
			"refresh token whatever verdict it returned")
	}
}

// TestRedditProbe_DoesNotSeedTheDispatchCache pins the other direction: the throwaway client the
// probe builds must not become the client the next dispatch uses.
//
// Writing it back would hand a dispatch burst a token minted for a connection test, and would
// re-couple the two lifetimes this fix exists to separate — silently, since both callers would
// still work until a token aged out.
func TestRedditProbe_DoesNotSeedTheDispatchCache(t *testing.T) {
	var tokenCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "access_token") {
			tokenCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"bearer"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"t2_acct"}`)
	}))
	defer srv.Close()

	d := NewRedditDispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(srv.URL), reddit.WithTokenURL(srv.URL+"/api/v1/access_token"))

	if err := d.ProbeConnection(context.Background(), "tlf", model.ProviderRedditAds); err != nil {
		t.Fatalf("ProbeConnection = %v, want success against a healthy connection", err)
	}
	probeCalls := tokenCalls.Load()

	// A dispatch-shaped resolve afterwards must mint its OWN token, not inherit the probe's.
	client, _, err := d.resolveRedditClientWithCreds(context.Background(), "tlf", model.ProviderRedditAds, d.creds.resolveOwned)
	if err != nil {
		t.Fatalf("resolveRedditClientWithCreds = %v", err)
	}
	if verr := client.VerifyAccount(context.Background()); verr != nil {
		t.Fatalf("VerifyAccount = %v", verr)
	}
	if tokenCalls.Load() == probeCalls {
		t.Error("the dispatch path reused the probe's access token, so the probe wrote the shared " +
			"client cache; a connection test must not decide which token campaign creation runs on")
	}
}
