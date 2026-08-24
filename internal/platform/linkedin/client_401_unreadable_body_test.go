// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// unauthorizedTruncatedBody serves a 401 whose body CANNOT be read to completion: it
// advertises a Content-Length far larger than what it writes, then hijacks and closes the
// connection, so buf.ReadFrom fails with an unexpected-EOF class error. This is the real
// shape of the gap — a mid-flight reset after the status line is on the wire — and it
// reaches doRequest's body-read-failure arm rather than the readable-body 401 arm.
func unauthorizedTruncatedBody() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "500")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("short"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		}
	}
}

// unauthorizedOversizedBody serves a 401 whose body EXCEEDS maxResponseBytes, reaching
// doRequest's over-cap arm — the second path that returned before the 401 arm.
func unauthorizedOversizedBody() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"x":"` + strings.Repeat("A", maxResponseBytes+100) + `"}`))
	}
}

// TestDoRequest_401WithUnreadableBodyIsStillAnExpiry is the core statement of the gap the
// previous fix left behind. The 401-ambiguity fix classified 401s only on the arm that
// successfully READ the response body. Two earlier arms — a body-read failure and an
// over-cap body — returned a bare *apiError first, and createOutcomeAmbiguous's apiError
// arm covers only 3xx/429/5xx, so a mutating POST answered 401 on either arm classified as
// a DEFINITE failure: CreateCampaign returned nil, the dispatcher RELEASED the claim, and a
// retry could duplicate a campaign group LinkedIn may already have committed and be billing.
//
// An unreadable body does not make a 401 any less a 401. Both facts the status carries must
// survive on every arm: the reconnect signal AND, on a mutating method, the ambiguity.
//
// Method-gated across POST and GET in one table so the fix cannot become "every 401 is
// ambiguous": the GET rows are what keep a search 401 a plain expiry.
func TestDoRequest_401WithUnreadableBodyIsStillAnExpiry(t *testing.T) {
	arms := map[string]http.HandlerFunc{
		"body read fails mid-flight": unauthorizedTruncatedBody(),
		"body exceeds the cap":       unauthorizedOversizedBody(),
	}
	for armName, handler := range arms {
		t.Run(armName, func(t *testing.T) {
			for _, method := range []string{http.MethodPost, http.MethodGet} {
				t.Run(method, func(t *testing.T) {
					srv := httptest.NewServer(handler)
					defer srv.Close()

					c := NewClient(Credentials{AccessToken: "t"}, testConfig(),
						WithBaseURL(srv.URL), WithClock(fixedClock()))

					_, err := c.doRequest(context.Background(), method, "adCampaignGroups",
						map[string]any{"a": 1}, nil, nil)

					// The actionable half: the operator must still be told to reconnect. A bare
					// apiError here is an opaque upstream error the dispatcher's
					// ErrCredentialsExpired re-tag can never match.
					var ce *credentialsExpiredError
					if !errors.As(err, &ce) {
						t.Fatalf("err = %v (%T), want a *credentialsExpiredError — an unreadable "+
							"body does not make a 401 any less a 401", err, err)
					}
					if !errors.Is(err, ErrCredentialsExpired) {
						t.Errorf("err = %v, want ErrCredentialsExpired", err)
					}
					// The classification half: the fields the classifier reads must be populated
					// on THIS arm, not just on the readable-body arm.
					if ce.Method != method {
						t.Errorf("Method = %q, want %q — without it the method gate cannot classify", ce.Method, method)
					}
					if ce.StatusCode != http.StatusUnauthorized {
						t.Errorf("StatusCode = %d, want %d", ce.StatusCode, http.StatusUnauthorized)
					}
					// And the outcome: ambiguous for a mutation, definite for a read.
					wantAmbiguous := method == http.MethodPost
					if got := IsOutcomeUnconfirmed(err); got != wantAmbiguous {
						t.Errorf("IsOutcomeUnconfirmed = %v, want %v for a %s 401 whose body was unreadable: "+
							"a mutating 401 may follow a committed create, so classifying it definite "+
							"releases the dispatch claim on a possibly-billing resource", got, wantAmbiguous, method)
					}
				})
			}
		})
	}
}

// TestDoRequest_401WithUnreadableBodyInvalidatesCachedToken pins the SECOND thing the gap
// cost, which the classification assertions above cannot see: the read-failure and over-cap
// arms never called invalidateAccessToken(), so a token LinkedIn had already rejected
// survived in the cache and was replayed by the next caller.
//
// Observed through the token-exchange count over two calls: with invalidation the second
// call must re-exchange (2 exchanges); without it the cached token is replayed and the
// count stays at 1.
func TestDoRequest_401WithUnreadableBodyInvalidatesCachedToken(t *testing.T) {
	arms := map[string]http.HandlerFunc{
		"body read fails mid-flight": unauthorizedTruncatedBody(),
		"body exceeds the cap":       unauthorizedOversizedBody(),
	}
	for armName, handler := range arms {
		t.Run(armName, func(t *testing.T) {
			var exchanges int32
			tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&exchanges, 1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"access_token":"minted","expires_in":3600}`))
			}))
			defer tokenSrv.Close()

			apiSrv := httptest.NewServer(handler)
			defer apiSrv.Close()

			c := NewClient(
				Credentials{RefreshToken: "rt", ClientID: "cid", ClientSecret: "sec",
					ConnectionName: "the LinkedIn connection"},
				testConfig(), WithBaseURL(apiSrv.URL), withTokenURL(tokenSrv.URL))

			for i := 0; i < 2; i++ {
				_, err := c.doRequest(context.Background(), http.MethodPost, "adCampaignGroups",
					map[string]any{"a": 1}, nil, nil)
				if !errors.Is(err, ErrCredentialsExpired) {
					t.Fatalf("call %d: err = %v, want ErrCredentialsExpired", i, err)
				}
			}

			if got := atomic.LoadInt32(&exchanges); got != 2 {
				t.Errorf("token exchanges = %d, want 2: a 401 whose body could not be read must "+
					"still evict the rejected token from the cache, or the next caller replays a "+
					"token LinkedIn has already refused", got)
			}
		})
	}
}

