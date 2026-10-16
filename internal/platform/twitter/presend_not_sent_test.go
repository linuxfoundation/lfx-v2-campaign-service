// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// countingServer is an X stand-in that answers 200 {} and counts every request it receives, so a
// test can prove a failure happened before anything was transmitted.
func countingServer(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &n
}

// unparsableBase is the server's own origin with a path segment url.Parse refuses ("%zz" is not a
// valid escape), so buildOAuthHeader fails while the host stays the counting server's.
func unparsableBase(base string) string { return base + "/%zz" }

// assertNotSent is the shared verdict: the error carries ErrWriteNotSent (what the dispatcher's
// NOT_SENT arm reads), ProbeNotSent recognises it, it is not classified unconfirmed, it keeps the
// cause reachable, it does not render the request URL, and nothing reached the server.
func assertNotSent(t *testing.T, err error, base string, hits *atomic.Int32) {
	t.Helper()
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if !errors.Is(err, ErrWriteNotSent) {
		t.Errorf("errors.Is(err, ErrWriteNotSent) = false for %v; the dispatcher would stamp a request X never saw REJECTED", err)
	}
	if !ProbeNotSent(err) {
		t.Errorf("ProbeNotSent = false for %v", err)
	}
	if IsOutcomeUnconfirmed(err) {
		t.Errorf("a request that never left the process was classified unconfirmed: %v", err)
	}
	if u, _ := url.Parse(base); u != nil && strings.Contains(err.Error(), u.Host) {
		t.Errorf("error renders the request URL: %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("server saw %d requests, want 0", n)
	}
}

// Every pre-transmission exit of the X client's send paths carries ErrWriteNotSent: the two in
// doRequestAbs (sign, build) and the sign exit of doBodyRequest. doBodyRequest's build exit is
// unreachable once signing succeeded (its method is fixed and the URL already parsed), so it is
// marked for consistency and has no case here.
func TestPreSendExits_AreNotSent(t *testing.T) {
	t.Run("doRequestAbs: request cannot be signed", func(t *testing.T) {
		base, hits := countingServer(t)
		c := testClient(unparsableBase(base))
		_, err := c.doRequest(context.Background(), http.MethodPost, "targeting_criteria", map[string]string{"line_item_id": "li1"}, false)
		assertNotSent(t, err, base, hits)
		var ue *url.Error
		if !errors.As(err, &ue) {
			t.Errorf("the url.Parse cause is not reachable through %v", err)
		}
	})
	t.Run("doRequestAbs: request cannot be built", func(t *testing.T) {
		base, hits := countingServer(t)
		c := testClient(base)
		// http.NewRequestWithContext refuses a method that is not an RFC 7230 token; signing does not.
		_, err := c.doRequest(context.Background(), "BAD METHOD", "targeting_criteria", nil, false)
		assertNotSent(t, err, base, hits)
	})
	t.Run("doBodyRequest: upload cannot be signed", func(t *testing.T) {
		base, hits := countingServer(t)
		c := testClient(base)
		c.uploadBaseURL = unparsableBase(base)
		_, err := c.doBodyRequest(context.Background(), c.uploadBaseURL+"/2/media/upload/initialize", "/2/media/upload/initialize", "application/json", []byte(`{}`))
		assertNotSent(t, err, base, hits)
	})
}

// The two keyword write levers the dispatcher classifies per item: a request that could not be
// signed reaches their callers as ErrWriteNotSent (NOT_SENT), never as a refusal X did not issue.
func TestKeywordWriteLevers_UnsignableRequestIsNotSent(t *testing.T) {
	t.Run("create negative keyword", func(t *testing.T) {
		base, hits := countingServer(t)
		c := testClient(unparsableBase(base))
		_, err := c.CreateNegativeKeywordCriterion(context.Background(), "li1", TargetingBroadKeyword, "jobs")
		assertNotSent(t, err, base, hits)
	})
	t.Run("delete targeting criterion", func(t *testing.T) {
		base, hits := countingServer(t)
		c := testClient(unparsableBase(base))
		err := c.DeleteTargetingCriterion(context.Background(), "k1")
		assertNotSent(t, err, base, hits)
	})
}
