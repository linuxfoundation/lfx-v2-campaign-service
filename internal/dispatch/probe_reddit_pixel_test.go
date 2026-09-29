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

// redditConnWithoutPixel is activeRedditConn with the conversion pixel removed — a connection
// saved before that column existed, or one an UpdateRedditAds that omitted the field cleared.
// Both are reachable states, not hypotheticals.
func redditConnWithoutPixel(creds string) *model.Connection {
	c := activeRedditConn(creds)
	c.ProviderConfig = map[string]string{}
	return c
}

// TestRedditProbe_MissingConversionPixelFailsTheTest covers the defect this endpoint exists to
// remove, in the one shape the credential and account checks cannot see.
//
// reddit.Client.CreateCampaign refuses EVERY objective when no conversion pixel is configured
// — not only the documented conversions case; the live API was confirmed on 2026-08-13 to
// reject a CLICKS create with {"field":"conversion_pixel_id"} — and it refuses before any
// upstream call, so the rejection is certain. A probe that stopped at VerifyAccount therefore
// answered OK: true for a connection guaranteed to fail at first use, which is precisely the
// "tests clean, fails on dispatch" failure LFXV2-2665 is about.
func TestRedditProbe_MissingConversionPixelFailsTheTest(t *testing.T) {
	// atomic: the handler runs on its own goroutine and the assertions read this from the test
	// goroutine.
	var accountReads atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "access_token") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"bearer"}`)
			return
		}
		accountReads.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"t2_acct"}`)
	}))
	defer srv.Close()

	d := NewRedditDispatcher(fakeConnReader{conn: redditConnWithoutPixel(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(srv.URL), reddit.WithTokenURL(srv.URL+"/api/v1/access_token"))

	err := d.ProbeConnection(context.Background(), "tlf", model.ProviderRedditAds)
	if err == nil {
		t.Fatal("ProbeConnection reported a healthy connection that names no conversion pixel; " +
			"reddit refuses every campaign create without one, so this connection cannot dispatch")
	}
	if !errors.Is(err, domain.ErrConnectionProbeFailed) {
		t.Errorf("ProbeConnection = %v, want a CONFIRMED failure: whether a pixel is configured "+
			"is known for certain, not something an inconclusive answer should hedge", err)
	}
	if errors.Is(err, domain.ErrConnectionProbeInconclusive) {
		t.Errorf("ProbeConnection = %v, must not be inconclusive: that reports an unreachable platform", err)
	}

	// The verdict must name the missing field and must NOT read as a credential problem — the
	// credential is fine, and sending the operator to re-authorise it wastes the one thing this
	// endpoint is supposed to save them.
	msg := err.Error()
	if !strings.Contains(msg, "conversion pixel id") {
		t.Errorf("verdict %q does not name the missing field, so it does not say what to fix", msg)
	}
	if !strings.Contains(msg, "credential reaches") {
		t.Errorf("verdict %q does not say the credential authenticated; an operator reading this "+
			"cannot tell it is not a credential problem", msg)
	}

	// Ordering is part of the contract: reachability is checked FIRST, so a dead credential is
	// reported as a dead credential rather than as a missing pixel. Reaching this verdict
	// therefore means a real upstream call happened, which is why this verdict is deliberately
	// NOT marked not-attempted.
	if accountReads.Load() == 0 {
		t.Error("the probe never read the account, so it answered about the pixel without " +
			"establishing that the credential works at all")
	}
	if errors.Is(err, domain.ErrConnectionProbeNotAttempted) {
		t.Error("the missing-pixel verdict is marked not-attempted, but it is reached only after " +
			"a completed account read; marking it deletes a genuine upstream call from the metrics")
	}
}

// TestRedditProbe_DeadCredentialIsReportedBeforeTheMissingPixel is the ordering half.
//
// A connection can be broken twice over. Answering the pixel first would hand the operator a
// field to fill in on a connection whose real problem is an unusable credential — they fix the
// pixel, re-test, and learn the actual problem on the second round trip.
func TestRedditProbe_DeadCredentialIsReportedBeforeTheMissingPixel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "access_token") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"t2_acct"}`)
	}))
	defer srv.Close()

	d := NewRedditDispatcher(fakeConnReader{conn: redditConnWithoutPixel(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(srv.URL), reddit.WithTokenURL(srv.URL+"/api/v1/access_token"))

	err := d.ProbeConnection(context.Background(), "tlf", model.ProviderRedditAds)
	if err == nil {
		t.Fatal("ProbeConnection reported a healthy connection whose refresh token reddit refused")
	}
	if strings.Contains(err.Error(), "conversion pixel") {
		t.Errorf("verdict %q blames the pixel on a connection whose credential reddit rejected; "+
			"the pixel check must run only after reachability is established", err)
	}
}