// TestUpdateCampaignStatus_401WithUnreadableBodyIsUnconfirmed covers the skipBody path,
// which has the SAME hole and is not reached by the doRequest table above. A status update
// passes noResponseBody=true, but the skipBody early-returns are gated on a 2xx — so a
// NON-2xx falls through to exactly the same read-failure/over-cap arms. The status cascade
// tunnels PARTIAL_UPDATE over POST, so a 401 there is a mutating 401 and must report the
// toggle UNCONFIRMED rather than a clean failure.
func TestUpdateCampaignStatus_401WithUnreadableBodyIsUnconfirmed(t *testing.T) {
	arms := map[string]http.HandlerFunc{
		"body read fails mid-flight": unauthorizedTruncatedBody(),
		"body exceeds the cap":       unauthorizedOversizedBody(),
	}
	for armName, handler := range arms {
		t.Run(armName, func(t *testing.T) {
			srv := httptest.NewServer(handler)
			defer srv.Close()

			c := NewClient(Credentials{AccessToken: "t"}, testConfig(),
				WithBaseURL(srv.URL), WithClock(fixedClock()))

			err := c.UpdateCampaignStatus(context.Background(), "555", StatusPaused)
			if err == nil {
				t.Fatal("a 401 must not read as a successful status update")
			}
			if !errors.Is(err, ErrCredentialsExpired) {
				t.Errorf("err = %v, want ErrCredentialsExpired — the skipBody path must classify "+
					"a 401 like every other arm", err)
			}
			if !IsOutcomeUnconfirmed(err) {
				t.Errorf("err = %v, want IsOutcomeUnconfirmed: the status update is tunneled over "+
					"POST, so a 401 may follow a flip LinkedIn already applied — reporting it "+
					"definite leaves the DB disagreeing with upstream", err)
			}
		})
	}
}

// TestCreateCampaign_GroupPOST401UnreadableBodyRetainsClaim is the end-to-end statement:
// the same defect the readable-body case already pins, but reached through the unreadable
// body arms. CreateCampaign must return a NON-NIL partial result plus an UNCONFIRMED error
// — the dispatcher's claim-retention rule keys on `result == nil` alone, so a nil here
// releases the claim and orphans a billable campaign group.
func TestCreateCampaign_GroupPOST401UnreadableBodyRetainsClaim(t *testing.T) {
	arms := map[string]http.HandlerFunc{
		"body read fails mid-flight": unauthorizedTruncatedBody(),
		"body exceeds the cap":       unauthorizedOversizedBody(),
	}
	for armName, handler := range arms {
		t.Run(armName, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// The find-existing lookups are GETs and must succeed so the flow reaches the
				// create POST; otherwise the test would pass for the wrong reason.
				if r.Method == http.MethodGet {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"elements":[],"metadata":{}}`))
					return
				}
				handler(w, r)
			}))
			defer srv.Close()

			c := NewClient(Credentials{AccessToken: "t"}, testConfig(),
				WithBaseURL(srv.URL), WithClock(fixedClock()))
			res, err := c.CreateCampaign(context.Background(), validCampaignInput())

			if err == nil {
				t.Fatal("expected an error when the group create POST is answered 401")
			}
			if res == nil {
				t.Fatal("result = nil; want a NON-NIL partial result — a 401 whose body could not " +
					"be read can still follow a committed group create, and the dispatcher retains " +
					"the claim on result != nil alone, so a nil here orphans a billable group")
			}
			if !IsOutcomeUnconfirmed(err) {
				t.Errorf("err = %v; want IsOutcomeUnconfirmed — the group create outcome is unknowable", err)
			}
			if !errors.Is(err, ErrCredentialsExpired) {
				t.Errorf("err = %v; want ErrCredentialsExpired to remain reachable", err)
			}
		})
	}
}

// TestGetCampaignMetrics_401WithUnreadableBodyInvalidatesCachedToken covers metrics.go's
// PARALLEL request path, which had the same structural hole. Its ambiguity classification
// was never wrong (the method is a hard-coded GET, which the method gate keeps definite),
// but the CACHE-INVALIDATION half was: a 401 whose body could not be read returned a bare
// apiError, leaving the rejected token cached for the next read to replay and giving the
// operator an opaque upstream error instead of the reconnect signal.
func TestGetCampaignMetrics_401WithUnreadableBodyInvalidatesCachedToken(t *testing.T) {
	arms := map[string]http.HandlerFunc{
		"body read fails mid-flight": unauthorizedTruncatedBody(),
		"body exceeds the cap":       unauthorizedOversizedBody(),
	}
	for armName, handler := range arms {
		t.Run(armName, func(t *testing.T) {
			var exchanges int32
			tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&exchanges, 1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"access_token":"minted","expires_in":3600}`))
			}))
			defer tokenSrv.Close()

			apiSrv := httptest.NewServer(handler)
			defer apiSrv.Close()

			c := NewClient(
				Credentials{RefreshToken: "rt", ClientID: "cid", ClientSecret: "sec",
					ConnectionName: "the LinkedIn connection"},
				RuntimeConfig{DefaultAccountID: "account123"},
				WithBaseURL(apiSrv.URL), withTokenURL(tokenSrv.URL))

			ctx := context.Background()
			for i := 0; i < 2; i++ {
				_, err := c.GetCampaignMetrics(ctx, "account123", "123456", model.MetricsWindowToday)
				if !errors.Is(err, ErrCredentialsExpired) {
					t.Fatalf("call %d: err = %v, want ErrCredentialsExpired — a metrics 401 whose "+
						"body is unreadable is still an expiry, and a bare apiError is exactly the "+
						"opaque upstream error the dispatcher re-tag can never match", i, err)
				}
				// A read created nothing: the method gate must keep it DEFINITE.
				if IsOutcomeUnconfirmed(err) {
					t.Errorf("call %d: err = %v; must NOT be unconfirmed — an analytics GET creates nothing", i, err)
				}
			}

			if got := atomic.LoadInt32(&exchanges); got != 2 {
				t.Errorf("token exchanges = %d, want 2: the rejected token must be evicted even when "+
					"the 401's body could not be read", got)
			}
		})
	}
}
